#!/bin/sh
# Smyčka na WiFiLinku: najdi displej Carbit/EasyConn přes Wi-Fi Direct, připoj se, drž vmoto-wfl v chodu.
# Když spojení spadne (vypnutá motorka), restartuje wpa_supplicant a hledá znovu.
# Pozastavení: touch /etc/vmoto/DISABLE
# Nastavení (MAC displeje, výkon): /etc/vmoto/vmoto.conf
# Bez DASH se připojí k jakémukoli displeji v dosahu (Wi-Fi Direct název RM_*), při více k nejsilnějšímu.
[ -f /etc/vmoto/vmoto.conf ] && . /etc/vmoto/vmoto.conf
DASH="${DASH:-}"
TXP="${TXP:-0}"              # cílový výkon v mBm; 0 už dává hardwarový index 0 = minimum čipu (tx_power_idx)
CTRL=/tmp/wpa_ctrl
W="wpa_cli -p $CTRL -i wlan0"

log() { echo "$(date '+%F %T') [vmoto] $*"; }

wifi_restart() {
  killall wpa_supplicant 2>/dev/null
  sleep 1
  cat > /tmp/p2p.conf <<EOF
ctrl_interface=DIR=$CTRL
update_config=0
country=US
device_name=WiFiLink
device_type=10-0050F204-5
config_methods=virtual_push_button physical_push_button
p2p_go_intent=0
EOF
  ip addr flush dev wlan0 2>/dev/null
  ip link set wlan0 down
  iw dev wlan0 set type managed 2>/dev/null
  ip link set wlan0 up
  iw dev wlan0 set txpower fixed "$TXP"
  wpa_supplicant -B -i wlan0 -c /tmp/p2p.conf -D nl80211 -f /tmp/wpa.log
  sleep 2
}

connected() { $W status 2>/dev/null | grep -q 'wpa_state=COMPLETED'; }

lower() { echo "$1" | tr 'A-F' 'a-f'; }

# vybere displej: pevná MAC z vmoto.conf, jinak nejsilnější zařízení RM_* -> PEER, PEER_FREQ
pick_dash() {
  best=""; best_lvl=-1000; best_freq=""; best_name=""
  for p in $($W p2p_peers 2>/dev/null); do
    info="$($W p2p_peer "$p" 2>/dev/null)"
    name="$(echo "$info" | sed -n 's/^device_name=//p')"
    lvl="$(echo "$info" | sed -n 's/^level=//p')"; lvl="${lvl:--999}"
    freq="$(echo "$info" | sed -n 's/^listen_freq=//p')"
    if [ -n "$DASH" ]; then
      [ "$(lower "$p")" = "$(lower "$DASH")" ] || continue
    else
      case "$name" in RM_*) ;; *) continue ;; esac
    fi
    if [ "$lvl" -gt "$best_lvl" ]; then best="$p"; best_lvl="$lvl"; best_freq="$freq"; best_name="$name"; fi
  done
  [ -n "$best" ] || return 1
  PEER="$best"; PEER_FREQ="$best_freq"
  log "displej $best_name ($best), signál $best_lvl dBm, kanál ${best_freq:-?} MHz"
}

find_and_join() {
  $W p2p_flush >/dev/null
  $W p2p_find 10 >/dev/null
  i=0
  while [ $i -lt 10 ]; do
    if pick_dash; then
      $W p2p_stop_find >/dev/null
      log "displej nalezen, připojuji"
      $W p2p_prov_disc "$PEER" pbc join >/dev/null
      sleep 3
      fopt=""
      [ -n "$PEER_FREQ" ] && [ "$PEER_FREQ" != 0 ] && fopt="freq=$PEER_FREQ"
      $W p2p_connect "$PEER" pbc join $fopt >/dev/null
      j=0
      while [ $j -lt 30 ]; do connected && return 0; sleep 1; j=$((j + 1)); done
      log "připojení se nepovedlo"
      return 1
    fi
    sleep 1; i=$((i + 1))
  done
  $W p2p_stop_find >/dev/null
  return 1
}

log "start, displej ${DASH:-jakýkoli Carbit (RM_*)}, výkon index $TXP"
wifi_restart
lost=0
while true; do
  if [ -f /etc/vmoto/DISABLE ]; then sleep 10; continue; fi
  if connected; then
    lost=0
    if ! ip -4 addr show wlan0 | grep -q 'inet 192.168.2.'; then
      udhcpc -i wlan0 -n -q -t 5 >/dev/null 2>&1 && log "IP $(ip -4 addr show wlan0 | awk '/inet /{print $2}')"
    fi
  else
    lost=$((lost + 1))
    if [ $lost -ge 3 ]; then
      log "displej není připojený, hledám"
      wifi_restart
      if find_and_join; then
        log "připojeno: $($W status | grep ^ssid=)"
        udhcpc -i wlan0 -n -q -t 5 >/dev/null 2>&1 && log "IP $(ip -4 addr show wlan0 | awk '/inet /{print $2}')"
      fi
      lost=0
      continue
    fi
  fi
  pgrep -f /tmp/vmoto-wfl >/dev/null || { log "spouštím vmoto-wfl"; /tmp/vmoto-wfl >> /tmp/wfl.log 2>&1 & }
  sleep 2
done
