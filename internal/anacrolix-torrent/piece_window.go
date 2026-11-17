package torrent

// PieceWindowState is one piece of a contiguous window, as the dashboard's piece map draws it.
type PieceWindowState struct {
	Complete bool
	Length   int64
	Missing  int64 // bytes not yet received
	Deadline bool  // a stream deadline is set (SetStreamDeadlines)
}

// PieceWindow snapshots pieces [first, first+n) under one read lock; out-of-range pieces are skipped.
func (t *Torrent) PieceWindow(first, n int) []PieceWindowState {
	t.cl.rLock()
	defer t.cl.rUnlock()
	if !t.haveInfo() || n <= 0 {
		return nil
	}
	total := t.numPieces()
	out := make([]PieceWindowState, 0, n)
	for i := first; i < first+n && i < total; i++ {
		if i < 0 {
			continue
		}
		p := t.piece(i)
		_, dl := t.pieceDeadlines[i]
		out = append(out, PieceWindowState{
			Complete: t.pieceComplete(i),
			Length:   int64(p.length()),
			Missing:  int64(p.bytesLeft()),
			Deadline: dl,
		})
	}
	return out
}
