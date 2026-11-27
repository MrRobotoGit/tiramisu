package notice

import (
	"math"
	"sort"
)

type interval struct {
	Start, End int64  // absolute byte range
	Time, Dur  uint64 // Cluster timestamp and content duration, TimestampScale units
}

// placement binds a format's clip to one film: track numbers and TimestampScale.
type placement struct {
	scale uint64   // ns per timestamp unit
	video uint64   // video track number
	audio []uint64 // aligned with Clip.Audio; 0 = no blocks for that entry
}

const (
	maxRel         = 30000 // longest span of one synthetic Cluster, timestamp units (int16 relative times)
	maxSpanNs      = 30e9  // and in time, whatever the TimestampScale
	maxIntervalNs  = 10 * 60 * 1e9
	maxIvClusters  = 1024
	clusterOverhd  = 4 + 8 + 1 + 1 + 8 // Cluster ID + 8-byte size, Timestamp element
	minFrameDurNs  = 4_000_000
	maxFrameDurNs  = 1_000_000_000
	minAudioDurNs  = 1_000_000
	maxIntervalsNo = 200_000
)

type event struct {
	t     int64 // relative to the interval, timestamp units
	track uint64
	key   bool
	data  []byte
}

// validClip refuses clips whose frame durations would make an interval's event count unbounded.
func validClip(c *Clip) error {
	if c == nil || len(c.Video) == 0 || c.FrameDur < minFrameDurNs || c.FrameDur > maxFrameDurNs {
		return ErrUnsupported
	}
	for _, a := range c.Audio {
		if a.Frame != nil && !(a.FrameDur >= minAudioDurNs && a.FrameDur <= maxFrameDurNs) {
			return ErrUnsupported
		}
	}
	return nil
}

// checkDur refuses an interval longer than maxIntervalNs or needing too many Clusters.
func checkDur(pl placement, dur uint64) error {
	if pl.scale == 0 || dur > uint64(maxIntervalNs)/pl.scale {
		return ErrStructure
	}
	if numClusters(dur, pl.scale) > maxIvClusters {
		return ErrUnsupported
	}
	return nil
}

// countBelow is the number of k >= 0 with k*step < lim.
func countBelow(step, lim float64) int {
	if lim <= 0 || step <= 0 {
		return 0
	}
	n := int(math.Ceil(lim / step))
	for n > 0 && float64(n-1)*step >= lim {
		n--
	}
	for float64(n)*step < lim {
		n++
	}
	return n
}

func blockElemLen(track uint64, data int) int64 {
	blk := int64(sizeLen(track) + 3 + data)
	return int64(len(idBytes(idSimpleBlock))+sizeLen(uint64(blk))) + blk
}

// stream is one track's blocks in an interval: frame k at units(k*step), frames cycling.
type stream struct {
	step   float64 // ns
	n      int     // blocks in the interval
	track  uint64
	video  bool
	frames [][]byte
	prefix []int64 // prefix[i]: element bytes of frames[:i]
}

func (s *stream) t(k int, scale uint64) int64 { return units(float64(k)*s.step, scale) }

// bytesBefore is the element bytes of blocks [0, k).
func (s *stream) bytesBefore(k int) int64 {
	c := len(s.frames)
	return int64(k/c)*s.prefix[c] + s.prefix[k%c]
}

// countBefore is the number of blocks with time < t (times never decrease with k).
func (s *stream) countBefore(t int64, scale uint64) int {
	return sort.Search(s.n, func(k int) bool { return s.t(k, scale) >= t })
}

// streams lists the interval's tracks, video first then audio in Clip order (the tie order).
func streams(c *Clip, pl placement, dur uint64) []stream {
	durNs := float64(dur) * float64(pl.scale)
	mk := func(step float64, track uint64, video bool, frames [][]byte) stream {
		s := stream{step: step, n: countBelow(step, durNs), track: track, video: video, frames: frames}
		s.prefix = make([]int64, len(frames)+1)
		for i, fr := range frames {
			s.prefix[i+1] = s.prefix[i] + blockElemLen(track, len(fr))
		}
		return s
	}
	ss := []stream{mk(float64(c.FrameDur), pl.video, true, c.Video)}
	for a, ac := range c.Audio {
		if audioOn(pl, a, ac) {
			ss = append(ss, mk(ac.FrameDur, pl.audio[a], false, [][]byte{ac.Frame}))
		}
	}
	return ss
}

// intervalSize is the byte size of iv's Clusters without the filling Void, in closed form.
func intervalSize(c *Clip, pl placement, iv interval) (int64, error) {
	if err := checkDur(pl, iv.Dur); err != nil {
		return 0, err
	}
	size := numClusters(iv.Dur, pl.scale) * clusterOverhd
	for _, s := range streams(c, pl, iv.Dur) {
		size += s.bytesBefore(s.n)
	}
	return size, nil
}

// span is one synthetic Cluster's length in timestamp units.
func span(scale uint64) uint64 { return max(min(maxRel, uint64(maxSpanNs)/scale), 1) }

func numClusters(dur, scale uint64) int64 {
	sp := span(scale)
	return max(int64((dur+sp-1)/sp), 1)
}

func audioOn(pl placement, a int, ac AudioClip) bool {
	return ac.Frame != nil && a < len(pl.audio) && pl.audio[a] != 0 && ac.FrameDur > 0
}

// ivPlan places iv's synthetic Clusters: Cluster m spans [m*span, (m+1)*span) timestamp
// units (the last one takes the rest) and starts offs[m] bytes into the interval.
type ivPlan struct {
	iv       interval
	scale    uint64
	ss       []stream
	ncl      int
	bounds   [][]int // bounds[m][s]: first block of stream s in Cluster m; bounds[ncl] = n
	offs     []int64 // len ncl+1; offs[ncl] = iv.End-iv.Start (the last Cluster holds the Void)
	widen    int     // the last Timestamp's size vint grows by one byte when one byte is left over
	lastVoid int64
	void     bool // the whole interval is one Void
}

func newIvPlan(c *Clip, pl placement, iv interval) (*ivPlan, error) {
	size, err := intervalSize(c, pl, iv)
	if err != nil {
		return nil, err
	}
	total := iv.End - iv.Start
	p := &ivPlan{iv: iv, scale: pl.scale, ss: streams(c, pl, iv.Dur), ncl: int(numClusters(iv.Dur, pl.scale))}
	if size > total { // a Cue gap too short for the notice: Clusters without blocks, the image stays
		p.ss, size = nil, int64(p.ncl)*clusterOverhd
	}
	if size > total { // not even the Cluster headers: one Void, the demuxer resyncs at the next Cue
		if total < 2 {
			return nil, ErrTooSmall
		}
		return &ivPlan{iv: iv, scale: pl.scale, ncl: 1, offs: []int64{0, total}, void: true}, nil
	}
	for m := 0; m <= p.ncl; m++ {
		b := make([]int, len(p.ss))
		for i := range p.ss {
			switch {
			case m == p.ncl:
				b[i] = p.ss[i].n
			case m > 0:
				b[i] = p.ss[i].countBefore(int64(m)*int64(span(pl.scale)), pl.scale)
			}
		}
		p.bounds = append(p.bounds, b)
	}
	p.offs = make([]int64, p.ncl+1)
	for m := 0; m < p.ncl; m++ {
		p.offs[m+1] = p.offs[m] + p.content(m)
	}
	switch rest := total - p.offs[p.ncl]; rest {
	case 0:
	case 1:
		p.widen = 1
	default:
		p.lastVoid = rest
	}
	p.offs[p.ncl] = total
	return p, nil
}

// content is Cluster m's bytes without widening or Void.
func (p *ivPlan) content(m int) int64 {
	n := int64(clusterOverhd)
	for i := range p.ss {
		n += p.ss[i].bytesBefore(p.bounds[m+1][i]) - p.ss[i].bytesBefore(p.bounds[m][i])
	}
	return n
}

// cluster builds Cluster m into one slice of its exact size; the last one ends with a Void
// whose body is a zeros piece.
func (p *ivPlan) cluster(m int) ([]piece, error) {
	if p.void {
		h, z := voidPiece(p.offs[1])
		return []piece{{data: h, zeros: z}}, nil
	}
	last := m == p.ncl-1
	content := p.content(m)
	widen, void := 0, int64(0)
	var vh []byte
	var vz int64
	if last {
		widen, void = p.widen, p.lastVoid
		if void > 0 {
			vh, vz = voidPiece(void)
		}
	}
	want := content + int64(widen) + int64(len(vh))
	b := make([]byte, 0, want)
	b = appendElementHeader(b, idCluster, uint64(content-12+int64(widen)+void), 8)
	b = appendElementHeader(b, idTimestamp, 8, 1+widen)
	base := int64(m) * int64(span(p.scale))
	ts := p.iv.Time + uint64(base)
	for i := 7; i >= 0; i-- {
		b = append(b, byte(ts>>(8*i)))
	}
	for _, e := range p.events(m) {
		rel := e.t - base
		flags := byte(0)
		if e.key {
			flags = 0x80
		}
		b = appendElementHeader(b, idSimpleBlock, uint64(sizeLen(e.track)+3+len(e.data)), 0)
		b = append(appendSize(b, e.track, 0), byte(rel>>8), byte(rel), flags)
		b = append(b, e.data...)
	}
	b = append(b, vh...)
	if int64(len(b)) != want {
		return nil, ErrStructure // closed-form size and built bytes disagree: never serve them
	}
	return []piece{{data: b, zeros: vz}}, nil
}

// events of Cluster m ordered by time; ties keep stream order (video first).
func (p *ivPlan) events(m int) []event {
	var evs []event
	for i, s := range p.ss {
		for k := p.bounds[m][i]; k < p.bounds[m+1][i]; k++ {
			j := k % len(s.frames)
			evs = append(evs, event{t: s.t(k, p.scale), track: s.track, key: !s.video || j == 0, data: s.frames[j]})
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].t < evs[j].t })
	return evs
}

// at maps a byte offset inside the interval to its Cluster.
func (p *ivPlan) at(rel int64) int {
	return sort.Search(p.ncl, func(m int) bool { return p.offs[m+1] > rel })
}

// intervalPieces builds every Cluster of iv (tests and small intervals only).
func intervalPieces(c *Clip, pl placement, iv interval) ([]piece, error) {
	p, err := newIvPlan(c, pl, iv)
	if err != nil {
		return nil, err
	}
	var out []piece
	for m := 0; m < p.ncl; m++ {
		ps, err := p.cluster(m)
		if err != nil {
			return nil, err
		}
		out = append(out, ps...)
	}
	return out, nil
}

func units(ns float64, scale uint64) int64 { return int64(math.Round(ns / float64(scale))) }
