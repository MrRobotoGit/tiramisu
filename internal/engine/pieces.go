package engine

import (
	"time"

	"tiramisu/internal/piecestore"
)

// PieceControl is the engine side of the streamer's piece scheduling (storage.md §7.2).
type PieceControl interface {
	SetPiecePriorities(map[int]piecestore.Priority)
	SetStreamDeadlines(first, n int, firstDue time.Time, interval time.Duration)
	ClearStreamDeadlinesRange(first, n int)
	SetStreamCursors(offsets []int64)
	ForgetPartialPiece(i int) bool
	UpdateCompletion(i int)
}

// Pieces is the piece control of a hydrated torrent; nil on a DB-only view.
func (t *Torrent) Pieces() PieceControl {
	if t.bt == nil {
		return nil
	}
	return t.bt
}
