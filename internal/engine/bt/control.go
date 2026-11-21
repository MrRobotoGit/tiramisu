package bt

import (
	"time"

	"github.com/anacrolix/torrent/types"

	"tiramisu/internal/piecestore"
)

// Piece control for the streamer (storage.md §7.2). Every call takes the client lock: never
// call one while holding a store lock.

// SetPiecePriorities applies every level in one client-lock acquisition.
func (t *Torrent) SetPiecePriorities(prios map[int]piecestore.Priority) {
	if len(prios) == 0 || !t.HasInfo() {
		return
	}
	m := make(map[int]types.PiecePriority, len(prios))
	for i, p := range prios {
		m[i] = types.PiecePriority(p)
	}
	t.t.SetPiecePriorities(m)
}

// SetStreamDeadlines schedules n pieces from first, due at firstDue and one interval apart.
func (t *Torrent) SetStreamDeadlines(first, n int, firstDue time.Time, interval time.Duration) {
	if t.HasInfo() {
		t.t.SetStreamDeadlines(first, n, firstDue, interval)
	}
}

// ClearStreamDeadlinesRange drops the deadlines of pieces [first, first+n).
func (t *Torrent) ClearStreamDeadlinesRange(first, n int) {
	if t.HasInfo() {
		t.t.ClearStreamDeadlinesRange(first, n)
	}
}

// SetStreamCursors orders the chunks of the pieces holding these torrent offsets from the
// offset on (the fork does it for its own readers; the streamer has none).
func (t *Torrent) SetStreamCursors(offsets []int64) {
	if t.HasInfo() {
		t.t.SetStreamCursors(offsets)
	}
}

// ForgetPartialPiece reports whether the engine forgot the partial chunks of piece i, so its
// buffer may be freed (storage.md §3.6).
func (t *Torrent) ForgetPartialPiece(i int) bool { return t.t.ForgetPartialPiece(i) }
