// Webová stránka WiFiLinku jen na ethernetu (jako web.py na Pi):
//
//	/                    HTML stránka, obnovuje se sama
//	/api/status          stav jako JSON (displej, přenos, Wi-Fi, teploty, log)
//	/api/camera          GET: nastavení kamery (rozsahy, živé a uložené hodnoty)
//	                     POST {"image.contrast": 55, ...}: nastaví hned přes API majesticu (/api/v1/set)
//	/api/camera/save     POST: zapíše živé hodnoty do /etc/majestic.yaml (vydrží restart)
//	/api/camera/defaults POST: výchozí hodnoty naživo, bez uložení
//	/api/camera/revert   POST: vrátí uložené hodnoty naživo
//
// Majestic drží změny z /api/v1/set jen v paměti a každá změna na okamžik přenastaví enkodér
// (obraz běží dál, displej dostane nový úplný snímek). Proto Uložit zapisuje do YAML zvlášť.
// Zesílení a WDR API odmítá, zrcadlení/převrácení API přijme, ale snímač nezmění: zapíšou se hned
// do YAML a majestic se restartuje (~5 s bez obrazu).
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
	webAddr      = flag.String("web", ":80", "webová stránka (port; IP se vezme z rozhraní -webif)")
	webIf        = flag.String("webif", "eth0", "rozhraní pro webovou stránku (prázdné = poslouchat na adrese z -web)")
	majesticURL  = flag.String("majestic", "http://127.0.0.1:8080", "HTTP API majesticu")
	majesticYAML = flag.String("yaml", "/etc/majestic.yaml", "konfigurace majesticu pro Uložit")
)

//go:embed web.html
var webPage []byte

// ---------- statistiky přenosu (plní handleMediaData každých 10 s) ----------

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

// ---------- ovládání kamery ----------

// Jak se hodnota u majesticu mění:
//
//	""        naživo přes /api/v1/set, Uložit zapíše do YAML
//	"restart" jen přes YAML + restart majesticu (API klíč odmítne) - ukládá se hned
//	"night"   /night/on|off, stav z /metrics/night - neukládá se (po restartu vždy den)
type camCtrl struct {
	Key     string   `json:"key"` // sekce.klíč v majesticu
	Mode    string   `json:"mode,omitempty"`
	DropDef bool     `json:"-"` // výchozí hodnota = klíč z YAML smazat
	Group   string   `json:"group"`
	Label   string   `json:"label"`
	Type    string   `json:"type"` // bool | range | select
	Min     float64  `json:"min,omitempty"`
	Max     float64  `json:"max,omitempty"`
	Step    float64  `json:"step,omitempty"`
	Unit    string   `json:"unit,omitempty"`
	Options []string `json:"options,omitempty"`
	Def     string   `json:"default"`
}

// Kodek, rozlišení a profil se nenabízí: displej chce H.264 1024×592 a úprava SPS počítá s Main.
var camCtrls = []camCtrl{
	// zrcadlení/převrácení nastavuje majestic snímači jen při startu (API změní config, snímač ne)
	{Key: "image.mirror", Mode: "restart", Group: "Obraz", Label: "Zrcadlit (zpětné zrcátko)", Type: "bool", Def: "true"},
	{Key: "image.flip", Mode: "restart", Group: "Obraz", Label: "Převrátit vzhůru nohama", Type: "bool", Def: "false"},
	{Key: "image.luminance", Group: "Obraz", Label: "Jas", Type: "range", Min: 0, Max: 100, Step: 1, Def: "47"},
	{Key: "image.contrast", Group: "Obraz", Label: "Kontrast", Type: "range", Min: 0, Max: 100, Step: 1, Def: "50"},
	{Key: "image.saturation", Group: "Obraz", Label: "Sytost", Type: "range", Min: 0, Max: 100, Step: 1, Def: "40"},
	{Key: "image.hue", Group: "Obraz", Label: "Odstín", Type: "range", Min: 0, Max: 100, Step: 1, Def: "50"},
	{Key: "night", Mode: "night", Group: "Obraz", Label: "Černobílý (noční) režim", Type: "bool", Def: "false"},
	{Key: "isp.exposure", Group: "Expozice", Label: "Nejdelší expozice", Type: "range", Min: 1, Max: 40, Step: 1, Unit: "ms", Def: "14"},
	{Key: "isp.antiFlicker", Group: "Expozice", Label: "Proti blikání světel", Type: "select",
		Options: []string{"disabled", "50", "60"}, Def: "disabled"},
	{Key: "isp.slowShutter", Group: "Expozice", Label: "Pomalá závěrka potmě", Type: "select",
		Options: []string{"disabled", "low", "medium", "high"}, Def: "disabled"},
	// aGain = strop analogového zesílení v násobcích (4 -> "analog: 4"), nad 64 majestic omezí na 64;
	// výchozí hodnota se z YAML smaže (bez klíče majestic hlásí "analog: 128")
	{Key: "isp.aGain", Mode: "restart", DropDef: true, Group: "Expozice", Label: "Strop zesílení snímače", Type: "range",
		Min: 1, Max: 64, Step: 1, Unit: "×", Def: "64"},
	{Key: "isp.wdr", Mode: "restart", Group: "Expozice", Label: "WDR (zkušebně)", Type: "select",
		Options: []string{"0", "1"}, Def: "0"},
	{Key: "video0.bitrate", Group: "Video", Label: "Datový tok", Type: "range", Min: 2000, Max: 20000, Step: 500, Unit: "kbit/s", Def: "8000"},
	{Key: "video0.fps", Group: "Video", Label: "Snímků za sekundu", Type: "range", Min: 15, Max: 30, Step: 1, Def: "25"},
	{Key: "video0.gopSize", Group: "Video", Label: "Úplný snímek každých", Type: "range", Min: 0.2, Max: 2, Step: 0.1, Unit: "s", Def: "0.5"},
	{Key: "video0.rcMode", Group: "Video", Label: "Řízení datového toku", Type: "select",
		Options: []string{"cbr", "vbr", "avbr"}, Def: "cbr"},
	{Key: "video0.minQp", Group: "Video", Label: "Nejlepší kvalita (min. QP)", Type: "range", Min: 0, Max: 51, Step: 1, Def: "12"},
	{Key: "video0.maxQp", Group: "Video", Label: "Nejhorší kvalita (max. QP)", Type: "range", Min: 0, Max: 51, Step: 1, Def: "48"},
	{Key: "video0.qpDelta", Group: "Video", Label: "Úplný snímek kvalitněji o (QP)", Type: "range", Min: -12, Max: 12, Step: 1, Def: "-7"},
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

// normalize ověří a zaokrouhlí hodnotu pro majestic
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
		return "", fmt.Errorf("%s: neplatná volba %q", c.Key, raw)
	case "range":
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return "", fmt.Errorf("%s: není číslo", c.Key)
		}
		f = math.Max(c.Min, math.Min(c.Max, math.Round(f/c.Step)*c.Step))
		if c.Step < 1 {
			return strconv.FormatFloat(f, 'f', 1, 64), nil
		}
		return strconv.FormatFloat(f, 'f', 0, 64), nil
	}
	return "", fmt.Errorf("%s: neznámý typ", c.Key)
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
		case "restart": // platí to, co je v YAML (majestic se s ním spustil)
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

// majesticMetrics čte /metrics (text Prometheus): isp_exposure, isp_again, isp_dgain, isp_fps, night_enabled, ...
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

var camMu sync.Mutex // změny kamery jedna po druhé (restart majesticu trvá několik sekund)

// applyValues rozdělí hodnoty podle způsobu změny a provede je
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
					v = "" // výchozí = klíč v YAML nemít, jako před úpravou
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

// restartMajestic zapíše hodnoty do YAML, restartuje majestic a vrátí mu neuložené živé změny
func restartMajestic(yamlVals map[string]string) error {
	before, err := majesticLive()
	if err != nil {
		return err
	}
	if err := yamlWrite(*majesticYAML, yamlVals); err != nil {
		return err
	}
	log.Printf("[web] restart majesticu kvůli %v", yamlVals)
	// stejně jako S95vmoto; majestic se odpojí od sh a běží dál sám
	exec.Command("sh", "-c", "killall majestic; sleep 2; majestic -s > /tmp/majestic.log 2>&1 &").Run()
	deadline := time.Now().Add(20 * time.Second)
	for {
		time.Sleep(time.Second)
		if _, err := majesticLive(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("majestic po restartu neodpovídá")
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
		return fmt.Errorf("majestic odpověděl %s", resp.Status)
	}
	return nil
}

// ---------- /etc/majestic.yaml (jen dvě úrovně: sekce a klíč) ----------

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
	for full, v := range vals { // prázdná hodnota = klíč smazat
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
	return os.WriteFile(path, data, 0644) // např. když je soubor připojený bind mountem
}

// ---------- stav ----------

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
	reLogSkip = regexp.MustCompile(`\[10920\] 10s:|\[ec-init\] dial`) // opakuje se každé 3 s, než je displej
)

func webStatus(s *state) map[string]interface{} {
	s.mu.Lock()
	conns, since := s.ctrlConns, s.since
	s.mu.Unlock()

	wpa := kv(run("wpa_cli", "-p", "/tmp/wpa_ctrl", "-i", "wlan0", "status"), "=")
	link := kv(run("iw", "dev", "wlan0", "link"), ":")
	state := "hledá displej"
	switch {
	case fileExists("/etc/vmoto/DISABLE"):
		state = "pozastaveno (DISABLE)"
	case conns > 0:
		state = "připojeno k displeji"
	case wpa["wpa_state"] == "COMPLETED":
		state = "Wi-Fi připojeno, čeká na displej"
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

// aktuální stav automatiky snímače z majesticu (zesílení je v 1/1024)
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

func cameraReply(w http.ResponseWriter, msg string) {
	live, err := majesticLive()
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "msg": "majestic neodpovídá: " + err.Error(),
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
		return "chyba: " + err.Error()
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
			cameraReply(w, "chyba: neplatný JSON")
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
				cameraReply(w, "chyba: "+err.Error())
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
		cameraReply(w, errMsg(applyValues(vals), "výchozí hodnoty (neuloženo)"))
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
		cameraReply(w, errMsg(applyValues(vals), "vráceno na uložené"))
	})
	mux.HandleFunc("/api/camera/save", func(w http.ResponseWriter, r *http.Request) {
		camMu.Lock()
		defer camMu.Unlock()
		live, err := majesticLive()
		if err != nil {
			cameraReply(w, "chyba: "+err.Error())
			return
		}
		vals := map[string]string{}
		for _, c := range camCtrls {
			if v, ok := live[c.Key]; ok && c.Mode == "" { // "restart" už v YAML je, "night" se neukládá
				vals[c.Key] = v
			}
		}
		err = yamlWrite(*majesticYAML, vals)
		if err == nil {
			log.Printf("[web] nastavení kamery uloženo do %s", *majesticYAML)
		}
		cameraReply(w, errMsg(err, "uloženo – vydrží restart"))
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
				log.Printf("[web] %v (zkouším znovu)", err)
				lastErr = err.Error()
			}
			time.Sleep(10 * time.Second)
			continue
		}
		log.Printf("[web] http://%s/", addr)
		lastErr = ""
		err = http.Serve(ln, mux)
		log.Printf("[web] konec: %v", err)
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
