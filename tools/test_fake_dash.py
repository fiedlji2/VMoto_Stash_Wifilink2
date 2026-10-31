"""Lokální test vmoto-wfl: simulovaný displej + simulované RTP z majesticu."""
import base64, json, socket, struct, subprocess, sys, threading, time

BIN = sys.argv[1]
P = struct.Struct("<IIII")
M = struct.Struct("<HHI")


def pxc(cmd, payload=b""):
    ln = 16 + len(payload)
    return P.pack(cmd, ln, cmd ^ ln, 0) + payload


def read_pxc(s):
    h = b""
    while len(h) < 16:
        h += s.recv(16 - len(h))
    cmd, ln, chk, _ = P.unpack(h)
    assert cmd ^ ln == chk
    body = b""
    while len(body) < ln - 16:
        body += s.recv(ln - 16 - len(body))
    return cmd, body


ok = {}
# simulovaný ohlašovací port displeje
srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", 20930)); srv.listen(4)


def dash_ec():
    c, _ = srv.accept()
    cmd, body = read_pxc(c)
    ok["ec_init"] = (hex(cmd), json.loads(body))
    c.sendall(pxc(cmd + 1, b'{"status":true}'))
    c.close()


threading.Thread(target=dash_ec, daemon=True).start()
proc = subprocess.Popen([BIN, "-dash", "127.0.0.1:20930", "-rtp", "127.0.0.1:25600"],
                        stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
time.sleep(2.5)

# C2P
c2p = socket.create_connection(("127.0.0.1", 10922)); c2p.settimeout(5)
c2p.sendall(pxc(0x10000)); assert read_pxc(c2p)[0] == 0x10001
c2p.sendall(pxc(0x10010, json.dumps({"HUID": "E001122334455", "HUName": "JCDZ00-TEST", "supportFunction": 128}).encode()))
cmd, body = read_pxc(c2p); ci = json.loads(body)
ok["client_info"] = (hex(cmd), sorted(ci.keys())[:6], len(ci["pubkey"]), len(base64.b64decode(ci["encryptedHUID"])))
c2p.sendall(pxc(0x103E0, b'{"client_set":"easy_conn","sn":"TESTSN"}')); read_pxc(c2p)
c2p.sendall(pxc(0x70000000)); ok["c2p_hb"] = hex(read_pxc(c2p)[0])

# P2C
p2c = socket.create_connection(("127.0.0.1", 10922)); p2c.settimeout(5)
p2c.sendall(pxc(0x20000)); assert read_pxc(p2c)[0] == 0x20001
got = []
t0 = time.time()
while time.time() - t0 < 3:
    cmd, body = read_pxc(p2c)
    got.append((hex(cmd), body[:60]))
    p2c.sendall(pxc(cmd + 1))
ok["p2c"] = got

# média: konfigurace + start
m = socket.create_connection(("127.0.0.1", 10921)); m.settimeout(5)
cfg = struct.pack("<hhiiiHHi", 1024, 592, 0, 2, 2, 0, 0, 8388608) + bytes(8) + b"X" * 172
m.sendall(M.pack(0x10, len(cfg), 0) + cfg)
h = m.recv(16); cmd, ln, _ = M.unpack(h[:8]); enc, w, hh = struct.unpack("<ihh", h[8:16])
ok["config_reply"] = (hex(cmd), enc, w, hh)
m.sendall(M.pack(0x70, 0, 0)); ok["start"] = hex(M.unpack(m.recv(8))[0])

# data: připojit, pak poslat RTP (STAP-A SPS+PPS, IDR po FU-A, P-snímek)
d = socket.create_connection(("127.0.0.1", 10920)); d.settimeout(5)
time.sleep(0.3)
u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
seq = [0]


def rtp(payload, ts, marker):
    seq[0] += 1
    hdr = struct.pack("!BBHII", 0x80, (0x80 if marker else 0) | 96, seq[0], ts, 1234)
    u.sendto(hdr + payload, ("127.0.0.1", 25600))


sps, pps = b"\x67\x42\xc0\x1f" + b"\x11" * 8, b"\x68\xce\x3c\x80"
idr = b"\x65" + bytes(range(256)) * 10
for f in range(3):
    ts = 90000 + f * 3600
    stap = b"\x78" + struct.pack("!H", len(sps)) + sps + struct.pack("!H", len(pps)) + pps
    rtp(stap, ts, False)
    body = idr[1:]
    chunks = [body[i:i + 1000] for i in range(0, len(body), 1000)]
    for i, ch in enumerate(chunks):
        fh = (0x80 if i == 0 else 0) | (0x40 if i == len(chunks) - 1 else 0) | 5
        rtp(bytes([0x7C, fh]) + ch, ts, i == len(chunks) - 1)
    time.sleep(0.04)
frames = []
for _ in range(2):
    d.sendall(M.pack(0x72, 0, 0))
    n = struct.unpack("<I", d.recv(4))[0]
    data = b""
    while len(data) < n:
        data += d.recv(n - len(data))
    nals = [data[i + 4] & 0x1F for i in range(len(data) - 4) if data[i:i + 4] == b"\0\0\0\1"]
    frames.append((n, nals, data == b"\0\0\0\1" + sps + b"\0\0\0\1" + pps + b"\0\0\0\1" + idr))
ok["frames"] = frames

proc.terminate()
out = proc.communicate(timeout=5)[0].decode(errors="replace")
for k, v in ok.items():
    print(f"{k}: {v}")
print("--- log"); print("\n".join(out.splitlines()[-14:]))
