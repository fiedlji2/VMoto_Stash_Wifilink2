<#
.SYNOPSIS
  Nainstaluje Vmoto (obraz kamery na displej Vmoto Stash) na RunCam WiFiLink připojený ethernetem.

.DESCRIPTION
  Čistý WiFiLink (OpenIPC RubyFPV, SSC338Q + IMX415) převede do stavu z 31. 10. 2026:
  ovladač 8812eu s Wi-Fi Direct, vmoto-wfl (protokol displeje + webová stránka), automatický start,
  nastavení kamery z majestic-settings.txt. RubyFPV se jen vypne (vrácení: /etc/vmoto/restore-ruby.sh).

  Postup: PC v síti 192.168.1.x (např. 192.168.1.20) kabelem do WiFiLinku, pak
    powershell -ExecutionPolicy Bypass -File install-wifilink.ps1
  Na čistém WiFiLinku se jednou zeptá na heslo roota (výchozí OpenIPC: 12345) a nahraje SSH klíč.
  Spustit jde i znovu (aktualizace na verzi v této složce).

.PARAMETER Ip
  Adresa WiFiLinku na ethernetu (výchozí OpenIPC 192.168.1.10).
.PARAMETER Key
  SSH klíč. Když neexistuje, vytvoří se.
.PARAMETER NoReboot
  Po instalaci nerestartovat.
#>
param(
	[string]$Ip = "192.168.1.10",
	[string]$Key = "",
	[switch]$NoReboot
)
$ErrorActionPreference = "Stop"
$dir = $PSScriptRoot
$files = "install.sh", "S95vmoto", "vmoto-loop.sh", "restore-ruby.sh", "vmoto.conf", "majestic-settings.txt",
	"8812eu.ko.xz", "vmoto-wfl.xz", "vmoto-crop", "lib\libmi_sys.so", "lib\libmi_vpe.so"
$textFiles = "install.sh", "S95vmoto", "vmoto-loop.sh", "restore-ruby.sh", "vmoto.conf", "majestic-settings.txt"

function Step($t) { Write-Host "`n=== $t" -ForegroundColor Cyan }
function Fail($t) { Write-Host "CHYBA: $t" -ForegroundColor Red; exit 1 }

if (-not $Key) {
	$Key = "$env:USERPROFILE\.ssh\vmoto_wifilink_ed25519"
	if (-not (Test-Path $Key) -and (Test-Path "$env:USERPROFILE\.ssh\vmoto_pi_ed25519")) { $Key = "$env:USERPROFILE\.ssh\vmoto_pi_ed25519" }
}
# Pi i WiFiLink mají stejnou výchozí adresu 192.168.1.10 a po přeflashování se mění klíč hostitele:
# known_hosts se pro tohle přímé spojení kabelem nepoužívá
$sshOpts = "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=NUL", "-o", "LogLevel=ERROR", "-o", "ConnectTimeout=8"
$sshKey = @("-i", $Key) + $sshOpts + @("-o", "BatchMode=yes")

Step "Kontrola souborů a nástrojů"
foreach ($f in $files) { if (-not (Test-Path (Join-Path $dir $f))) { Fail "chybí $f" } }
foreach ($t in "ssh", "scp", "ssh-keygen") { if (-not (Get-Command $t -ErrorAction SilentlyContinue)) { Fail "chybí $t (Windows: Volitelné funkce -> Klient OpenSSH)" } }
$tcp = New-Object Net.Sockets.TcpClient
if (-not $tcp.ConnectAsync($Ip, 22).Wait(4000)) { Fail "WiFiLink na $Ip`:22 neodpovídá (kabel, IP počítače 192.168.1.x?)" }
$tcp.Close()
Write-Host "WiFiLink $Ip odpovídá, klíč $Key"

Step "SSH klíč"
if (-not (Test-Path $Key)) {
	New-Item -ItemType Directory -Force (Split-Path $Key) | Out-Null
	ssh-keygen -q -t ed25519 -N '""' -C vmoto-wifilink -f $Key
	Write-Host "vytvořen nový klíč $Key"
}
ssh @sshKey root@$Ip "true" 2>$null
if ($LASTEXITCODE -ne 0) {
	Write-Host "Klíč na WiFiLinku ještě není. Zadej heslo roota WiFiLinku (výchozí OpenIPC: 12345):" -ForegroundColor Yellow
	Get-Content "$Key.pub" | ssh @sshOpts root@$Ip "mkdir -p /etc/dropbear && cat >> /etc/dropbear/authorized_keys && chmod 600 /etc/dropbear/authorized_keys"
	ssh @sshKey root@$Ip "true" 2>$null
	if ($LASTEXITCODE -ne 0) { Fail "přihlášení klíčem nefunguje" }
}
Write-Host "přihlášení klíčem OK"

Step "Nahrání balíčku"
$tmp = Join-Path $env:TEMP "vmoto-install"
if (Test-Path $tmp) { Remove-Item -Recurse -Force $tmp }
New-Item -ItemType Directory $tmp | Out-Null
foreach ($f in $files) {
	$src = Join-Path $dir $f
	if ($textFiles -contains $f) {
		# skripty pro Linux: bez CR a bez BOM (git na Windows může konce řádků převést)
		$t = [IO.File]::ReadAllText($src).Replace("`r", "").TrimStart([char]0xFEFF)
		[IO.File]::WriteAllText((Join-Path $tmp $f), $t, (New-Object Text.UTF8Encoding $false))
	} else {
		Copy-Item $src $tmp
	}
}
$md5 = foreach ($f in "8812eu.ko.xz", "vmoto-wfl.xz") { "$((Get-FileHash (Join-Path $dir $f) -Algorithm MD5).Hash.ToLower())  $f" }
$ver = "nainstalováno $(Get-Date -Format 'yyyy-MM-dd HH:mm') z $env:COMPUTERNAME`n" + ($md5 -join "`n") + "`n"
[IO.File]::WriteAllText((Join-Path $tmp "VERSION"), $ver, (New-Object Text.UTF8Encoding $false))
ssh @sshKey root@$Ip "rm -rf /tmp/vmoto-install && mkdir -p /tmp/vmoto-install"
# scp -O: starý protokol (dropbear bez sftp)
$up = Get-ChildItem $tmp | ForEach-Object { $_.FullName }
scp -O -q @sshKey $up "root@${Ip}:/tmp/vmoto-install/"
if ($LASTEXITCODE -ne 0) { Fail "nahrání selhalo" }
$remote = ssh @sshKey root@$Ip "cd /tmp/vmoto-install && md5sum 8812eu.ko.xz vmoto-wfl.xz"
foreach ($line in $md5) { if ($remote -notcontains $line) { Fail "kontrolní součet nesedí: $line / $remote" } }
Write-Host "nahráno, kontrolní součty sedí"

Step "Instalace na WiFiLinku"
$out = ssh @sshKey root@$Ip "sh /tmp/vmoto-install/install.sh 2>&1; rm -rf /tmp/vmoto-install"
$out | ForEach-Object { Write-Host "  $_" }
if (-not ($out -match "INSTALL_OK")) { Fail "instalace neproběhla (výpis výše)" }

if ($NoReboot) { Write-Host "`nHotovo bez restartu. Projeví se po restartu WiFiLinku." -ForegroundColor Green; exit 0 }

Step "Restart a kontrola"
ssh @sshKey root@$Ip "sync; (sleep 1; reboot) >/dev/null 2>&1 &"
Start-Sleep 20
$st = $null
for ($i = 0; $i -lt 30 -and -not $st; $i++) {
	Start-Sleep 4
	try { $st = (Invoke-WebRequest -UseBasicParsing -TimeoutSec 4 "http://$Ip/api/status").Content | ConvertFrom-Json } catch { }
}
if (-not $st) { Fail "webová stránka http://$Ip/ po restartu neodpovídá" }
Write-Host "stav: $($st.state), majestic běží: $($st.majestic), teplota čipu $($st.device.soc_temp) °C"
Write-Host "`nHotovo. Stránka: http://$Ip/  (displej se připojí, až bude motorka zapnutá)" -ForegroundColor Green
