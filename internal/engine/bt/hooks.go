package bt

import "github.com/anacrolix/torrent"

// Fork hooks used by Tiramisu (engine.md §4); the types are the fork's plain structs.
type (
	PieceWindowState      = torrent.PieceWindowState
	Gradient2Stats        = torrent.Gradient2Stats
	PeerTransportSnapshot = torrent.PeerTransportSnapshot
)

func (t *Torrent) SetWarmupActive(active bool, fileID int)    { t.t.SetWarmupActive(active, fileID) }
func (t *Torrent) SetPlaybackPressure(active bool, off int64) { t.t.SetPlaybackPressure(active, off) }
func (t *Torrent) PieceWindow(first, n int) []PieceWindowState {
	return t.t.PieceWindow(first, n)
}
func (t *Torrent) AnnounceTracked(onDone func(peers, errs, total int, failures []string)) {
	t.t.AnnounceTracked(onDone)
}
func (t *Torrent) HedgeTriggerCount() int64                  { return t.t.HedgeTriggerCount() }
func (t *Torrent) PieceDeadlineCount() int                   { return t.t.PieceDeadlineCount() }
func (t *Torrent) HedgeCircuitOpen() bool                    { return t.t.HedgeCircuitOpen() }
func (t *Torrent) Gradient2Stats() Gradient2Stats            { return t.t.Gradient2Stats() }
func (t *Torrent) PeerTransportStats() PeerTransportSnapshot { return t.t.PeerTransportStats() }
func (t *Torrent) PeerEjectCount() int64                     { return t.t.PeerEjectCount() }
func (t *Torrent) PeerEjectCountUTP() int64                  { return t.t.PeerEjectCountUTP() }
func (t *Torrent) PeerChurnCounts() (total, utp int64)       { return t.t.PeerChurnCounts() }

// V304 bans are process-wide in the fork.
func V304BannedCount() int           { return torrent.V304BannedCount() }
func V304SetOnBan(f func(ip string)) { torrent.V304SetOnBan(f) }
func V304LoadBans(ips []string)      { torrent.V304LoadBans(ips) }
