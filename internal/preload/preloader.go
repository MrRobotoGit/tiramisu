package preload

import (
	"log"
	"os"
	"sync"
	"time"

	"tiramisu/internal/gostorm/native"
)

// PreloadStrategy defines the preload size based on torrent download speed
type PreloadStrategy int

const (
	StrategyLow    PreloadStrategy = iota // 6 MB - slow downloads
	StrategyMedium                        // 10 MB - moderate downloads
	StrategyHigh                          // 16 MB - fast downloads
)

// Strategy size mappings
const (
	PreloadSizeLow    = 6 * 1024 * 1024  // 6 MB
	PreloadSizeMedium = 10 * 1024 * 1024 // 10 MB
	PreloadSizeHigh   = 16 * 1024 * 1024 // 16 MB
)

// Speed thresholds (bytes per second)
const (
	SpeedThresholdHigh   = 15 * 1024 * 1024 // 15 MiB/s
	SpeedThresholdMedium = 4 * 1024 * 1024  // 4 MiB/s
)

// TorrentStats represents torrent statistics from GoStorm API
// PeerPreloader manages adaptive preload based on peer speed
// PeerPreloader manages adaptive preload based on peer speed
type PeerPreloader struct {
	nativeBridge  *native.NativeClient // V160: Native Bridge
	strategyCache map[string]*StrategyEntry
	mu            sync.RWMutex
	cacheTTL      time.Duration
	logger        *log.Logger
}

// StrategyEntry caches strategy decisions with TTL
type StrategyEntry struct {
	Strategy  PreloadStrategy
	UpdatedAt time.Time
	Speed     float64 // Last observed speed (bytes/sec)
}

// NewPeerPreloader creates a new peer-based preloader
func NewPeerPreloader(nativeBridge *native.NativeClient) *PeerPreloader {
	return &PeerPreloader{
		nativeBridge:  nativeBridge,
		strategyCache: make(map[string]*StrategyEntry),
		cacheTTL:      5 * time.Second, // V162: Reduced to 5s thanks to StatHighFreq optimization
		logger:        log.New(os.Stdout, "[PeerPreload] ", log.LstdFlags),
	}
}

// Cleanup removes stale entries from the strategy cache to prevent memory leaks
func (pp *PeerPreloader) Cleanup() {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	now := time.Now()
	// Use 1 hour as retention for strategy history
	threshold := 1 * time.Hour
	removed := 0

	for hash, entry := range pp.strategyCache {
		if now.Sub(entry.UpdatedAt) > threshold {
			delete(pp.strategyCache, hash)
			removed++
		}
	}

	if removed > 0 {
		pp.logger.Printf("Cleanup: removed %d stale strategy entries", removed)
	}
}

