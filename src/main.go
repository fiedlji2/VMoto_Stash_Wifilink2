// vmoto-wfl: "telefon" pro displej Vmoto Stash (Carbit / EasyConn) běžící přímo na RunCam WiFiLink.
// Port Python loggeru (logger.py) - jen to, co je potřeba pro obraz:
//   - ohlášení na displej (10930, 0x70000010), znovu kdykoli displej nemá řídicí spojení
//   - PXC 10922: C2P (CLIENT_INFO s vlastním RSA klíčem, heartbeat, SN) a P2C (heartbeat, FOREGROUND, odpověď na SN)
//   - média 10921 (konfigurace, heartbeat, start dat) a 10920 (snímek H.264 na každou žádost 0x72)
//   - zdroj videa: RTP/H.264 z majesticu (outgoing udp://127.0.0.1:5600)
//   - webová stránka se stavem a nastavením kamery jen na ethernetu (web.go)
// Protokol: viz PROTOKOL.md.
package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

var (
	dashAddr  = flag.String("dash", "192.168.2.1:10930", "ohlašovací port displeje")
	rtpAddr   = flag.String("rtp", "127.0.0.1:5600", "kde poslouchat RTP z majesticu")
	pkgName   = flag.String("package", "net.easyconn.easyride.wws", "packageName telefonu")
	phoneUUID = flag.String("uuid", "wifilink-0001", "phoneUUID")
	hbEvery   = flag.Duration("hb", time.Second, "interval P2C heartbeatu")
	queueMax  = flag.Int("queue", 4, "max. snímků ve frontě (s P-snímky)")
	spsFix    = flag.Bool("spsfix", true, "upravit SPS: max_num_reorder_frames=0 (nižší zpoždění dekodéru)")
	dumpPath  = flag.String("dump", "", "uložit prvních 100 snímků (Annex-B) do souboru, pro ladění")
)

const (
	cmdECInit      = 0x70000010
	cmdHeartbeat   = 0x70000000
	cmdClientInfo  = 0x00010010
	cmdCheckSN     = 0x000103E0
	cmdForeground  = 0x00020020
	cmdCheckSNResp = 0x000201C0
	connC2P        = 0x00010000
	connP2C        = 0x00020000
)

// ---------- rámce ----------

// PXC: u32 cmd | u32 total_len | u32 cmd^total_len | u32 0, little-endian
func pxcPacket(cmd uint32, payload []byte) []byte {
	ln := uint32(16 + len(payload))
	b := make([]byte, 16, ln)
	binary.LittleEndian.PutUint32(b[0:], cmd)
	binary.LittleEndian.PutUint32(b[4:], ln)
	binary.LittleEndian.PutUint32(b[8:], cmd^ln)
	return append(b, payload...)
}

func readPXC(r io.Reader) (uint32, []byte, error) {
	var h [16]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	cmd := binary.LittleEndian.Uint32(h[0:])
	ln := binary.LittleEndian.Uint32(h[4:])
	if cmd^ln != binary.LittleEndian.Uint32(h[8:]) || ln < 16 || ln > 1<<20 {
		return 0, nil, fmt.Errorf("neplatná PXC hlavička % x", h)
	}
	p := make([]byte, ln-16)
	_, err := io.ReadFull(r, p)
	return cmd, p, err
}

// média: u16 cmd | u16 délka těla | u32 token
func mediaPacket(cmd uint16, body []byte) []byte {
	b := make([]byte, 8, 8+len(body))
	binary.LittleEndian.PutUint16(b[0:], cmd)
	binary.LittleEndian.PutUint16(b[2:], uint16(len(body)))
	return append(b, body...)
}

func readMedia(r io.Reader) (uint16, []byte, error) {
	var h [8]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	cmd := binary.LittleEndian.Uint16(h[0:])
	ln := binary.LittleEndian.Uint16(h[2:])
	p := make([]byte, ln)
	_, err := io.ReadFull(r, p)
	return cmd, p, err
}

// ---------- stav ----------

type state struct {
	mu        sync.Mutex
	ctrlConns int
	ctrlLast  time.Time
	since     time.Time // poslední připojení řídicího kanálu displeje (pro webovou stránku)
	pendingSN map[string]interface{}
	capW      int
	capH      int
	key       *rsa.PrivateKey
	pubB64    string
}

func (s *state) encryptHUID(huid string) string {
	// PKCS#1 v1.5 typ 1 nad surovými daty = "zašifrováno soukromým klíčem" jako v Javě
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.Hash(0), []byte(huid))
	if err != nil {
		log.Printf("encryptHUID: %v", err)
		return ""
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func jsonBytes(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ---------- ohlášení (10930) ----------

func ecInit(s *state) {
	c, err := net.DialTimeout("tcp", *dashAddr, 3*time.Second)
	if err != nil {
		log.Printf("[ec-init] %v", err)
		return
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	body := jsonBytes(map[string]string{"phoneType": "Android", "packageName": *pkgName})
	if _, err := c.Write(pxcPacket(cmdECInit, body)); err != nil {
		return
	}
	cmd, p, err := readPXC(c)
	if err != nil {
		log.Printf("[ec-init] bez odpovědi: %v", err)
		return
	}
	log.Printf("[ec-init] RX 0x%08X %s", cmd, bytes.TrimSpace(p))
}

func ecInitLoop(s *state) {
	for {
		s.mu.Lock()
		need := s.ctrlConns == 0 && time.Since(s.ctrlLast) > 3*time.Second
		s.mu.Unlock()
		if need {
			ecInit(s)
			s.mu.Lock()
			s.ctrlLast = time.Now()
			s.mu.Unlock()
		}
		time.Sleep(time.Second)
	}
}

// ---------- PXC 10922 ----------

func clientInfoReply(s *state, req map[string]interface{}) []byte {
	huid, _ := req["HUID"].(string)
	sf := req["supportFunction"]
	if sf == nil {
		sf = 0
	}
	return jsonBytes(map[string]interface{}{
		"pxcVersion": "1.0.2", "phoneUUID": *phoneUUID, "phoneBrand": "RunCam", "phoneModel": "WiFiLink",
		"phoneOsVersion": "34", "phoneOs": "Android", "package": *pkgName, "versionCode": 47, "token": 0,
		"pubkey": s.pubB64, "encryptedHUID": s.encryptHUID(huid), "bluetoothName": "WiFiLink",
		"supportH264IFrame": true, "supportFunction": sf, "appVersionFingerPrint": "V:2.4(47)-wifilink-release",
	})
}

func handleCtrl(s *state, c net.Conn) {
	defer c.Close()
	s.mu.Lock()
	s.ctrlConns++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.ctrlConns--
		s.ctrlLast = time.Now()
		s.mu.Unlock()
	}()
	cmd, p, err := readPXC(c)
	if err != nil {
		return
	}
	if cmd == connC2P {
		s.mu.Lock()
		s.since = time.Now()
		s.mu.Unlock()
	}
	if _, err := c.Write(pxcPacket(cmd+1, p)); err != nil {
		return
	}
	switch cmd {
	case connC2P:
		log.Printf("[10922] C2P připojeno %s", c.RemoteAddr())
		c2p(s, c)
	case connP2C:
		log.Printf("[10922] P2C připojeno %s", c.RemoteAddr())
		p2c(s, c)
	default:
		log.Printf("[10922] typ kanálu 0x%08X - jen potvrzeno", cmd)
		io.Copy(io.Discard, c)
	}
}

func c2p(s *state, c net.Conn) {
	for {
		cmd, p, err := readPXC(c)
		if err != nil {
			log.Printf("[C2P] konec: %v", err)
			return
		}
		var reply []byte
		switch cmd {
		case cmdHeartbeat:
			reply = pxcPacket(cmd+1, nil)
		case cmdClientInfo:
			var req map[string]interface{}
			json.Unmarshal(p, &req)
			log.Printf("[C2P] CLIENT_INFO od %v (%v)", req["HUName"], req["HUID"])
			reply = pxcPacket(cmd+1, clientInfoReply(s, req))
		case cmdCheckSN:
			var req map[string]interface{}
			json.Unmarshal(p, &req)
			s.mu.Lock()
			s.pendingSN = req
			s.mu.Unlock()
			reply = pxcPacket(cmd+1, []byte("{}"))
		default:
			log.Printf("[C2P] 0x%08X %s", cmd, bytes.TrimSpace(p))
			reply = pxcPacket(cmd+1, []byte("{}"))
		}
		if _, err := c.Write(reply); err != nil {
			return
		}
	}
}

func p2c(s *state, c net.Conn) {
	go func() { // odpovědi displeje (0x70000001, 0x00020021, ...) jen čteme
		for {
			if _, _, err := readPXC(c); err != nil {
				c.Close()
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond)
	s.mu.Lock()
	w, h := s.capW, s.capH
	s.mu.Unlock()
	if w == 0 {
		w, h = 1024, 592
	}
	fg := jsonBytes(map[string]interface{}{"displayRotation": 1, "width": max(w, h), "height": min(w, h),
		"enableAccessibility": false, "enableAOAHid": false})
	if _, err := c.Write(pxcPacket(cmdForeground, fg)); err != nil {
		return
	}
	log.Printf("[P2C] APPSTATUS_FOREGROUND %dx%d", max(w, h), min(w, h))
	for {
		time.Sleep(*hbEvery)
		s.mu.Lock()
		sn := s.pendingSN
		s.pendingSN = nil
		s.mu.Unlock()
		if sn != nil {
			// jako telefon bez internetu: SN nelze ověřit, pokračuje se
			body := jsonBytes(map[string]interface{}{"isOk": true, "errCode": -100, "errMsg": "Network Not Available",
				"id": sn["sn"], "client_set": sn["client_set"]})
			if _, err := c.Write(pxcPacket(cmdCheckSNResp, body)); err != nil {
				return
			}
			log.Printf("[P2C] odpověď na kontrolu SN")
		}
		if _, err := c.Write(pxcPacket(cmdHeartbeat, nil)); err != nil {
			log.Printf("[P2C] konec: %v", err)
			return
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- média 10921 ----------

func handleMediaCtrl(s *state, c net.Conn) {
	defer c.Close()
	for {
		cmd, body, err := readMedia(c)
		if err != nil {
			return
		}
		var out []byte
		switch cmd {
		case 0x10: // REQ_RV_CONFIG_CAPTURE
			if len(body) >= 4 {
				w := int(int16(binary.LittleEndian.Uint16(body[0:]))) &^ 15
				h := int(int16(binary.LittleEndian.Uint16(body[2:]))) &^ 15
				s.mu.Lock()
				s.capW, s.capH = w, h
				s.mu.Unlock()
				log.Printf("[10921] konfigurace %dx%d", w, h)
				r := make([]byte, 8)
				binary.LittleEndian.PutUint32(r[0:], 2) // H.264
				binary.LittleEndian.PutUint16(r[4:], uint16(w))
				binary.LittleEndian.PutUint16(r[6:], uint16(h))
				out = mediaPacket(0x11, r)
			}
		case 0x30:
			r := make([]byte, 8)
			binary.LittleEndian.PutUint32(r[0:], 5002)
			binary.LittleEndian.PutUint32(r[4:], 1)
			out = mediaPacket(0x31, r)
		case 0x40:
			out = mediaPacket(0x41, nil)
		case 0x60:
			out = mediaPacket(0x61, []byte(`{"supportFunction":0}`))
		case 0x70:
			log.Printf("[10921] start dat")
			out = mediaPacket(0x71, nil)
		case 0x110:
			out = mediaPacket(0x111, []byte("OK\x00"))
		case 0x20: // dotyk / tlačítka
		default:
			out = mediaPacket(0x7F02, nil)
		}
		if out != nil {
			if _, err := c.Write(out); err != nil {
				return
			}
		}
	}
}

// ---------- média 10920 ----------

func handleMediaData(v *video, c net.Conn) {
	defer c.Close()
	v.flush()
	var sent, empty, polls, depthSum int
	t0 := time.Now()
	for {
		cmd, _, err := readMedia(c)
		if err != nil {
			log.Printf("[10920] konec: %v", err)
			return
		}
		if cmd != 0x72 {
			c.Write(mediaPacket(0x7F02, nil))
			continue
		}
		au := v.next(250 * time.Millisecond)
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(au)))
		if _, err := c.Write(append(hdr[:], au...)); err != nil {
			return
		}
		if au == nil {
			empty++
		} else {
			sent++
		}
		v.mu.Lock()
		depthSum += len(v.queue)
		v.mu.Unlock()
		polls++
		if d := time.Since(t0); d >= 10*time.Second {
			v.mu.Lock()
			setStreamStats(streamStats{SentPerS: float64(sent) / d.Seconds(), PollsPerS: float64(polls) / d.Seconds(),
				Empty: empty, FromCamera: v.frames, Dropped: v.dropped, IDROnly: v.allIDR,
				QueueAvg: float64(depthSum) / float64(max(polls, 1))})
			log.Printf("[10920] 10s: odesláno %.1f/s, prázdných %d, z kamery %d, zahozeno %d, IDR-only %v, fronta prům. %.2f",
				float64(sent)/d.Seconds(), empty, v.frames, v.dropped, v.allIDR, float64(depthSum)/float64(max(polls, 1)))
			v.mu.Unlock()
			sent, empty, polls, depthSum, t0 = 0, 0, 0, 0, time.Now()
		}
	}
}

// ---------- video: RTP H.264 -> access units ----------

type video struct {
	mu       sync.Mutex
	cond     *sync.Cond
	queue    [][]byte
	waitIDR  bool
	frames   int
	dropped  int
	allIDR   bool
	idrCount int
}

func newVideo() *video {
	v := &video{}
	v.cond = sync.NewCond(&v.mu)
	return v
}

func (v *video) push(au []byte, idr bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.frames++
	if idr {
		v.idrCount++
	}
	// považuj stream za "jen IDR", pokud posledních ~20 snímků byly všechny IDR
	if v.frames%20 == 0 {
		v.allIDR = v.idrCount == 20
		v.idrCount = 0
	}
	if v.allIDR {
		v.dropped += len(v.queue)
		v.queue = append(v.queue[:0], au) // vždy jen nejnovější
		v.cond.Broadcast()
		return
	}
	if v.waitIDR && !idr {
		v.dropped++
		return
	}
	v.waitIDR = false
	if idr && len(v.queue) > 0 {
		// úplný snímek jde dekódovat sám: starší snímky ve frontě zahodit, jinak by
		// fronta zůstala trvale plná (displej bere snímky stejně rychle, jak přicházejí)
		v.dropped += len(v.queue)
		v.queue = v.queue[:0]
	}
	if len(v.queue) >= *queueMax {
		v.dropped += len(v.queue)
		v.queue = v.queue[:0]
		if !idr {
			v.waitIDR = true
			v.dropped++
			return
		}
	}
	v.queue = append(v.queue, au)
	v.cond.Broadcast()
}

func (v *video) flush() {
	v.mu.Lock()
	v.queue = v.queue[:0]
	v.waitIDR = true
	v.mu.Unlock()
}

func (v *video) next(timeout time.Duration) []byte {
	deadline := time.Now().Add(timeout)
	v.mu.Lock()
	defer v.mu.Unlock()
	for len(v.queue) == 0 {
		if time.Now().After(deadline) {
			return nil
		}
		t := time.AfterFunc(time.Until(deadline), func() { v.mu.Lock(); v.cond.Broadcast(); v.mu.Unlock() })
		v.cond.Wait()
		t.Stop()
	}
	au := v.queue[0]
	v.queue = v.queue[1:]
	return au
}

var startCode = []byte{0, 0, 0, 1}

func rtpReader(v *video, addr string) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatalf("RTP %s: %v", addr, err)
	}
	log.Printf("RTP poslouchá na %s", addr)
	buf := make([]byte, 65536)
	var au bytes.Buffer
	var curTS uint32
	var idr, haveTS bool
	var fu bytes.Buffer
	var dump *os.File
	dumped := 0
	if *dumpPath != "" {
		dump, _ = os.Create(*dumpPath)
	}
	emit := func() {
		if au.Len() > 0 {
			v.push(append([]byte(nil), au.Bytes()...), idr)
			if dump != nil && dumped < 100 {
				dump.Write(au.Bytes())
				if dumped++; dumped == 100 {
					dump.Close()
					log.Printf("[RTP] uloženo 100 snímků do %s", *dumpPath)
				}
			}
		}
		au.Reset()
		idr = false
	}
	var spsOrig, spsNew []byte
	addNAL := func(n []byte) {
		if len(n) == 0 {
			return
		}
		t := n[0] & 0x1F
		if t == 5 {
			idr = true
		}
		if t == 7 && len(n) >= 4 && *spsFix {
			// SPS bez přeřazování snímků -> dekodér displeje nedrží snímky v zásobě
			if !bytes.Equal(n, spsOrig) {
				spsOrig = append([]byte(nil), n...)
				fixed, err := rewriteSPS(n)
				if err != nil {
					log.Printf("[RTP] SPS nejde upravit (%v), posílám původní", err)
					fixed = spsOrig
				}
				spsNew = fixed
				log.Printf("[RTP] SPS profile_idc=%d level=%d: % x -> % x", n[1], n[3], n, spsNew)
			}
			n = spsNew
		}
		au.Write(startCode)
		au.Write(n)
	}
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			log.Printf("RTP: %v", err)
			continue
		}
		p := buf[:n]
		if n < 12 || p[0]>>6 != 2 {
			continue
		}
		cc := int(p[0] & 0x0F)
		marker := p[1]&0x80 != 0
		ts := binary.BigEndian.Uint32(p[4:8])
		off := 12 + 4*cc
		if p[0]&0x10 != 0 && len(p) >= off+4 { // rozšíření hlavičky
			off += 4 + 4*int(binary.BigEndian.Uint16(p[off+2:]))
		}
		if off >= len(p) {
			continue
		}
		if p[0]&0x20 != 0 { // padding
			p = p[:len(p)-int(p[len(p)-1])]
		}
		pl := p[off:]
		if haveTS && ts != curTS {
			emit() // nový časový údaj = nový snímek
		}
		curTS, haveTS = ts, true
		switch t := pl[0] & 0x1F; {
		case t >= 1 && t <= 23:
			addNAL(pl)
		case t == 24: // STAP-A
			q := pl[1:]
			for len(q) >= 2 {
				sz := int(binary.BigEndian.Uint16(q))
				if 2+sz > len(q) {
					break
				}
				addNAL(q[2 : 2+sz])
				q = q[2+sz:]
			}
		case t == 28 && len(pl) >= 2: // FU-A
			fh := pl[1]
			if fh&0x80 != 0 {
				fu.Reset()
				fu.WriteByte(pl[0]&0xE0 | fh&0x1F)
			}
			fu.Write(pl[2:])
			if fh&0x40 != 0 {
				addNAL(fu.Bytes())
				fu.Reset()
			}
		}
		if marker {
			emit()
		}
	}
}

// ---------- main ----------

func serve(port int, h func(net.Conn)) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("port %d: %v", port, err)
	}
	log.Printf("TCP :%d poslouchá", port)
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go h(c)
	}
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		log.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	s := &state{key: key, pubB64: base64.StdEncoding.EncodeToString(der)}
	v := newVideo()

	go rtpReader(v, *rtpAddr)
	go serve(54321, func(c net.Conn) { io.Copy(io.Discard, c); c.Close() })
	go serve(10922, func(c net.Conn) { handleCtrl(s, c) })
	go serve(10921, func(c net.Conn) { handleMediaCtrl(s, c) })
	go serve(10920, func(c net.Conn) { handleMediaData(v, c) })
	go webServer(s)
	ecInitLoop(s)
}
