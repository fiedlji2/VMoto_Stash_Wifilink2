#!/bin/sh
# Instalace Vmoto na RunCam WiFiLink (OpenIPC RubyFPV). Soubory musí být v /tmp/vmoto-install/.
# Z PC to celé udělá install-wifilink.ps1. Jde spustit znovu (přeinstalace / aktualizace).
# Vrácení RubyFPV: /etc/vmoto/restore-ruby.sh + reboot
set -e
cd /tmp/vmoto-install
fail() { echo "CHYBA: $*"; exit 1; }

echo "--- kontrola WiFiLinku"
SOC="$(fw_printenv soc 2>/dev/null | cut -d= -f2)"
SENSOR="$(fw_printenv sensor 2>/dev/null | cut -d= -f2)"
KERNEL="$(uname -r)"
echo "SoC $SOC, snímač $SENSOR, jádro $KERNEL, $(grep OPENIPC_VERSION /etc/os-release)"
[ "$SOC" = ssc338q ] || fail "čekám SoC ssc338q"
[ "$SENSOR" = imx415 ] || fail "čekám snímač imx415"
[ "$KERNEL" = 4.9.84 ] || fail "ovladač 8812eu.ko je přeložený pro jádro 4.9.84"
xz -dc 8812eu.ko.xz > /tmp/8812eu-check.ko || fail "8812eu.ko.xz nejde rozbalit"
OURS="$(strings /tmp/8812eu-check.ko | grep '^vermagic=' || true)"
rm -f /tmp/8812eu-check.ko
STOCK_KO="$(find /lib/modules -name '8812eu.ko*' | head -1)"
if [ -n "$STOCK_KO" ]; then
	STOCK="$(strings "$STOCK_KO" | grep '^vermagic=' || true)"
	[ "$OURS" = "$STOCK" ] || fail "vermagic nesedí: náš '$OURS', firmware '$STOCK'"
fi
echo "ovladač: $OURS"
find /lib/modules -name 'cfg80211.ko*' | grep -q . || fail "ve firmwaru chybí cfg80211.ko"
xz -t vmoto-wfl.xz || fail "vmoto-wfl.xz nejde rozbalit"
command -v cli >/dev/null || fail "chybí nástroj cli (OpenIPC)"

echo "--- RubyFPV zastavit"
if [ -f /etc/init.d/S73ruby ]; then
	/etc/init.d/S73ruby stop >/dev/null 2>&1 || true
fi
killall ruby_start ruby_rt_vehicle ruby_tx_telemetry ruby_logger 2>/dev/null || true
/etc/init.d/S95vmoto stop >/dev/null 2>&1 || true

echo "--- soubory do flash"
mkdir -p /etc/vmoto
rm -f /etc/vmoto/*.old
cp 8812eu.ko.xz vmoto-wfl.xz vmoto-loop.sh restore-ruby.sh vmoto.conf /etc/vmoto/
chmod +x /etc/vmoto/vmoto-loop.sh /etc/vmoto/restore-ruby.sh
cp S95vmoto /etc/init.d/S95vmoto
chmod +x /etc/init.d/S95vmoto
[ -f VERSION ] && cp VERSION /etc/vmoto/VERSION

echo "--- majestic"
grep -q ' /etc/majestic.yaml ' /proc/mounts && umount /etc/majestic.yaml
[ -f /etc/vmoto/majestic.yaml.ruby ] || cp /etc/majestic.yaml /etc/vmoto/majestic.yaml.ruby
grep -v '^[[:space:]]*#' majestic-settings.txt | while read -r key val; do
	[ -n "$key" ] || continue
	if [ -n "$val" ]; then
		cli -s ".$key" "$val"
	else
		cli -d ".$key" || true
	fi
	echo "  $key = $val"
done

echo "--- RubyFPV vypnout (vratně)"
if [ -f /etc/init.d/S73ruby ]; then mv /etc/init.d/S73ruby /etc/vmoto/S73ruby.disabled; fi
rm -f /etc/vmoto/DISABLE
sync
ls -la /etc/vmoto
df -h /overlay | tail -1
echo INSTALL_OK
