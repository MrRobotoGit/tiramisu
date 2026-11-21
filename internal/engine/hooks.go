package engine

import (
	"sync/atomic"

	"tiramisu/internal/engine/bt"
)

// Fork hooks Tiramisu calls on a hydrated torrent (engine.md §4); no-ops on DB-only views.
type (
	PieceWindowState      = bt.PieceWindowState
	PeerTransportSnapshot = bt.PeerTransportSnapshot
)

func (t *Torrent) SetWarmupActive(active bool, fileID int) {
	if t.bt != nil {
		t.bt.SetWarmupActive(active, fileID)
	}
}

func (t *Torrent) SetPlaybackPressure(active bool, offset int64) {
	if t.bt != nil {
		t.bt.SetPlaybackPressure(active, offset)
	}
}

func (t *Torrent) PieceWindow(first, n int) []PieceWindowState {
	if t.bt == nil {
		return nil
	}
	return t.bt.PieceWindow(first, n)
}

// AnnounceTracked forces a re-announce (the tuner, on a weak swarm).
func (t *Torrent) AnnounceTracked(onDone func(peers, errs, total int, failures []string)) {
	if t.bt != nil {
		t.bt.AnnounceTracked(onDone)
	}
}

func (t *Torrent) HedgeTriggerCount() int64 {
	if t.bt == nil {
		return 0
	}
	return t.bt.HedgeTriggerCount()
}

func (t *Torrent) PieceDeadlineCount() int {
	if t.bt == nil {
		return 0
	}
	return t.bt.PieceDeadlineCount()
}

func (t *Torrent) HedgeCircuitOpen() bool { return t.bt != nil && t.bt.HedgeCircuitOpen() }

// Private reports the info's private flag (the tuner skips private swarms).
func (t *Torrent) Private() bool { return t.bt != nil && t.bt.Private() }

// aggressive is the storage aggressive mode request; the streamer (plan 3) reads it.
type aggressive struct {
	on    atomic.Bool
	limit atomic.Int32
}

// SetAggressiveMode records the request; without metadata it does nothing, as today.
func (t *Torrent) SetAggressiveMode(enabled bool, masterLimit int) {
	if t.bt == nil || t.Store() == nil {
		return
	}
	t.aggr.limit.Store(int32(masterLimit))
	t.aggr.on.Store(enabled)
}

// AggressiveMode returns the last request.
func (t *Torrent) AggressiveMode() (bool, int) { return t.aggr.on.Load(), int(t.aggr.limit.Load()) }
