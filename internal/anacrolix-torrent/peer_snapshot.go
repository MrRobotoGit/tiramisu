package torrent

// PeerSnapshot is one connected peer as the monitor sees it: where it is, how many useful
// bytes it has sent so far, and whether it has the whole torrent.
type PeerSnapshot struct {
	Addr        string
	UsefulBytes int64
	SentBytes   int64 // piece data we uploaded to this peer
	Seed        bool
	UTP         bool
}

// PeerSnapshots lists the connected peers (tiramisu: per-peer rates for the swarm view).
func (t *Torrent) PeerSnapshots() []PeerSnapshot {
	t.cl.rLock()
	defer t.cl.rUnlock()
	out := make([]PeerSnapshot, 0, len(t.conns))
	for c := range t.conns {
		all, known := c.peerHasAllPieces()
		addr := ""
		if c.RemoteAddr != nil {
			addr = c.RemoteAddr.String()
		}
		out = append(out, PeerSnapshot{Addr: addr, UsefulBytes: c._stats.BytesReadUsefulData.Int64(), SentBytes: c._stats.BytesWrittenData.Int64(), Seed: all && known, UTP: c.utp()})
	}
	return out
}
