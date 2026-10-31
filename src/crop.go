// Digital zoom via the SigmaStar scaler (VPE). Its output port takes a crop window in sensor coordinates
// (3840x2160) and scales it to the video size, so zooming keeps the sensor's full detail up to
// sensor width / video width (~3.6x). Majestic has no option for it; the small C helper vmoto-crop
// (MI_VPE_SetPortCrop from libmi_vpe) sets it from outside while majestic runs. The window survives encoder
// reloads (/api/v1/set) but not a majestic restart (at boot majestic starts with the full sensor), so a watchdog
// compares it with /proc every second and re-applies it.
// The window always has the video's aspect ratio (1024x592 = 1.73 vs. sensor 16:9), which also removes
// the ~3 % horizontal squeeze of the full sensor picture.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"
)

var (
	cropTool = flag.String("croptool", "/etc/vmoto/vmoto-crop", "helper that sets the scaler crop (empty = no zoom)")
	cropLibs = flag.String("croplibs", "/etc/vmoto/lib", "LD_LIBRARY_PATH for the helper (libmi_sys, libmi_vpe)")
	viewFile = flag.String("viewfile", "/etc/vmoto/view.json", "saved zoom / pan")
	viewLive = flag.String("viewlive", "/tmp/vmoto-view.json", "unsaved zoom / pan in RAM (survives a restart of this program)")
)

type view struct {
	Zoom float64 `json:"zoom"`
	PanX float64 `json:"pan_x"` // -1 left .. +1 right
	PanY float64 `json:"pan_y"` // -1 up .. +1 down
}

var (
	viewMu   sync.Mutex
	curView  = view{Zoom: 1}
	cropNote string // last state for the status page
)

func readView(path string) (view, bool) {
	v := view{Zoom: 1}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &v) != nil {
		return view{Zoom: 1}, false
	}
	return v, true
}

func writeView(path string, v view) error {
	b, _ := json.Marshal(v)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadView: the saved view (Save)
func loadView() (view, bool) { return readView(*viewFile) }

func saveView(v view) error { return writeView(*viewFile, v) }

// setView changes the live view and keeps a copy in RAM
func setView(v view) {
	viewMu.Lock()
	curView = v
	viewMu.Unlock()
	writeView(*viewLive, v)
}

// state of VPE channel 0 from the driver: sensor input size, current port crop, output size
type vpeState struct{ inW, inH, cx, cy, cw, ch, outW, outH int }

var (
	reVpeIn   = regexp.MustCompile(`(?m)^\s*0\s+0\s+1\((\d+)x(\d+)\)`)
	reVpePort = regexp.MustCompile(`(?m)^\s*0\s+0\s+0\s+\S+\s+1\s+\S+\(\s*\d+,\s*\d+\)\s+(\d+),\s*(\d+),\s*(\d+),\s*(\d+)\s+(\d+)\s+(\d+)`)
)

func readVPE() (vpeState, error) {
	b, err := os.ReadFile("/proc/mi_modules/mi_vpe/mi_vpe0")
	if err != nil {
		return vpeState{}, err
	}
	in, port := reVpeIn.FindSubmatch(b), reVpePort.FindSubmatch(b)
	if in == nil || port == nil {
		return vpeState{}, fmt.Errorf("VPE channel 0 not running")
	}
	n := func(x []byte) int { i, _ := strconv.Atoi(string(x)); return i }
	s := vpeState{inW: n(in[1]), inH: n(in[2]), cx: n(port[1]), cy: n(port[2]), cw: n(port[3]), ch: n(port[4]),
		outW: n(port[5]), outH: n(port[6])}
	// while majestic starts, the port briefly reports 0x0
	if s.inW < 64 || s.inH < 64 || s.outW < 64 || s.outH < 64 || s.outW > s.inW || s.outH > s.inH {
		return vpeState{}, fmt.Errorf("VPE channel 0 not running")
	}
	return s, nil
}

// cropFor computes the crop window (x, y, w, h) for a view and the maximum zoom without upscaling
func cropFor(v view, s vpeState) (x, y, w, h int, maxZoom float64) {
	bw, bh := float64(s.inW), float64(s.inH)
	if s.inW*s.outH > s.inH*s.outW { // sensor wider than the video: full height
		bw = float64(s.inH) * float64(s.outW) / float64(s.outH)
	} else {
		bh = float64(s.inW) * float64(s.outH) / float64(s.outW)
	}
	maxZoom = bw / float64(s.outW)
	z := math.Max(1, math.Min(maxZoom, v.Zoom))
	w = int(math.Round(bw/z/8)) * 8
	h = int(math.Round(bh/z/8)) * 8
	pos := func(free int, pan float64) int {
		p := float64(free) / 2 * (1 + math.Max(-1, math.Min(1, pan)))
		return int(math.Max(0, math.Min(float64(free), math.Round(p/2)*2)))
	}
	return pos(s.inW-w, v.PanX), pos(s.inH-h, v.PanY), w, h, maxZoom
}

// applyView sets the crop for the current view if the driver has a different one
func applyView() error {
	if *cropTool == "" {
		return nil
	}
	viewMu.Lock()
	v := curView
	viewMu.Unlock()
	s, err := readVPE()
	if err != nil {
		return err
	}
	x, y, w, h, maxZ := cropFor(v, s)
	note := fmt.Sprintf("%d,%d %dx%d (max %.1fx)", x, y, w, h, maxZ)
	if s.cx == x && s.cy == y && s.cw == w && s.ch == h {
		viewMu.Lock()
		cropNote = note
		viewMu.Unlock()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, *cropTool, strconv.Itoa(x), strconv.Itoa(y), strconv.Itoa(w), strconv.Itoa(h))
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+*cropLibs)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("vmoto-crop: %v %s", err, out)
	}
	viewMu.Lock()
	cropNote = note
	viewMu.Unlock()
	return nil
}

// viewWatchdog re-applies the crop after majestic (re)starts - detected by state, not by event: every second it
// compares the crop the driver really uses with the wanted one. Logs only changes of the error state.
func viewWatchdog() {
	// unsaved changes from before a restart of this program first, then the saved view
	if v, ok := readView(*viewLive); ok {
		setView(v)
	} else if v, ok := loadView(); ok {
		setView(v)
	}
	last := "-" // log the first state too
	for {
		msg := ""
		if err := applyView(); err != nil {
			msg = err.Error()
		}
		if msg != last {
			if msg != "" {
				log.Printf("[crop] %s", msg)
			} else {
				viewMu.Lock()
				log.Printf("[crop] window %s", cropNote)
				viewMu.Unlock()
			}
			last = msg
		}
		time.Sleep(time.Second)
	}
}
