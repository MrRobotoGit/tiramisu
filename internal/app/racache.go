package app

import (
	"sync/atomic"
	"tiramisu/internal/streampolicy"

	"tiramisu/internal/racache"
	"tiramisu/internal/warmup"
)

// newRaCache builds the read-ahead cache on the process's config, logger and open handles.
func newRaCache() *racache.Cache {
	return racache.New(racache.Deps{
		Config:     gc,
		Logf:       logger.Printf,
		Go:         safeGo,
		Responsive: func() bool { return eng.Responsive() },
		Readers:    handleReaders,
	})
}

// handleReaders lists the open handles on path; reads only atomics, safe under a shard lock.
func handleReaders(path string) []racache.Reader {
	var rs []racache.Reader
	activeHandles.Range(func(key, _ interface{}) bool {
		h, ok := key.(*MkvHandle)
		if ok && h.path == path {
			off := atomic.LoadInt64(&h.lastOff)
			rs = append(rs, racache.Reader{Off: off, Primary: h.isPrimaryHandle.Load(), Tail: streampolicy.ShouldServeTailFromSSD(off, h.size, warmup.TailWarmupSize)})
		}
		return true
	})
	return rs
}
