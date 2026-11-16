package torr

import (
	"sync/atomic"
	"time"

	"tiramisu/internal/gostorm/settings"
)

// A cold start waits mostly for a peer to unchoke us: more open connections are more chances.
// The limit is raised only below coldStartConns (the Pi 4 ceiling) and restored once data flows.
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

// coldBoostLimit returns the cold-start limit for a configured limit, and whether it raises it.
func coldBoostLimit(configured int) (int, bool) {
	if configured < coldStartConns {
		return coldStartConns, true
	}
	return configured, false
}

// coldBoostOver reports whether the boost has done its job or run out of time.
func coldBoostOver(useful, base int64, age time.Duration) bool {
	return useful-base >= coldBoostEndBytes || age >= coldBoostMaxAge
}

// BoostColdStart raises the connection limit once per activation, until endColdBoost restores it.
func (t *Torrent) BoostColdStart() {
	if t == nil || t.Torrent == nil || settings.BTsets == nil {
		return
	}
	limit, raise := coldBoostLimit(settings.BTsets.ConnectionsLimit)
	if !raise || !t.coldBoost.state.CompareAndSwap(coldBoostIdle, coldBoostActive) {
		return
	}
	st := t.Torrent.Stats()
	t.coldBoost.baseBytes.Store(st.BytesReadUsefulData.Int64())
	t.coldBoost.startNano.Store(time.Now().UnixNano())
	t.Torrent.SetMaxEstablishedConns(limit)
}

// endColdBoost restores the configured limit; UpdateStats calls it every second.
func (t *Torrent) endColdBoost(useful int64) {
	if t.coldBoost.state.Load() != coldBoostActive {
		return
	}
	start := t.coldBoost.startNano.Load()
	if start == 0 || !coldBoostOver(useful, t.coldBoost.baseBytes.Load(), time.Since(time.Unix(0, start))) {
		return
	}
	if !t.coldBoost.state.CompareAndSwap(coldBoostActive, coldBoostDone) {
		return
	}
	if t.Torrent != nil && settings.BTsets != nil {
		t.Torrent.SetMaxEstablishedConns(settings.BTsets.ConnectionsLimit)
	}
}
