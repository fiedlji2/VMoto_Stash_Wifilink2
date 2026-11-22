// Wi-Fi TX power from the web page. The driver takes a target in mBm (iw dev wlan0 set txpower fixed) and maps it
// to the chip's power index in 0.25 dB steps (/proc/net/rtl88x2eu/wlan0/tx_power_idx). Index 0 is reached already
// at ~4-5 dBm, so lower targets give the chip minimum. Live via iw; Save writes TXP= in vmoto.conf, which
// vmoto-loop.sh applies after every (re)connect to the dash.
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var vmotoConf = flag.String("vmotoconf", "/etc/vmoto/vmoto.conf", "loop configuration with TXP= (mBm)")

var reTxp = regexp.MustCompile(`txpower (-?[\d.]+) dBm`)

// txpLive: current target of the driver in dBm (empty if wlan0 is missing)
func txpLive() string {
	m := reTxp.FindStringSubmatch(run("iw", "dev", "wlan0", "info"))
	if m == nil {
		return ""
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	return strconv.FormatFloat(f, 'f', 0, 64)
}

func txpSet(dbm string) error {
	f, err := strconv.ParseFloat(dbm, 64)
	if err != nil {
		return err
	}
	run("iw", "dev", "wlan0", "set", "txpower", "fixed", strconv.Itoa(int(f*100)))
	if got := txpLive(); got != strconv.Itoa(int(f)) {
		return fmt.Errorf("iw: power %s dBm, want %s", got, dbm)
	}
	return nil
}

// txpSaved: TXP from vmoto.conf in dBm ("" without the line)
func txpSaved() string {
	b, err := os.ReadFile(*vmotoConf)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "TXP=") { // Go 1.18: no strings.CutPrefix
			mbm, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(l, "TXP=")))
			if err != nil {
				return ""
			}
			return strconv.Itoa(mbm / 100)
		}
	}
	return ""
}

// txpSave replaces (or adds) TXP= in vmoto.conf, the rest of the file stays
func txpSave(dbm string) error {
	f, err := strconv.ParseFloat(dbm, 64)
	if err != nil {
		return err
	}
	line := "TXP=" + strconv.Itoa(int(f*100))
	b, _ := os.ReadFile(*vmotoConf)
	lines, done := strings.Split(strings.TrimRight(string(b), "\n"), "\n"), false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "TXP=") {
			lines[i], done = line, true
		}
	}
	if !done {
		lines = append(lines, line)
	}
	tmp := *vmotoConf + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, *vmotoConf)
}
