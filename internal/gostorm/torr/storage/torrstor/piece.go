package torrstor

import (
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"

	"gostream/internal/gostorm/log"
	"gostream/internal/gostorm/settings"
)

var (
	// V303: Atomic Shield Protection
	// Using atomic.Int64 to store last corruption Unix timestamp for thread-safety.
	lastCorruptionUnix atomic.Int64
	// shieldActive tracks if we are currently forcing STRICT mode due to corruption.
	shieldActive atomic.Bool
	// isWatchdogRunning prevents multiple goroutine spawns.
	isWatchdogRunning atomic.Bool
	// staticCorruptionCount tracks consecutive corrupted pieces for delayed activation.
	staticCorruptionCount atomic.Int32
	// strictCycleCount escalates the clean streak required each time STRICT is re-triggered.
	// 1st cycle: 30s, 2nd: 60s, 3rd: 90s, 4th+: 120s. Resets on media.stop.
	strictCycleCount atomic.Int32
)

// IsResponsive returns the effective state of ResponsiveMode,
// taking into account both user settings and active corruption shield.
func IsResponsive() bool {
	// If user manually disabled ResponsiveMode, it stays OFF regardless of shield.
	// If user enabled it, we return true ONLY if shield is NOT active.
	return settings.GetResponsiveMode() && !shieldActive.Load()
}

// ResetShield resets the Adaptive Shield to its base state.
// Called on media.stop to start fresh for the next viewing.
func ResetShield() {
	shieldActive.Store(false)
	isWatchdogRunning.Store(false)
	staticCorruptionCount.Store(0)
	strictCycleCount.Store(0)
}

type Piece struct {
	storage.PieceImpl `json:"-"`

	Id   int   `json:"-"`
	Size int64 `json:"size"`

	Complete atomic.Bool `json:"-"`
	Accessed int64       `json:"accessed"`

	mPiece *MemPiece `json:"-"`

	cache *Cache `json:"-"`
}

func NewPiece(id int, cache *Cache) *Piece {
	p := &Piece{
		Id:    id,
		cache: cache,
	}

	// V256: RAM is always the primary torrent cache.
	// UseDisk now controls our FUSE-layer disk warmup cache, not native GoStorm piece storage.
	p.mPiece = NewMemPiece(p)
	return p
}

func (p *Piece) WriteAt(b []byte, off int64) (n int, err error) {
	return p.mPiece.WriteAt(b, off)
}

func (p *Piece) ReadAt(b []byte, off int64) (n int, err error) {
	return p.mPiece.ReadAt(b, off)
}

func (p *Piece) MarkComplete() error {
	p.Complete.Store(true)
	return nil
}

func (p *Piece) MarkNotComplete() error {
	p.Complete.Store(false)

	// V-evict-guard: buffer nil = pezzo evicted dalla cache, non corruzione da peer.
	// Evita falsi positivi AdaptiveShield durante eviction sotto pressione RAM.
	p.mPiece.mu.RLock()
	hasData := p.mPiece.buffer != nil
	p.mPiece.mu.RUnlock()
	if !hasData {
		return nil
	}

	// V303: Adaptive Responsive Shield
	// Corruption detected: update last seen Unix timestamp
	now := time.Now().Unix()
	lastCorruptionUnix.Store(now)

	// V305: Delayed STRICT Activation to prevent micro-stutters.
	// First corruption event bans the peer (engine level) but keeps FAST mode.
	// Consecutive or rapid corruption forces STRICT mode.
	if settings.GetAdaptiveShield() && settings.GetResponsiveMode() && !shieldActive.Load() {
		count := staticCorruptionCount.Add(1)
		if count > 1 {
			cycle := strictCycleCount.Add(1)
			cleanNeeded := int64(30) * int64(cycle)
			if cleanNeeded > 120 {
				cleanNeeded = 120
			}
			log.TLogln("[AdaptiveShield] Persistent corruption - Force STRICT mode (Shield: ACTIVE, cycle", cycle, ", need", cleanNeeded, "s clean)")
			shieldActive.Store(true)
		} else {
			log.TLogln("[AdaptiveShield] Single corruption detected for piece", p.Id, "- FAST mode preserved, monitoring...")
		}
	}

	// Start watchdog on first corruption to clear pending state if no follow-up arrives.
	// Previously gated on shieldActive, which left staticCorruptionCount=1 dangling indefinitely.
	if staticCorruptionCount.Load() >= 1 && !isWatchdogRunning.Swap(true) {
		go func() {
			for {
				time.Sleep(1 * time.Second)
				last := lastCorruptionUnix.Load()
				elapsed := time.Since(time.Unix(last, 0))

				cycle := strictCycleCount.Load()
				cleanNeeded := time.Duration(30*cycle) * time.Second
				if cleanNeeded < 30*time.Second {
					cleanNeeded = 30 * time.Second
				}
				if cleanNeeded > 120*time.Second {
					cleanNeeded = 120 * time.Second
				}

				if elapsed > cleanNeeded {
					if shieldActive.Swap(false) {
						log.TLogln("[AdaptiveShield] Clean streak detected (", cleanNeeded.Seconds(), "s) - Restoring FAST mode (Shield: OFF)")
					}
					staticCorruptionCount.Store(0)
					isWatchdogRunning.Store(false)
					return
				}
			}
		}()
	}

	return nil
}

func (p *Piece) Completion() storage.Completion {
	return storage.Completion{
		Complete: p.Complete.Load(),
		Ok:       true,
	}
}

func (p *Piece) Release() {
	p.mPiece.Release()
	if !p.cache.isClosed && p.cache.torrent != nil {
		p.cache.torrent.Piece(p.Id).SetPriority(torrent.PiecePriorityNone)
		p.cache.torrent.Piece(p.Id).UpdateCompletion()
	}
}
