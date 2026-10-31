# Carbit / EasyConn protocol (motorcycle dash ↔ phone) – verified in practice

Verified on a Vmoto Stash dash (Carbit, `sdkVersion 0.9.23.3`) with a Raspberry Pi 4 and later a RunCam WiFiLink
acting as the "phone". Everything below comes from captured traffic and from the interface documentation needed
for interoperability. Dash-specific identifiers are replaced by `<HUID>`, `<HUName>`, `<MAC>`.
(Same as in the Raspberry Pi project, translated; section 6 adds the WiFiLink findings.)

## 1. Wi-Fi Direct
- The dash is an **autonomous GO** (group `DIRECT-xx`), P2P device `RM_<HUID>`, MAC `<MAC>`
  (HUID on the dash = "E" + MAC without colons). Listens and operates on **5745 MHz (channel 149)**.
- Connecting: `p2p_prov_disc <MAC> pbc join` → `p2p_connect <MAC> pbc join freq=<listen_freq>` (WPS PBC, no password).
  The dash refuses GO negotiation (it already is the GO).
- DHCP from the dash: phone `192.168.2.20`, dash `192.168.2.1` (DHCP also offers a default gateway – ignore it).
- **Pi 4 / 3 A+ built-in Wi-Fi (Broadcom):** the firmware disables channels 149–165 for EU →
  `iw reg set US` + `iw dev wlan0 set txpower fixed 1300` (13 dBm; in the EU 5725–5875 MHz is allowed up to 25 mW).
- **NetworkManager** tears down a foreign P2P group immediately → wlan0 must be `unmanaged` for NM.
- **WiFiLink (RTL8812EU):** the stock driver has P2P disabled; see [WIFILINK.md](WIFILINK.md).

## 2. Discovery (mDNS)
The dash advertises `EasyConn._EasyConn._tcp.local` → `buildroot.local:10930`, TXT:
`flavor=51 huname=<HUName> port=10930 channel=21321 packagename=net.easyconn.easyride.wws huid=<HUID> ip=192.168.2.1`

In addition, every ~4 s the dash connects to the phone's **TCP 54321** and disconnects right away (a check, sends nothing).

## 3. PXC frame (ports 10930, 10922)
16 B little-endian: `u32 cmd | u32 total_len | u32 cmd^total_len | u32 x` + payload (JSON).
Reply = `cmd + 1`. The dash fills field `x` (meaning unknown); sending 0 is fine.

## 4. Sequence
| # | Direction | Port | cmd | Content |
|---|---|---|---|---|
| 1 | phone → dash | 10930 | `0x70000010` | `{"phoneType":"Android","packageName":"net.easyconn.easyride.wws"}` |
| 2 | dash → phone | 10930 | `0x70000011` | `{"status":true}` |
| 3 | dash → phone | 10922 (conn. A) | `0x00010000` | channel type C2P, reply `0x00010001` |
| 4 | dash → phone | A | `0x00010010` | CLIENT_INFO (JSON ~1.1 kB: HUID, HUName, sdkVersion, …) |
| 5 | phone → dash | A | `0x00010011` | phone's reply (see below) |
| 6 | dash → phone | A | `0x00010690` | `{"usbSpeed":0,"wifiSpeed":0}` |
| 7 | dash → phone | 10922 (conn. B) | `0x00020000` | channel type P2C, reply `0x00020001` |
| 8 | dash → phone | A | `0x70000000` | heartbeat, reply `0x70000001` (empty) |
| 9 | dash → phone | A | `0x000103E0` | SN check `{"client_set":"easy_conn","sn":"…"}` |
| 10 | phone → dash | B | `0x000201C0` | SN check result (like a phone without internet, see below) |
| 11 | phone → dash | B | `0x00020020` | **APPSTATUS_FOREGROUND – without it the dash takes the video but does not show it** |
| 12 | phone → dash | B | `0x70000000` | phone heartbeat ~1×/s, **required** – without it the dash closes everything after ~7 s |
| 13 | dash → phone | 10921 | `0x0010` | REQ_RV_CONFIG_CAPTURE (section 5) → `0x0011` |
| 14 | dash ↔ phone | 10921 | `0x0040`/`0x0041` | media heartbeat (1×/s) |
| 15 | dash → phone | 10921 | `0x0070` → `0x0071` | data start |
| 16 | dash → phone | 10920 | `0x0072` | "give me a frame" → `u32 length` + 1 H.264 frame |

Bodies:
- `0x00020020`: `{"displayRotation":1,"width":1024,"height":592,"enableAccessibility":false,"enableAOAHid":false}`
- `0x000201C0`: `{"isOk":true,"errCode":-100,"errMsg":"Network Not Available","id":"<sn>","client_set":"easy_conn"}`
  (same behaviour as the official app without an internet connection)

### Reply to CLIENT_INFO (0x00010011)
Keys: `pxcVersion "1.0.2"`, `phoneUUID`, `phoneBrand`, `phoneModel`, `phoneOsVersion`, `phoneOs "Android"`,
`package`, `versionCode`, `token 0`, `pubkey`, `encryptedHUID`, `bluetoothName`, `supportH264IFrame true`,
`supportFunction`, `appVersionFingerPrint`.
- `pubkey` = base64 DER SubjectPublicKeyInfo of the phone's **own** RSA-1024 key pair (the app generates it itself).
- `encryptedHUID` = the HUID encrypted with the phone's private key (RSA PKCS#1 v1.5, block type 1), base64.
- No vendor keys are needed.

## 5. Media channels 10921 (control) and 10920 (data)
Header 8 B LE: `u16 cmd | u16 body length | u32 token (0)`.

**REQ_RV_CONFIG_CAPTURE (0x10)**, body 32 B + encryptedHUID:
`i16 width 1024 | i16 height 592 | i32 wantFps 0 | i32 wantEncoder 2 (H.264) | i32 supportCodec 2 |
u16 minQ | u16 maxQ | i32 bitRate 8388608 | u8 capScreenMode | u8 touchMode | u8 orientation |
u8 displayId | u8 videoType | u8 supportExtendProtocol | 2 B reserved | encryptedHUID (echoed)`

**Replies on 10921:** `0x11` = `i32 encoder 2 | i16 w | i16 h`; `0x31` = `i32 5002 | i32 1`;
`0x41` empty; `0x61` JSON `{"supportFunction":0}`; `0x71` empty; `0x111` `"OK\0"`; anything else `0x7F02`.

**Data 10920:** the dash sends `0x72`, the phone replies `u32 LE length` + one H.264 Annex-B access unit
(repeat SPS/PPS). `u32 0` = no frame. The dash sets the pace (pull).

## 6. Video – what works best (measured)
- The dash decoder honours the SPS VUI `bitstream_restriction`. Without it (H.264 Main from the WiFiLink's
  SigmaStar encoder) it buffers up to ~7 frames (~400 ms total). With `max_num_reorder_frames=0` and
  `max_dec_frame_buffering=1` frames are shown at once (the Pi's HW encoder already writes these).
- **Raspberry Pi:** H.264 Baseline, IDR-only, and the **newest** frame on every `0x72` request: older frames are
  dropped without breaking the picture, latency does not build up. ~150 ms camera → dash.
- **WiFiLink:** H.264 Main, I-frame every 0.5 s with P-frames (much better quality at the same bitrate), SPS rewritten
  as above, frame queue reset on every IDR frame. ~100 ms camera → dash.
- The dash asks for ~23–26 frames/s. A bitrate of 10–12 Mbit/s (~50 kB/frame) removes visible banding;
  the Wi-Fi Direct link (~50–70 Mbit/s) has headroom.
