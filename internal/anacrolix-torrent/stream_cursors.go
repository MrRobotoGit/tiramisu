package torrent

// SetStreamCursors declares the torrent offsets where consumers without a Reader read next.
// Within the pieces holding them, chunks are requested from the offset on, as for a Reader's
// position (see chunkRank). The set replaces the previous one; nil clears it.
func (t *Torrent) SetStreamCursors(offsets []int64) {
	t.cl.lock()
	defer t.cl.unlock()
	t.setStreamCursors(offsets)
}

func (t *Torrent) setStreamCursors(offsets []int64) {
	if !t.haveInfo() {
		return
	}
	var m map[pieceIndex]RequestIndex
	for _, abs := range offsets {
		if abs < 0 || abs >= t.length() {
			continue
		}
		req, ok := t.offsetRequest(abs)
		if !ok {
			continue
		}
		if m == nil {
			m = make(map[pieceIndex]RequestIndex, len(offsets))
		}
		ri := t.requestIndexFromRequest(req)
		piece := pieceIndex(req.Index)
		if cur, ok := m[piece]; !ok || ri < cur {
			m[piece] = ri
		}
	}
	t.streamCursors = m
	t.iterPeers(func(p *Peer) { p.updateRequests("Torrent.SetStreamCursors") })
}
