package notice

import (
	"encoding/binary"
	"errors"
	"slices"
	"sort"
)

// MP4 films keep their moov: the player has the sample tables, so the notice only replaces the
// bytes of each sample in [V, E) with content of exactly the same size. Video samples get the
// clip's frames padded with a filler NAL (a GOP restarts on the first sample an IDR fits in);
// audio samples get a silent frame followed by zeros. E is the end of the mdat holding V. The
// IDR goes on every sync sample of the film, where a seek lands.

var errMP4 = errors.New("notice: unsupported or truncated MP4 structure")

const (
	maxMoov       = 64 << 20
	maxMP4Samples = 4 << 20
	maxTopBoxes   = 4096
)

// MP4Track is one trak of the moov, with its samples' file positions.
type MP4Track struct {
	Kind          string // "vide", "soun", other handlers
	Codec         string // "h264", "hevc"; audio: "aac", "ac3", "eac3"; "" = other
	Config        []byte // avcC / hvcC record
	Width, Height int
	Colour        VUI // colr nclx
	DVProfile     int
	Rate          int
	Channels      int
	ASC           []byte
	FrameDur      int64 // ns, median sample duration
	Offs          []int64
	Sizes         []uint32
	Sync          []uint32 // stss: 1-based sync sample numbers, sorted; nil = every sample
}

func (t *MP4Track) isSync(i int) bool {
	if t.Sync == nil {
		return true
	}
	k := sort.Search(len(t.Sync), func(k int) bool { return t.Sync[k] >= uint32(i+1) })
	return k < len(t.Sync) && t.Sync[k] == uint32(i+1)
}

// Movie is what the notice needs of an MP4.
type Movie struct {
	Duration float64 // seconds
	Tracks   []MP4Track
	Mdats    [][2]int64 // data extents of the top-level mdat boxes
}

func (mv *Movie) Video() *MP4Track {
	for i := range mv.Tracks {
		if mv.Tracks[i].Kind == "vide" && mv.Tracks[i].Codec != "" {
			return &mv.Tracks[i]
		}
	}
	return nil
}

type box struct {
	typ        string
	start, end int64 // the box, header included
	hdr        int64
}

// readBox decodes the box header at b[0:]; size 0 runs to limit.
func readBox(b []byte, at, limit int64) (box, bool) {
	if len(b) < 8 {
		return box{}, false
	}
	size, hdr := int64(binary.BigEndian.Uint32(b)), int64(8)
	switch size {
	case 1:
		if len(b) < 16 {
			return box{}, false
		}
		size, hdr = int64(binary.BigEndian.Uint64(b[8:16])), 16
	case 0:
		size = limit - at
	}
	if size < hdr || at+size > limit || size < 0 {
		return box{}, false
	}
	return box{typ: string(b[4:8]), start: at, end: at + size, hdr: hdr}, true
}

// MP4Top walks the top-level boxes through read (local bytes only): the moov body's extent and
// the mdats' data extents.
func MP4Top(read func(off int64, p []byte) int, size int64) (moov [2]int64, mdats [][2]int64, err error) {
	var h [16]byte
	for at, n := int64(0), 0; at < size && n < maxTopBoxes; n++ {
		k := read(at, h[:])
		b, ok := readBox(h[:k], at, size)
		if !ok {
			return moov, nil, errMP4
		}
		switch b.typ {
		case "moov":
			moov = [2]int64{b.start + b.hdr, b.end}
		case "mdat":
			mdats = append(mdats, [2]int64{b.start + b.hdr, b.end})
		case "moof":
			return moov, nil, ErrUnsupported // fragmented
		}
		at = b.end
	}
	if moov[1] == 0 || len(mdats) == 0 {
		return moov, nil, errMP4
	}
	if moov[1]-moov[0] > maxMoov {
		return moov, nil, ErrUnsupported
	}
	return moov, mdats, nil
}

// children calls fn for each child box of body (positions relative to body).
func mp4Children(body []byte, fn func(b box, data []byte) bool) {
	for at := int64(0); at < int64(len(body)); {
		b, ok := readBox(body[at:], at, int64(len(body)))
		if !ok {
			return
		}
		if !fn(b, body[b.start+b.hdr:b.end]) {
			return
		}
		at = b.end
	}
}

func mp4Child(body []byte, path ...string) []byte {
	for _, typ := range path {
		var found []byte
		mp4Children(body, func(b box, data []byte) bool {
			if b.typ == typ {
				found = data
				return false
			}
			return true
		})
		if found == nil {
			return nil
		}
		body = found
	}
	return body
}

func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

// ParseMoov reads the moov body (the box without its header) and the mdat extents.
func ParseMoov(body []byte, mdats [][2]int64) (*Movie, error) {
	mv := &Movie{Mdats: mdats}
	if h := mp4Child(body, "mvhd"); len(h) >= 20 {
		switch h[0] {
		case 0:
			if ts := be32(h[12:]); ts > 0 {
				mv.Duration = float64(be32(h[16:])) / float64(ts)
			}
		case 1:
			if len(h) >= 32 && be32(h[20:]) > 0 {
				mv.Duration = float64(binary.BigEndian.Uint64(h[24:])) / float64(be32(h[20:]))
			}
		}
	}
	total := 0
	var bad error
	mp4Children(body, func(b box, data []byte) bool {
		if b.typ != "trak" {
			return true
		}
		t, err := parseTrak(data)
		if err != nil {
			bad = err
			return false
		}
		if total += len(t.Sizes); total > maxMP4Samples {
			bad = ErrUnsupported
			return false
		}
		mv.Tracks = append(mv.Tracks, t)
		return true
	})
	if bad != nil {
		return nil, bad
	}
	if mv.Video() == nil {
		return nil, ErrUnsupported
	}
	return mv, nil
}

func parseTrak(trak []byte) (MP4Track, error) {
	var t MP4Track
	mdia := mp4Child(trak, "mdia")
	if hd := mp4Child(mdia, "hdlr"); len(hd) >= 12 {
		t.Kind = string(hd[8:12])
	}
	var scale uint32
	if md := mp4Child(mdia, "mdhd"); len(md) >= 16 {
		if md[0] == 1 && len(md) >= 24 {
			scale = be32(md[20:])
		} else {
			scale = be32(md[12:])
		}
	}
	stbl := mp4Child(mdia, "minf", "stbl")
	if stbl == nil {
		return t, nil
	}
	if sd := mp4Child(stbl, "stsd"); len(sd) >= 16 {
		t.parseEntry(sd[8:])
	}
	if t.Kind != "vide" && t.Kind != "soun" {
		return t, nil
	}
	if err := t.parseSamples(stbl); err != nil {
		return t, err
	}
	if ss := mp4Child(stbl, "stss"); len(ss) >= 8 {
		n := min(int(be32(ss[4:])), (len(ss)-8)/4)
		t.Sync = make([]uint32, n)
		for i := range t.Sync {
			t.Sync[i] = be32(ss[8+4*i:])
		}
		slices.Sort(t.Sync)
	}
	if st := mp4Child(stbl, "stts"); len(st) >= 8 && scale > 0 {
		var ds []uint32
		n := int(be32(st[4:]))
		for i := 0; i < n && 8+i*8+8 <= len(st) && len(ds) < 64; i++ {
			ds = append(ds, be32(st[8+i*8+4:]))
		}
		if len(ds) > 0 {
			slices.Sort(ds)
			t.FrameDur = int64(float64(ds[len(ds)/2]) / float64(scale) * 1e9)
		}
	}
	return t, nil
}

// parseEntry reads the first sample entry: codec, its configuration and colour.
func (t *MP4Track) parseEntry(e []byte) {
	b, ok := readBox(e, 0, int64(len(e)))
	if !ok {
		return
	}
	body := e[b.hdr:b.end]
	var kids []byte
	switch b.typ {
	case "avc1", "avc3", "hvc1", "hev1", "dvh1", "dvhe", "dva1", "dvav":
		if len(body) < 78 {
			return
		}
		t.Width, t.Height = int(binary.BigEndian.Uint16(body[24:])), int(binary.BigEndian.Uint16(body[26:]))
		kids = body[78:]
		mp4Children(kids, func(c box, data []byte) bool {
			switch c.typ {
			case "avcC":
				t.Codec, t.Config = "h264", data
			case "hvcC":
				t.Codec, t.Config = "hevc", data
			case "colr":
				if len(data) >= 11 && string(data[:4]) == "nclx" {
					t.Colour = VUI{Present: true, Primaries: int(binary.BigEndian.Uint16(data[4:])),
						TRC: int(binary.BigEndian.Uint16(data[6:])), Matrix: int(binary.BigEndian.Uint16(data[8:])),
						FullRange: data[10]&0x80 != 0}
				}
			case "dvcC", "dvvC", "dvwC":
				if len(data) >= 3 {
					t.DVProfile = int(data[2] >> 1)
				}
			}
			return true
		})
	case "mp4a", "ac-3", "ec-3":
		if len(body) < 28 {
			return
		}
		skip := 28
		switch binary.BigEndian.Uint16(body[8:]) { // QuickTime sound entry versions
		case 1:
			skip += 16
		case 2:
			skip += 36
		}
		if len(body) < skip {
			return
		}
		t.Channels, t.Rate = int(binary.BigEndian.Uint16(body[16:])), int(be32(body[24:])>>16)
		t.Codec = map[string]string{"mp4a": "aac", "ac-3": "ac3", "ec-3": "eac3"}[b.typ]
		if b.typ == "mp4a" {
			t.ASC = esdsASC(mp4Child(body[skip:], "esds"))
			if t.ASC == nil {
				t.Codec = ""
			}
		}
	}
}

// esdsASC is the DecoderSpecificInfo of an esds box (AAC only: objectTypeIndication 0x40).
func esdsASC(b []byte) []byte {
	if len(b) < 4 {
		return nil
	}
	b = b[4:]
	desc := func(b []byte) (tag byte, body, rest []byte, ok bool) {
		if len(b) < 2 {
			return 0, nil, nil, false
		}
		tag, n, i := b[0], 0, 1
		for ; i < len(b) && i <= 4; i++ {
			n = n<<7 | int(b[i]&0x7F)
			if b[i]&0x80 == 0 {
				i++
				break
			}
		}
		if i+n > len(b) {
			return 0, nil, nil, false
		}
		return tag, b[i : i+n], b[i+n:], true
	}
	tag, es, _, ok := desc(b)
	if !ok || tag != 3 || len(es) < 3 {
		return nil
	}
	flags, p := es[2], 3
	if flags&0x80 != 0 {
		p += 2
	}
	if flags&0x40 != 0 && p < len(es) {
		p += 1 + int(es[p])
	}
	if flags&0x20 != 0 {
		p += 2
	}
	if p > len(es) {
		return nil
	}
	tag, dc, _, ok := desc(es[p:])
	if !ok || tag != 4 || len(dc) < 13 || dc[0] != 0x40 {
		return nil
	}
	tag, asc, _, ok := desc(dc[13:])
	if !ok || tag != 5 {
		return nil
	}
	return asc
}

// parseSamples computes every sample's file offset from stsz, stsc and stco/co64.
func (t *MP4Track) parseSamples(stbl []byte) error {
	sz, sc := mp4Child(stbl, "stsz"), mp4Child(stbl, "stsc")
	co, wide := mp4Child(stbl, "stco"), false
	if co == nil {
		co, wide = mp4Child(stbl, "co64"), true
	}
	if len(sz) < 12 || len(sc) < 8 || len(co) < 8 {
		return errMP4
	}
	fixed, count := be32(sz[4:]), int(be32(sz[8:]))
	if count > maxMP4Samples || fixed == 0 && 12+4*count > len(sz) {
		return errMP4
	}
	chunks := int(be32(co[4:]))
	w := 4
	if wide {
		w = 8
	}
	if 8+w*chunks > len(co) {
		return errMP4
	}
	chunkOff := func(i int) int64 {
		if wide {
			return int64(binary.BigEndian.Uint64(co[8+8*i:]))
		}
		return int64(be32(co[8+4*i:]))
	}
	entries := int(be32(sc[4:]))
	if 8+12*entries > len(sc) {
		return errMP4
	}
	t.Offs, t.Sizes = make([]int64, 0, count), make([]uint32, 0, count)
	s := 0
	for e := 0; e < entries && s < count; e++ {
		first, per := int(be32(sc[8+12*e:]))-1, int(be32(sc[8+12*e+4:]))
		last := chunks
		if e+1 < entries {
			last = int(be32(sc[8+12*(e+1):])) - 1
		}
		if first < 0 || last > chunks || per <= 0 {
			return errMP4
		}
		for c := first; c < last && s < count; c++ {
			off := chunkOff(c)
			for k := 0; k < per && s < count; k++ {
				size := fixed
				if size == 0 {
					size = be32(sz[12+4*s:])
				}
				t.Offs, t.Sizes = append(t.Offs, off), append(t.Sizes, size)
				off += int64(size)
				s++
			}
		}
	}
	if s != count {
		return errMP4
	}
	return nil
}

// FormatOfMP4 is FormatOf for an MP4's video and audio tracks.
func FormatOfMP4(mv *Movie) (Format, error) {
	v := mv.Video()
	x := Format{Codec: v.Codec, Width: v.Width, Height: v.Height, BitDepth: 8, NALLength: 4}
	switch {
	case v.Codec == "h264" && len(v.Config) > 4:
		x.NALLength = int(v.Config[4]&3) + 1
	case v.Codec == "hevc" && len(v.Config) > 21:
		x.NALLength = int(v.Config[21]&3) + 1
		if v.Config[17]&7 != 0 {
			x.BitDepth = 10
		}
	default:
		return x, ErrUnsupported
	}
	vui := x.vui(&Film{}, v.Config, nil)
	if !vui.Present {
		vui = v.Colour
	}
	switch vui.TRC {
	case 16:
		x.Transfer = TransferPQ
	case 18:
		x.Transfer = TransferHLG
	}
	x.FullRange, x.DVProfile = vui.FullRange, v.DVProfile
	if x.DVProfile == 5 {
		x.Transfer, x.FullRange = TransferDV5, true
	}
	if x.Codec == "h264" || x.BitDepth == 8 {
		x.Transfer = TransferSDR
	}
	if x.FrameDur = v.FrameDur; x.FrameDur < minFrameDurNs || x.FrameDur > maxFrameDurNs {
		x.FrameDur = defaultFrameDur
	}
	for _, t := range mv.Tracks {
		if t.Kind == "soun" {
			x.Audio = append(x.Audio, AudioFormat{Codec: t.Codec, Rate: t.Rate, Channels: t.Channels, ASC: t.ASC})
		}
	}
	return x, nil
}

type mp4Sample struct {
	off  int64
	size uint32
	kind uint8 // sampleZero, sampleVideo, sampleAudio
	sync bool  // a sync sample of the film: where a seek lands
	role int32 // video: clip frame, -1 filler only; audio: clip audio index
}

const (
	sampleZero = iota
	sampleVideo
	sampleAudio
)

// MP4Layout serves the notice bytes of [V, E) of an MP4.
type MP4Layout struct {
	V, E    int64
	samples []mp4Sample // sorted by offset
	clip    *Clip
	nalLen  int
	hevc    bool
}

// NewMP4Layout plans [V, E) for the clip c of format x.
func NewMP4Layout(mv *Movie, v int64, x Format, c *Clip) (*MP4Layout, error) {
	if err := validClip(c); err != nil {
		return nil, err
	}
	l := &MP4Layout{V: v, clip: c, nalLen: x.NALLength, hevc: x.Codec == "hevc"}
	for _, m := range mv.Mdats {
		if v >= m[0] && v < m[1] {
			l.E = m[1]
		}
	}
	if l.E == 0 {
		return nil, errMP4
	}
	audio := 0
	for _, t := range mv.Tracks {
		kind, role := uint8(sampleZero), int32(-1)
		switch {
		case t.Kind == "vide" && t.Codec == x.Codec && t.Config != nil:
			kind = sampleVideo
		case t.Kind == "soun":
			if audio < len(c.Audio) && c.Audio[audio].Frame != nil {
				kind, role = sampleAudio, int32(audio)
			}
			audio++
		}
		for i, off := range t.Offs {
			end := off + int64(t.Sizes[i])
			if end <= v || off >= l.E {
				continue
			}
			k := kind
			if off < v { // the sample V cuts: its rest is zeros
				k = sampleZero
			}
			l.samples = append(l.samples, mp4Sample{off: off, size: t.Sizes[i], kind: k, role: role, sync: k == sampleVideo && t.isSync(i)})
		}
	}
	sort.Slice(l.samples, func(i, j int) bool { return l.samples[i].off < l.samples[j].off })
	gop := -1 // next clip frame; -1 waits for a sample the IDR fits in
	for i := range l.samples {
		s := &l.samples[i]
		if s.kind != sampleVideo {
			continue
		}
		s.role = -1
		// A seek lands on the film's sync samples: each one restarts the GOP on an IDR.
		if s.sync || gop < 0 || gop >= len(c.Video) {
			gop = -1
			if l.fits(len(c.Video[0]), s.size) {
				s.role, gop = 0, 1
			}
			continue
		}
		if l.fits(len(c.Video[gop]), s.size) {
			s.role = int32(gop)
			gop++
		}
	}
	return l, nil
}

func (l *MP4Layout) fillerMin() int {
	if l.hevc {
		return l.nalLen + 3
	}
	return l.nalLen + 2
}

func (l *MP4Layout) fits(n int, size uint32) bool {
	return int(size) == n || int(size) >= n+l.fillerMin()
}

// ReadAt fills p with the bytes at off; the whole read must lie inside [V, E).
func (l *MP4Layout) ReadAt(p []byte, off int64) (int, error) {
	if off < l.V || off+int64(len(p)) > l.E {
		return 0, errRange
	}
	clear(p)
	i := sort.Search(len(l.samples), func(i int) bool { return l.samples[i].off+int64(l.samples[i].size) > off })
	for ; i < len(l.samples); i++ {
		s := l.samples[i]
		if s.off >= off+int64(len(p)) {
			break
		}
		lo, hi := max(s.off, off), min(s.off+int64(s.size), off+int64(len(p)))
		l.sample(p[lo-off:hi-off], s, lo-s.off)
	}
	return len(p), nil
}

// sample writes the bytes [rel, rel+len(dst)) of sample s (dst is zeroed).
func (l *MP4Layout) sample(dst []byte, s mp4Sample, rel int64) {
	var head []byte
	switch {
	case s.kind == sampleAudio:
		head = l.clip.Audio[s.role].Frame
	case s.kind == sampleVideo && s.role >= 0:
		head = l.clip.Video[s.role]
	case s.kind == sampleVideo && int(s.size) < l.fillerMin():
		return
	case s.kind != sampleVideo:
		return
	}
	if rel < int64(len(head)) {
		copy(dst, head[rel:])
	}
	if s.kind != sampleVideo || int(s.size) == len(head) {
		return // audio: zeros after the frame
	}
	// A filler NAL covers [len(head), size).
	start, total := int64(len(head)), int64(s.size)-int64(len(head))
	hdr := []byte{0x0C}
	if l.hevc {
		hdr = []byte{0x4C, 0x01}
	}
	for k := range dst {
		j := rel + int64(k) - start
		if j < 0 {
			continue
		}
		switch n := int64(l.nalLen); {
		case j < n:
			dst[k] = byte((total - n) >> (8 * (n - 1 - j)))
		case j < n+int64(len(hdr)):
			dst[k] = hdr[j-n]
		case j == total-1:
			dst[k] = 0x80
		default:
			dst[k] = 0xFF
		}
	}
}
