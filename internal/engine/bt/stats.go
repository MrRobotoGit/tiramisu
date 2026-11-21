package bt

import "github.com/anacrolix/torrent"

// Stats are a torrent's cumulative counters and current peer counts.
type Stats struct {
	BytesWritten, BytesWrittenData                        int64
	BytesRead, BytesReadData, BytesReadUsefulData         int64
	ChunksWritten, ChunksRead, ChunksReadUseful           int64
	ChunksReadWasted, PiecesDirtiedGood, PiecesDirtiedBad int64

	TotalPeers, PendingPeers, ActivePeers, ConnectedSeeders, HalfOpenPeers int
}

func (t *Torrent) Stats() Stats {
	s := t.t.Stats()
	return Stats{
		BytesWritten: s.BytesWritten.Int64(), BytesWrittenData: s.BytesWrittenData.Int64(),
		BytesRead: s.BytesRead.Int64(), BytesReadData: s.BytesReadData.Int64(),
		BytesReadUsefulData: s.BytesReadUsefulData.Int64(), ChunksWritten: s.ChunksWritten.Int64(),
		ChunksRead: s.ChunksRead.Int64(), ChunksReadUseful: s.ChunksReadUseful.Int64(),
		ChunksReadWasted: s.ChunksReadWasted.Int64(), PiecesDirtiedGood: s.PiecesDirtiedGood.Int64(),
		PiecesDirtiedBad: s.PiecesDirtiedBad.Int64(), TotalPeers: s.TotalPeers,
		PendingPeers: s.PendingPeers, ActivePeers: s.ActivePeers,
		ConnectedSeeders: s.ConnectedSeeders, HalfOpenPeers: s.HalfOpenPeers,
	}
}

// BytesCompleted counts verified pieces plus dirty chunks.
func (t *Torrent) BytesCompleted() int64 { return t.t.BytesCompleted() }

// PeerSnapshot is one connected peer.
type PeerSnapshot = torrent.PeerSnapshot

func (t *Torrent) PeerSnapshots() []PeerSnapshot { return t.t.PeerSnapshots() }
