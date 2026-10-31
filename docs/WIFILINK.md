# RunCam WiFiLink → Carbit / EasyConn dash – technical notes

Verified 30–31 Oct 2026 on a Vmoto Stash dash: the WiFiLink's IMX415 camera directly on the dash,
25 frames/s, **~100 ms latency** (stopwatch, camera → dash). Dash protocol: [PROTOCOL.md](PROTOCOL.md).

## Hardware / firmware
SigmaStar SSC338Q (2× Cortex-A7, 91 MB RAM for Linux), Sony IMX415, Wi-Fi RTL8812EU
(`8812eu.ko`, rtl88x2eu v5.15.0.1-249-g9245f8bd9), RubyFPV on OpenIPC 2.6.03.04
(Buildroot 2024.02.10, Linux 4.9.84). Flash: 5.7 MB overlay. Ethernet 192.168.1.10.

## What was needed
1. **Driver with Wi-Fi Direct.** The stock `8812eu.ko` is built with `CONFIG_P2P = n` → `wpa_supplicant` reports
   "P2P disabled". [driver/build.sh](../driver/build.sh) builds the OpenIPC firmware for `ssc338q_rubyfpv_generic`
   (OpenIPC builder) and rebuilds the driver with `CONFIG_P2P = y` (~20 min the first time).
   Vermagic `4.9.84 SMP preempt mod_unload ARMv7 thumb2 p2v8` – `install.sh` compares it with the firmware's driver.
   ```
   # Docker
   docker build -t wfl-driver driver && docker run --rm -v "$PWD/out:/out" wfl-driver
   # or directly on Linux / WSL
   OUT="$PWD/out" bash driver/build.sh
   xz -9 --check=crc32 --lzma2=preset=9,dict=4MiB -c out/8812eu.ko > install/8812eu.ko.xz
   ```
2. **Wi-Fi Direct:** `wpa_supplicant` from the firmware; the dash is an autonomous GO → `p2p_prov_disc <MAC> pbc join`
   and `p2p_connect <MAC> pbc join freq=<listen_freq>`. The group interface is `wlan0` itself, DHCP via `udhcpc`.
3. **Video:** majestic → `video0: h264, profile main, 1024x592, 25 fps, gopSize 0.5`,
   `outgoing: udp://127.0.0.1:5600` (RTP). Majestic does not accept `profile baseline`.
4. **Dash protocol:** `vmoto-wfl` (Go, [src/](../src/)) – announce on 10930, PXC 10922 (C2P/P2C),
   media 10921/10920, RTP → H.264 access units, web page.

## Low latency
- **SPS rewrite** (`src/sps.go`): majestic (SigmaStar) encodes H.264 Main without VUI `bitstream_restriction` →
  the dash decoder held up to ~7 frames (DPB for level 3.1, ≈400 ms total). The program adds
  `max_num_reorder_frames=0`, `max_dec_frame_buffering=1` to every SPS → ~100 ms (verified with
  `ffmpeg -bsf:v trace_headers`).
- **GOP 0.5 s with P-frames** – much better quality than IDR-only frames at the same bitrate.
- **Queue reset on every IDR** – otherwise it stays full for good (the dash takes frames exactly as fast as they
  arrive) and the picture is 3–4 frames behind.

## On the WiFiLink (what `install/install.sh` does)
- `/etc/vmoto/`: `8812eu.ko.xz`, `vmoto-wfl.xz`, `vmoto-loop.sh`, `vmoto.conf`, `restore-ruby.sh`, `VERSION`,
  `vmoto-crop` + `lib/` (zoom), `view.json` (saved zoom), backup `majestic.yaml.ruby`, disabled `S73ruby.disabled`.
- `/etc/init.d/S95vmoto`: unpacks the driver and the program into `/tmp` (RAM), `modprobe cfg80211` →
  `insmod 8812eu.ko` → majestic → `vmoto-loop.sh`.
- `/etc/majestic.yaml` per `install/majestic-settings.txt` (`cli -s`), majestic on port 8080.
- Logs in RAM: `/tmp/vmoto.log`, `/tmp/wfl.log`, `/tmp/majestic.log` (in Czech).
- Pause: `touch /etc/vmoto/DISABLE` + reboot. Restore RubyFPV: `/etc/vmoto/restore-ruby.sh` + reboot.

**`.xz` with a dictionary of at most 4 MiB** (`xz -9 --check=crc32 --lzma2=preset=9,dict=4MiB`): busybox `xz`
allocates the whole dictionary from the header; with the default 64 MiB it reports `corrupted data` while
majestic is running.

**TX power:** `iw dev wlan0 set txpower fixed <mBm>`; already `0` gives hardware index 0 = chip minimum in
`/proc/net/rtl88x2eu/wlan0/tx_power_idx`. The link next to the bike holds (RSSI −40 to −60 dBm). Output power
after the module's amplifier was not measured.

## Web page (`src/web.go`, `src/web.html`)
http://192.168.1.10/, listens only on the `eth0` address. English with a CS switch. Majestic moved to port 8080.
- Status: dash (network, channel, signal, link rate from `iw dev wlan0 link`), stream, SoC (`ipcinfo --temp`) and
  Wi-Fi module temperature, exposure and sensor gain from majestic's `/metrics`, RAM, log.
- **Live** via `/api/v1/set`: brightness, contrast, saturation, hue, max exposure, anti-flicker, slow shutter
  (`disabled/low/medium/high`, the API accepts anything), bitrate, fps, GOP, rate control, `minQp/maxQp/qpDelta`.
  Every change re-creates the encoder channel (the picture keeps running). *Save* writes `/etc/majestic.yaml`.
- **Majestic restart** (~5 s without picture, saved at once): the API refuses `isp.aGain` and `isp.wdr`;
  it accepts `image.mirror/flip` but the sensor does not change (`/proc/mi_modules/mi_sensor/mi_sensor0`:
  `bmirror/bflip`). After the restart the page re-applies unsaved live changes. `aGain` = analog gain limit as a
  multiple (capped at 64; without the key majestic reports `analog: 128`). No visible effect of `wdr` in the log.
- Black & white mode: `/night/on|off`, state `night_enabled` in `/metrics`, not saved.

## Zoom (scaler crop next to majestic)
Picture path: IMX415 (3840×2160@30, raw Bayer over MIPI) → VIF → ISP (colours, exposure) → **VPE scaler** →
H.264 encoder → majestic (only takes the finished bitstream) → RTP → `vmoto-wfl`. Majestic configures these
hardware blocks via the SigmaStar MI SDK at start; `video0.size` only sets the scaler output.

- The VPE output port has a crop window **in sensor coordinates**
  (`/proc/mi_modules/mi_vpe/mi_vpe0`: `PortCrop 0,0,3840,2160` → `OutputW/H 1024x592`). Shrinking a smaller
  window to 1024×592 is a zoom with the sensor's full detail, up to ~3.6× (no upscaling).
- Majestic does not expose it (its `video0.crop` crops the *output* and shrinks the video, e.g. to 512×296).
  The MI drivers allow a second process to talk to the running VPE ("have another bin connect" in dmesg), so
  [src/crop/vmoto-crop.c](../src/crop/vmoto-crop.c) calls `MI_SYS_Init` + `MI_VPE_SetPortCrop(0, 0, rect)` from
  `libmi_vpe.so` (OpenIPC package `sigmastar-osdrv-infinity6e`, libraries in `/etc/vmoto/lib`).
  The change is immediate, without re-creating the encoder (no jerk); verified on frames from the stream.
- The window survives majestic's encoder reloads (`/api/v1/set`) but **not a majestic restart**, which starts
  with the full sensor. `vmoto-wfl` (`src/crop.go`) checks the driver's crop every second and re-applies the
  wanted one (~1–2 s after majestic restarts, also at boot). While majestic starts, the port briefly reports
  `0x0` – such states are ignored.
- Zoom/pan from the page are live; unsaved values are kept in `/tmp/vmoto-view.json` (survive a restart of
  `vmoto-wfl`), *Save* writes `/etc/vmoto/view.json`.
- Sensor modes from the driver: 3840×2160@30 (full sensor), 2560×1440@60 (centre crop ~1.3×),
  1920×1080@90 (full field of view, binning). Majestic picks the mode by `video0.fps`; the dash takes no more than
  ~25–30 frames/s, so the 4K@30 mode plus the scaler crop is the way to zoom.

## Development
```
cd src && ./build.sh              # vmoto-wfl for ARMv7 + install/vmoto-wfl.xz (Go >= 1.18, xz)
src/crop/build.sh                 # vmoto-crop + SigmaStar libs to install/ (needs the OpenIPC builder tree)
go build -o vmoto-wfl-x86 . && python3 ../tools/test_fake_dash.py ./vmoto-wfl-x86   # simulated dash
```
Replacing the program without rebooting the WiFiLink: unpack to `/tmp/vmoto-wfl.new`, **first** `mv` it over
`/tmp/vmoto-wfl`, then `killall vmoto-wfl` (otherwise the loop restarts it from the old file in time).
Upload binaries with `scp -O` (dropbear has no sftp).
