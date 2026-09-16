package native

import (
	"sync"
	"time"

	"tiramisu/internal/gostorm/torr"
)

// A release can stop answering long after it was added, and its metainfo stays in the
// state DB for good (V255): Info() is then non-nil on every Open, the metadata wait in
// Wake never runs, and the only detector the reaper has never fires. Reachability is
// therefore decided here, on the read paths, where the evidence that settles it passes:
// bytes.

const (
	// deadSwarmCooldown bounds how often one hash may be condemned. A failed playback
	// emits dozens of empty reads; without this the counter the weekly review reads
	// would carry thousands of failures, all from a single attempt.
	deadSwarmCooldown = 5 * time.Minute
	// aliveReportEvery bounds the repeats only. The first read that returns bytes always
	// reports, so a recovered release clears its counter immediately; what this drops is
	// the DB lookup behind every subsequent cache miss.
	aliveReportEvery = time.Minute
)

// activePeersFn reports established connections for a hash. A var so the verdict can be
// exercised without a live torrent, the same seam as streamRangeFn.
var activePeersFn = activePeers

type verdict struct {
	alive bool
	at    time.Time
}

// lastVerdict is the last outcome reported per hash. Guarded by a mutex rather than a
// sync.Map because the decision below is read-then-write: FetchBlock and FetchAhead run
// concurrently on the same hash, and two of them reading the old state before either
// writes would let both through, which is how one failed playback once wrote three
// failures from two moments.
var (
	verdictMu   sync.Mutex
	lastVerdict = map[string]verdict{}
)

// activePeers counts connections that exist, not addresses that are remembered.
// TotalPeers and PendingPeers both count t.peers, which AddTorrent refills from the
// PeerAddrs cached in the DB: a dead release would carry phantom peers for good and
// could never be condemned, which is the same blind spot metainfo caching already
// created once.
func activePeers(hash string) int {
	t := torr.PeekTorrent(hash)
	if t == nil || t.Torrent == nil {
		return 0
	}
	return t.Torrent.Stats().ActivePeers
}

// reportReachability turns the outcome of one read into a verdict for the reaper.
//
// Bytes are proof of life and clear the counter. Zero bytes prove nothing on their own,
// since a read can end early for reasons that say nothing about the swarm, so a failure
// is only recorded when the read waited out its whole timeout and nothing was connected
// either. Anything in between stays silent: no verdict is better than a wrong one.
//
// An Open that never asks for bytes never reaches here, which is what keeps a library
// scan from condemning a release it only probed.
func reportReachability(hash string, n int, timedOut bool) {
	if ReachabilityOutcome == nil || hash == "" {
		return
	}
	if n > 0 {
		if !shouldReport(hash, true) {
			return
		}
		ReachabilityOutcome(hash, true)
		return
	}
	if !timedOut || activePeersFn(hash) > 0 {
		return
	}
	if !shouldReport(hash, false) {
		return
	}
	ReachabilityOutcome(hash, false)
}

// shouldReport throttles repeats of the verdict already standing, and lets a change of
// verdict through at once: a swarm that comes back must clear its counter on the first
// byte, not at the end of somebody's window. The check and the stamp happen under one
// lock, so concurrent reads of the same hash produce one verdict, not one each.
func shouldReport(hash string, alive bool) bool {
	now := time.Now()

	verdictMu.Lock()
	defer verdictMu.Unlock()

	if p, ok := lastVerdict[hash]; ok && p.alive == alive {
		window := deadSwarmCooldown
		if alive {
			window = aliveReportEvery
		}
		if now.Sub(p.at) < window {
			return false
		}
	}
	lastVerdict[hash] = verdict{alive: alive, at: now}
	return true
}

// forgetReachability drops the throttle state of a hash that is going away, so a hash
// added again later starts from no verdict instead of inheriting one.
func forgetReachability(hash string) {
	verdictMu.Lock()
	defer verdictMu.Unlock()
	delete(lastVerdict, hash)
}
