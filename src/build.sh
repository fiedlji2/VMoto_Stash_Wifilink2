#!/usr/bin/env bash
# Přeloží vmoto-wfl pro WiFiLink (ARMv7) a zabalí ho do ../install/vmoto-wfl.xz. Potřeba Go >= 1.18 a xz.
set -euo pipefail
cd "$(dirname "$0")"
go vet .
go test .
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags '-s -w' -o vmoto-wfl .
# busybox xz na WiFiLinku si alokuje celý slovník z hlavičky: max. 4 MiB
xz -9 --check=crc32 --lzma2=preset=9,dict=4MiB -c vmoto-wfl > ../install/vmoto-wfl.xz
cmp <(xz -dc ../install/vmoto-wfl.xz) vmoto-wfl
echo "vmoto-wfl $(stat -c %s vmoto-wfl) B -> install/vmoto-wfl.xz $(stat -c %s ../install/vmoto-wfl.xz) B"
