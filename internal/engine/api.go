package engine

import "context"

// API is the in-process surface that replaces :8090 for main, the syncs, the library, the
// monitor and the bridge (engine.md §4, §5).
type API interface {
	Ready() <-chan struct{}
	Up() bool
	Config() Config
	Reconnect(Config) error

	Get(hash string) *Torrent
	Peek(hash string) *Torrent
	Add(link string, meta Meta) (*Torrent, error)
	AddTorrentFile(data []byte, meta Meta) (*Torrent, error)
	WaitFiles(ctx context.Context, hash string) ([]File, error)
	Drop(hash string) bool
	Remove(hash string) error
	ListRefs() ([]Ref, error)
	ListHydrated() []*Torrent
	ListStreaming() []*Torrent

	CollectPeerTransportStats() PeerTransportStats
	CollectGradient2Stats() Gradient2Stats
	HedgeTotals() (triggers int64, deadlinePieces int, circuitOpen bool)
	V304BannedCount() int
	SetPeerInfo(PeerInfoFunc)

	SetBlockListEnabled(bool)
	SetBlockListFilter(pattern string) error
	ReadBlockedIP() (*Blocklist, error)
	SetIPBlocklist(*Blocklist)
}

var _ API = (*Engine)(nil)
