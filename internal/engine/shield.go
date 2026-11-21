package engine

import (
	"os"

	"tiramisu/internal/engine/bt"
)

// ShieldStatus aggregates the AdaptiveShield of every torrent for /metrics; zero when the
// client is down.
func (e *Engine) ShieldStatus() (active bool, last, need int64) {
	if c := e.currentClient(); c != nil {
		return c.Store().ShieldStatus()
	}
	return false, 0, 0
}

// ResetShield resets the shield of one hydrated torrent (seek, revival, media.stop).
func (e *Engine) ResetShield(hash string) {
	if t := e.Get(hash); t != nil {
		if ts := t.Store(); ts != nil {
			ts.Shield().Reset()
		}
	}
}

// Responsive reports whether unverified data may be served: ResponsiveMode on and no shield
// in STRICT. The raCache tags its buffers with it.
func (e *Engine) Responsive() bool {
	if e == nil || !e.config().ResponsiveMode {
		return false
	}
	active, _, _ := e.ShieldStatus()
	return !active
}

// BlocklistCounters are the fork's rejection counters (/metrics).
func BlocklistCounters() (rejections, distinctIPs uint64) { return bt.BlocklistCounters() }

// BlockedRange is one blocklist range and the distinct addresses it rejected.
type BlockedRange = bt.BlockedRange

// BlocklistTopRanges lists the busiest ranges (/metrics/blocklist).
func BlocklistTopRanges(n int) []BlockedRange { return bt.BlocklistTopRanges(n) }

// CountRangesFile counts the usable ranges of a blocklist file; 0 when unreadable.
func CountRangesFile(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	return CountRanges(f)
}
