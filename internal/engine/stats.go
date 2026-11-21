package engine

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// torrentStats is the per-torrent state the tick maintains.
type torrentStats struct {
	mu       sync.Mutex
	lastAt   time.Time
	lastRead int64
	lastSent int64

	down, up atomic.Uint64 // float64 bits, bytes/s
}

func (s *torrentStats) speeds() (down, up float64) {
	return math.Float64frombits(s.down.Load()), math.Float64frombits(s.up.Load())
}

// updateStats computes speeds from the total bytes read and written (wasted and protocol
// bytes included, as today) and ends cold boosts.
func (e *Engine) updateStats() {
	now := e.now()
	for _, t := range e.ListHydrated() {
		st := t.bt.Stats()
		s := &t.stats
		s.mu.Lock()
		if dt := now.Sub(s.lastAt).Seconds(); !s.lastAt.IsZero() && dt > 0 {
			s.down.Store(math.Float64bits(float64(max(st.BytesRead-s.lastRead, 0)) / dt))
			s.up.Store(math.Float64bits(float64(max(st.BytesWritten-s.lastSent, 0)) / dt))
		}
		s.lastAt, s.lastRead, s.lastSent = now, st.BytesRead, st.BytesWritten
		s.mu.Unlock()
		t.endColdBoost(st.BytesReadUsefulData)
	}
}

// Status is the torrent status JSON; keys are the ones consumers read today.
type Status struct {
	Title            string     `json:"title"`
	Category         string     `json:"category,omitempty"`
	Poster           string     `json:"poster,omitempty"`
	Data             string     `json:"data,omitempty"`
	Timestamp        int64      `json:"timestamp"`
	Name             string     `json:"name,omitempty"`
	Hash             string     `json:"hash"`
	Stat             State      `json:"stat"`
	StatString       string     `json:"stat_string"`
	LoadedSize       int64      `json:"loaded_size,omitempty"`
	TorrentSize      int64      `json:"torrent_size,omitempty"`
	PreloadedBytes   int64      `json:"preloaded_bytes,omitempty"`
	CacheCapacity    int64      `json:"cache_capacity,omitempty"` // the piece cache PreloadedBytes fills
	DownloadSpeed    float64    `json:"download_speed,omitempty"`
	UploadSpeed      float64    `json:"upload_speed,omitempty"`
	TotalPeers       int        `json:"total_peers,omitempty"`
	PendingPeers     int        `json:"pending_peers,omitempty"`
	ActivePeers      int        `json:"active_peers,omitempty"`
	ConnectedSeeders int        `json:"connected_seeders,omitempty"`
	HalfOpenPeers    int        `json:"half_open_peers,omitempty"`
	BytesWritten     int64      `json:"bytes_written,omitempty"`
	BytesWrittenData int64      `json:"bytes_written_data,omitempty"`
	BytesRead        int64      `json:"bytes_read,omitempty"`
	BytesReadData    int64      `json:"bytes_read_data,omitempty"`
	BytesReadUseful  int64      `json:"bytes_read_useful_data,omitempty"`
	ChunksWritten    int64      `json:"chunks_written,omitempty"`
	ChunksRead       int64      `json:"chunks_read,omitempty"`
	ChunksReadUseful int64      `json:"chunks_read_useful,omitempty"`
	ChunksReadWasted int64      `json:"chunks_read_wasted,omitempty"`
	PiecesDirtyGood  int64      `json:"pieces_dirtied_good,omitempty"`
	PiecesDirtyBad   int64      `json:"pieces_dirtied_bad,omitempty"`
	IsPriority       bool       `json:"is_priority"`
	PeerRates        []PeerRate `json:"peer_rates,omitempty"`
	FileStats        []File     `json:"file_stats,omitempty"`
}

// Status builds the full status; DB-only torrents carry metadata and state only.
func (t *Torrent) Status() Status {
	m := t.Meta()
	st := State(t.state.Load())
	s := Status{Title: m.Title, Category: m.Category, Poster: m.Poster, Data: m.Data, Timestamp: t.Timestamp(),
		Hash: t.hash, Stat: st, StatString: st.String(), TorrentSize: t.Size(), IsPriority: t.IsPriority.Load()}
	if t.bt == nil {
		return s
	}
	s.Name = t.bt.Name()
	if ts := t.Store(); ts != nil {
		s.PreloadedBytes, s.CacheCapacity = ts.Filled(), ts.Capacity()
	}
	s.DownloadSpeed, s.UploadSpeed = t.stats.speeds()
	if t.bt.HasInfo() {
		s.LoadedSize = t.bt.BytesCompleted()
		s.FileStats = t.Files()
	}
	b := t.bt.Stats()
	s.TotalPeers, s.PendingPeers, s.ActivePeers, s.ConnectedSeeders, s.HalfOpenPeers = b.TotalPeers, b.PendingPeers, b.ActivePeers, b.ConnectedSeeders, b.HalfOpenPeers
	s.BytesWritten, s.BytesWrittenData, s.BytesRead, s.BytesReadData, s.BytesReadUseful = b.BytesWritten, b.BytesWrittenData, b.BytesRead, b.BytesReadData, b.BytesReadUsefulData
	s.ChunksWritten, s.ChunksRead, s.ChunksReadUseful, s.ChunksReadWasted = b.ChunksWritten, b.ChunksRead, b.ChunksReadUseful, b.ChunksReadWasted
	s.PiecesDirtyGood, s.PiecesDirtyBad = b.PiecesDirtiedGood, b.PiecesDirtiedBad
	s.PeerRates = t.rates.sample(t.bt.PeerSnapshots(), t.e.now(), t.e.peerInfo())
	return s
}

// HighFreq is the short status the tuner polls (V162).
type HighFreq struct {
	Hash             string
	DownloadSpeed    float64
	IsPriority       bool
	TotalPeers       int
	ActivePeers      int
	ConnectedSeeders int
	LoadedSize       int64
}

// StatHighFreq is nil for a DB-only torrent.
func (t *Torrent) StatHighFreq() *HighFreq {
	if t.bt == nil {
		return nil
	}
	b := t.bt.Stats()
	down, _ := t.stats.speeds()
	h := &HighFreq{Hash: t.hash, DownloadSpeed: down, IsPriority: t.IsPriority.Load(),
		TotalPeers: b.TotalPeers, ActivePeers: b.ActivePeers, ConnectedSeeders: b.ConnectedSeeders}
	if t.bt.HasInfo() {
		h.LoadedSize = t.bt.BytesCompleted()
	}
	return h
}

// ActivePeers is the swarm liveness measure: never TotalPeers/PendingPeers, which count the
// phantom peers injected from the stored list.
func (t *Torrent) ActivePeers() int {
	if t.bt == nil {
		return 0
	}
	return t.bt.Stats().ActivePeers
}

// ListStreaming lists the hydrated torrents being streamed: consumers held or data flowing.
func (e *Engine) ListStreaming() []*Torrent {
	var out []*Torrent
	for _, t := range e.ListHydrated() {
		if down, _ := t.stats.speeds(); t.Consumers() > 0 || down > 0 {
			out = append(out, t)
		}
	}
	return out
}

func (e *Engine) peerInfo() PeerInfoFunc {
	if p := e.peerInfoFn.Load(); p != nil {
		return *p
	}
	return nil
}

// SetPeerInfo installs the GeoIP hook of the swarm view; nil disables it.
func (e *Engine) SetPeerInfo(f PeerInfoFunc) {
	if f == nil {
		e.peerInfoFn.Store(nil)
		return
	}
	e.peerInfoFn.Store(&f)
}
