package library

import "context"

// FileStat is one file inside a torrent, as GoStorm reports it. ID is the 1-based
// index the stream URL addresses the file by.
type FileStat struct {
	ID     int    `json:"id"`
	Path   string `json:"path"`
	Length int64  `json:"length"`
}

// TorrentStats is the torrent view GoStorm returns.
type TorrentStats struct {
	Hash        string     `json:"hash"`
	Title       string     `json:"title"`
	Length      int64      `json:"length"`
	ActivePeers int        `json:"active_peers"`
	FileStats   []FileStat `json:"file_stats"`
}

// GoStorm is the engine API the library manager needs. *engines.GoStormClient
// implements it; tests substitute a fake.
type GoStorm interface {
	AddTorrent(ctx context.Context, magnet, title string) (string, error)
	GetTorrentInfo(ctx context.Context, hash string, maxWait int) (*TorrentStats, error)
	RemoveTorrent(ctx context.Context, hash string) error
	ListTorrents(ctx context.Context) ([]TorrentStats, error)
}
