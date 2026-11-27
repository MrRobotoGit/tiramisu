package notice

import (
	"errors"
	"math"
	"sort"
	"sync"
)

// maxDurationNs bounds the Info Duration we trust (one week).
const maxDurationNs = 7 * 24 * 3600 * 1e9

// durationUnits is the film's Duration if it is finite, positive and plausible.
func durationUnits(f *Film) (uint64, bool) {
	d := f.Duration
	if math.IsNaN(d) || math.IsInf(d, 0) || d <= 0 || d*float64(f.TimestampScale) > maxDurationNs {
		return 0, false
	}
	return uint64(d), true
}

// tailBound is E: the first tail element (Cues, Tags, SeekHead…) at or after splEnd, else
// fileSize. A tail element inside [v, splEnd) means the splice overwrote it: refused.
func tailBound(f *Film, v, splEnd, fileSize int64) (int64, error) {
	e := fileSize
	tails := append(append([]int64(nil), f.TailElems...), f.SeekHeads...)
	if f.CuesPos > 0 {
		tails = append(tails, f.CuesPos)
	}
	for _, t := range tails {
		if t >= v && t < splEnd {
			return 0, ErrStructure
		}
		if t >= splEnd && t < e {
			e = t
		}
	}
	if splEnd > e {
		return 0, ErrStructure
	}
	return e, nil
}

// timeline splits [splEnd, E) into one synthetic interval per Cue gap (or per ten seconds of
// bytes without Cues). Cue times must increase with position.
func timeline(f *Film, cues []Cue, v, splEnd int64, nextTime uint64, fileSize, bitrate int64) ([]interval, int64, error) {
	if !f.HasSeekHead || f.TimestampScale == 0 {
		return nil, 0, ErrStructure
	}
	e, err := tailBound(f, v, splEnd, fileSize)
	if err != nil {
		return nil, 0, err
	}
	second := uint64(1_000_000_000 / f.TimestampScale)
	var ivs []interval
	add := func(start, end int64, t, d uint64) {
		if end > start {
			ivs = append(ivs, interval{start, end, t, max(d, 1)})
		}
	}
	if len(cues) > 0 {
		var cs []Cue
		for _, c := range cues {
			if c.Pos >= splEnd && c.Pos < e {
				if len(cs) > 0 && (c.Pos <= cs[len(cs)-1].Pos || c.Time <= cs[len(cs)-1].Time) {
					return nil, 0, ErrStructure
				}
				cs = append(cs, c)
			}
		}
		if len(cs) > maxIntervalsNo {
			return nil, 0, ErrUnsupported
		}
		if len(cs) == 0 {
			add(splEnd, e, nextTime, second)
			return ivs, e, nil
		}
		if cs[0].Time > nextTime {
			add(splEnd, cs[0].Pos, nextTime, cs[0].Time-nextTime)
		} else {
			add(splEnd, cs[0].Pos, nextTime, 1)
		}
		for i, c := range cs {
			if i+1 < len(cs) {
				add(c.Pos, cs[i+1].Pos, c.Time, cs[i+1].Time-c.Time)
				continue
			}
			d := second
			if end, ok := durationUnits(f); ok && end > c.Time {
				d = end - c.Time
			}
			add(c.Pos, e, c.Time, d)
		}
		return ivs, e, nil
	}
	if bitrate <= 0 {
		return nil, 0, ErrStructure
	}
	// One interval per GOP of the clip (10 s of the film's bytes): each starts on an IDR.
	const span = gopNs / 1_000_000_000
	step := bitrate * span
	n := max((e-splEnd)/step, 1)
	if n > maxIntervalsNo {
		return nil, 0, ErrUnsupported
	}
	for k := int64(0); k < n; k++ {
		end := splEnd + (k+1)*step
		if k == n-1 {
			end = e
		}
		// The last interval takes the rest: its duration follows its bytes.
		d := uint64(float64(end-splEnd-k*step) / float64(bitrate) * float64(second))
		add(splEnd+k*step, end, nextTime+uint64(k)*span*second, max(d, second))
	}
	return ivs, e, nil
}

// lruKeep is how many built Clusters and interval plans a Layout keeps (handles reading
// different places).
const lruKeep = 3

type built struct {
	i, m int
	ps   []piece
}

// Layout serves the fake bytes of [V, E): the splice, then one synthetic interval at a time.
type Layout struct {
	V, E   int64
	splice []piece
	spEnd  int64
	ivs    []interval
	clip   *Clip
	pl     placement

	mu    sync.Mutex
	lru   []built   // last = most recent
	plans []*ivPlan // last = most recent
}

// NewLayout plans [V,E) for a film whose Cluster at clusterStart was served up to
// clusterStart+len(prefix). An empty prefix means V is on a Cue. Every interval's size is
// checked here so a range too small for its content refuses the whole layout before any byte
// is served. bitrate is in bytes per second.
func NewLayout(f *Film, cues []Cue, prefix []byte, clusterStart, fileSize, bitrate int64, x Format, c *Clip) (*Layout, error) {
	return newLayout(f, cues, prefix, clusterStart, nil, fileSize, bitrate, x, c)
}

// NewLayoutAt plans [at, E) for a film whose Cluster at `at` (a boundary no byte past which was
// served) starts at time t0: no Cue needs to name it.
func NewLayoutAt(f *Film, cues []Cue, at int64, t0 uint64, fileSize, bitrate int64, x Format, c *Clip) (*Layout, error) {
	return newLayout(f, cues, nil, at, &t0, fileSize, bitrate, x, c)
}

func newLayout(f *Film, cues []Cue, prefix []byte, clusterStart int64, t0 *uint64, fileSize, bitrate int64, x Format, c *Clip) (*Layout, error) {
	if err := validClip(c); err != nil {
		return nil, err
	}
	if f.Video() == nil || f.TimestampScale == 0 {
		return nil, ErrStructure
	}
	pl := placement{scale: f.TimestampScale, video: f.Video().Number}
	for i, a := range f.Audio() {
		if i < len(x.Audio) && x.Audio[i].Codec != "" {
			pl.audio = append(pl.audio, a.Number)
		} else {
			pl.audio = append(pl.audio, 0)
		}
	}
	l := &Layout{V: clusterStart + int64(len(prefix)), clip: c, pl: pl}
	var next uint64
	if len(prefix) == 0 {
		t, ok := cueAt(cues, clusterStart)
		if t0 != nil {
			t, ok = *t0, true
		}
		if !ok {
			return nil, ErrStructure // V on an unknown Cluster boundary: its time is not known
		}
		l.spEnd, next = clusterStart, t
	} else {
		frame := uint64(max(units(float64(c.FrameDur), f.TimestampScale), 1))
		s, err := PlanSplice(prefix, clusterStart, pl.video, frame)
		if err != nil {
			return nil, err
		}
		if s.End < l.V || piecesLen(s.pieces) != s.End-l.V {
			return nil, ErrStructure
		}
		next = s.NextTime
		if !s.HasTime { // the Cluster's Timestamp was not served: take it from its Cue
			t, ok := cueAt(cues, s.ClusterStart)
			if !ok {
				return nil, ErrStructure
			}
			next += t
		}
		l.splice, l.spEnd = s.pieces, s.End
	}
	ivs, e, err := timeline(f, cues, l.V, l.spEnd, next, fileSize, bitrate)
	if err != nil {
		return nil, err
	}
	l.ivs, l.E = ivs, e
	for _, iv := range ivs { // a gap too short for the notice gets empty Clusters or a Void (newIvPlan)
		if _, err := intervalSize(c, pl, iv); err != nil {
			return nil, err
		}
		if iv.End-iv.Start < 2 {
			return nil, ErrTooSmall
		}
	}
	return l, nil
}

// cueAt is the time of the Cue on the Cluster at pos.
func cueAt(cues []Cue, pos int64) (uint64, bool) {
	i := sort.Search(len(cues), func(i int) bool { return cues[i].Pos >= pos })
	if i == len(cues) || cues[i].Pos != pos {
		return 0, false
	}
	return cues[i].Time, true
}

var errRange = errors.New("notice: read outside [V, E)")

// ReadAt fills p with the fake bytes at off; the whole read must lie inside [V, E).
func (l *Layout) ReadAt(p []byte, off int64) (int, error) {
	if off < l.V || off+int64(len(p)) > l.E {
		return 0, errRange
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for n < len(p) {
		at := off + int64(n)
		ps, start, err := l.segment(at)
		if err != nil {
			return n, err
		}
		k := copyPieces(p[n:], ps, at-start)
		if k == 0 {
			return n, ErrStructure // pieces shorter than their range: never spin
		}
		n += k
	}
	return n, nil
}

// segment returns the pieces covering at and the absolute offset where they start.
func (l *Layout) segment(at int64) ([]piece, int64, error) {
	if at < l.spEnd {
		return l.splice, l.V, nil
	}
	i := sort.Search(len(l.ivs), func(i int) bool { return l.ivs[i].End > at })
	if i == len(l.ivs) {
		return nil, 0, errRange
	}
	p, err := l.plan(i)
	if err != nil {
		return nil, 0, err
	}
	m := p.at(at - p.iv.Start)
	start := p.iv.Start + p.offs[m]
	for k, b := range l.lru {
		if b.i == i && b.m == m {
			l.lru = append(append(l.lru[:k:k], l.lru[k+1:]...), b)
			return b.ps, start, nil
		}
	}
	ps, err := p.cluster(m)
	if err != nil {
		return nil, 0, err
	}
	if len(l.lru) == lruKeep {
		l.lru = l.lru[1:]
	}
	l.lru = append(l.lru, built{i, m, ps})
	return ps, start, nil
}

// plan returns interval i's Cluster plan, from a small LRU.
func (l *Layout) plan(i int) (*ivPlan, error) {
	for k, p := range l.plans {
		if p.iv == l.ivs[i] {
			l.plans = append(append(l.plans[:k:k], l.plans[k+1:]...), p)
			return p, nil
		}
	}
	p, err := newIvPlan(l.clip, l.pl, l.ivs[i])
	if err != nil {
		return nil, err
	}
	if len(l.plans) == lruKeep {
		l.plans = l.plans[1:]
	}
	l.plans = append(l.plans, p)
	return p, nil
}

// copyPieces copies from the run of pieces, starting rel bytes in; returns bytes copied.
func copyPieces(dst []byte, ps []piece, rel int64) int {
	n := 0
	for _, pc := range ps {
		if d := int64(len(pc.data)); rel < d {
			n += copy(dst[n:], pc.data[rel:])
			rel = 0
		} else {
			rel -= d
		}
		if n == len(dst) {
			return n
		}
		if rel < pc.zeros {
			k := int(min(pc.zeros-rel, int64(len(dst)-n)))
			clear(dst[n : n+k])
			n += k
			rel = 0
		} else {
			rel -= pc.zeros
		}
		if n == len(dst) {
			return n
		}
	}
	return n
}

// ClusterContaining walks the Cluster headers from the Cluster at `from` to the one holding off,
// reading 12 bytes per Cluster through read (local bytes only). ok is false at a gap, an
// unknown-size Cluster or anything that is not a Cluster.
func ClusterContaining(read func(off int64, p []byte) int, from, off int64) (int64, bool) {
	var h [12]byte
	for range maxWalk {
		if read(from, h[:]) < len(h) {
			return 0, false
		}
		id, size, n, ok := readHeader(h[:])
		if !ok || id != idCluster || size == unknownSize {
			return 0, false
		}
		end := from + int64(n) + int64(size)
		if off < end {
			return from, true
		}
		from = end
	}
	return 0, false
}

// maxWalk bounds ClusterContaining (a 100 GB film in 1 MB Clusters).
const maxWalk = 100_000

// ClusterTime reads the Timestamp of the Cluster at `at` from its first local bytes.
func ClusterTime(read func(off int64, p []byte) int, at int64) (uint64, bool) {
	var b [64]byte
	n := read(at, b[:])
	id, size, h, ok := readHeader(b[:n])
	if !ok || id != idCluster {
		return 0, false
	}
	body := b[h:n]
	if size != unknownSize && uint64(len(body)) > size {
		body = body[:size]
	}
	for len(body) > 0 {
		cid, csize, ch, ok := readHeader(body)
		if !ok || csize == unknownSize || uint64(len(body)-ch) < csize {
			return 0, false
		}
		if cid == idTimestamp {
			return beUint(body[ch : ch+int(csize)]), true
		}
		body = body[ch+int(csize):]
	}
	return 0, false
}
