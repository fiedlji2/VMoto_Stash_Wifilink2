# RunCam WiFiLink → displej Carbit / EasyConn – technické poznámky

Ověřeno 30.–31. 10. 2026 na displeji Vmoto Stash: obraz z kamery IMX415 ve WiFiLinku přímo na displeji,
25 snímků/s, **zpoždění ~100 ms** (stopky, kamera → displej). Protokol displeje: [PROTOCOL.md](PROTOCOL.md).

## Hardware / firmware
SigmaStar SSC338Q (2× Cortex-A7, 91 MB RAM pro Linux), Sony IMX415, Wi-Fi RTL8812EU
(`8812eu.ko`, rtl88x2eu v5.15.0.1-249-g9245f8bd9), RubyFPV nad OpenIPC 2.6.03.04
(Buildroot 2024.02.10, jádro 4.9.84). Flash: overlay 5,7 MB. Ethernet 192.168.1.10.

## Co bylo potřeba
1. **Ovladač s Wi-Fi Direct.** Původní `8812eu.ko` je přeložený s `CONFIG_P2P = n` → `wpa_supplicant` hlásí
   „P2P disabled“. [driver/build.sh](../driver/build.sh) sestaví OpenIPC firmware pro `ssc338q_rubyfpv_generic`
   (OpenIPC builder) a ovladač přeloží znovu s `CONFIG_P2P = y` (~20 min napoprvé).
   Vermagic `4.9.84 SMP preempt mod_unload ARMv7 thumb2 p2v8` – `install.sh` ho porovná s ovladačem ve firmwaru.
   ```
   # Docker
   docker build -t wfl-driver driver && docker run --rm -v "$PWD/out:/out" wfl-driver
   # nebo přímo v Linuxu / WSL
   OUT="$PWD/out" bash driver/build.sh
   xz -9 --check=crc32 --lzma2=preset=9,dict=4MiB -c out/8812eu.ko > install/8812eu.ko.xz
   ```
2. **Wi-Fi Direct:** `wpa_supplicant` z firmwaru, displej je autonomní GO → `p2p_prov_disc <MAC> pbc join`
   a `p2p_connect <MAC> pbc join freq=<listen_freq>`. Skupinové rozhraní je přímo `wlan0`, DHCP `udhcpc`.
3. **Video:** majestic → `video0: h264, profile main, 1024x592, 25 fps, gopSize 0.5`,
   `outgoing: udp://127.0.0.1:5600` (RTP). `profile baseline` majestic nepřijme.
4. **Protokol displeje:** `vmoto-wfl` (Go, [src/](../src/)) – ohlášení 10930, PXC 10922 (C2P/P2C),
   média 10921/10920, RTP → H.264 access units, webová stránka.

## Nízké zpoždění
- **Úprava SPS** (`src/sps.go`): majestic (SigmaStar) kóduje H.264 Main bez VUI `bitstream_restriction` →
  dekodér displeje držel až ~7 snímků (DPB pro level 3.1, ≈400 ms celkem). Program do každého SPS doplní
  `max_num_reorder_frames=0`, `max_dec_frame_buffering=1` → ~100 ms (ověřeno `ffmpeg -bsf:v trace_headers`).
- **GOP 0,5 s s P-snímky** – výrazně lepší kvalita než jen IDR snímky při stejném datovém toku.
- **Fronta se srovná na každém IDR** – jinak zůstane trvale plná (displej bere snímky stejně rychle,
  jak přicházejí) a obraz je o 3–4 snímky pozadu.

## Instalace na WiFiLinku (co dělá `install/install.sh`)
- `/etc/vmoto/`: `8812eu.ko.xz`, `vmoto-wfl.xz`, `vmoto-loop.sh`, `vmoto.conf`, `restore-ruby.sh`, `VERSION`,
  záloha `majestic.yaml.ruby`, vypnutý `S73ruby.disabled`.
- `/etc/init.d/S95vmoto`: rozbalí ovladač a program do `/tmp` (RAM), `modprobe cfg80211` → `insmod 8812eu.ko`
  → majestic → `vmoto-loop.sh`.
- `/etc/majestic.yaml` podle `install/majestic-settings.txt` (`cli -s`), majestic na portu 8080.
- Logy v RAM: `/tmp/vmoto.log`, `/tmp/wfl.log`, `/tmp/majestic.log`.
- Pozastavit: `touch /etc/vmoto/DISABLE` + reboot. Vrátit RubyFPV: `/etc/vmoto/restore-ruby.sh` + reboot.

**`.xz` se slovníkem max. 4 MiB** (`xz -9 --check=crc32 --lzma2=preset=9,dict=4MiB`): busybox `xz` si alokuje
celý slovník z hlavičky; s výchozími 64 MiB při běžícím majesticu hlásí `corrupted data`.

**Vysílací výkon:** `iw dev wlan0 set txpower fixed <mBm>`; už `0` dává v `/proc/net/rtl88x2eu/wlan0/tx_power_idx`
hardwarový index 0 = minimum čipu. Spojení u motorky drží (RSSI −40 až −60 dBm). Výkon za zesilovačem modulu
nebyl měřen.

## Webová stránka (`src/web.go`, `src/web.html`)
http://192.168.1.10/, poslouchá jen na adrese `eth0`. Majestic je přesunutý na port 8080.
- Stav: displej (síť, kanál, signál, rychlost z `iw dev wlan0 link`), přenos, teplota čipu (`ipcinfo --temp`)
  a Wi-Fi modulu, expozice a zesílení snímače z majesticu `/metrics`, RAM, log.
- **Naživo** přes `/api/v1/set`: jas, kontrast, sytost, odstín, nejdelší expozice, anti-flicker, pomalá závěrka
  (`disabled/low/medium/high`, API vezme i nesmysl), datový tok, fps, GOP, řízení toku, `minQp/maxQp/qpDelta`.
  Každá změna znovu vytvoří kanál enkodéru (obraz běží dál). *Uložit* zapíše do `/etc/majestic.yaml`.
- **Restart majesticu** (~5 s bez obrazu, ukládá se hned): `isp.aGain` a `isp.wdr` API odmítne;
  `image.mirror/flip` API přijme, ale snímač nezmění (`/proc/mi_modules/mi_sensor/mi_sensor0`: `bmirror/bflip`).
  Po restartu stránka vrátí neuložené živé změny. `aGain` = strop analogového zesílení v násobcích
  (nad 64 omezeno na 64; bez klíče majestic hlásí `analog: 128`). Účinek `wdr` v logu vidět není.
- Černobílý režim: `/night/on|off`, stav `night_enabled` v `/metrics`, neukládá se.

## Zoom (nejde)
- `video0.crop` se zadává v souřadnicích výstupu (1024×592), zmenší i výsledné video (např. 512×296)
  a přes API ho majestic nevezme.
- Režimy snímače z ovladače: 3840×2160@30 (celý čip), 2560×1440@60 (středový výřez ~1,3×),
  1920×1080@90 (celé zorné pole, binning). Majestic vybírá podle `video0.fps`; displej ale nebere víc
  než ~25–30 snímků/s. Druhý kanál na 60 fps majestic omezí na 30.
- Skutečný zoom by vyžadoval vlastní obrazový program se vstupním výřezem škálovače (místo majesticu).

## Vývoj
```
cd src && ./build.sh              # vmoto-wfl pro ARMv7 + install/vmoto-wfl.xz (Go >= 1.18, xz)
go build -o vmoto-wfl-x86 . && python3 ../tools/test_fake_dash.py ./vmoto-wfl-x86   # simulovaný displej
```
Výměna programu bez restartu WiFiLinku: rozbalit do `/tmp/vmoto-wfl.new`, **nejdřív** `mv` přes `/tmp/vmoto-wfl`,
pak `killall vmoto-wfl` (jinak ho smyčka stihne znovu spustit ze starého souboru).
Binárky nahrávat přes `scp -O` (dropbear bez sftp).
