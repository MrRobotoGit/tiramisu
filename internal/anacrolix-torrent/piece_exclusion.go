package torrent

import (
	"net/netip"
	"slices"
)

// pieceExclusion is set on a piece whose hash check failed with every chunk from peers (#39).
// The piece is then requested only from peers that touched none of its failed attempts; when
// none of those can serve it, from one suspect per attempt in rotation, so the next attempt
// either passes (smartban names the corruptor) or fails with a sole dirtier (banned).
type pieceExclusion struct {
	suspects map[netip.Addr]struct{}
	failures int
}

func (ex *pieceExclusion) suspect(p *Peer) bool {
	if !p.bannableAddr.Ok {
		return false
	}
	_, ok := ex.suspects[p.bannableAddr.Value.Unmap()]
	return ok
}

// excludeDirtiers records the dirtiers of piece i's failed attempt. Call before they are cleared.
func (t *Torrent) excludeDirtiers(i pieceIndex) {
	p := t.piece(i)
	if p.exclusion == nil {
		p.exclusion = &pieceExclusion{suspects: make(map[netip.Addr]struct{})}
		if t.excludedPieces == nil {
			t.excludedPieces = make(map[pieceIndex]struct{})
		}
		t.excludedPieces[i] = struct{}{}
	}
	for c := range p.dirtiers {
		if c.bannableAddr.Ok {
			p.exclusion.suspects[c.bannableAddr.Value.Unmap()] = struct{}{}
		}
	}
	p.exclusion.failures++
}

func (t *Torrent) clearExclusion(i pieceIndex) {
	if t.pieces[i].exclusion == nil {
		return
	}
	t.pieces[i].exclusion = nil
	delete(t.excludedPieces, i)
}

// releaseExclusions reopens the pieces a newly banned address had touched: their corruptor is
// known, the other suspects are cleared.
func (t *Torrent) releaseExclusions(ip netip.Addr) {
	ip = ip.Unmap()
	released := false
	for i := range t.excludedPieces {
		if _, ok := t.pieces[i].exclusion.suspects[ip]; ok {
			t.clearExclusion(i)
			released = true
		}
	}
	if released {
		t.iterPeers(func(p *Peer) { p.updateRequests("piece exclusion released") })
	}
}

// exclusionAllows reports whether p may request piece i. Hot path: one nil check per piece.
func (p *Peer) exclusionAllows(i pieceIndex) bool {
	ex := p.t.pieces[i].exclusion
	if ex == nil || !ex.suspect(p) {
		return true
	}
	turn, outsider := p.t.exclusionTurn(i, ex)
	if outsider {
		return false
	}
	// No suspect can serve it now: allowing keeps us interested, so they unchoke us.
	return turn == nil || turn == p
}

// exclusionTurn returns the suspect whose turn it is to serve piece i, or outsider when a peer
// outside the suspects can serve it. Only peers that have the piece and let us request it count.
func (t *Torrent) exclusionTurn(i pieceIndex, ex *pieceExclusion) (turn *Peer, outsider bool) {
	var suspects []*Peer
	for pc := range t.conns {
		c := &pc.Peer
		if !c.peerHasPiece(i) || (c.peerChoking && !c.peerAllowedFast.Contains(i)) {
			continue
		}
		if !ex.suspect(c) {
			return nil, true
		}
		suspects = append(suspects, c)
	}
	if len(suspects) == 0 {
		return nil, false
	}
	slices.SortFunc(suspects, func(a, b *Peer) int {
		return a.bannableAddr.Value.Unmap().Compare(b.bannableAddr.Value.Unmap())
	})
	return suspects[(ex.failures+i)%len(suspects)], false
}
