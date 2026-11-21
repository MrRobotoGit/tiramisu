package engine

import (
	"sync/atomic"
	"time"
)

// A cold start waits mostly for a peer to unchoke us: more connections are more chances. The
// limit is raised only below coldStartConns (the Pi 4 ceiling) and restored once data flows.
const (
	coldStartConns    = 35
	coldBoostEndBytes = 1 << 20
	coldBoostMaxAge   = 30 * time.Second
)

const (
	coldBoostIdle int32 = iota
	coldBoostActive
	coldBoostDone
)

type coldBoost struct {
	state     atomic.Int32
	startNano atomic.Int64
	baseBytes atomic.Int64
}

func coldBoostLimit(configured int) (int, bool) {
	if configured < coldStartConns {
		return coldStartConns, true
	}
	return configured, false
}

func coldBoostOver(useful, base int64, age time.Duration) bool {
	return useful-base >= coldBoostEndBytes || age >= coldBoostMaxAge
}

// BoostColdStart raises the connection limit once per activation (the first player read past
// the header); the tick restores it after 1 MiB of useful data or 30s.
func (t *Torrent) BoostColdStart() {
	if t.bt == nil {
		return
	}
	limit, raise := coldBoostLimit(t.e.config().ConnectionsLimit)
	if !raise || !t.cold.state.CompareAndSwap(coldBoostIdle, coldBoostActive) {
		return
	}
	t.cold.baseBytes.Store(t.bt.Stats().BytesReadUsefulData)
	t.cold.startNano.Store(t.e.now().UnixNano())
	t.bt.SetMaxConns(limit)
}

func (t *Torrent) endColdBoost(useful int64) {
	if t.cold.state.Load() != coldBoostActive {
		return
	}
	start := t.cold.startNano.Load()
	if !coldBoostOver(useful, t.cold.baseBytes.Load(), t.e.now().Sub(time.Unix(0, start))) {
		return
	}
	if t.cold.state.CompareAndSwap(coldBoostActive, coldBoostDone) {
		t.bt.SetMaxConns(t.e.config().ConnectionsLimit)
	}
}
