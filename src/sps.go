package main

// Úprava H.264 SPS pro nízké zpoždění dekodéru displeje:
// doplní/přepíše VUI bitstream_restriction (max_num_reorder_frames=0, max_dec_frame_buffering=1),
// aby dekodér nedržel snímky v zásobě. Profil Main/High bez B-snímků (SigmaStar) to splňuje.

import "errors"

type bitReader struct {
	b   []byte
	pos int // v bitech
}

func (r *bitReader) u(n int) (uint32, error) {
	var v uint32
	for i := 0; i < n; i++ {
		if r.pos >= len(r.b)*8 {
			return 0, errors.New("konec SPS")
		}
		bit := (r.b[r.pos/8] >> (7 - uint(r.pos%8))) & 1
		v = v<<1 | uint32(bit)
		r.pos++
	}
	return v, nil
}

func (r *bitReader) ue() (uint32, error) {
	zeros := 0
	for {
		b, err := r.u(1)
		if err != nil {
			return 0, err
		}
		if b == 1 {
			break
		}
		zeros++
		if zeros > 31 {
			return 0, errors.New("chybný Exp-Golomb")
		}
	}
	if zeros == 0 {
		return 0, nil
	}
	v, err := r.u(zeros)
	return (1<<uint(zeros) - 1) + v, err
}

func (r *bitReader) se() (int32, error) {
	v, err := r.ue()
	if v&1 == 1 {
		return int32((v + 1) / 2), err
	}
	return -int32(v / 2), err
}

type bitWriter struct {
	b    []byte
	nbit int
}

func (w *bitWriter) u(n int, v uint32) {
	for i := n - 1; i >= 0; i-- {
		if w.nbit%8 == 0 {
			w.b = append(w.b, 0)
		}
		if (v>>uint(i))&1 == 1 {
			w.b[len(w.b)-1] |= 1 << (7 - uint(w.nbit%8))
		}
		w.nbit++
	}
}

func (w *bitWriter) ue(v uint32) {
	v++
	n := 0
	for t := v; t > 1; t >>= 1 {
		n++
	}
	w.u(n, 0)
	w.u(n+1, v)
}

// copyBits přepíše bity z readeru od pozice from do aktuální pozice
func copyBits(w *bitWriter, r *bitReader, from int) {
	save := r.pos
	r.pos = from
	for r.pos < save {
		b, _ := r.u(1)
		w.u(1, b)
	}
}

func unescapeRBSP(n []byte) []byte {
	out := make([]byte, 0, len(n))
	zeros := 0
	for _, c := range n {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

func escapeRBSP(n []byte) []byte {
	out := make([]byte, 0, len(n)+8)
	zeros := 0
	for _, c := range n {
		if zeros >= 2 && c <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

func skipHRD(r *bitReader) error {
	cnt, err := r.ue()
	if err != nil {
		return err
	}
	if _, err := r.u(8); err != nil { // bit_rate_scale, cpb_size_scale
		return err
	}
	for i := uint32(0); i <= cnt; i++ {
		r.ue()
		r.ue()
		if _, err := r.u(1); err != nil {
			return err
		}
	}
	_, err = r.u(20) // 4x 5 bitů délek
	return err
}

// rewriteSPS vrátí SPS NAL (s hlavičkou, bez start kódu) s VUI bitstream_restriction pro nulové přeřazování.
func rewriteSPS(nal []byte) ([]byte, error) {
	if len(nal) < 4 || nal[0]&0x1F != 7 {
		return nil, errors.New("není SPS")
	}
	rb := unescapeRBSP(nal[1:])
	r := &bitReader{b: rb}
	profile, _ := r.u(8)
	r.u(16) // constraint + level
	r.ue()  // sps id
	if profile == 100 || profile == 110 || profile == 122 || profile == 244 || profile == 44 ||
		profile == 83 || profile == 86 || profile == 118 || profile == 128 || profile == 138 || profile == 139 || profile == 134 {
		cf, _ := r.ue()
		if cf == 3 {
			r.u(1)
		}
		r.ue()
		r.ue()
		r.u(1)
		if sm, _ := r.u(1); sm == 1 {
			return nil, errors.New("SPS se scaling maticemi neumím")
		}
	}
	r.ue() // log2_max_frame_num
	poc, _ := r.ue()
	if poc == 0 {
		r.ue()
	} else if poc == 1 {
		r.u(1)
		r.se()
		r.se()
		n, _ := r.ue()
		for i := uint32(0); i < n; i++ {
			r.se()
		}
	}
	numRef, _ := r.ue()
	r.u(1)
	r.ue()
	r.ue()
	if fmo, _ := r.u(1); fmo == 0 {
		r.u(1)
	}
	r.u(1)
	if crop, _ := r.u(1); crop == 1 {
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	vuiFlagPos := r.pos
	vui, err := r.u(1)
	if err != nil {
		return nil, err
	}
	// vše před vui_parameters_present_flag beze změny, flag = 1
	w2 := &bitWriter{}
	copyBits(w2, r, 0)
	w2.b[len(w2.b)-1] |= 1 << (7 - uint((vuiFlagPos)%8))
	if vui == 0 {
		// aspect_ratio, overscan, video_signal, chroma_loc, timing, nal_hrd, vcl_hrd, pic_struct = 0
		w2.u(8, 0)
	} else {
		vs := r.pos
		if ar, _ := r.u(1); ar == 1 {
			if idc, _ := r.u(8); idc == 255 {
				r.u(32)
			}
		}
		if o, _ := r.u(1); o == 1 {
			r.u(1)
		}
		if vsig, _ := r.u(1); vsig == 1 {
			r.u(4)
			if cd, _ := r.u(1); cd == 1 {
				r.u(24)
			}
		}
		if cl, _ := r.u(1); cl == 1 {
			r.ue()
			r.ue()
		}
		if ti, _ := r.u(1); ti == 1 {
			r.u(32)
			r.u(32)
			r.u(1)
		}
		nh, _ := r.u(1)
		if nh == 1 {
			if err := skipHRD(r); err != nil {
				return nil, err
			}
		}
		vh, _ := r.u(1)
		if vh == 1 {
			if err := skipHRD(r); err != nil {
				return nil, err
			}
		}
		if nh == 1 || vh == 1 {
			r.u(1)
		}
		_, err := r.u(1) // pic_struct_present_flag
		if err != nil {
			return nil, err
		}
		copyBits(w2, r, vs) // VUI až po pic_struct_present_flag beze změny
		// původní bitstream_restriction (pokud byl) se zahodí a napíše se nový
	}
	w2.u(1, 1) // bitstream_restriction_flag
	w2.u(1, 1) // motion_vectors_over_pic_boundaries_flag
	w2.ue(0)   // max_bytes_per_pic_denom
	w2.ue(0)   // max_bits_per_mb_denom
	w2.ue(16)  // log2_max_mv_length_horizontal
	w2.ue(16)  // log2_max_mv_length_vertical
	w2.ue(0)   // max_num_reorder_frames
	dpb := numRef
	if dpb < 1 {
		dpb = 1
	}
	w2.ue(dpb) // max_dec_frame_buffering
	w2.u(1, 1) // rbsp_stop_one_bit
	for w2.nbit%8 != 0 {
		w2.u(1, 0)
	}
	return append([]byte{nal[0]}, escapeRBSP(w2.b)...), nil
}
