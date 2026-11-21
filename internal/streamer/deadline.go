package streamer

import (
	"sync"
	"time"
)

const (
	deadlineEvery = 2 * time.Second
	deadlineStale = 15 * time.Second
	rateAlpha     = 0.3
)

// deadlineState measures how fast a stream is drained and when its schedule was pushed
// (storage.md §4.4). The rate is the consumer's, since Read advances only as it drains.
type deadlineState struct {
	mu        sync.Mutex
	rate      float64 // bytes/s, EWMA
	sampleAt  time.Time
	sampled   int64
	fresh     int64 // bytes read since the last push
	pushed    time.Time
	installed bool
	set       pieceRange // pieces this stream gave a deadline: the only ones it clears
}

// pieceRange is [first, first+n).
type pieceRange struct{ first, n int }

func (r pieceRange) has(i int) bool { return i >= r.first && i < r.first+r.n }

func (d *deadlineState) sample(n int64, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fresh += n
	if d.sampleAt.IsZero() {
		d.sampleAt = now // the first read only starts the clock
		return
	}
	d.sampled += n
	if el := now.Sub(d.sampleAt); el >= time.Second {
		inst := float64(d.sampled) / el.Seconds()
		if d.rate == 0 {
			d.rate = inst
		} else {
			d.rate = rateAlpha*inst + (1-rateAlpha)*d.rate
		}
		d.sampleAt, d.sampled = now, 0
	}
}

// clear forgets the schedule and returns the pieces it had installed.
func (d *deadlineState) clear() pieceRange {
	d.mu.Lock()
	defer d.mu.Unlock()
	set := d.set
	d.installed, d.fresh, d.rate, d.sampleAt, d.sampled, d.set = false, 0, 0, time.Time{}, 0, pieceRange{}
	return set
}

func (d *deadlineState) installedRange() pieceRange {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.set
}

// deadlines pushes the playout schedule every 2s while the stream is drained at ≥64KiB/s, and
// clears a schedule not refreshed for 15s (a paused player reads nothing). A stream clears
// only pieces it installed and no other stream holds now (storage.md §4.4).
func (s *Stream) deadlines(now time.Time, sh shares, numPieces int) {
	d := &s.dl
	d.mu.Lock()
	push := d.fresh > 0 && now.Sub(d.pushed) >= deadlineEvery && d.rate >= deadlineMinBps
	stale := !push && d.installed && now.Sub(d.pushed) >= deadlineStale
	rate, old := d.rate, d.set
	if push {
		d.pushed, d.fresh = now, 0
	}
	if stale {
		d.installed, d.set = false, pieceRange{}
	}
	d.mu.Unlock()
	ctl := s.t.ctl
	if stale {
		s.t.clearDeadlines(old, pieceRange{}, s)
		return
	}
	if !push {
		return
	}
	sp := s.span()
	if sp.pos >= sp.fileEnd {
		return
	}
	fw := s.fileWindow()
	pl := sh.pieceLen
	first := pieceOf(sp.pos, pl)
	n := min(deadlineMax, int(sh.readahead/pl)+2, fw.Last-first+1)
	left := int64(first+1)*pl - sp.pos
	due := now.Add(time.Duration(float64(left) / rate * float64(time.Second)))
	interval := time.Duration(float64(pl) / rate * float64(time.Second))
	set := pieceRange{first, n}
	d.mu.Lock()
	d.installed, d.set = true, set
	d.mu.Unlock()
	s.t.clearDeadlines(old, set, s)
	ctl.SetStreamDeadlines(first, n, due, interval)
}

// clearDeadlines clears the pieces of r outside keep and outside every schedule another stream
// than self holds now.
func (t *Torrent) clearDeadlines(r, keep pieceRange, self *Stream) {
	if r.n <= 0 {
		return
	}
	var others []pieceRange
	t.mu.Lock()
	for o := range t.streams {
		if o != self {
			if or := o.dl.installedRange(); or.n > 0 {
				others = append(others, or)
			}
		}
	}
	t.mu.Unlock()
	held := func(i int) bool {
		if keep.has(i) {
			return true
		}
		for _, o := range others {
			if o.has(i) {
				return true
			}
		}
		return false
	}
	for i := r.first; i < r.first+r.n; {
		if held(i) {
			i++
			continue
		}
		j := i + 1
		for j < r.first+r.n && !held(j) {
			j++
		}
		t.ctl.ClearStreamDeadlinesRange(i, j-i)
		i = j
	}
}
