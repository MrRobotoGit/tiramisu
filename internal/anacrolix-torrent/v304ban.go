package torrent

import "sync"

// V304: Explicit per-session IP ban set, keyed by IP string.
// Unlike badPeerIPs (netip.Addr), string keys avoid IPv4/IPv6 normalization mismatches.
// Populated when a peer accumulates >= badPeerThreshold corrupt pieces.
// Persists for the entire process lifetime — no way to reconnect after eviction.
var v304BannedIPs sync.Map

// V304BannedCount returns the number of IPs banned since process start.
func V304BannedCount() int {
	n := 0
	v304BannedIPs.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}
