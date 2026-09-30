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

// stealRequestGraceElapsed reports whether req has been outstanding with its current holder long
// enough for another peer to take it. requestState.when is rewritten on every issue, so the grace
// is per holder: each peer gets one uninterrupted attempt.
func (t *Torrent) stealRequestGraceElapsed(req RequestIndex) bool {
	grace := t.cl.config.StealRequestGrace
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
