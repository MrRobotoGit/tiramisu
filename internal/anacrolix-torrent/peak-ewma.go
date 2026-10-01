package torrent

import (
	"expvar"
	"math"
	"os"
	"time"
)

// peakEwmaEnvKey overrides ClientConfig.PeakEwma for clients built from NewDefaultClientConfig.
// Off by default: unmeasured, so it must be turned on for a comparison run rather than shipped.
const peakEwmaEnvKey = "TORRENT_PEAK_EWMA"

var peakEwmaEffective = expvar.NewString("peakEwma")

func peakEwmaFromEnv() bool {
	on := false
	switch os.Getenv(peakEwmaEnvKey) {
	case "0", "false":
		on = false
	case "1", "true":
		on = true
	}
	if on {
		peakEwmaEffective.Set("on")
	} else {
		peakEwmaEffective.Set("off")
	}
	return on
}

// peakLatencyHalfLife is how quickly a past peak is forgotten: the stored peak halves every
// interval without a larger sample. Short so one slow answer does not condemn a peer for a whole
// warmup window.
const peakLatencyHalfLife = 3 * time.Second

// peakLatencyFreshness is how long the peak is trusted at all. A peer that has sent nothing for
// this long has an unknown speed, not a fast one: without this check a stalled/choked-but-alive
// peer's peak decays toward zero, it looks like the cheapest holder, and rescues of its blocks
// are vetoed exactly when they are needed. Stale means "no data", so the queue rules decide.
const peakLatencyFreshness = 5 * time.Second

// peakLatency is a time-decayed maximum of request-to-chunk latencies. A load balancer keeps the
// peak rather than the mean so a backend that answered slowly is avoided before its average
// catches up (Finagle/Linkerd peak-EWMA). Guarded by t.cl's lock.
type peakLatency struct {
	value time.Duration
	at    time.Time
}

func (p *peakLatency) add(now time.Time, d time.Duration) {
	if d <= 0 {
		return
	}
	decayed := p.decayed(now)
	if d >= decayed {
		p.value = d
	} else {
		p.value = decayed
	}
	p.at = now
}

func (p *peakLatency) decayed(now time.Time) time.Duration {
	if p.value <= 0 || p.at.IsZero() {
		return 0
	}
	elapsed := now.Sub(p.at)
	if elapsed <= 0 {
		return p.value
	}
	return time.Duration(float64(p.value) * math.Pow(0.5, elapsed.Seconds()/peakLatencyHalfLife.Seconds()))
}

// get returns the decayed peak at now, and ok false when there has never been a sample or the
// last one is stale. A silent peer is unknown, not fast.
func (p *peakLatency) get(now time.Time) (time.Duration, bool) {
	d := p.decayed(now)
	if d <= 0 || now.Sub(p.at) > peakLatencyFreshness {
		return 0, false
	}
	return d, true
}

// peakEwmaPermits decides whether a steal pays off, using the peak-EWMA placement rule: cost is
// peak latency times the requests in flight, and the stealer must expect to finish sooner than
// the holder. The stealer's cost adds the request it would take (+1); the holder already has it
// in flight, so its cost is its current queue. This is the signal the removed rate veto lacked:
// a recent peak latency, not a demand-limited throughput EWMA. With no peak on either side the
// queue rules in stealPermitted decide alone.
//
// Structural note: when both peaks are equal the comparison reduces to stealerPending+1 <
// holderPending, i.e. the existing queue-depth rule. The peak changes the outcome only when the
// two peers' peaks differ, so its marginal effect is confined to that regime.
//
// Known bias: the holder's block is necessarily old (the grace guarantees age before a steal),
// so it sits mid-queue, not at the tail; holderPending overstates its remaining time and makes
// the veto more permissive than the model suggests.
func peakEwmaPermits(stealerPeak time.Duration, stealerOK bool, stealerPending int64,
	holderPeak time.Duration, holderOK bool, holderPending int64) bool {
	if !stealerOK || !holderOK || stealerPeak <= 0 || holderPeak <= 0 {
		return true
	}
	stealerCost := float64(stealerPeak) * float64(stealerPending+1)
	holderCost := float64(holderPeak) * float64(holderPending)
	return stealerCost < holderCost
}
