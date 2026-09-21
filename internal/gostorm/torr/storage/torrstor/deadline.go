package torrstor

import (
	"time"
)

// Deadline-first scheduling. A piece N bytes ahead of the read position is due in N/rate
// seconds, so the read position and its rate are enough to schedule the whole window ahead.
// Handing that schedule to the request order lets a late piece outrank a merely high-priority
// one, which binary piece priorities cannot express.
//
// The rate measured here is the rate this reader is drained at, which on the FUSE path is the
// pump's, not the player's. At steady state the two are the same by conservation; during a
// catch-up burst it tracks the swarm instead and the deadlines come out tighter than reality
// (the safe direction), and across an idle pump window the EWMA averages over the gap and
// understates. That is why the schedule is advisory: it reorders requests, it never gates them.
const (
	// deadlineRateInterval paces the consumption-rate sampling. Players read in bursts out of
	// their own buffer, so the window has to be wide enough to average a burst and its idle gap.
	deadlineRateInterval = 1 * time.Second
	deadlineRateAlpha    = 0.3
	// deadlineUpdateInterval paces pushing the schedule down to the torrent. Rewriting it on
	// every read would churn the request-order btree for no gain.
	deadlineUpdateInterval = 2 * time.Second
	// deadlineMaxPieces bounds how far ahead we schedule. Beyond the readahead window the
	// estimate is guesswork and the pieces are not requestable yet anyway.
	deadlineMaxPieces = 64
	// deadlineMinRate ignores implausibly slow rates (a paused player still dribbles reads):
	// below this we leave the torrent on pure priority ordering rather than invent deadlines.
	deadlineMinRate = 64 * 1024 // bytes/s
	// deadlineStaleAfter drops a schedule nothing has refreshed. A pause produces no reads at
	// all, so sampling stops and the last deadlines would otherwise sit there expiring in place
	// and ranking ahead of everything else.
	deadlineStaleAfter = 15 * time.Second
)

// sampleConsumption folds the bytes just handed to the player into the playout-rate EWMA.
// Caller must not hold r.mu.
func (r *Reader) sampleConsumption(n int) {
	if n <= 0 {
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.deadlineBytes += int64(n)
	if r.deadlineRateAt.IsZero() {
		r.deadlineRateAt = now
		r.mu.Unlock()
		return
	}
	dt := now.Sub(r.deadlineRateAt)
	if dt < deadlineRateInterval {
		r.mu.Unlock()
		return
	}
	inst := float64(r.deadlineBytes) / dt.Seconds()
	r.deadlineBytes = 0
	r.deadlineRateAt = now
	if r.deadlineRate == 0 {
		r.deadlineRate = inst
	} else {
		r.deadlineRate = deadlineRateAlpha*inst + (1-deadlineRateAlpha)*r.deadlineRate
	}
	rate := r.deadlineRate
	due := now.Sub(r.deadlineSetAt) >= deadlineUpdateInterval
	offset := r.offset
	readahead := r.readahead
	r.mu.Unlock()

	if !due {
		return
	}
	if r.pushDeadlines(offset, readahead, rate, now) {
		r.mu.Lock()
		r.deadlineSetAt = now
		r.mu.Unlock()
	}
}

// pushDeadlines converts the current playhead and rate into a per-piece schedule and installs it.
// Reports whether a schedule was installed.
func (r *Reader) pushDeadlines(offset, readahead int64, rate float64, now time.Time) bool {
	if rate < deadlineMinRate || r.cache == nil || r.cache.pieceLength <= 0 {
		return false
	}
	t := r.file.Torrent()
	if t == nil || t.Info() == nil {
		return false
	}
	// Absolute position inside the torrent, not inside the file.
	abs := offset + r.file.Offset()
	pieceLen := r.cache.pieceLength
	first, firstDue, interval := deadlineSchedule(abs, pieceLen, rate, now)
	if interval <= 0 {
		return false
	}

	n := deadlineMaxPieces
	if window := int(readahead/pieceLen) + 2; window < n {
		n = window
	}
	if n <= 0 {
		return false
	}
	t.SetStreamDeadlines(first, n, firstDue, interval)
	return true
}

// clearDeadlines drops this reader's schedule. Called on seek and on close: after a seek the old
// schedule describes pieces the player will no longer reach in that order. Only this file's
// piece range is cleared, so a sibling reader on another episode of the same pack keeps its own.
func (r *Reader) clearDeadlines() {
	r.mu.Lock()
	r.deadlineRate = 0
	r.deadlineBytes = 0
	r.deadlineRateAt = time.Time{}
	r.deadlineSetAt = time.Time{}
	r.mu.Unlock()
	t := r.file.Torrent()
	if t == nil || r.cache == nil || r.cache.pieceLength <= 0 {
		return
	}
	first, n := r.filePieceRange()
	t.ClearStreamDeadlinesRange(first, n)
}

// filePieceRange returns the piece span covered by this reader's file.
func (r *Reader) filePieceRange() (first, numPieces int) {
	pieceLen := r.cache.pieceLength
	begin := r.file.Offset()
	end := begin + r.file.Length()
	first = int(begin / pieceLen)
	last := int((end - 1) / pieceLen)
	return first, last - first + 1
}

// dropStaleDeadlines clears a schedule that has stopped being refreshed. Driven from the cache's
// periodic reader tick, because a paused player issues no reads and so cannot notice by itself.
func (r *Reader) dropStaleDeadlines() {
	r.mu.Lock()
	set := r.deadlineSetAt
	r.mu.Unlock()
	if set.IsZero() || time.Since(set) < deadlineStaleAfter {
		return
	}
	r.clearDeadlines()
}

// deadlineSchedule converts an absolute read position and a consumption rate into the schedule
// for the pieces ahead: which piece the position sits in, when that piece must be complete, and
// how long each subsequent piece buys. Split out from pushDeadlines to be testable on its own.
func deadlineSchedule(abs, pieceLen int64, rate float64, now time.Time) (first int, firstDue time.Time, interval time.Duration) {
	if pieceLen <= 0 || rate <= 0 {
		return 0, now, 0
	}
	first = int(abs / pieceLen)
	// The piece under the read position is due once the reader reaches its end; every later
	// piece one piece-time after the one before it.
	remaining := pieceLen - (abs % pieceLen)
	firstDue = now.Add(time.Duration(float64(remaining) / rate * float64(time.Second)))
	interval = time.Duration(float64(pieceLen) / rate * float64(time.Second))
	return
}
