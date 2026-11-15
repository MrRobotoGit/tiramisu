package torrent

// ForgetPartialPiece makes the torrent forget the chunks it holds for an incomplete piece that
// nothing wants any more, so the storage may free the piece's buffer. A seek leaves such pieces
// behind: their requests are cancelled, the cache cannot evict them (it only evicts complete
// pieces), and each keeps a whole piece allocated until the torrent closes.
//
// It refuses, returning false, while the piece is complete, wanted, being hashed or written, or
// while any request for one of its chunks is outstanding or awaiting a cancel acknowledgement,
// since a chunk still in flight would land after the forget. A chunk landing anyway is written
// and marked dirty again, which keeps dirty bits and stored data consistent.
func (t *Torrent) ForgetPartialPiece(index int) bool {
	t.cl.lock()
	defer t.cl.unlock()
	if t.closed.IsSet() || t.info == nil || index < 0 || index >= t.numPieces() {
		return false
	}
	p := t.piece(index)
	if t.pieceComplete(index) || t.wantPieceIndex(index) || p.hashing || p.marking ||
		p.queuedForHash() || p.atomicPendingWrites.Load() != 0 || p.numDirtyChunks() == 0 {
		return false
	}
	start := t.pieceRequestIndexOffset(index)
	end := t.pieceRequestIndexOffset(index + 1)
	for ri := start; ri < end; ri++ {
		if _, ok := t.requestState[ri]; ok {
			return false
		}
	}
	for pc := range t.conns {
		if roaringBitmapRangeCardinality[RequestIndex](&pc.requestState.Cancelled, start, end) != 0 {
			return false
		}
	}
	t.pendAllChunkSpecs(index)
	t.clearPieceTouchers(index)
	t.updatePiecePriority(index, "Torrent.ForgetPartialPiece")
	return true
}
