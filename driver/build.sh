#!/usr/bin/env bash
# Sestaví jádro a ovladač 8812eu pro WiFiLink (ssc338q_rubyfpv_generic) a ovladač přeloží s CONFIG_P2P=y.
# Výstup: /out/8812eu.ko (+ log a vermagic).
set -euo pipefail
# WSL přidává do PATH cesty z Windows s mezerami, Buildroot pak odmítne běžet
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
DEVICE="${DEVICE:-ssc338q_rubyfpv_generic}"
OUT="${OUT:-/out}"
mkdir -p "$OUT"
exec > >(tee -a "$OUT/build.log") 2>&1

cd "$HOME"
if [ ! -d builder ]; then
  git clone --depth=1 https://github.com/OpenIPC/builder.git
fi
cd builder
if [ ! -d openipc ]; then
  git clone --depth=1 https://github.com/OpenIPC/firmware.git openipc
  # soubory zařízení (defconfig, konfigurace jádra) do stromu firmwaru, stejně jako builder.sh
  cp -afv "devices/$DEVICE/"* openipc/
  [ -d package ] && cp -afv package/* openipc/general/package/ || true
fi
cd openipc

# 1) celé sestavení (toolchain, jádro, balíčky) - napoprvé trvá desítky minut
make BOARD="$DEVICE" 2>&1 | tail -n 40

BR=$(ls -d output/build 2>/dev/null || find . -maxdepth 3 -type d -name build -path '*output*' | head -1)
DRV=$(ls -d "$BR"/rtl88x2eu-openipc* | head -1)
echo "driver dir: $DRV"

# 2) zapnout P2P v ovladači a přeložit ho znovu
sed -i -E 's/^CONFIG_P2P[[:space:]]*=[[:space:]]*n/CONFIG_P2P = y/' "$DRV/Makefile"
sed -i -E 's/^CONFIG_AP_MODE[[:space:]]*=[[:space:]]*n/CONFIG_AP_MODE = y/' "$DRV/Makefile"
grep -nE '^CONFIG_(P2P|AP_MODE)[[:space:]]*=' "$DRV/Makefile"
rm -f "$DRV"/.stamp_built "$DRV"/.stamp_*installed* "$DRV"/*.ko
make BOARD="$DEVICE" br-rtl88x2eu-openipc-rebuild 2>&1 | tail -n 30 || \
  make -C "$(pwd)/buildroot"* O="$(pwd)/output" rtl88x2eu-openipc-rebuild 2>&1 | tail -n 30

KO=$(find "$DRV" -name '8812eu.ko' | head -1)
cp -v "$KO" "$OUT/8812eu.ko"
strings "$OUT/8812eu.ko" | grep -E '^(vermagic|version)=' | tee "$OUT/vermagic.txt"
echo "P2P symbols: $(strings "$OUT/8812eu.ko" | grep -c rtw_p2p_)" | tee -a "$OUT/vermagic.txt"
echo HOTOVO
