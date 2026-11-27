package notice

import "errors"

// VUI is the colour description of an SPS; Present is false when the SPS carries none.
type VUI struct {
	Present                bool
	Primaries, TRC, Matrix int
	FullRange              bool
}

var errSPS = errors.New("notice: SPS truncated or unsupported")

type bitReader struct {
	b   []byte
	pos int // bits
	err bool
	geo spsGeom
}

// spsGeom is the coded picture size and where the cropping fields lie in the RBSP (bits).
type spsGeom struct {
	w, h            int
	cropAt, cropEnd int
	ctu             int // HEVC CTU size
}

func (r *bitReader) u(n int) uint64 {
	var v uint64
	for range n {
		if r.pos >= 8*len(r.b) {
			r.err = true
			return 0
		}
		v = v<<1 | uint64(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

func (r *bitReader) flag() bool { return r.u(1) == 1 }

func (r *bitReader) ue() uint64 {
	z := 0
	for !r.err && r.u(1) == 0 {
		if z++; z > 31 {
			r.err = true
			return 0
		}
	}
	return (uint64(1)<<z - 1) + r.u(z)
}

func (r *bitReader) se() int64 {
	v := r.ue()
	if v&1 == 1 {
		return int64(v+1) / 2
	}
	return -int64(v / 2)
}

// rbsp drops the emulation prevention bytes (00 00 03).
func rbsp(nal []byte) []byte {
	out := make([]byte, 0, len(nal))
	zeros := 0
	for _, c := range nal {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

func (r *bitReader) vuiColour() VUI {
	if r.flag() { // aspect_ratio_info_present
		if r.u(8) == 255 {
			r.u(32)
		}
	}
	if r.flag() { // overscan_info_present
		r.u(1)
	}
	var v VUI
	if r.flag() { // video_signal_type_present
		r.u(3)
		v.FullRange = r.flag()
		if r.flag() { // colour_description_present
			v.Present = true
			v.Primaries, v.TRC, v.Matrix = int(r.u(8)), int(r.u(8)), int(r.u(8))
		}
	}
	return v
}

// AVCVUI reads the VUI colour of an H.264 SPS NAL (header byte included).
func AVCVUI(nal []byte) (VUI, error) {
	v, _, err := avcSPS(nal)
	return v, err
}

func avcSPS(nal []byte) (VUI, spsGeom, error) {
	v, r, err := avcParse(nal)
	return v, r.geo, err
}

func avcParse(nal []byte) (VUI, *bitReader, error) {
	r := &bitReader{b: rbsp(nal)}
	v, err := r.avc()
	return v, r, err
}

func (r *bitReader) avc() (VUI, error) {
	if r.u(8)&0x1F != 7 {
		return VUI{}, errSPS
	}
	profile := r.u(8)
	r.u(16)
	r.ue()
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := r.ue()
		if chroma == 3 {
			r.u(1)
		}
		r.ue()
		r.ue()
		r.u(1)
		if r.flag() { // seq_scaling_matrix_present
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := range lists {
				if r.flag() {
					size := 16
					if i >= 6 {
						size = 64
					}
					last, next := int64(8), int64(8)
					for range size {
						if next != 0 {
							next = (last + r.se() + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	}
	r.ue() // log2_max_frame_num_minus4
	switch r.ue() {
	case 0:
		r.ue()
	case 1:
		r.u(1)
		r.se()
		r.se()
		n := r.ue()
		if n > 255 {
			return VUI{}, errSPS
		}
		for range n {
			r.se()
		}
	}
	r.ue()
	r.u(1)
	wmb, hmu := int(r.ue())+1, int(r.ue())+1
	frames := 2
	if r.flag() { // frame_mbs_only
		frames = 1
	} else {
		r.u(1)
	}
	r.u(1)
	r.geo = spsGeom{w: wmb * 16, h: hmu * 16 * frames, cropAt: r.pos}
	if r.flag() { // frame_cropping
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	r.geo.cropEnd = r.pos
	if r.err {
		return VUI{}, errSPS
	}
	if !r.flag() {
		return VUI{}, nil
	}
	v := r.vuiColour()
	if r.err {
		return VUI{}, errSPS
	}
	return v, nil
}

// HEVCVUI reads the VUI colour of an HEVC SPS NAL (2-byte header included).
func HEVCVUI(nal []byte) (VUI, error) {
	v, _, err := hevcParse(nal)
	return v, err
}

func hevcParse(nal []byte) (VUI, *bitReader, error) {
	r := &bitReader{b: rbsp(nal)}
	v, err := r.hevc()
	return v, r, err
}

func (r *bitReader) hevc() (VUI, error) {
	if r.u(16)>>9&0x3F != 33 {
		return VUI{}, errSPS
	}
	r.u(4)
	maxSub := int(r.u(3))
	r.u(1)
	// profile_tier_level(1, maxSub)
	r.u(88)
	r.u(8)
	prof, lvl := make([]bool, maxSub), make([]bool, maxSub)
	for i := range maxSub {
		prof[i], lvl[i] = r.flag(), r.flag()
	}
	if maxSub > 0 {
		for i := maxSub; i < 8; i++ {
			r.u(2)
		}
	}
	for i := range maxSub {
		if prof[i] {
			r.u(88)
		}
		if lvl[i] {
			r.u(8)
		}
	}
	r.ue()
	if r.ue() == 3 {
		r.u(1)
	}
	w, h := int(r.ue()), int(r.ue())
	r.geo = spsGeom{w: w, h: h, cropAt: r.pos}
	if r.flag() { // conformance_window
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	r.geo.cropEnd = r.pos
	r.ue()
	r.ue()
	pocBits := int(r.ue()) + 4
	if pocBits > 16 {
		return VUI{}, errSPS
	}
	first := maxSub
	if r.flag() { // sub_layer_ordering_info_present
		first = 0
	}
	for i := first; i <= maxSub; i++ {
		r.ue()
		r.ue()
		r.ue()
	}
	minCB := int(r.ue()) + 3
	r.geo.ctu = 1 << (minCB + int(r.ue()))
	for range 4 {
		r.ue()
	}
	if r.flag() && r.flag() { // scaling_list_enabled, sps_scaling_list_data_present
		for size := 0; size < 4; size++ {
			step := 1
			if size == 3 {
				step = 3
			}
			for m := 0; m < 6; m += step {
				if !r.flag() {
					r.ue()
					continue
				}
				coefs := min(64, 1<<(4+(size<<1)))
				if size > 1 {
					r.se()
				}
				for range coefs {
					r.se()
				}
			}
		}
	}
	r.u(1)
	r.u(1)
	if r.flag() { // pcm_enabled
		r.u(8)
		r.ue()
		r.ue()
		r.u(1)
	}
	if !stRefPicSets(r) {
		return VUI{}, errSPS
	}
	if r.flag() { // long_term_ref_pics_present
		n := r.ue()
		if n > 32 {
			return VUI{}, errSPS
		}
		for range n {
			r.u(pocBits)
			r.u(1)
		}
	}
	r.u(1)
	r.u(1)
	if r.err {
		return VUI{}, errSPS
	}
	if !r.flag() {
		return VUI{}, nil
	}
	v := r.vuiColour()
	if r.err {
		return VUI{}, errSPS
	}
	return v, nil
}

// stRefPicSets skips the SPS short-term reference picture sets (7.3.7, derivation 7.4.8).
func stRefPicSets(r *bitReader) bool {
	n := int(r.ue())
	if n > 64 {
		return false
	}
	s0, s1 := make([][]int64, n), make([][]int64, n)
	for idx := range n {
		if idx != 0 && r.flag() { // inter_ref_pic_set_prediction
			ref := idx - 1
			sign := r.u(1)
			d := int64(r.ue()) + 1
			if sign == 1 {
				d = -d
			}
			nd := len(s0[ref]) + len(s1[ref])
			use := make([]bool, nd+1)
			for j := 0; j <= nd; j++ {
				used := r.flag()
				use[j] = used || r.flag()
			}
			var a, b []int64
			for j := len(s1[ref]) - 1; j >= 0; j-- {
				if p := s1[ref][j] + d; p < 0 && use[len(s0[ref])+j] {
					a = append(a, p)
				}
			}
			if d < 0 && use[nd] {
				a = append(a, d)
			}
			for j := range s0[ref] {
				if p := s0[ref][j] + d; p < 0 && use[j] {
					a = append(a, p)
				}
			}
			for j := len(s0[ref]) - 1; j >= 0; j-- {
				if p := s0[ref][j] + d; p > 0 && use[j] {
					b = append(b, p)
				}
			}
			if d > 0 && use[nd] {
				b = append(b, d)
			}
			for j := range s1[ref] {
				if p := s1[ref][j] + d; p > 0 && use[len(s0[ref])+j] {
					b = append(b, p)
				}
			}
			s0[idx], s1[idx] = a, b
		} else {
			neg, pos := r.ue(), r.ue()
			if neg > 16 || pos > 16 {
				return false
			}
			var p int64
			for range neg {
				p -= int64(r.ue()) + 1
				r.u(1)
				s0[idx] = append(s0[idx], p)
			}
			p = 0
			for range pos {
				p += int64(r.ue()) + 1
				r.u(1)
				s1[idx] = append(s1[idx], p)
			}
		}
		if r.err {
			return false
		}
	}
	return true
}

// spsNAL returns the first SPS NAL in an avcC or hvcC CodecPrivate.
func spsNAL(codec string, cp []byte) []byte {
	be16 := func(b []byte) int { return int(b[0])<<8 | int(b[1]) }
	switch codec {
	case "h264":
		if len(cp) < 8 || cp[5]&0x1F == 0 {
			return nil
		}
		if n := be16(cp[6:]); 8+n <= len(cp) {
			return cp[8 : 8+n]
		}
	case "hevc":
		if len(cp) < 23 {
			return nil
		}
		p := 23
		for range int(cp[22]) {
			if p+3 > len(cp) {
				return nil
			}
			typ, cnt := cp[p]&0x3F, be16(cp[p+1:])
			p += 3
			for range cnt {
				if p+2 > len(cp) {
					return nil
				}
				n := be16(cp[p:])
				if p+2+n > len(cp) {
					return nil
				}
				if typ == 33 {
					return cp[p+2 : p+2+n]
				}
				p += 2 + n
			}
		}
	}
	return nil
}

// frameSPS finds an in-band SPS in one access unit (NAL length framing).
func frameSPS(codec string, frame []byte, nalLen int) []byte {
	for p := 0; nalLen > 0 && p+nalLen <= len(frame); {
		n := int(beUint(frame[p : p+nalLen]))
		p += nalLen
		if n <= 0 || p+n > len(frame) {
			return nil
		}
		nal := frame[p : p+n]
		if codec == "h264" && nal[0]&0x1F == 7 || codec == "hevc" && nal[0]>>1&0x3F == 33 {
			return nal
		}
		p += n
	}
	return nil
}

type bitWriter struct {
	b   []byte
	pos int
}

func (w *bitWriter) put(v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.pos%8 == 0 {
			w.b = append(w.b, 0)
		}
		w.b[len(w.b)-1] |= byte(v>>i&1) << (7 - w.pos%8)
		w.pos++
	}
}

func (w *bitWriter) ue(v uint64) {
	n := 0
	for x := v + 1; x > 1; x >>= 1 {
		n++
	}
	w.put(0, n)
	w.put(v+1, n+1)
}

// ebsp adds the emulation prevention bytes back (00 00 0x, x <= 3).
func ebsp(b []byte) []byte {
	out := make([]byte, 0, len(b)+len(b)/64)
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

// CropSPS rewrites an SPS NAL so its visible picture is w×h, cut evenly from the coded picture
// (frame cropping in H.264, the conformance window in HEVC, 4:2:0 units of two samples). ok is
// false when w×h is larger than the coded picture or odd.
func CropSPS(codec string, nal []byte, w, h int) ([]byte, bool) {
	var r *bitReader
	var err error
	if codec == "hevc" {
		_, r, err = hevcParse(nal)
	} else {
		_, r, err = avcParse(nal)
	}
	g := r.geo
	if err != nil || w <= 0 || h <= 0 || w > g.w || h > g.h || w%2 != 0 || h%2 != 0 || g.cropEnd == 0 {
		return nil, false
	}
	last := -1 // rbsp_stop_one_bit
	for i := 8*len(r.b) - 1; i >= 0; i-- {
		if r.b[i/8]>>(7-i%8)&1 == 1 {
			last = i
			break
		}
	}
	if last < g.cropEnd {
		return nil, false
	}
	bw := &bitWriter{}
	copyBits := func(from, to int) {
		for i := from; i < to; i++ {
			bw.put(uint64(r.b[i/8]>>(7-i%8)&1), 1)
		}
	}
	copyBits(0, g.cropAt)
	cx, cy := (g.w-w)/2, (g.h-h)/2 // in units of two samples
	// ffmpeg keeps a left crop only on a 32-unit (64-sample) boundary and widens the picture
	// otherwise: the rest goes to the right, the picture moves at most 32 samples.
	left := cx / 2 / 32 * 32
	bw.put(1, 1)
	bw.ue(uint64(left))
	bw.ue(uint64(cx - left))
	bw.ue(uint64(cy / 2))
	bw.ue(uint64(cy - cy/2))
	copyBits(g.cropEnd, last+1)
	hdr := 1
	if codec == "hevc" {
		hdr = 2
	}
	return append(append([]byte(nil), nal[:hdr]...), ebsp(bw.b[hdr:])...), true
}
