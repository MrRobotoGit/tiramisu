package torr

// PeerTransportStats aggregates the connected swarm across all active torrents, split by
// transport. It answers whether uTP peers earn the connection slots they hold: slots are capped
// by ConnectionsLimit, so a transport's share of connections is only worth its share of useful
// bytes.
type PeerTransportStats struct {
	ConnsTCP int `json:"conns_tcp"`
	ConnsUTP int `json:"conns_utp"`
	// Connections with a seeded throughput EWMA, i.e. the ones the Bps figures cover. The EWMA
	// is only sampled while the hedge watchdog runs (warmup or playback pressure), so outside
	// those windows the rates go stale and these counts fall behind the conn counts.
	RatedTCP     int     `json:"rated_tcp"`
	RatedUTP     int     `json:"rated_utp"`
	UsefulBpsTCP float64 `json:"useful_bps_tcp"`
	UsefulBpsUTP float64 `json:"useful_bps_utp"`
	// Proactive drops since startup. The UTP fields are subsets of their totals.
	EjectTotal int64 `json:"eject_total"`
	EjectUTP   int64 `json:"eject_utp"`
	ChurnTotal int64 `json:"churn_total"`
	ChurnUTP   int64 `json:"churn_utp"`
}

// CollectPeerTransportStats sums the per-transport peer view over every active torrent.
func CollectPeerTransportStats() (s PeerTransportStats) {
	for _, tr := range ListActiveTorrent() {
		if tr == nil || tr.Torrent == nil {
			continue
		}
		snap := tr.Torrent.PeerTransportStats()
		s.ConnsTCP += snap.ConnsTCP
		s.ConnsUTP += snap.ConnsUTP
		s.RatedTCP += snap.RatedTCP
		s.RatedUTP += snap.RatedUTP
		s.UsefulBpsTCP += snap.UsefulBpsTCP
		s.UsefulBpsUTP += snap.UsefulBpsUTP
		s.EjectTotal += tr.Torrent.PeerEjectCount()
		s.EjectUTP += tr.Torrent.PeerEjectCountUTP()
		churn, churnUTP := tr.Torrent.PeerChurnCounts()
		s.ChurnTotal += churn
		s.ChurnUTP += churnUTP
	}
	return
}
