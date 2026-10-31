# vmoto-carbit-wifilink

Turn a **RunCam WiFiLink** (the FPV camera + Wi-Fi transmitter with OpenIPC / RubyFPV) into a wireless
rear-view camera for motorcycles whose TFT dash supports **phone projection via Carbit / EasyConn**
(e.g. Vmoto Stash). The WiFiLink pretends to be the phone app, connects to the dash over Wi-Fi Direct and
streams its own camera as H.264 – no phone, no app, no Raspberry Pi, no modification of the bike.

This is the WiFiLink equivalent of the Raspberry Pi project
[VMoto_Stash_RPI_USB_Camera](https://github.com/fiedlji2/VMoto_Stash_RPI_USB_Camera) (same dash protocol).

- Power on → the WiFiLink finds **any Carbit dash in range** (strongest signal wins) and starts streaming
- Bike restarted → reconnects automatically (~20 s after the dash is up)
- **~100 ms** glass-to-glass latency, 25 fps, 1024×592, hardware H.264 from the built-in IMX415 camera
- **Digital zoom 1–3.6× at full sharpness** – a centred crop is taken from the 4K sensor before scaling
- Mirrored image (rear-view mirror), brightness/contrast/saturation, exposure, gain limit, anti-flicker, bitrate…
- Status & settings web page over Ethernet (EN/CS), most changes apply **live** on the dash, *Save* keeps them
- One-command installer from a Windows PC; RubyFPV is only disabled and can be restored

> [!WARNING]
> **Vibe-coded, for testing only.** This project was put together quickly with an AI assistant, tested on one
> bike and one WiFiLink. It may contain bugs, it is not a finished product and it comes with no warranty.
> Use it for experiments and demos, not as safety equipment on the road.

> **Česky:** viz [níže](#česky).

## Hardware

| Part | Tested |
|---|---|
| Camera + Wi-Fi | **RunCam WiFiLink** – SigmaStar SSC338Q, Sony IMX415, Realtek RTL8812EU |
| Firmware | **OpenIPC 2.6.03.04 RubyFPV** (as shipped), Linux 4.9.84 – the installer refuses anything else |
| Dash | **Vmoto Stash** (Carbit `sdkVersion 0.9.23.3`, Wi-Fi Direct on 5 GHz channel 149) |
| Access | Ethernet cable to a Windows PC (Wi-Fi is fully used by the dash link) |

## Install

1. Connect the WiFiLink to the PC with an Ethernet cable and set the PC's Ethernet to
   `192.168.1.20 / 255.255.255.0` (the WiFiLink is `192.168.1.10`).
2. Download this repository (*Code → Download ZIP*) and in the `install` folder run:
   ```
   powershell -ExecutionPolicy Bypass -File install-wifilink.ps1
   ```
   On a fresh WiFiLink it asks **once** for the root password (OpenIPC default `12345`) and installs an SSH key.
   It checks the hardware and firmware first, uploads the package, installs it and reboots the WiFiLink.
3. Switch the bike's dash to *phone connection* mode and power the WiFiLink. Done.

Settings page: **http://192.168.1.10/**. Running the installer again updates to the version in the folder.
Restore RubyFPV: `ssh root@192.168.1.10 /etc/vmoto/restore-ruby.sh` and reboot.

Configuration before installing:
- `install/majestic-settings.txt` – camera and video settings (written to `/etc/majestic.yaml`)
- `install/vmoto.conf` – `DASH=` empty = any Carbit dash, or a fixed dash MAC; `TXP=` TX power (0 = chip minimum)

## Web page

- Connection state, dash network/channel/signal, frames per second, dropped frames, SoC and Wi-Fi temperature,
  current exposure and sensor gain, recent log
- Camera: mirror/flip, brightness, contrast, saturation, hue, black & white, **zoom**, max exposure,
  anti-flicker, slow shutter, sensor gain limit, WDR, bitrate, fps, I-frame interval, rate control, QP limits
- Live via majestic's API; gain, WDR and mirror/flip need a camera restart (~5 s without picture, saved at once)
- **EN / CS** language switch (English by default, remembered in the browser)
- JSON API: `/api/status`, `/api/camera` (see `src/web.go`)

## How it works

`S95vmoto` unpacks the patched Wi-Fi driver and the program into RAM at boot, starts majestic (camera →
hardware H.264 → RTP on localhost) and `vmoto-loop.sh`, which runs `wpa_supplicant`, discovers the dash
(Wi-Fi Direct device `RM_*`), joins its group with WPS push-button and keeps `vmoto-wfl` running.
`vmoto-wfl` (Go) implements the phone side of the Carbit/EasyConn protocol and serves the H.264 frames.

Key pieces:
- **Wi-Fi Direct driver:** the stock `8812eu.ko` is built with `CONFIG_P2P=n`. `driver/` rebuilds it with
  P2P enabled from OpenIPC sources (rtl88x2eu, GPL-2.0).
- **Low latency:** majestic encodes H.264 Main without VUI `bitstream_restriction`, so the dash decoder buffered
  ~7 frames (~400 ms). `vmoto-wfl` rewrites every SPS with `max_num_reorder_frames=0` and
  `max_dec_frame_buffering=1` → ~100 ms. The frame queue is reset on every IDR frame.
- **Zoom:** the SoC's hardware scaler shrinks the 3840×2160 sensor picture to 1024×592. Majestic cannot choose
  which part of the sensor goes in, so the small helper `vmoto-crop` sets the scaler's crop window
  (`MI_VPE_SetPortCrop`) next to the running majestic; `vmoto-wfl` re-applies it whenever majestic restarts.
  The window always has the dash's aspect ratio, which also fixes the ~3 % squeeze of the full sensor picture.

Details: [docs/WIFILINK.md](docs/WIFILINK.md), protocol notes: [docs/PROTOCOL.md](docs/PROTOCOL.md).
The installer's messages, the WiFiLink logs and code comments outside the web server are in Czech.

Files: `install/` (installer and everything that goes to the WiFiLink), `src/` (vmoto-wfl source, `build.sh`),
`driver/` (driver build), `tools/test_fake_dash.py` (local test against a simulated dash).

## Notes & legal

- **Radio:** the dash uses 5 GHz channel 149 (5745 MHz). The loop sets country **US** for the P2P link and the
  lowest TX power the chip allows (TX power index 0). The real output after the module's amplifier was not
  measured. In the EU, 5725–5875 MHz is allowed for short-range devices up to 25 mW e.i.r.p. You are
  responsible for complying with the radio rules in your country.
- Not affiliated with Carbit, EasyConn, RunCam, OpenIPC or any motorcycle maker. Names are trademarks of their owners.
  The protocol was analysed for interoperability with the owner's own device; no vendor keys or code are included.
- Watching a screen while riding is your responsibility – use it as a mirror, not as entertainment.
- The WiFiLink gets warm (SoC ~45–70 °C seen); the page shows the temperature.

## License

GPL-3.0 – see [LICENSE](LICENSE). The bundled `install/8812eu.ko.xz` is built from the GPL-2.0
[rtl88x2eu](https://github.com/OpenIPC/firmware) driver; `driver/build.sh` reproduces it.
`install/lib/libmi_sys.so` and `libmi_vpe.so` are SigmaStar's SDK libraries as distributed by OpenIPC
(package `sigmastar-osdrv-infinity6e`); they are not covered by the GPL and remain their owner's property.

---

## Česky

> [!WARNING]
> **Vibecoded projekt, jen pro testovací účely.** Vzniklo rychle s pomocí AI asistenta a vyzkoušené je jen
> na jedné motorce a jednom WiFiLinku. Může obsahovat chyby, není to hotový produkt a není na to žádná záruka.
> Je to na zkoušení a předvádění, ne jako bezpečnostní výbava do provozu.

RunCam WiFiLink jako bezdrátová zpětná kamera pro motorky s displejem Carbit / EasyConn (např. Vmoto Stash).
WiFiLink se tváří jako aplikace v telefonu, připojí se k displeji přes Wi-Fi Direct a posílá obraz své kamery
(~100 ms zpoždění, 25 snímků/s). Obdoba projektu pro Raspberry Pi
[VMoto_Stash_RPI_USB_Camera](https://github.com/fiedlji2/VMoto_Stash_RPI_USB_Camera).

**Instalace:** WiFiLink (firmware OpenIPC 2.6.03.04 RubyFPV, jak se prodává) kabelem do PC, PC na ethernetu
`192.168.1.20 / 255.255.255.0`, stáhnout repozitář a ve složce `install`:
```
powershell -ExecutionPolicy Bypass -File install-wifilink.ps1
```
Na novém WiFiLinku se jednou zeptá na heslo roota (výchozí `12345`), nahraje SSH klíč, zkontroluje hardware
a firmware, nainstaluje a restartuje. Pak displej motorky přepnout do režimu pro připojení mobilu a zapnout WiFiLink.

Nastavení na **http://192.168.1.10/**: zrcadlení, **zoom 1–3,6× v plné ostrosti** (výřez ze středu 4K
snímače), jas, kontrast, sytost, expozice, strop zesílení, WDR, datový tok… – většina se projeví hned, tlačítko *Save* je zachová. Zesílení, WDR a zrcadlení restartují
kameru (~5 s bez obrazu) a ukládají se hned. Stránka je anglicky, přepínačem **CS** vpravo nahoře se přepne
do češtiny (prohlížeč si volbu pamatuje).

Před instalací jde upravit `install/majestic-settings.txt` (nastavení kamery) a `install/vmoto.conf`
(`DASH=` prázdné = jakýkoli displej Carbit v dosahu, jinak MAC displeje; `TXP=` vysílací výkon).
Vrácení RubyFPV: `/etc/vmoto/restore-ruby.sh` a restart. Technické poznámky: [docs/WIFILINK.md](docs/WIFILINK.md).
