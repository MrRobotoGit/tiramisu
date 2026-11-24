package engine

import (
	"sync"
	"sync/atomic"
	"time"
)

// helpWindow is [first, last] from R: piece x when (x_end−R)/c ≤ (x_start−pos)/b, at least one per
// helper (all when b or c is 0), within capBytes from first and lastPiece; last < first is empty.
func helpWindow(pos, readEnd, b, c, capBytes, pieceLen int64, lastPiece, helpers int) (first, last int) {
	if pieceLen <= 0 || helpers < 1 {
		return 0, -1
	}
	first = int((readEnd + pieceLen - 1) / pieceLen)
	ceiling := min(lastPiece, first+int(capBytes/pieceLen)-1) // the helper's cache, counted from R
	floor := first + helpers - 1
	switch {
	case b <= 0 || c <= 0:
		last = floor
	case c >= b:
		last = ceiling
	default:
		last = first - 1
		for x := first; x <= ceiling; x++ {
			deliver := float64(int64(x+1)*pieceLen-readEnd) / float64(c)
			need := float64(int64(x)*pieceLen-pos) / float64(b)
			if deliver > need {
				break
			}
			last = x
		}
		last = max(last, floor)
	}
	return first, min(last, ceiling)
}

// deliveryMeter measures the helpers' delivery C from the Haves of window pieces arriving on friend
// links, between one window and the next. have runs under the client lock: a leaf mutex only.
type deliveryMeter struct {
	mu          sync.Mutex
	first, last int
	at          time.Time    // the window was sent
	known       map[int]bool // window pieces announced so far
	pending     int          // pieces not announced when the window was sent
	got         int          // announced since
	lastAt      time.Time
	c           float64 // bytes/s; 0: unknown
	total       atomic.Int64
}

func (d *deliveryMeter) have(piece int, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.at.IsZero() || piece < d.first || piece > d.last || d.known[piece] {
		return
	}
	d.known[piece] = true
	d.got++
	d.lastAt = now
	d.total.Add(1)
}

// delivered reports, for the window in flight, the pieces the helpers announced and those asked.
func (d *deliveryMeter) delivered() (got, pending int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.got, d.pending
}

// nextLead is how many pieces past R the next window starts. A window the watcher overtook with
// nothing delivered doubles it, up to limit; one delivered in full halves it; a partial one keeps it.
func nextLead(lead int, overtaken bool, got, pending, limit int) int {
	switch {
	case overtaken && got == 0:
		lead = max(1, 2*lead)
	case pending > 0 && got >= pending:
		lead /= 2
	}
	return min(lead, max(limit, 0))
}

// estimate is C as the window in flight measures it now. A measurement in which nothing arrived
// goes on into the next window: an interval shorter than a piece's delivery proves nothing.
func (d *deliveryMeter) estimate(pieceLen int64, now time.Time) float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.measureLocked(pieceLen, now)
}

func (d *deliveryMeter) measureLocked(pieceLen int64, now time.Time) float64 {
	if !d.at.IsZero() && d.pending > 0 && d.got > 0 {
		end := now
		if d.got >= d.pending {
			end = d.lastAt // they finished early: the time after is not theirs
		}
		if el := end.Sub(d.at).Seconds(); el > 0 {
			d.c = float64(int64(d.got)*pieceLen) / el
		}
	}
	return d.c
}

// next closes the measurement of the window in flight, opens one over [first, last] and returns C.
// held reports a piece the watcher completed itself: never asked of the helpers.
func (d *deliveryMeter) next(first, last int, pieceLen int64, now time.Time, held func(int) bool) float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	keep := !d.at.IsZero() && d.pending > 0 && d.got == 0
	c := d.measureLocked(pieceLen, now)
	known := map[int]bool{}
	pending := 0
	for i := first; i <= last; i++ {
		switch {
		case d.known[i]:
			known[i] = true
		case held != nil && held(i):
		default:
			pending++
		}
	}
	d.first, d.last, d.known, d.pending = first, last, known, pending
	if !keep {
		d.at, d.got = now, 0
	}
	return c
}
