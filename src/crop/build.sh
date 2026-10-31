#!/usr/bin/env bash
# Builds vmoto-crop for the WiFiLink and copies the SigmaStar libraries to install/.
# Needs the OpenIPC builder tree from driver/build.sh (cross compiler + libraries): OPENIPC=~/builder/openipc
set -euo pipefail
cd "$(dirname "$0")"
O="${OPENIPC:-$HOME/builder/openipc}"
CC="$O/output/host/bin/arm-openipc-linux-gnueabihf-gcc"
L="$O/general/package/sigmastar-osdrv-infinity6e/files/lib"
# libmi_vpe also references MI_ISP_* (libmi_isp), which we never call -> allow undefined symbols
"$CC" -O2 -Wall -s -o ../../install/vmoto-crop vmoto-crop.c -L"$L" -lmi_vpe -lmi_sys -Wl,--allow-shlib-undefined
mkdir -p ../../install/lib
cp "$L/libmi_vpe.so" "$L/libmi_sys.so" ../../install/lib/
ls -la ../../install/vmoto-crop ../../install/lib/
