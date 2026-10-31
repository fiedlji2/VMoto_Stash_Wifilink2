package main

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// SPS z WiFiLinku (majestic, Main profil) a ze souborů v SPS_FILES (Annex-B) přepíše a uloží
// do /tmp/sps_<n>.h264 pro kontrolu přes ffmpeg -bsf:v trace_headers.
func TestRewriteSPS(t *testing.T) {
	inputs := map[string][]byte{}
	dev, _ := hex.DecodeString(strings.ReplaceAll("67 4d 00 1f e9 00 80 09 74 20 00 00 7d 20 00 18 70 40 80", " ", ""))
	inputs["device"] = dev
	for _, f := range strings.Fields(os.Getenv("SPS_FILES")) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, nal := range splitAnnexB(data) {
			if len(nal) > 0 && nal[0]&0x1F == 7 {
				inputs[f] = nal
				break
			}
		}
	}
	for name, sps := range inputs {
		out, err := rewriteSPS(sps)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%s:\n  in  % x\n  out % x", name, sps, out)
		safe := strings.NewReplacer("/", "_", ".", "_").Replace(name)
		os.WriteFile("/tmp/sps_"+safe+".bin", append([]byte{0, 0, 0, 1}, out...), 0o644)
	}
}

func splitAnnexB(b []byte) [][]byte {
	var out [][]byte
	var starts []int
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			starts = append(starts, i+3)
			i += 2
		}
	}
	for k, s := range starts {
		e := len(b)
		if k+1 < len(starts) {
			e = starts[k+1] - 3
			for e > s && b[e-1] == 0 {
				e--
			}
		}
		out = append(out, b[s:e])
	}
	return out
}
