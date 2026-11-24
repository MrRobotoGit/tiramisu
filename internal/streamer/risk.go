package streamer

import (
	"math"
	"slices"
	"time"

	"tiramisu/internal/piecestore"
)

// Stall-risk trigger: a stream whose ready bytes would run out at the swarm's pessimistic
// goodput pauses the speculative reads (KindPrefetch) of the other torrents until it is safe again.
// Its own torrent's prefetch keeps going: there the stream's deadlines already come first.
const (
	goodputSamples    = 30 // 1s samples of the torrent's delivered goodput
	goodputMinSamples = 5
	riskQuantileLow   = 0.10             // with a thin buffer: the swarm's pessimistic tail
	riskQuantileHigh  = 0.50             // with a deep buffer: its median
	bufferLow         = 10 * time.Second // content ready at or below which riskQuantileLow holds
	bufferHigh        = 60 * time.Second // content ready at or above which riskQuantileHigh holds
	riskEnter         = 10 * time.Second // runway below which a stream is at risk
	riskExit          = 20 * time.Second // runway at or above which it is safe again
	maxRunway         = time.Hour        // "unbounded"
	riskLogEvery      = time.Minute
)

// goodput is a rolling window of the bytes the torrent's store received per second. Seconds in
// which nothing was requested are skipped: they measure a full window, not the swarm.
type goodput struct {
	ring  [goodputSamples]float64 // bytes/s
	n     int
	next  int
	at    time.Time
	bytes int64
}

func (g *goodput) observe(now time.Time, total int64, wanting bool) {
	if g.at.IsZero() || !wanting {
		g.at, g.bytes = now, total
		return
	}
	el := now.Sub(g.at)
	if el < time.Second {
		return
	}
	g.ring[g.next] = float64(total-g.bytes) / el.Seconds()
	g.next = (g.next + 1) % len(g.ring)
	g.n = min(g.n+1, len(g.ring))
	g.at, g.bytes = now, total
}

// quantile is the nearest-rank q-quantile of the window; false with too few samples.
func (g *goodput) quantile(q float64) (float64, bool) {
	if g.n < goodputMinSamples {
		return 0, false
	}
	s := slices.Clone(g.ring[:g.n])
	slices.Sort(s)
	i := max(int(math.Ceil(q*float64(len(s))))-1, 0)
	return s[i], true
}

// runway is how long ahead bytes last against consumption when the swarm delivers p (bytes/s):
// unbounded when p keeps up.
func runway(ahead int64, consumption, p float64) time.Duration {
	deficit := consumption - p
	if !(deficit > 0) {
		return maxRunway
	}
	sec := float64(ahead) / max(deficit, 1)
	if sec >= maxRunway.Seconds() {
		return maxRunway
	}
	return time.Duration(sec * float64(time.Second))
}

// riskState is the trigger with hysteresis: on below riskEnter, off again at riskExit.
type riskState struct{ on bool }

func (r *riskState) update(rw time.Duration) bool {
	switch {
	case !r.on && rw < riskEnter:
		r.on = true
	case r.on && rw >= riskExit:
		r.on = false
	}
	return r.on
}

// aheadOf is the bytes ready from the stream's position: whole readable pieces in a row (written
// or verified; verified only in STRICT), up to the readahead.
func aheadOf(ts *piecestore.TorrentStore, sp span, readahead int64) int64 {
	pl := ts.PieceLen()
	ready := piecestore.Written
	if !ts.Responsive() {
		ready = piecestore.Verified
	}
	end := min(sp.pos+readahead, sp.end)
	at := sp.pos
	for at < end {
		p := ts.Piece(pieceOf(at, pl))
		if p == nil || p.State() < ready {
			break
		}
		at = int64(p.Index()+1) * pl
	}
	return max(min(at, end)-sp.pos, 0)
}

// quantileForBuffer picks the goodput quantile from the seconds of content ready:
// pessimistic when the buffer is thin, the median when it is deep, linear in between.
func quantileForBuffer(sec float64) float64 {
	lo, hi := bufferLow.Seconds(), bufferHigh.Seconds()
	switch {
	case sec <= lo:
		return riskQuantileLow
	case sec >= hi:
		return riskQuantileHigh
	}
	return riskQuantileLow + (riskQuantileHigh-riskQuantileLow)*(sec-lo)/(hi-lo)
}

// assessRisk samples the goodput and updates the torrent's trigger from its streams' shortest
// runway; the Streamer counts the torrents at risk. Called by the pass. A stream consumes at the
// film's bitrate when known; the pump's drain follows the player's buffering and is a fallback.
func (t *Torrent) assessRisk(now time.Time, streams []*Stream, sh shares) {
	t.gp.observe(now, t.ts.Received(), len(t.local) > 0)
	rw, worst := maxRunway, (*Stream)(nil)
	var worstAhead int64
	var worstRate, worstGP, worstQ float64
	for _, s := range streams {
		rate := s.dl.consumption()
		if b := s.bitrate.Load(); b > 0 {
			rate = float64(b)
		}
		ahead := s.lead.Load() + aheadOf(t.ts, s.span(), sh.readahead)
		buffer := maxRunway.Seconds()
		if rate > 0 {
			buffer = float64(ahead) / rate
		}
		q := quantileForBuffer(buffer)
		gp, known := t.gp.quantile(q)
		if !known {
			continue
		}
		if r := runway(ahead, rate, gp); r < rw {
			rw, worst, worstAhead, worstRate, worstGP, worstQ = r, s, ahead, rate, gp, q
		}
	}
	// Asked only near the threshold: Playing walks the playback registry.
	if f := t.s.cfg.Playing; f != nil && (rw < riskEnter || t.risk.on) && !f(t.ts.Hash()) {
		rw = maxRunway
	}
	was := t.risk.on
	on := t.risk.update(rw)
	if on == was {
		return
	}
	t.riskOn.Store(on)
	t.s.addRisk(on)
	if on {
		if worst != nil && now.Sub(t.lastRiskLog) >= riskLogEvery {
			t.lastRiskLog = now
			t.s.cfg.Logf("[Streamer] Stall risk on %s: runway %.1fs (%dMB ahead, %.1f MB/s consumed, P%.0f goodput %.1f MB/s): speculative reads paused",
				t.ts.Hash(), rw.Seconds(), worstAhead>>20, worstRate/(1<<20), worstQ*100, worstGP/(1<<20))
		}
	}
}

// releaseRisk gives back the torrent's count when it stops scheduling.
func (t *Torrent) releaseRisk() {
	t.passMu.Lock()
	defer t.passMu.Unlock()
	if t.risk.on {
		t.risk.on = false
		t.riskOn.Store(false)
		t.s.addRisk(false)
	}
}

// addRisk counts a torrent entering (on) or leaving the risk state.
func (s *Streamer) addRisk(on bool) {
	s.riskMu.Lock()
	defer s.riskMu.Unlock()
	switch {
	case on:
		if s.atRisk++; s.atRisk == 1 {
			close(s.riskCh)
		}
	case s.atRisk > 0:
		if s.atRisk--; s.atRisk == 0 {
			s.riskCh = make(chan struct{})
		}
	}
}

func (s *Streamer) torrentsAtRisk() int {
	s.riskMu.Lock()
	defer s.riskMu.Unlock()
	return s.atRisk
}

// pausedCh is closed while speculative reads are paused.
func (s *Streamer) pausedCh() <-chan struct{} {
	s.riskMu.Lock()
	defer s.riskMu.Unlock()
	return s.riskCh
}

// AtRisk is how many torrents are at risk now: while it is above 0 speculative reads pause.
func (s *Streamer) AtRisk() int { return s.torrentsAtRisk() }

// othersAtRisk reports whether a torrent other than t is at risk: then t's speculative reads pause.
func (t *Torrent) othersAtRisk() bool {
	n := t.s.torrentsAtRisk()
	if t.riskOn.Load() {
		n--
	}
	return n > 0
}

// withoutPrefetch drops the speculative consumers (prefetches and helps) from a plan.
func withoutPrefetch(spans []span) []span {
	out := make([]span, 0, len(spans))
	for _, sp := range spans {
		if sp.kind != KindPrefetch && sp.kind != KindHelp {
			out = append(out, sp)
		}
	}
	return out
}
