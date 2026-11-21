package streamer

import (
	"slices"
	"time"
)

// demandPins is how many incomplete pieces of a demand range get a pin: the piece holding the
// blocked read and the next one. A FetchAhead window is pinned only until its head is handed
// over (Demote) or while a follower is blocked on it (Await).
const demandPins = 2

// pinBase is about 2004 in Unix milliseconds: below any playout deadline, which is a real
// instant (in the fork a deadline outranks every priority).
const pinBase = 1 << 40

// pinDue is the pin deadline of the demand whose wait began seq-th: the oldest wait first, so
// a reader that blocks again and again (an analysis outrunning a thin swarm) cannot cut ahead
// of a read already waiting. A window whose caller has its bytes is demoted and unpinned, so
// it never holds back a seek. Prefetches are never pinned.
func pinDue(seq int64) time.Time { return time.UnixMilli(pinBase + seq) }

// headTier separates the deadline tiers below the playout schedules: pins first, then the
// piece a stream's reader waits on, then the one a prefetch's reader waits on.
const headTier = 1 << 30

// headDue is the deadline of the piece a consumer's reader waits on, its wait begun seq-th,
// oldest first within a kind. Behind the blocked FUSE reads, ahead of every playout schedule:
// the swarm serves the consumers the pipe is draining in turn, instead of one stream's whole
// schedule before any other consumer moves; the pump keeps precedence over prefetches.
func headDue(kind Kind, seq int64) time.Time {
	tier := int64(headTier)
	if kind != KindStream {
		tier *= 2
	}
	return time.UnixMilli(pinBase + tier + seq)
}

// pin keeps a pin on the first incomplete pieces of every demand range and a head deadline
// on the piece under every other waiting consumer, and removes them from pieces nobody needs
// that way any more. Runs after the playout deadlines, which may have overwritten or cleared
// one in the same pass.
func (t *Torrent) pin(spans []span, pl int64, np int) {
	want := map[int]time.Time{}
	keep := func(i int, due time.Time) {
		if cur, ok := want[i]; !ok || due.Before(cur) {
			want[i] = due
		}
	}
	for _, sp := range spans {
		if sp.pos >= sp.end {
			continue
		}
		if sp.kind != KindDemand {
			if i := pieceOf(sp.pos, pl); sp.waiting != 0 && i < np && !t.ts.Piece(i).Complete() {
				keep(i, headDue(sp.kind, sp.waiting))
			}
			continue
		}
		w := demandWindow(sp, pl, np)
		for i, k := w.First, 0; i <= w.Last && k < demandPins; i++ {
			if t.ts.Piece(i).Complete() {
				continue
			}
			keep(i, pinDue(sp.seq))
			k++
		}
	}
	for i := range t.pins {
		if _, ok := want[i]; !ok {
			t.ctl.ClearStreamDeadlinesRange(i, 1)
			delete(t.pins, i)
		}
	}
	for i, due := range want {
		t.ctl.SetStreamDeadlines(i, 1, due, time.Millisecond)
		t.pins[i] = true
	}
}

// cursors gives the engine every consumer's read position, so the chunks of the piece under
// it are requested from there on, not from the piece's start (the fork's Reader cursor).
func (t *Torrent) cursors(spans []span) {
	var offs []int64
	for _, sp := range spans {
		if sp.pos < sp.end {
			offs = append(offs, sp.pos)
		}
	}
	slices.Sort(offs)
	if slices.Equal(offs, t.cursorsSet) {
		return
	}
	t.cursorsSet = offs
	t.ctl.SetStreamCursors(offs)
}
