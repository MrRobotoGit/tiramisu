package catalog

import (
	"expvar"
	"sync"
	"sync/atomic"
)

// Passive egress instrumentation. It changes no behaviour: it only records what a concurrency
// limiter would need to decide (in-flight pressure and failure mix per host), so the limiter is
// added only if there is actually something to limit. Published as expvar "catalogEgress" for
// the /debug/vars endpoint.
type hostStat struct {
	requests    atomic.Int64
	inflight    atomic.Int64
	maxInflight atomic.Int64
	retries     atomic.Int64
	ok2xx       atomic.Int64
	err4xx      atomic.Int64
	err429      atomic.Int64
	err5xx      atomic.Int64
	failures    atomic.Int64 // network errors and timeouts
}

var egressStats sync.Map // host -> *hostStat

func hostStatFor(host string) *hostStat {
	if v, ok := egressStats.Load(host); ok {
		return v.(*hostStat)
	}
	s := &hostStat{}
	actual, _ := egressStats.LoadOrStore(host, s)
	return actual.(*hostStat)
}

func (s *hostStat) observeInflight(cur int64) {
	for {
		max := s.maxInflight.Load()
		if cur <= max || s.maxInflight.CompareAndSwap(max, cur) {
			return
		}
	}
}

func init() {
	expvar.Publish("catalogEgress", expvar.Func(func() interface{} {
		out := map[string]map[string]int64{}
		egressStats.Range(func(k, v interface{}) bool {
			s := v.(*hostStat)
			out[k.(string)] = map[string]int64{
				"requests":     s.requests.Load(),
				"inflight":     s.inflight.Load(),
				"max_inflight": s.maxInflight.Load(),
				"retries":      s.retries.Load(),
				"ok_2xx":       s.ok2xx.Load(),
				"err_4xx":      s.err4xx.Load(),
				"err_429":      s.err429.Load(),
				"err_5xx":      s.err5xx.Load(),
				"failures":     s.failures.Load(),
			}
			return true
		})
		return out
	}))
}
