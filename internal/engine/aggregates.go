package engine

import "tiramisu/internal/engine/bt"

// PeerTransportStats aggregates the connected swarm of every hydrated torrent by transport.
type PeerTransportStats struct {
	ConnsTCP     int     `json:"conns_tcp"`
	ConnsUTP     int     `json:"conns_utp"`
	RatedTCP     int     `json:"rated_tcp"`
	RatedUTP     int     `json:"rated_utp"`
	UsefulBpsTCP float64 `json:"useful_bps_tcp"`
	UsefulBpsUTP float64 `json:"useful_bps_utp"`
	EjectTotal   int64   `json:"eject_total"`
	EjectUTP     int64   `json:"eject_utp"`
	ChurnTotal   int64   `json:"churn_total"`
	ChurnUTP     int64   `json:"churn_utp"`
}

// Gradient2Stats aggregates the closed-loop limiters of every hydrated torrent.
type Gradient2Stats = bt.Gradient2Stats

func (e *Engine) CollectPeerTransportStats() (s PeerTransportStats) {
	for _, t := range e.ListHydrated() {
		snap := t.bt.PeerTransportStats()
		s.ConnsTCP += snap.ConnsTCP
		s.ConnsUTP += snap.ConnsUTP
		s.RatedTCP += snap.RatedTCP
		s.RatedUTP += snap.RatedUTP
		s.UsefulBpsTCP += snap.UsefulBpsTCP
		s.UsefulBpsUTP += snap.UsefulBpsUTP
		s.EjectTotal += t.bt.PeerEjectCount()
		s.EjectUTP += t.bt.PeerEjectCountUTP()
		churn, churnUTP := t.bt.PeerChurnCounts()
		s.ChurnTotal += churn
		s.ChurnUTP += churnUTP
	}
	return s
}

func (e *Engine) CollectGradient2Stats() (s Gradient2Stats) {
	for _, t := range e.ListHydrated() {
		snap := t.bt.Gradient2Stats()
		if snap.Peers == 0 {
			continue
		}
		if s.Peers == 0 || snap.LimitMin < s.LimitMin {
			s.LimitMin = snap.LimitMin
		}
		s.LimitMax = max(s.LimitMax, snap.LimitMax)
		s.Peers += snap.Peers
		s.BackingOff += snap.BackingOff
	}
	return s
}

// HedgeTotals sums the hedge counters /metrics reports.
func (e *Engine) HedgeTotals() (triggers int64, deadlinePieces int, circuitOpen bool) {
	for _, t := range e.ListHydrated() {
		triggers += t.bt.HedgeTriggerCount()
		deadlinePieces += t.bt.PieceDeadlineCount()
		circuitOpen = circuitOpen || t.bt.HedgeCircuitOpen()
	}
	return
}

// V304BannedCount counts the fork's banned IPs, persisted bans included.
func (e *Engine) V304BannedCount() int { return bt.V304BannedCount() }

// SetV304Bans seeds persisted bans and installs the new-ban callback. Called after Start, once
// the state DB is open (as before the switch): a ban made earlier is not persisted.
func SetV304Bans(persisted []string, onBan func(ip string)) {
	bt.V304LoadBans(persisted)
	bt.V304SetOnBan(onBan)
}
