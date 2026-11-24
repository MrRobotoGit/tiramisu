package engine

import (
	"log"
	"time"

	"tiramisu/internal/engine/bt"
	"tiramisu/internal/metadb"
)

// Config holds the engine knobs, with the JSON keys of the settings document (plan 2c).
type Config struct {
	CacheSize                int64 // bytes per torrent
	ConnectionsLimit         int
	TorrentDisconnectTimeout int // seconds
	RetrackersMode           int // 0 keep, 1 add defaults, 2 clear, 3 replace with defaults
	EnableDebug              bool
	EnableIPv6               bool
	DisableTCP               bool
	DisableUTP               bool
	DisableDHT               bool
	DisablePEX               bool
	DisableUpload            bool
	ForceEncrypt             bool
	AggressivePeerManagement bool
	DownloadRateLimit        int // kB/s
	UploadRateLimit          int // kB/s
	PeersListenPort          int
	DisableUPNP              bool
	ResponsiveMode           bool
	AdaptiveShield           bool
	FriendsMaxHelps          int // friends' playbacks helped at once; 0 = default (2), negative = none
}

const (
	defaultCacheSize   = 128 << 20
	defaultConnections = 25
	defaultTimeout     = 30 // the save clamp for <= 0
)

// normalized applies the clamps of engine.md §3.1.
func (c Config) normalized() Config {
	if c.CacheSize <= 0 {
		c.CacheSize = defaultCacheSize
	}
	if c.ConnectionsLimit <= 0 {
		c.ConnectionsLimit = defaultConnections
	}
	if c.TorrentDisconnectTimeout <= 0 {
		c.TorrentDisconnectTimeout = defaultTimeout
	}
	return c
}

func (c Config) timeout() time.Duration {
	return time.Duration(c.TorrentDisconnectTimeout) * time.Second
}

// portForwarding: the library's UPnP maps the peer port, as GoStorm did, unless turned off or
// Tiramisu's NAT-PMP owns the port.
func (c Config) portForwarding(natpmp bool) bool { return !c.DisableUPNP && !natpmp }

func (c Config) btOptions() bt.Options {
	return bt.Options{
		Debug: c.EnableDebug, EnableIPv6: c.EnableIPv6, DisableTCP: c.DisableTCP,
		DisableUTP: c.DisableUTP, DisableDHT: c.DisableDHT, DisablePEX: c.DisablePEX,
		DisableUpload: c.DisableUpload, ForceEncrypt: c.ForceEncrypt,
		AggressivePeerManagement: c.AggressivePeerManagement, ConnectionsLimit: c.ConnectionsLimit,
		DownloadRateLimit: c.DownloadRateLimit, UploadRateLimit: c.UploadRateLimit,
		ListenPort: c.PeersListenPort,
	}
}

// Deps are the engine's collaborators.
type Deps struct {
	DB       *metadb.DB
	StateDir string // trackers.txt
	NATPMP   bool   // Tiramisu's NAT-PMP owns the peer port
	Logf     func(format string, args ...any)
}

func (d Deps) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}
