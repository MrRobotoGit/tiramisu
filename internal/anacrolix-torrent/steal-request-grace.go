package torrent

import (
	"os"
	"time"
)

// stealRequestGraceEnvKey sets ClientConfig.StealRequestGrace for clients built from
// NewDefaultClientConfig, as a time.ParseDuration value ("0", "250ms"), so the grace can be
// swept across runs without rebuilding. Same key as upstream.
const stealRequestGraceEnvKey = "TORRENT_STEAL_REQUEST_GRACE"

// stealRequestGraceFromEnv returns the grace from the environment, 0 (disabled, the historical
// behaviour) when unset or unparsable.
func stealRequestGraceFromEnv() time.Duration {
	d, err := time.ParseDuration(os.Getenv(stealRequestGraceEnvKey))
	if err != nil {
		return 0
	}
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
func stealPermitted(reason string, diff int64, stealerLast, holderLast time.Time, graceElapsed bool) bool {
	// An update caused by a cancel comes from a steal: stealing back would ping-pong the request.
	if reason == "Peer.cancel" {
		return false
	}
	// Don't steal from the poor: only for one more request than the holder keeps, and on a tie
	// only if the stealer received a useful chunk more recently.
	if diff > 1 || (diff == 1 && !stealerLast.After(holderLast)) {
		return false
	}
	return graceElapsed
}
