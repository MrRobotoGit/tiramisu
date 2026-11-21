package streamer

import (
	"sync"
	"sync/atomic"
	"time"

	"tiramisu/internal/piecestore"
)

const (
	parkAfter      = 60 * time.Second
	evictThrottle  = time.Second
	evictPressure  = 90 // percent of the cache: above it every pass scans
	sweepEvery     = 5 * time.Second
	strandedIdle   = 20 // seconds
	starvedLogStep = time.Minute
)

// Streamer owns one scheduler per open torrent store.
type Streamer struct {
	cfg Config

	mu   sync.Mutex
	tors map[*piecestore.TorrentStore]*Torrent
	stop chan struct{}
	wg   sync.WaitGroup
	seq  atomic.Int64 // consumer open order
}

// New returns a Streamer; schedulers start on first use.
func New(cfg Config) *Streamer {
	return &Streamer{cfg: cfg.withDefaults(), tors: map[*piecestore.TorrentStore]*Torrent{}, stop: make(chan struct{})}
}

// Tunables returns the settings in use now.
func (s *Streamer) Tunables() Tunables { return s.cfg.Tunables() }

// Torrent returns the scheduler of ts, starting it on first use. It stops when ts closes.
func (s *Streamer) Torrent(ts *piecestore.TorrentStore, ctl Control) *Torrent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.tors[ts]; t != nil {
		return t
	}
	t := &Torrent{s: s, ts: ts, ctl: ctl, streams: map[*Stream]struct{}{}, wake: make(chan struct{}, 1), local: map[int]piecestore.Priority{}, pins: map[int]bool{}}
	select {
	case <-s.stop:
		t.dead = true
		return t
	default:
	}
	if ts.Closed() {
		t.dead = true
		return t
	}
	s.tors[ts] = t
	ts.SetOnData(t.trigger)
	s.wg.Add(1)
	go t.run()
	return t
}

// Close stops every scheduler and waits for them. Streams stay readable; nothing schedules.
func (s *Streamer) Close() {
	s.mu.Lock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Torrent schedules the engine for one torrent store.
type Torrent struct {
	s   *Streamer
	ts  *piecestore.TorrentStore
	ctl Control

	mu        sync.Mutex
	streams   map[*Stream]struct{}
	dead      bool
	aggrOn    bool
	aggrLimit int
	clears    []pieceRange // deadlines of closed streams, cleared on the next pass

	wake chan struct{}

	passMu     sync.Mutex // one pass at a time; guards the fields below
	local      map[int]piecestore.Priority
	pins       map[int]bool // pieces holding a demand pin
	cursorsSet []int64      // read positions last given to the engine
	lastScan   time.Time
	lastSweep  time.Time
	lastStarve time.Time
}

// Store is the torrent's piece store.
func (t *Torrent) Store() *piecestore.TorrentStore { return t.ts }

// SetAggressive records the engine's aggressive request (masterLimit 0: ConnectionsLimit).
func (t *Torrent) SetAggressive(on bool, masterLimit int) {
	t.mu.Lock()
	t.aggrOn, t.aggrLimit = on, masterLimit
	t.mu.Unlock()
	t.trigger()
}

// trigger asks for a pass; one pending request is enough.
func (t *Torrent) trigger() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *Torrent) run() {
	defer t.s.wg.Done()
	defer t.exit()
	tk := time.NewTicker(t.s.cfg.Tick)
	defer tk.Stop()
	for {
		select {
		case <-t.s.stop:
			return
		case <-t.ts.Done():
			return
		case <-tk.C:
		case <-t.wake:
		}
		if t.ts.Closed() {
			return
		}
		t.safePass(time.Now())
	}
}

func (t *Torrent) exit() {
	t.ts.SetOnData(nil)
	t.mu.Lock()
	t.dead = true
	t.mu.Unlock()
	t.s.mu.Lock()
	if t.s.tors[t.ts] == t {
		delete(t.s.tors, t.ts)
	}
	t.s.mu.Unlock()
}

// safePass keeps the scheduler alive across a panic in one pass (e372fb9).
func (t *Torrent) safePass(now time.Time) {
	t.passMu.Lock()
	defer t.passMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			t.s.cfg.Logf("[Streamer] pass panic on %s: %v", t.ts.Hash(), r)
		}
	}()
	t.pass(now)
}

// snapshot lists the active consumers; idle streams are parked while another stream exists.
func (t *Torrent) snapshot(now time.Time) (spans []span, streams []*Stream, aggr bool, limit int, clears []pieceRange) {
	t.mu.Lock()
	defer t.mu.Unlock()
	registered := 0
	for s := range t.streams {
		if s.kind == KindStream {
			registered++
		}
	}
	for s := range t.streams {
		if s.kind == KindStream && registered > 1 && s.idle(now) {
			continue
		}
		spans = append(spans, s.span())
		if s.kind == KindStream {
			streams = append(streams, s)
		}
	}
	clears, t.clears = t.clears, nil
	return spans, streams, t.aggrOn, t.aggrLimit, clears
}

func (t *Torrent) pass(now time.Time) {
	pl, np := t.ts.PieceLen(), t.ts.NumPieces()
	if pl <= 0 || np == 0 {
		return
	}
	tun := t.s.cfg.Tunables()
	spans, streams, aggr, limit, clears := t.snapshot(now)
	sh := computeShares(tun, pl, len(streams), aggr, limit, t.ts.Filled())
	for _, r := range clears {
		t.clearDeadlines(r, pieceRange{}, nil)
	}
	want := desired(spans, sh, np, func(i int) bool { return t.ts.Piece(i).Complete() })
	if calls := diff(t.local, want); len(calls) > 0 {
		t.ctl.SetPiecePriorities(calls)
	}
	for _, s := range streams {
		s.deadlines(now, sh, np)
	}
	t.pin(spans, pl, np) // after the playout deadlines: a blocked read outranks them
	t.cursors(spans)
	t.evict(now, tun, spans, sh)
}
