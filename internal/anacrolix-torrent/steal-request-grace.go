package torrent

import (
	"expvar"
	"log"
	"os"
	"time"
)

// stealRequestGraceEnvKey sets ClientConfig.StealRequestGrace for clients built from
// NewDefaultClientConfig, as a time.ParseDuration value ("0", "250ms"), so the grace can be
// swept across runs without rebuilding. Same key as upstream.
const stealRequestGraceEnvKey = "TORRENT_STEAL_REQUEST_GRACE"

// defaultStealRequestGrace is measured, not argued: on the Pi, 250ms cut duplicate blocks from
// 28% to 8% and stalls from 35 to 24 on the same 6GB of a 4K stream. Upstream defaults to 0.
const defaultStealRequestGrace = 250 * time.Millisecond

// stealRequestGraceEffective publishes the grace the client runs with, so a sweep reads the arm
// it measured from /debug/vars instead of trusting the value it meant to set.
var stealRequestGraceEffective = expvar.NewString("stealRequestGrace")

// stealRequestGraceFromEnv returns the grace from the environment ("0" disables the check), the
// default when unset. Upstream panics on a malformed value; a 24/7 service would crash-loop on a
// typo instead, so it logs loudly and runs the default, which /debug/vars then shows.
func stealRequestGraceFromEnv() time.Duration {
	value, set := os.LookupEnv(stealRequestGraceEnvKey)
	d := defaultStealRequestGrace
	if set {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			log.Printf("ERROR: %s=%q is not a duration (e.g. 250ms): request stealing runs with the default grace %v", stealRequestGraceEnvKey, value, defaultStealRequestGrace)
		} else {
			d = parsed
		}
	}
	stealRequestGraceEffective.Set(d.String())
	return d
}

// peerGraceMin is both the floor of the per-peer grace and the ceiling for an urgent request: the
// fixed value the Pi run measured as good. The per-peer grace may only extend it for a slow
// holder, never shorten it, so a fast holder is not robbed any earlier than today. An urgent
// request near the playhead must not wait a slow holder's full mean, but must not lose the grace
// either: a zero grace would re-open the duplicate window the fixed value closed.
const (
	peerGraceMin = defaultStealRequestGrace
	peerGraceMax = time.Second
)

// clampPeerGrace bounds a per-peer grace to [peerGraceMin, peerGraceMax].
func clampPeerGrace(d time.Duration) time.Duration {
	if d < peerGraceMin {
		return peerGraceMin
	}
	if d > peerGraceMax {
		return peerGraceMax
	}
	return d
}

// peerStealGrace returns how long req must stay outstanding with holder before another peer may
// take it. It is the holder's own mean delivery latency, clamped: the grace may only grow past the
// measured fixed value, for a slow holder, so a peer delivering on its normal schedule is not
// robbed mid-delivery and its block is not duplicated. An urgent request is capped at that fixed
// value instead. With no warmed estimate the configured fixed grace applies; a configured grace
// <= 0 disables the check entirely (the upstream-comparator arm).
func (t *Torrent) peerStealGrace(holder *Peer, urgent bool) time.Duration {
	base := t.cl.config.StealRequestGrace
	if base <= 0 {
		return 0
	}
	grace := base
	if holder != nil {
		if d, ok := holder.meanLatency(); ok {
			grace = clampPeerGrace(d)
		}
	}
	if urgent && grace > peerGraceMin {
		grace = peerGraceMin
	}
	return grace
}

// stealRequestGraceElapsed reports whether req has been outstanding with its current holder long
// enough for another peer to take it. requestState.when is rewritten on every issue, so the grace
// is per holder: each peer gets one uninterrupted attempt. holder is the peer currently holding
// req; urgent marks a request whose piece carries a near playout deadline.
func (t *Torrent) stealRequestGraceElapsed(req RequestIndex, holder *Peer, urgent bool) bool {
	grace := t.peerStealGrace(holder, urgent)
	if grace <= 0 {
		return true
	}
	return time.Since(t.requestState[req].when) >= grace
}

// stealPermitted decides whether a peer may take a request another peer holds. diff is the
// stealer's queue depth after the steal minus the holder's after losing it. Backported from
// upstream 41fc76adf and 23d8abf90.
func stealPermitted(reason string, diff int64, stealerLast, holderLast time.Time, graceElapsed func() bool) bool {
	// An update caused by a cancel comes from a steal: stealing back would ping-pong the request.
	if reason == "Peer.cancel" {
		return false
	}
	// Don't steal from the poor: only for one more request than the holder keeps, and on a tie
	// only if the stealer received a useful chunk more recently.
	if diff > 1 || (diff == 1 && !stealerLast.After(holderLast)) {
		return false
	}
	return graceElapsed()
}
