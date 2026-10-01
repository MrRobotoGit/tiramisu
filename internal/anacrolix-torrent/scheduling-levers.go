package torrent

import (
	"expvar"
	"math"
	"os"
	"sort"
	"time"
)

// Opt-in scheduling levers adapted from Netflix concurrency-limits, each behind its own variable
// so it can be measured on its own. All are off by default.
const (
	// TORRENT_GRADIENT2_AIMD: a peer Reject of a request it held backs the Gradient2 limit off
	// (AIMDLimit's drop rule; Gradient2Limit ignores didDrop). AIMD's timeout is not ported: a
	// chunk that arrives late is a slow success, and Gradient2's gradient already answers latency.
	gradient2AIMDEnvKey = "TORRENT_GRADIENT2_AIMD"
	// TORRENT_GRADIENT2_WINDOWED: Gradient2 is fed one percentile per window (WindowedLimit with a
	// PercentileSampleWindow) instead of every 16KB chunk.
	gradient2WindowedEnvKey = "TORRENT_GRADIENT2_WINDOWED"
	// TORRENT_REQUEST_RESERVE: requests due soon may exceed a full queue by a reserved share
	// (AbstractPartitionedLimiter's guaranteed partition).
	requestReserveEnvKey = "TORRENT_REQUEST_RESERVE"
)

var (
	gradient2AIMDEffective     = expvar.NewString("gradient2AIMD")
	gradient2WindowedEffective = expvar.NewString("gradient2Windowed")
	requestReserveEffective    = expvar.NewString("requestReserve")
)

// envFlag reads an opt-in "1"/"true" variable and publishes its effective state.
func envFlag(key string, effective *expvar.String) bool {
	v := os.Getenv(key)
	on := v == "1" || v == "true"
	if effective != nil {
		if on {
			effective.Set("on")
		} else {
			effective.Set("off")
		}
	}
	return on
}

// AIMDLimit's back-off ratio.
const aimdBackoffRatio = 0.9

// onDrop is AIMDLimit's decrease: the limit backs off by 10%, floored at the minimum.
func (g *gradient2) onDrop() int {
	g.estimatedLimit = math.Max(float64(g.minLimit), g.estimatedLimit*aimdBackoffRatio)
	return g.limit()
}

// WindowedLimit defaults: one update per second, at least 10 samples, RTTs under 100us ignored.
// The median is the tracked RTT: robust to the bursts a 16KB chunk stream produces.
const (
	windowTime       = time.Second
	windowMinSamples = 10
	windowCapacity   = 200
	windowPercentile = 0.5
	windowMinRTT     = 100 * time.Microsecond
)

// percentileWindow is ImmutablePercentileSampleWindow: the samples of one window, the largest
// inflight seen and whether anything dropped. Unlike the reference, a full window still records
// inflight and drops.
type percentileWindow struct {
	rtts        []time.Duration
	maxInflight int
	didDrop     bool
}

func (w *percentileWindow) add(rtt time.Duration, inflight int, drop bool) {
	if len(w.rtts) < windowCapacity {
		w.rtts = append(w.rtts, rtt)
	}
	if inflight > w.maxInflight {
		w.maxInflight = inflight
	}
	w.didDrop = w.didDrop || drop
}

func (w *percentileWindow) ready() bool { return len(w.rtts) >= windowMinSamples }

func (w *percentileWindow) tracked() time.Duration {
	if len(w.rtts) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), w.rtts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	i := int(math.Round(float64(len(sorted))*windowPercentile)) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}

// windowedGradient2 is WindowedLimit around Gradient2. A dropped window applies one AIMD
// back-off even when it is short of samples, so a burst of Rejects is a single cut. Call under
// t.cl's lock.
type windowedGradient2 struct {
	g          *gradient2
	w          percentileWindow
	nextUpdate time.Time
}

func newWindowedGradient2(g *gradient2) *windowedGradient2 {
	return &windowedGradient2{g: g}
}

func (x *windowedGradient2) onSample(now time.Time, rtt time.Duration, inflight int, drop bool) {
	if rtt < windowMinRTT && !drop {
		return
	}
	x.w.add(rtt, inflight, drop)
	x.maybeFlush(now)
}

func (x *windowedGradient2) onDrop(now time.Time) {
	x.w.didDrop = true
	x.maybeFlush(now)
}

func (x *windowedGradient2) maybeFlush(now time.Time) {
	if !now.After(x.nextUpdate) {
		return
	}
	w := x.w
	x.w = percentileWindow{}
	x.nextUpdate = now.Add(windowTime)
	switch {
	case w.didDrop:
		x.g.onDrop()
	case w.ready():
		x.g.onSample(float64(w.tracked()), w.maxInflight)
	}
}

// Request reserve: a request whose piece is due within the horizon is urgent, and may exceed a
// full queue by a quarter of the limit (at least one slot), as a guaranteed partition does.
const (
	urgentHorizon      = 2 * time.Second
	urgentReserveShare = 0.25
)

func requestIsUrgent(deadlineMs int64, now time.Time) bool {
	return deadlineMs != 0 && deadlineMs <= now.Add(urgentHorizon).UnixMilli()
}

// urgentRequestCap is how many requests a peer may hold when the extra one is urgent: never past
// the peer's advertised limit or what the write buffer can carry.
func urgentRequestCap(limit int, peerMax int64) int {
	c := limit + int(math.Max(1, math.Ceil(float64(limit)*urgentReserveShare)))
	if c > maxLocalToRemoteRequests {
		c = maxLocalToRemoteRequests
	}
	if peerMax > 0 && int64(c) > peerMax {
		c = int(peerMax)
	}
	return c
}
