package main

import "testing"

func TestCropFor(t *testing.T) {
	s := vpeState{inW: 3840, inH: 2160, outW: 1024, outH: 592}
	cases := []struct {
		v          view
		x, y, w, h int
	}{
		{view{Zoom: 1}, 52, 0, 3736, 2160},     // dash aspect ratio, no squeeze
		{view{Zoom: 2}, 984, 540, 1872, 1080},  // centre
		{view{Zoom: 10}, 1408, 784, 1024, 592}, // capped at max (no upscaling)
	}
	for _, c := range cases {
		x, y, w, h, maxZ := cropFor(c.v, s)
		if x != c.x || y != c.y || w != c.w || h != c.h {
			t.Errorf("%+v: %d,%d %dx%d, want %d,%d %dx%d", c.v, x, y, w, h, c.x, c.y, c.w, c.h)
		}
		if maxZ < 3.64 || maxZ > 3.66 {
			t.Errorf("maxZoom %.3f", maxZ)
		}
	}
}

func TestReadVPEFormat(t *testing.T) {
	// lines from /proc/mi_modules/mi_vpe/mi_vpe0 (SSC338Q, majestic)
	txt := []byte("      0       0      1(3840x2160)  GB_12BPP         0         107(3840x2160)     2       305\n" +
		"      0       0       0   Frame       1   YUV420SP(   0,    0)   0,   0,3840,2160     1024      592       4927\n")
	in, port := reVpeIn.FindSubmatch(txt), reVpePort.FindSubmatch(txt)
	if in == nil || port == nil || string(in[1]) != "3840" || string(port[3]) != "3840" || string(port[5]) != "1024" {
		t.Fatalf("parse: in=%q port=%q", in, port)
	}
	// crop from x >= 1000: the driver puts no space after ")" (zoom ~2.3x)
	wide := []byte("      0       0       0   Frame       1   YUV420SP(   0,    0)1108, 612,1624, 936     1024      592       1007\n")
	if p := reVpePort.FindSubmatch(wide); p == nil || string(p[1]) != "1108" || string(p[4]) != "936" || string(p[6]) != "592" {
		t.Fatalf("parse wide: %q", p)
	}
	// while majestic starts: port 0x0 -> must not pass as a valid state
	starting := []byte("      0       0      1(3840x2160)  GB_12BPP\n" +
		"      0       0       0   Frame       1   YUV420SP(   0,    0)   0,   0,   0,   0        0        0\n")
	if p := reVpePort.FindSubmatch(starting); p != nil && string(p[5]) != "0" {
		t.Fatalf("parse starting: %q", p)
	}
}
