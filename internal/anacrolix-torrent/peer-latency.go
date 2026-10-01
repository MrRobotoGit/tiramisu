package torrent

import "time"

// The per-peer mean delivery latency (request to chunk) is sampled on every satisfied request,
// independent of which queue controller is active (Gradient2, the adaptive pipeline, or neither).
// It is the peer's typical time to deliver, which the per-peer steal grace reads. The window and
// warmup match Gradient2's long-RTT baseline, so the two averages are directly comparable.
const (
	peerMeanLatencyWindow = gradient2LongWindow
	peerMeanLatencyWarmup = gradient2Warmup
)

// requestMeanLatencyAvg returns the peer's mean-latency measurement, creating it on first use.
// Call under t.cl's lock.
func (cn *Peer) requestMeanLatencyAvg() *expAvgMeasurement {
	if cn.requestMeanLatency == nil {
		cn.requestMeanLatency = newExpAvgMeasurement(peerMeanLatencyWindow, peerMeanLatencyWarmup)
	}
	return cn.requestMeanLatency
}

// recordMeanLatency feeds one request-to-chunk latency sample into the peer's mean. Call under
// t.cl's lock.
func (cn *Peer) recordMeanLatency(d time.Duration) {
	cn.requestMeanLatencyAvg().add(float64(d))
}

// meanLatency returns the peer's mean request-to-chunk latency and whether it has seen enough
// samples to be representative. Call under t.cl's lock.
func (cn *Peer) meanLatency() (time.Duration, bool) {
	if cn.requestMeanLatency == nil || !cn.requestMeanLatency.ready() {
		return 0, false
	}
	return time.Duration(cn.requestMeanLatency.get()), true
}
