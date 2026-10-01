package torrent

import (
	"expvar"
	"math"
	"os"
)

// gradient2EnvKey overrides ClientConfig.Gradient2 for clients built from NewDefaultClientConfig.
// On by default; TORRENT_GRADIENT2=0 switches it off for a comparison run.
const gradient2EnvKey = "TORRENT_GRADIENT2"

// defaultGradient2 is on by decision, not by a paired measurement: it gave the lowest duplicate
// blocks in the sessions run (4.2-4.4%), its stalls were not conclusive (one session worse than
// the alternatives), and the peak rate observed stayed near 32 MB/s, attributed to the
// 32-request cap per peer.
const defaultGradient2 = true

var gradient2Effective = expvar.NewString("gradient2")

func gradient2FromEnv() bool {
	on := defaultGradient2
	switch os.Getenv(gradient2EnvKey) {
	case "0", "false":
		on = false
	case "1", "true":
		on = true
	}
	if on {
		gradient2Effective.Set("on")
	} else {
		gradient2Effective.Set("off")
	}
	return on
}

// This file ports Netflix's Gradient2 concurrency limit
// (com.netflix.concurrency.limits.limit.Gradient2Limit, Apache-2.0) so a peer's request queue is
// sized by a closed feedback loop instead of a fixed bandwidth-delay product. Unlike the minimum
// RTT used elsewhere, Gradient2 averages, because per-request latency is bursty and a minimum
// biases the baseline impractically low. The limit is recomputed per sample as
//
//	gradient = clamp[0.5, 1.0](rttTolerance * longRTT / shortRTT)   // 1.0 means no queuing
//	newLimit = gradient*limit + queueSize
//	newLimit = limit*(1-smoothing) + newLimit*smoothing
//	newLimit = clamp[minLimit, maxLimit](newLimit)
//
// The algorithm parameters follow the Java reference (rttTolerance 1.5, long-RTT decay 0.95), not
// the Go port (which drops the tolerance and decays by 0.9). The bounds are the engine's, not
// Netflix's server defaults: a torrent peer should not be forced to hold 20 requests in flight.
const (
	gradient2QueueSize    = 4
	gradient2Smoothing    = 0.2
	gradient2LongWindow   = 600
	gradient2Warmup       = 10
	gradient2RTTTolerance = 1.5

	// Engine bounds: start where the current pipeline starts, allow shallow peers to be shallow,
	// and keep the ceiling near the depth the queue actually lives at. Netflix's 200 is a server
	// concurrency bound; here a high ceiling lets Gradient2 grow the pipeline on a low-latency
	// swarm, which measured as more overlap and more steals/cancels than the adaptive target.
	gradient2EngineInitial = 16
	gradient2EngineMin     = 2
	gradient2EngineMax     = 32
)

// gradient2 is the per-peer limit state. Not safe for concurrent use: call under t.cl's lock.
type gradient2 struct {
	estimatedLimit float64
	shortRTT       float64
	longRTT        *expAvgMeasurement
	minLimit       int
	maxLimit       int
}

func newGradient2(initial, minLimit, maxLimit int) *gradient2 {
	return &gradient2{
		estimatedLimit: float64(initial),
		minLimit:       minLimit,
		maxLimit:       maxLimit,
		longRTT:        newExpAvgMeasurement(gradient2LongWindow, gradient2Warmup),
	}
}

func (g *gradient2) limit() int {
	return int(g.estimatedLimit)
}

// onSample is Gradient2Limit._update with the fixed queueSize(limit)=4 and the didDrop argument
// unused, exactly as the reference. It updates the long-RTT baseline even when app-limited, then
// leaves the limit untouched if the peer is not being pushed: growing it there would measure
// demand, not capacity. rtt units cancel in the gradient ratio.
func (g *gradient2) onSample(rtt float64, inflight int) int {
	if rtt <= 0 {
		return g.limit()
	}
	g.shortRTT = rtt
	longRTT := g.longRTT.add(rtt)

	// Latency returned to normal after a prolonged overload: pull the baseline down without
	// waiting for the slow exponential smoothing.
	if longRTT/g.shortRTT > 2 {
		g.longRTT.update(func(v float64) float64 { return v * 0.95 })
	}

	// App-limited: do not grow the limit. The reference compares against the pre-update longRTT.
	if float64(inflight) < g.estimatedLimit/2 {
		return g.limit()
	}

	gradient := math.Max(0.5, math.Min(1.0, gradient2RTTTolerance*longRTT/g.shortRTT))
	newLimit := g.estimatedLimit*gradient + gradient2QueueSize
	newLimit = g.estimatedLimit*(1-gradient2Smoothing) + newLimit*gradient2Smoothing
	newLimit = math.Max(float64(g.minLimit), math.Min(float64(g.maxLimit), newLimit))
	g.estimatedLimit = newLimit
	return g.limit()
}

// clampGradient2Limit caps the estimated limit by what the peer and the write buffer can hold,
// and floors it at the engine minimum. PeerMaxRequests is the peer-advertised limit.
func clampGradient2Limit(limit int, peerMax int64) int {
	cap := gradient2EngineMax
	if maxLocalToRemoteRequests < cap {
		cap = maxLocalToRemoteRequests
	}
	if peerMax > 0 && peerMax < int64(cap) {
		cap = int(peerMax)
	}
	if limit > cap {
		return cap
	}
	if limit < gradient2EngineMin {
		return gradient2EngineMin
	}
	return limit
}

// expAvgMeasurement is Netflix's ExpAvgMeasurement: a simple average for the first warmupWindow
// samples, then an exponential average with factor 2/(window+1).
type expAvgMeasurement struct {
	value        float64
	sum          float64
	count        int
	window       int
	warmupWindow int
}

func newExpAvgMeasurement(window, warmupWindow int) *expAvgMeasurement {
	return &expAvgMeasurement{window: window, warmupWindow: warmupWindow}
}

func (m *expAvgMeasurement) add(sample float64) float64 {
	if m.count < m.warmupWindow {
		m.count++
		m.sum += sample
		m.value = m.sum / float64(m.count)
	} else {
		factor := 2.0 / float64(m.window+1)
		m.value = m.value*(1-factor) + sample*factor
	}
	return m.value
}

func (m *expAvgMeasurement) get() float64 { return m.value }

func (m *expAvgMeasurement) update(f func(float64) float64) { m.value = f(m.value) }
