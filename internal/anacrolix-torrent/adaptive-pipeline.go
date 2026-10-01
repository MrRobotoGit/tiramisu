package torrent

import (
	"expvar"
	"os"
	"time"
)

// adaptivePipelineEnvKey overrides ClientConfig.AdaptivePipeline for clients built from
// NewDefaultClientConfig. The pipeline is on by default in code, so no deployment needs to set
// anything; the variable exists only to switch it off for a comparison run without rebuilding.
const adaptivePipelineEnvKey = "TORRENT_ADAPTIVE_PIPELINE"

// defaultAdaptivePipeline is on ahead of confirmation: on the same swarm it cut duplicate blocks
// (D 11.3% -> C 7.6%) but at ~1.1 sigma with n=1, and the pre-registered criterion
// (C.stall/GB <= D.stall/GB) was not met on the single run (5.4 vs 5.0), nor determined at n=1.
// The effect is not yet demonstrated. An interleaved D/C repeat is the test that settles it; flip
// this to false if that repeat fails, and do not read this default as a measured result.
const defaultAdaptivePipeline = true

var adaptivePipelineEffective = expvar.NewString("adaptivePipeline")

func adaptivePipelineFromEnv() bool {
	on := defaultAdaptivePipeline
	switch os.Getenv(adaptivePipelineEnvKey) {
	case "0", "false":
		on = false
	case "1", "true":
		on = true
	}
	if on {
		adaptivePipelineEffective.Set("on")
	} else {
		adaptivePipelineEffective.Set("off")
	}
	return on
}

const (
	// minLatencyWindow bounds how long a low latency sample is trusted: a path that got slower
	// must not keep the pipeline sized for a latency it no longer has.
	minLatencyWindow = 30 * time.Second
	// The pipeline keeps two latency-products of data in flight, as BBR does, between a floor
	// that keeps short paths busy and the fixed 2s target this replaces.
	adaptiveLatencyGain = 2
	adaptiveTargetFloor = 500 * time.Millisecond
	fixedPipelineTarget = 2 * time.Second
)

// adaptiveTargetLatency is the queue target for a peer whose minimum request latency is minLat.
// Adapted from TCP Vegas/BBR as used for concurrency limits (Netflix): queue only what keeps the
// path busy, instead of a fixed amount of time.
func adaptiveTargetLatency(minLat time.Duration, ok bool) time.Duration {
	if !ok {
		return fixedPipelineTarget
	}
	target := adaptiveLatencyGain * minLat
	if target < adaptiveTargetFloor {
		return adaptiveTargetFloor
	}
	if target > fixedPipelineTarget {
		return fixedPipelineTarget
	}
	return target
}

// windowedMin tracks the minimum of samples over the current and the previous window.
type windowedMin struct {
	cur, prev latencyBucket
}

type latencyBucket struct {
	start time.Time
	min   time.Duration
}

func (b latencyBucket) live(now time.Time) bool {
	return !b.start.IsZero() && now.Sub(b.start) < 2*minLatencyWindow
}

func (w *windowedMin) add(now time.Time, d time.Duration) {
	if w.cur.start.IsZero() || now.Sub(w.cur.start) >= minLatencyWindow {
		if w.cur.live(now) {
			w.prev = w.cur
		} else {
			w.prev = latencyBucket{}
		}
		w.cur = latencyBucket{start: now, min: d}
		return
	}
	if d < w.cur.min {
		w.cur.min = d
	}
}

func (w *windowedMin) get(now time.Time) (time.Duration, bool) {
	var m time.Duration
	ok := false
	for _, b := range []latencyBucket{w.cur, w.prev} {
		if b.live(now) && (!ok || b.min < m) {
			m, ok = b.min, true
		}
	}
	return m, ok
}
