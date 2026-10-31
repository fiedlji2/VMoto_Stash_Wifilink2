// Status and settings web page of the WiFiLink, Ethernet only (like web.py on the Pi):
//
//	/                    HTML page, refreshes itself (EN/CS, translated in the browser)
//	/api/status          status as JSON (dash, stream, Wi-Fi, temperatures, log)
//	/api/camera          GET: camera settings (ranges, live and saved values)
//	                     POST {"image.contrast": 55, ...}: applies at once via majestic's API (/api/v1/set)
//	/api/camera/save     POST: writes the live values to /etc/majestic.yaml (survives a restart)
//	/api/camera/defaults POST: default values live, not saved
//	/api/camera/revert   POST: back to the saved values live
//
// Majestic keeps /api/v1/set changes in memory only and every change briefly re-creates the encoder
// (the picture keeps running, the dash gets a new IDR frame). Hence Save writes the YAML separately.
// Gain and WDR are refused by the API; mirror/flip are accepted but the sensor does not change: those are
// written to the YAML at once and majestic is restarted (~5 s without picture).
//
// State and messages are sent as language-neutral keys, the page translates them.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	webAddr      = flag.String("web", ":80", "web page (port; the IP is taken from interface -webif)")
	webIf        = flag.String("webif", "eth0", "interface for the web page (empty = listen on the address from -web)")
	majesticURL  = flag.String("majestic", "http://127.0.0.1:8080", "majestic HTTP API")
	majesticYAML = flag.String("yaml", "/etc/majestic.yaml", "majestic configuration used by Save")
)

//go:embed web.html
var webPage []byte

// ---------- stream statistics (filled by handleMediaData every 10 s) ----------

type streamStats struct {
	SentPerS   float64   `json:"sent_per_s"`
	PollsPerS  float64   `json:"polls_per_s"`
	Empty      int       `json:"empty"`
	FromCamera int       `json:"from_camera"`
	Dropped    int       `json:"dropped"`
	IDROnly    bool      `json:"idr_only"`
	QueueAvg   float64   `json:"queue_avg"`
	At         time.Time `json:"-"`
}

var (
	statsMu   sync.Mutex
	lastStats streamStats
)

func setStreamStats(st streamStats) {
	st.At = time.Now()
	statsMu.Lock()
	lastStats = st
	statsMu.Unlock()
}

// ---------- camera controls ----------

// How majestic changes the value:
//
//	""        live via /api/v1/set, Save writes it to the YAML
//	"restart" only via YAML + majestic restart (the API refuses the key) - saved at once
//	"night"   /night/on|off, state from /metrics - not saved (always day after a restart)
type camCtrl struct {
	Key     string   `json:"key"` // section.key in majestic; the page shows a translated label for it
	Mode    string   `json:"mode,omitempty"`
	DropDef bool     `json:"-"` // default value = remove the key from the YAML
	Group   string   `json:"group"`
	Type    string   `json:"type"` // bool | range | select
	Min     float64  `json:"min,omitempty"`
	Max     float64  `json:"max,omitempty"`
	Step    float64  `json:"step,omitempty"`
	Unit    string   `json:"unit,omitempty"`
	Options []string `json:"options,omitempty"`
	Def     string   `json:"default"`
}

// Codec, resolution and profile are not offered: the dash wants H.264 1024×592 and the SPS rewrite expects Main.
var camCtrls = []camCtrl{
	// majestic sets mirror/flip on the sensor only at start (the API changes the config, not the sensor)
	{Key: "image.mirror", Mode: "restart", Group: "image", Type: "bool", Def: "true"},
	{Key: "image.flip", Mode: "restart", Group: "image", Type: "bool", Def: "false"},
	{Key: "image.luminance", Group: "image", Type: "range", Min: 0, Max: 100, Step: 1, Def: "47"},
	{Key: "image.contrast", Group: "image", Type: "range", Min: 0, Max: 100, Step: 1, Def: "50"},
	{Key: "image.saturation", Group: "image", Type: "range", Min: 0, Max: 100, Step: 1, Def: "40"},
	{Key: "image.hue", Group: "image", Type: "range", Min: 0, Max: 100, Step: 1, Def: "50"},
	{Key: "night", Mode: "night", Group: "image", Type: "bool", Def: "false"},
	{Key: "isp.exposure", Group: "exposure", Type: "range", Min: 1, Max: 40, Step: 1, Unit: "ms", Def: "14"},
	{Key: "isp.antiFlicker", Group: "exposure", Type: "select", Options: []string{"disabled", "50", "60"}, Def: "disabled"},
	{Key: "isp.slowShutter", Group: "exposure", Type: "select",
		Options: []string{"disabled", "low", "medium", "high"}, Def: "disabled"},
	// aGain = analog gain limit as a multiple (4 -> "analog: 4"), majestic caps it at 64;
	// the default removes the key from the YAML (without it majestic reports "analog: 128")
	{Key: "isp.aGain", Mode: "restart", DropDef: true, Group: "exposure", Type: "range",
		Min: 1, Max: 64, Step: 1, Unit: "×", Def: "64"},
	{Key: "isp.wdr", Mode: "restart", Group: "exposure", Type: "select", Options: []string{"0", "1"}, Def: "0"},
	{Key: "video0.bitrate", Group: "video", Type: "range", Min: 2000, Max: 20000, Step: 500, Unit: "kbit/s", Def: "8000"},
	{Key: "video0.fps", Group: "video", Type: "range", Min: 15, Max: 30, Step: 1, Def: "25"},
	{Key: "video0.gopSize", Group: "video", Type: "range", Min: 0.2, Max: 2, Step: 0.1, Unit: "s", Def: "0.5"},
	{Key: "video0.rcMode", Group: "video", Type: "select", Options: []string{"cbr", "vbr", "avbr"}, Def: "cbr"},
	{Key: "video0.minQp", Group: "video", Type: "range", Min: 0, Max: 51, Step: 1, Def: "12"},
	{Key: "video0.maxQp", Group: "video", Type: "range", Min: 0, Max: 51, Step: 1, Def: "48"},
	{Key: "video0.qpDelta", Group: "video", Type: "range", Min: -12, Max: 12, Step: 1, Def: "-7"},
}

func findCtrl(key string) *camCtrl {
	for i := range camCtrls {
		if camCtrls[i].Key == key {
			return &camCtrls[i]
		}
	}
	return nil
}

func valStr(v interface{}) string {
	switch x := v.(type) {
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return x
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

// normalize validates and rounds a value for majestic
func (c *camCtrl) normalize(raw string) (string, error) {
	switch c.Type {
	case "bool":
		return strconv.FormatBool(raw == "true" || raw == "1"), nil
	case "select":
		for _, o := range c.Options {
			if o == raw {
				return raw, nil
			}
		}
		return "", fmt.Errorf("%s: invalid option %q", c.Key, raw)
	case "range":
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return "", fmt.Errorf("%s: not a number", c.Key)
		}
		f = math.Max(c.Min, math.Min(c.Max, math.Round(f/c.Step)*c.Step))
		if c.Step < 1 {
			return strconv.FormatFloat(f, 'f', 1, 64), nil
		}
		return strconv.FormatFloat(f, 'f', 0, 64), nil
	}
	return "", fmt.Errorf("%s: unknown type", c.Key)
}

var httpc = &http.Client{Timeout: 4 * time.Second}

func majesticLive() (map[string]string, error) {
	resp, err := httpc.Get(*majesticURL + "/api/v1/config.json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var cfg map[string]map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config.json: %v", err)
	}
	out, saved, m := map[string]string{}, yamlRead(*majesticYAML), majesticMetrics()
	for _, c := range camCtrls {
		switch c.Mode {
		case "restart": // what is in the YAML applies (majestic was started with it)
			if v, ok := saved[c.Key]; ok {
				out[c.Key] = v
			} else {
				out[c.Key] = c.Def
			}
		case "night":
			out[c.Key] = strconv.FormatBool(m["night_enabled"] == "1")
		default:
			sec, key, _ := strings.Cut(c.Key, ".")
			if v, ok := cfg[sec][key]; ok {
				out[c.Key] = valStr(v)
			}
		}
	}
	return out, nil
}

// majesticMetrics reads /metrics (Prometheus text): isp_exposure, isp_again, isp_dgain, isp_fps, night_enabled, ...
func majesticMetrics() map[string]string {
	out := map[string]string{}
	resp, err := httpc.Get(*majesticURL + "/metrics")
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && !strings.HasPrefix(l, "#") {
			out[f[0]] = f[1]
		}
	}
	return out
}

var camMu sync.Mutex // camera changes one at a time (a majestic restart takes several seconds)

// applyValues splits the values by how majestic changes them and applies them
func applyValues(vals map[string]string) error {
	live, restart, night := map[string]string{}, map[string]string{}, ""
	saved := yamlRead(*majesticYAML)
	for k, v := range vals {
		c := findCtrl(k)
		switch c.Mode {
		case "restart":
			cur, ok := saved[k]
			if !ok {
				cur = c.Def
			}
			if cur != v {
				if c.DropDef && v == c.Def {
					v = "" // default = no key in the YAML, as before any change
				}
				restart[k] = v
			}
		case "night":
			night = v
		default:
			live[k] = v
		}
	}
	if len(restart) > 0 {
		if err := restartMajestic(restart); err != nil {
			return err
		}
	}
	if err := majesticSet(live); err != nil {
		return err
	}
	if night != "" {
		mode := "off"
		if night == "true" {
			mode = "on"
		}
		resp, err := httpc.Get(*majesticURL + "/night/" + mode)
		if err != nil {
			return err
		}
		resp.Body.Close()
	}
	return nil
}

// restartMajestic writes the values to the YAML, restarts majestic and re-applies unsaved live changes
func restartMajestic(yamlVals map[string]string) error {
	before, err := majesticLive()
	if err != nil {
		return err
	}
	if err := yamlWrite(*majesticYAML, yamlVals); err != nil {
		return err
	}
	log.Printf("[web] restarting majestic for %v", yamlVals)
	// same as S95vmoto; majestic detaches from sh and keeps running on its own
	exec.Command("sh", "-c", "killall majestic; sleep 2; majestic -s > /tmp/majestic.log 2>&1 &").Run()
	deadline := time.Now().Add(20 * time.Second)
	for {
		time.Sleep(time.Second)
		if _, err := majesticLive(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("majestic not responding after restart")
		}
	}
	restore := map[string]string{}
	for _, c := range camCtrls {
		if v, ok := before[c.Key]; ok && c.Mode != "restart" {
			restore[c.Key] = v
		}
	}
	return applyValues(restore)
}

func majesticSet(vals map[string]string) error {
	if len(vals) == 0 {
		return nil
	}
	q := url.Values{}
	for k, v := range vals {
		q.Set(k, v)
	}
	resp, err := httpc.Get(*majesticURL + "/api/v1/set?" + q.Encode())
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("majestic replied %s", resp.Status)
	}
	return nil
}

// ---------- /etc/majestic.yaml (two levels only: section and key) ----------

func isSection(line string) (string, bool) {
	t := strings.TrimRight(line, " \r")
	if t == "" || line[0] == ' ' || line[0] == '#' || !strings.HasSuffix(t, ":") {
		return "", false
	}
	return strings.TrimSuffix(t, ":"), true
}

func yamlRead(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	sec := ""
	for _, line := range strings.Split(string(b), "\n") {
		if s, ok := isSection(line); ok {
			sec = s
			continue
		}
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `'"`)
		out[sec+"."+k] = v
	}
	return out
}

func yamlWrite(path string, vals map[string]string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for full, v := range vals { // empty value = remove the key
		sec, key, _ := strings.Cut(full, ".")
		entry := "  " + key + ": " + v
		start := -1
		for i, l := range lines {
			if s, ok := isSection(l); ok && s == sec {
				start = i
				break
			}
		}
		if start < 0 {
			if v != "" {
				lines = append(lines, sec+":", entry)
			}
			continue
		}
		end, done := len(lines), false
		for i := start + 1; i < len(lines); i++ {
			if _, ok := isSection(lines[i]); ok {
				end = i
				break
			}
		}
		for i := start + 1; i < end; i++ {
			if strings.HasPrefix(lines[i], "  "+key+":") {
				if v == "" {
					lines = append(lines[:i], lines[i+1:]...)
				} else {
					lines[i] = entry
				}
				done = true
				break
			}
		}
		if !done && v != "" {
			lines = append(lines[:start+1], append([]string{entry}, lines[start+1:]...)...)
		}
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		if err := os.Rename(tmp, path); err == nil {
			return nil
		}
		os.Remove(tmp)
	}
	return os.WriteFile(path, data, 0644) // e.g. when the file is bind-mounted
}

// ---------- status ----------

func run(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out))
}

func tailLines(path string, n int, skip *regexp.Regexp) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const size = 64 << 10
	if st, err := f.Stat(); err == nil && st.Size() > size {
		f.Seek(st.Size()-size, 0)
	}
	buf := make([]byte, size)
	m, _ := f.Read(buf)
	var out []string
	for _, l := range strings.Split(string(buf[:m]), "\n") {
		if strings.TrimSpace(l) == "" || (skip != nil && skip.MatchString(l)) {
			continue
		}
		if len(l) > 200 {
			l = l[:200]
		}
		out = append(out, l)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func kv(text, sep string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), sep); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

var (
	reThermal = regexp.MustCompile(`temperature: (-?\d+)`)
	reLogSkip = regexp.MustCompile(`\[10920\] 10s:|\[ec-init\] dial`) // repeats every 3 s until the dash is there
)

func webStatus(s *state) map[string]interface{} {
	s.mu.Lock()
	conns, since := s.ctrlConns, s.since
	s.mu.Unlock()

	wpa := kv(run("wpa_cli", "-p", "/tmp/wpa_ctrl", "-i", "wlan0", "status"), "=")
	link := kv(run("iw", "dev", "wlan0", "link"), ":")
	state := "searching"
	switch {
	case fileExists("/etc/vmoto/DISABLE"):
		state = "paused"
	case conns > 0:
		state = "connected"
	case wpa["wpa_state"] == "COMPLETED":
		state = "wifi" // Wi-Fi Direct up, waiting for the dash
	}
	disp := map[string]interface{}{"ssid": wpa["ssid"], "freq": wpa["freq"], "ip": wpa["ip_address"],
		"signal": link["signal"], "tx_rate": link["tx bitrate"]}
	if conns > 0 && !since.IsZero() {
		disp["connected_s"] = int(time.Since(since).Seconds())
	}

	statsMu.Lock()
	st := lastStats
	statsMu.Unlock()
	var stream interface{}
	if conns > 0 && !st.At.IsZero() && time.Since(st.At) < 30*time.Second {
		stream = st
	}

	var wifiTemps []string
	if b, err := os.ReadFile("/proc/net/rtl88x2eu/wlan0/thermal_state"); err == nil {
		for _, m := range reThermal.FindAllStringSubmatch(string(b), -1) {
			wifiTemps = append(wifiTemps, m[1])
		}
	}
	var up float64
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		fmt.Sscan(string(b), &up)
	}
	load := ""
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 3 {
			load = strings.Join(f[:3], " / ")
		}
	}
	mem := map[string]string{}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		mem = kv(string(b), ":")
	}
	logs := append(tailLines("/tmp/vmoto.log", 10, nil), tailLines("/tmp/wfl.log", 15, reLogSkip)...)

	return map[string]interface{}{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"state":    state,
		"display":  disp,
		"stream":   stream,
		"majestic": run("pidof", "majestic") != "",
		"sensor":   sensorMetrics(),
		"device": map[string]interface{}{"soc_temp": run("ipcinfo", "--temp"), "wifi_temps": wifiTemps,
			"load": load, "mem_avail": mem["MemAvailable"], "mem_total": mem["MemTotal"], "uptime_min": int(up / 60)},
		"log": logs,
	}
}

// current state of the sensor's auto exposure from majestic (gain in 1/1024)
func sensorMetrics() map[string]string {
	m := majesticMetrics()
	if m["isp_exposure"] == "" {
		return nil
	}
	return map[string]string{"exposure": m["isp_exposure"], "again": m["isp_again"], "dgain": m["isp_dgain"],
		"fps": m["isp_fps"], "night": m["night_enabled"]}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ---------- HTTP ----------

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// cameraReply: msg is a key the page translates (m_saved, ...) or "error: <detail>" shown as is
func cameraReply(w http.ResponseWriter, msg string) {
	live, err := majesticLive()
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "msg": "error: majestic not responding: " + err.Error(),
			"ctrls": camCtrls, "live": map[string]string{}, "saved": yamlRead(*majesticYAML)})
		return
	}
	saved := map[string]string{}
	all := yamlRead(*majesticYAML)
	for _, c := range camCtrls {
		if v, ok := all[c.Key]; ok {
			saved[c.Key] = v
		}
	}
	writeJSON(w, map[string]interface{}{"ok": true, "msg": msg, "ctrls": camCtrls, "live": live, "saved": saved})
}

func errMsg(err error, ok string) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return ok
}

func webServer(s *state) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(webPage)
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, webStatus(s)) })
	mux.HandleFunc("/api/camera", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			cameraReply(w, "")
			return
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			cameraReply(w, "m_invalid_json")
			return
		}
		vals := map[string]string{}
		for k, v := range body {
			c := findCtrl(k)
			if c == nil {
				continue
			}
			n, err := c.normalize(valStr(v))
			if err != nil {
				cameraReply(w, "error: "+err.Error())
				return
			}
			vals[k] = n
		}
		camMu.Lock()
		defer camMu.Unlock()
		cameraReply(w, errMsg(applyValues(vals), "OK"))
	})
	mux.HandleFunc("/api/camera/defaults", func(w http.ResponseWriter, r *http.Request) {
		vals := map[string]string{}
		for _, c := range camCtrls {
			vals[c.Key] = c.Def
		}
		camMu.Lock()
		defer camMu.Unlock()
		cameraReply(w, errMsg(applyValues(vals), "m_defaults"))
	})
	mux.HandleFunc("/api/camera/revert", func(w http.ResponseWriter, r *http.Request) {
		all, vals := yamlRead(*majesticYAML), map[string]string{"night": "false"}
		for _, c := range camCtrls {
			if v, ok := all[c.Key]; ok && c.Mode == "" {
				if n, err := c.normalize(v); err == nil {
					vals[c.Key] = n
				}
			}
		}
		camMu.Lock()
		defer camMu.Unlock()
		cameraReply(w, errMsg(applyValues(vals), "m_reverted"))
	})
	mux.HandleFunc("/api/camera/save", func(w http.ResponseWriter, r *http.Request) {
		camMu.Lock()
		defer camMu.Unlock()
		live, err := majesticLive()
		if err != nil {
			cameraReply(w, "error: "+err.Error())
			return
		}
		vals := map[string]string{}
		for _, c := range camCtrls {
			if v, ok := live[c.Key]; ok && c.Mode == "" { // "restart" already is in the YAML, "night" is not saved
				vals[c.Key] = v
			}
		}
		err = yamlWrite(*majesticYAML, vals)
		if err == nil {
			log.Printf("[web] camera settings saved to %s", *majesticYAML)
		}
		cameraReply(w, errMsg(err, "m_saved"))
	})

	lastErr := ""
	for {
		addr := *webAddr
		if *webIf != "" {
			ip := ifaceIPv4(*webIf)
			if ip == "" {
				time.Sleep(5 * time.Second)
				continue
			}
			_, port, _ := net.SplitHostPort(*webAddr)
			addr = net.JoinHostPort(ip, port)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			if err.Error() != lastErr {
				log.Printf("[web] %v (retrying)", err)
				lastErr = err.Error()
			}
			time.Sleep(10 * time.Second)
			continue
		}
		log.Printf("[web] http://%s/", addr)
		lastErr = ""
		err = http.Serve(ln, mux)
		log.Printf("[web] stopped: %v", err)
		time.Sleep(5 * time.Second)
	}
}

func ifaceIPv4(name string) string {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}
