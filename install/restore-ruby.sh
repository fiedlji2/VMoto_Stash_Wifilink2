#!/bin/sh
# Vrátí WiFiLink do původního stavu s RubyFPV (po restartu).
/etc/init.d/S95vmoto stop
rm -f /etc/init.d/S95vmoto
[ -f /etc/vmoto/S73ruby.disabled ] && mv /etc/vmoto/S73ruby.disabled /etc/init.d/S73ruby
[ -f /etc/vmoto/majestic.yaml.ruby ] && cp /etc/vmoto/majestic.yaml.ruby /etc/majestic.yaml
sync
echo "Hotovo, restartuj WiFiLink (reboot). Soubory v /etc/vmoto zůstaly, smazat: rm -rf /etc/vmoto"
