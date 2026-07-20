package torrent

import (
	"sync"
	"sync/atomic"
)

// V304: Explicit IP ban set, keyed by IP string.
// Unlike badPeerIPs (netip.Addr), string keys avoid IPv4/IPv6 normalization mismatches.
// Populated either immediately (certain attribution: sole dirtier of a piece) or once a peer
// accumulates >= badPeerThreshold corrupt pieces (uncertain attribution: multi-dirtier) -
// badPeerThreshold is defined at its call site in torrent.go, not here.
var v304BannedIPs sync.Map

// v304CorruptCounts tracks corrupt-piece counts per IP (not per connection), so a
// corruptor can't reset its tally by reconnecting.
var v304CorruptCounts sync.Map // ip string -> *atomic.Int64

// v304OnBan, when set, is invoked once per newly banned IP (used by the GoStorm layer to
// persist bans). Set at startup before the client connects; called from a goroutine.
var v304OnBan func(ip string)

// V304SetOnBan registers the new-ban callback. Call before the client starts.
func V304SetOnBan(f func(ip string)) {
	v304OnBan = f
}

// V304LoadBans seeds the ban set from persisted state without firing the callback.
func V304LoadBans(ips []string) {
	for _, ip := range ips {
		v304BannedIPs.Store(ip, struct{}{})
	}
}

// v304AddCorrupt increments ip's corrupt-piece count and returns the new total.
func v304AddCorrupt(ip string) int64 {
	v, _ := v304CorruptCounts.LoadOrStore(ip, new(atomic.Int64))
	return v.(*atomic.Int64).Add(1)
}

// V304BannedCount returns the number of banned IPs (persisted + current session).
func V304BannedCount() int {
	n := 0
	v304BannedIPs.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}
