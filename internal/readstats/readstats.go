// Package readstats counts FUSE reads by source and their latency, for /metrics/profiling.
package readstats

import (
	"sync"
	"time"
)

// ReadTiming collects per-read latency metrics for profiling.
type ReadTiming struct {
	StartTime          time.Time
	MetadataLookupTime time.Duration
	HTTPFetchTime      time.Duration
	CacheHitTime       time.Duration
	TotalTime          time.Duration
	BytesRead          int
	IsStreaming        bool
	UsedCache          bool
}

// Stats accumulates read counts and latencies by source.
type Stats struct {
	mu                 sync.RWMutex
	TotalReads         int64
	CacheHits          int64
	HTTPFetches        int64
	AvgHTTPLatency     time.Duration
	AvgCacheLatency    time.Duration
	AvgMetadataLatency time.Duration
	StreamingReads     int64
	NonStreamingReads  int64
}

// RecordRead adds one read to the statistics.
func (ps *Stats) RecordRead(t *ReadTiming) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.TotalReads++

	if t.UsedCache {
		oldCount := ps.CacheHits
		ps.CacheHits++
		// Update average cache latency: (avg * old + new) / new
		ps.AvgCacheLatency = time.Duration((int64(ps.AvgCacheLatency)*oldCount + int64(t.CacheHitTime)) / ps.CacheHits)
	} else {
		oldCount := ps.HTTPFetches
		ps.HTTPFetches++
		// Update average HTTP latency: (avg * old + new) / new
		ps.AvgHTTPLatency = time.Duration((int64(ps.AvgHTTPLatency)*oldCount + int64(t.HTTPFetchTime)) / ps.HTTPFetches)
	}

	if t.IsStreaming {
		ps.StreamingReads++
	} else {
		ps.NonStreamingReads++
	}
}

// Stats returns current profiling statistics
func (ps *Stats) Stats() (totalReads, cacheHits, httpFetches, streamingReads int64, avgHTTPLatency, avgCacheLatency time.Duration, cacheHitRate float64) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	cacheHitRate = 0.0
	if ps.TotalReads > 0 {
		cacheHitRate = float64(ps.CacheHits) / float64(ps.TotalReads) * 100.0
	}

	return ps.TotalReads, ps.CacheHits, ps.HTTPFetches, ps.StreamingReads,
		ps.AvgHTTPLatency, ps.AvgCacheLatency, cacheHitRate
}
