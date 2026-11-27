// Package stallnotice decides, per virtual file, when the swarm cannot carry a film at startup and
// then serves the notice bytes of internal/notice in place of the film for the rest of the session.
// internal/app calls it from the FUSE read path; everything it needs comes in through Deps.
//
// The rule: from the open of the file until the swarm proves it carries the film's bitrate, a
// player read that waits on the swarm gets the notice once (a) the median goodput over at least
// minSamples seconds is below the bitrate, (b) the trackers answer with no peer, or (c) the read
// path gives up (Fail, in place of EIO). A session of which no byte reached the player gets the
// whole-file synthetic notice; otherwise the notice starts where the film's served bytes end.
package stallnotice

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tiramisu/internal/notice"
	"tiramisu/internal/streampolicy"
)

// Deps is what the manager needs from the engine, the caches and the app. Every func must be
// safe for concurrent use and must not block on the network.
type Deps struct {
	Enabled func() bool // the stall_notice setting, read on every decision
	// Goodput is the torrent's median delivered goodput (bytes/s) over the 1 s samples taken after
	// sample since, from the first delivered byte among them, and how many back it; ok false when
	// the torrent has no streamer state. GoodputSeq is the current sample sequence.
	Goodput    func(hash string, since int64) (p50 float64, samples int, ok bool)
	GoodputSeq func(hash string) int64
	Peers      func(hash string) int // connected peers
	// UnchokeSettle is how long until the connected peers that still keep us choked reach their
	// client's usual unchoke wait: goodput sampled before that measures the handshake, not the swarm.
	UnchokeSettle func(hash string) time.Duration
	// Announce forces a tracker announce and reports the peers returned and how many trackers
	// answered without error.
	Announce func(hash string, done func(peers, answered int))
	// Local copies real bytes at off that are already on this machine (SSD head, read-ahead
	// cache); it returns how many contiguous bytes from off it copied and never waits.
	Local func(path, hash string, fileID int, off int64, p []byte) int
	// Tail copies [off, off+len(p)) from the SSD tail window (the last TailSize bytes).
	Tail     func(hash string, fileID int, size int64, p []byte, off int64) int
	TailSize int64
	StopFilm func(path, hash string) // stops the pump and drops the torrent's priority
	Failed   func(path string)       // the session counts as a failed playback
	Logf     func(format string, args ...any)
	Tick     time.Duration // verdict cadence: the streamer's 1 s goodput sample
	TailZone func(size int64) int64
}

const (
	minSamples = 5        // goodput samples before a verdict (the streamer's goodputMinSamples)
	headProbe  = 32 << 20 // head bytes for the structure and the frame duration
	maxPrefix  = 64 << 20 // real bytes re-read to complete the interrupted Cluster
)

type blockedRead struct {
	off int64
	n   int
}

type reader interface {
	ReadAt(p []byte, off int64) (int, error)
}

// shown is a notice being served: zeros in [from, v), the notice in [v, e).
type shown struct {
	r          reader
	from, v, e int64
}

type session struct {
	hash     string
	fileID   int
	size     int64
	handles  map[any]bool
	window   bool // open until the swarm proves it carries the film
	notice   *shown
	tried    bool // the notice cannot be built for this session: today's behaviour until it ends
	deciding bool
	blocked  map[*blockedRead]bool
	served   bool // a real byte reached a reader
	// Where and when the player started on the film's body: the playback position is estimated
	// from them at the film's bitrate (pauses not known: the estimate only runs ahead).
	anchorOff int64
	anchorAt  time.Time
	frontier  int64 // end of the highest real read outside the tail zone
	film      *notice.Film
	mp4       *notice.Movie // an MP4 film: its sample tables, instead of film
	head      []byte
	bitrate   int64 // bytes/s: size over the Info duration
	announce  int   // 0 none, 1 asked, 2 answered: the forced announce of a swarm without peers
	peersAnn  int
	trackers  int
	since     int64     // goodput sample sequence at the first eligible blocked read; -1 before it
	stallSeq  int64     // goodput sequence when the current stall began (blocked went from none to some)
	stallAt   time.Time // when it began
	lastErr   string
	deferred  string // the last reason a verdict waited, logged once
	headAt    int64  // frontier at the last head that did not parse: retried once more is served
	cues      []notice.Cue
	cuesAt    time.Time // last Cues read that found none
}

// Manager holds one session per virtual path with open handles.
type Manager struct {
	d        Deps
	mu       sync.Mutex
	sessions map[string]*session
	byKey    map[any]string
	notices  atomic.Int64
	ready    atomic.Bool
	playing  atomic.Int32 // sessions serving the notice: Serve's fast path when none
}

func New(d Deps) *Manager {
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.Tick <= 0 {
		d.Tick = time.Second
	}
	if d.TailZone == nil {
		d.TailZone = streampolicy.TailProbeZoneSize
	}
	return &Manager{d: d, sessions: map[string]*session{}, byKey: map[any]string{}}
}

// SetReady records whether the clip bank loaded; without it nothing is decided.
func (m *Manager) SetReady(ok bool) { m.ready.Store(ok) }

// Ready reports what SetReady recorded (shown on /metrics).
func (m *Manager) Ready() bool { return m.ready.Load() }

// Notices counts the sessions that served the notice since start.
func (m *Manager) Notices() int64 { return m.notices.Load() }

func (m *Manager) on() bool { return m.ready.Load() && m.d.Enabled != nil && m.d.Enabled() }

// Open registers handle key on path; the first handle opens the session and its window. Calling
// it again for the same key (late hash resolution) only completes the session.
func (m *Manager) Open(key any, path, hash string, fileID int, size int64) {
	if hash == "" || size <= 0 || audioFile(path) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[path]
	if s == nil {
		s = &session{hash: hash, fileID: fileID, size: size, window: true, handles: map[any]bool{},
			blocked: map[*blockedRead]bool{}, since: -1}
		m.sessions[path] = s
	}
	s.handles[key] = true
	m.byKey[key] = path
}

// audioFile: music never gets the notice, a video file in place of a track.
func audioFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".flac", ".mp3", ".m4a", ".aac", ".ogg", ".opus", ".wav", ".alac", ".ape", ".wv", ".dsf", ".aiff":
		return true
	}
	return false
}

// Release drops handle key; the session ends with its last handle. Keys never opened are ignored.
func (m *Manager) Release(key any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path, ok := m.byKey[key]
	if !ok {
		return
	}
	delete(m.byKey, key)
	s := m.sessions[path]
	if s == nil {
		return
	}
	delete(s.handles, key)
	if len(s.handles) == 0 {
		if s.notice != nil {
			m.playing.Add(-1)
			m.d.Logf("[Notice] session ended for %s", filepath.Base(path))
		}
		delete(m.sessions, path)
	}
}

// Active reports whether path is serving the notice: the app must not restart its pump.
func (m *Manager) Active(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[path]
	return s != nil && s.notice != nil
}

// Served records a real read that reached a reader: where the film's served bytes end.
func (m *Manager) Served(path string, off int64, n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[path]
	if s == nil || s.notice != nil {
		return
	}
	s.served = true
	if s.anchorAt.IsZero() && off >= streampolicy.HeaderProbeZone && off < s.size-m.d.TailZone(s.size) {
		s.anchorOff, s.anchorAt = off, time.Now()
	}
	if end := off + int64(n); off < s.size-m.d.TailZone(s.size) && end > s.frontier {
		s.frontier = end
	}
}

// Serve fills dest with notice bytes when path is serving the notice and off lies in [from, E).
// When a read starts before that and runs into it, limit is how many bytes the caller may serve
// from the film; the FUSE refill loop then reads the rest, so each offset gets the bytes the
// layout assigns it.
func (m *Manager) Serve(path string, dest []byte, off int64) (n int, served bool, limit int) {
	limit = len(dest)
	if m.playing.Load() == 0 {
		return 0, false, limit
	}
	m.mu.Lock()
	s := m.sessions[path]
	if s == nil || s.notice == nil {
		m.mu.Unlock()
		return 0, false, limit
	}
	sh := *s.notice
	m.mu.Unlock()
	end := off + int64(len(dest))
	switch {
	case off >= sh.e || end <= sh.from:
		return 0, false, limit
	case off < sh.from:
		return 0, false, int(sh.from - off)
	}
	k := int(min(end, sh.e) - off)
	z := int(max(min(sh.v, off+int64(k))-off, 0)) // alignment bytes before a resume Cue
	clear(dest[:z])
	if z < k {
		if _, err := sh.r.ReadAt(dest[z:k], off+int64(z)); err != nil {
			m.drop(path, err)
			return 0, false, limit
		}
	}
	return k, true, limit
}

// Guard checks n real bytes read into dest at off just before they reach the kernel: a notice
// started while they were read takes over its range. It returns how many bytes to hand back.
func (m *Manager) Guard(path string, dest []byte, off int64, n int) int {
	if n <= 0 {
		return n
	}
	k, ok, limit := m.Serve(path, dest[:n], off)
	if ok {
		return k
	}
	return min(n, limit)
}

// pastNotice serves zeros for a read past the notice (the film's Cues and Tags): with the swarm
// unable to deliver them, an index-less file plays on instead of failing.
func (m *Manager) pastNotice(path string, dest []byte, off int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[path]
	if s == nil || s.notice == nil || off < s.notice.e {
		return false
	}
	clear(dest)
	return true
}

// drop abandons a notice that failed to serve: the film again, never a half notice.
func (m *Manager) drop(path string, err error) {
	m.mu.Lock()
	if s := m.sessions[path]; s != nil && s.notice != nil {
		s.notice, s.tried = nil, true
		m.playing.Add(-1)
	}
	m.mu.Unlock()
	m.d.Logf("[Notice] %s: notice dropped: %v", filepath.Base(path), err)
}

// Await runs fetch, the read path's blocking fetch into its own buffer, and while it blocks
// asks every Tick whether the swarm can carry the film. eligible says the reader is the
// session's player (primary handle). It returns served=true with the notice bytes in dest once
// the verdict is given; otherwise fetch's result, copied into dest.
func (m *Manager) Await(ctx context.Context, path string, off int64, eligible bool, dest []byte,
	fetch func(dest []byte) (int, error)) (n int, served bool, err error) {
	if !m.on() {
		n, err = fetch(dest)
		return n, false, err
	}
	if k, ok, _ := m.Serve(path, dest, off); ok {
		return k, true, nil
	}
	br := &blockedRead{off: off, n: len(dest)}
	if !eligible || !m.block(path, br) {
		n, err = fetch(dest)
		return n, false, err
	}
	defer m.unblock(path, br)
	// fetch keeps its buffer until it returns: dest must stay free for the notice.
	priv := make([]byte, len(dest))
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		k, e := fetch(priv)
		done <- result{k, e}
	}()
	tick := time.NewTicker(m.d.Tick)
	defer tick.Stop()
	for {
		select {
		case r := <-done:
			copy(dest, priv[:max(r.n, 0)])
			return r.n, false, r.err
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case <-tick.C:
			m.decide(path, false)
			if k, ok, _ := m.Serve(path, dest, off); ok {
				return k, true, nil
			}
			if m.pastNotice(path, dest, off) {
				return len(dest), true, nil
			}
		}
	}
}

// Fail is the read path giving up on a player read (its EIO): inside the window that is the
// verdict itself. It returns the bytes to serve instead of the error, if any.
func (m *Manager) Fail(path string, off int64, eligible bool, dest []byte) (int, bool) {
	if !m.on() {
		return 0, false
	}
	if k, ok, _ := m.Serve(path, dest, off); ok {
		return k, true
	}
	if m.pastNotice(path, dest, off) {
		return len(dest), true
	}
	br := &blockedRead{off: off, n: len(dest)}
	if !eligible || !m.block(path, br) {
		return 0, false
	}
	// A decision already running for another read (an Await tick) is waited for: skipping it
	// would turn this give-up into the EIO it replaces.
	for i := 0; i < 500 && m.deciding(path); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	m.decide(path, true)
	m.unblock(path, br)
	if k, ok, _ := m.Serve(path, dest, off); ok {
		return k, true
	}
	if m.pastNotice(path, dest, off) {
		return len(dest), true
	}
	return 0, false
}

// block registers a blocked player read of a session still in its window. The session's first
// one records the goodput sequence: only samples taken from then on judge.
func (m *Manager) block(path string, br *blockedRead) bool {
	m.mu.Lock()
	s := m.sessions[path]
	if s == nil || s.notice != nil || s.tried {
		m.mu.Unlock()
		return false
	}
	stall := len(s.blocked) == 0
	s.blocked[br] = true
	first, hash := s.since < 0, s.hash
	if stall {
		s.stallAt, s.stallSeq = time.Now(), 0
		if m.d.GoodputSeq != nil {
			s.stallSeq = -1 // set just below, outside the lock
		}
	}
	m.mu.Unlock()
	if stall && m.d.GoodputSeq != nil {
		seq := m.d.GoodputSeq(hash)
		m.mu.Lock()
		if s.stallSeq < 0 {
			s.stallSeq = seq
		}
		m.mu.Unlock()
	}
	if first {
		var seq int64
		if m.d.GoodputSeq != nil {
			seq = m.d.GoodputSeq(hash)
		}
		if m.d.UnchokeSettle != nil { // samples count once the peers just connected had time to unchoke us
			seq += int64((m.d.UnchokeSettle(hash) + m.d.Tick - 1) / m.d.Tick)
		}
		m.mu.Lock()
		if s.since < 0 {
			s.since = seq
		}
		m.mu.Unlock()
	}
	return true
}

func (m *Manager) deciding(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[path]
	return s != nil && s.deciding
}

func (m *Manager) unblock(path string, br *blockedRead) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[path]; s != nil {
		delete(s.blocked, br)
	}
}

// decide gives the verdict for the session's blocked reads; force is the read path's give-up.
// One decision runs at a time per session.
func (m *Manager) decide(path string, force bool) {
	m.mu.Lock()
	s := m.sessions[path]
	if s == nil || s.notice != nil || s.tried || s.deciding || len(s.blocked) == 0 {
		m.mu.Unlock()
		return
	}
	if !s.window { // the swarm proved it carries the film: only peers that died show the notice
		s.deciding = true
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			s.deciding = false
			m.mu.Unlock()
		}()
		m.decideDead(path, s, force)
		return
	}
	s.deciding = true
	served, since := s.served, max(s.since, 0)
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		s.deciding = false
		m.mu.Unlock()
	}()
	if !m.on() {
		return
	}
	var p50 float64
	var samples int
	if m.d.Goodput != nil {
		p50, samples, _ = m.d.Goodput(s.hash, since)
	}
	var f *notice.Film
	var err error
	if served {
		if f, err = m.film(path, s); errors.Is(err, errNotFilm) {
			// The bytes the player got are not a film's header (a peer's garbage, a piece that
			// failed its hash): no valid byte arrived.
			m.mu.Lock()
			s.served, served = false, false
			m.mu.Unlock()
		}
	}
	if !served {
		// Nothing of the film reached the player: no header, no bitrate. The whole file is the notice.
		if !force && !m.deadSwarm(s, samples, path) {
			return
		}
		syn, err := notice.NewSynthetic(s.size)
		if err != nil {
			m.refuse(path, err)
			return
		}
		m.show(path, s, &shown{r: syn, e: s.size}, func(s *session) bool { return !s.served },
			fmt.Sprintf("no byte of the film arrived (%d peers, %d samples): synthetic notice", m.peers(s.hash), samples))
		return
	}
	if err != nil {
		m.fail(path, s, force, fmt.Errorf("head: %w", err))
		return
	}
	m.mu.Lock()
	bitrate := s.bitrate
	m.mu.Unlock()
	if samples >= minSamples && p50 >= float64(bitrate) {
		m.mu.Lock()
		s.window = false // the swarm carries the film: this session never shows the notice
		m.mu.Unlock()
		return
	}
	if !force && m.d.UnchokeSettle != nil && m.d.UnchokeSettle(s.hash) > 0 {
		m.waits(path, s, "connected peers may still unchoke us") // peers that came after the first stall
		return
	}
	why := ""
	switch {
	case force:
		why = fmt.Sprintf("the read gave up (median %.2f MB/s over %d s)", p50/(1<<20), samples)
	case samples >= minSamples:
		if m.leadLeft(s, bitrate) {
			m.waits(path, s, "the player still has film ahead") // keep measuring: the swarm may ramp up
			return
		}
		why = fmt.Sprintf("swarm median %.2f MB/s over %d s", p50/(1<<20), samples)
	case m.deadSwarm(s, samples, path):
		why = "no peer and the trackers answered with none"
	default:
		return
	}
	sh, err := m.build(path, s, f)
	if err != nil {
		m.fail(path, s, force, err)
		return
	}
	m.show(path, s, sh, noRealPast(sh.from),
		fmt.Sprintf("%s (%d peers) below the film's %.2f MB/s: notice from %d MB", why, m.peers(s.hash),
			float64(bitrate)/(1<<20), sh.from>>20))
}

// decideDead is the verdict once the swarm proved it carries the film: the notice comes only when
// the peers died. No byte arrived for at least minSamples seconds of the current stall, and every
// connected peer that still keeps us choked is past its client's usual unchoke wait (a slow swarm
// is not a dead one: the read waits as today). The read path's give-up is the verdict too.
func (m *Manager) decideDead(path string, s *session, force bool) {
	if !m.on() {
		return
	}
	m.mu.Lock()
	seq, at := s.stallSeq, s.stallAt
	inFilm := false // a read in the film's body waits: a tail (Cues) read alone is not playback
	for br := range s.blocked {
		inFilm = inFilm || br.off < s.size-m.d.TailZone(s.size)
	}
	m.mu.Unlock()
	if !inFilm || seq < 0 && !force {
		return
	}
	samples, ok := 0, false
	if m.d.Goodput != nil {
		_, samples, ok = m.d.Goodput(s.hash, max(seq, 0))
	}
	if !ok {
		return // no streamer state: the torrent is not running (expired, removed), not a dead swarm
	}
	stalled := time.Since(at)
	if !force {
		if samples > 0 || stalled < minSamples*m.d.Tick {
			return
		}
		if m.d.UnchokeSettle != nil && m.d.UnchokeSettle(s.hash) > 0 {
			return // a connected peer may still unchoke us
		}
		if m.peers(s.hash) == 0 && !m.deadSwarm(s, samples, path) {
			return // no peer: the trackers decide, as at startup
		}
	}
	f, err := m.film(path, s)
	if err != nil {
		m.fail(path, s, force, fmt.Errorf("head: %w", err))
		return
	}
	sh, err := m.build(path, s, f)
	if err != nil {
		m.fail(path, s, force, err)
		return
	}
	why := fmt.Sprintf("the peers died: no byte for %d s of stall (%d peers, none left to unchoke us)", int(stalled.Seconds()), m.peers(s.hash))
	if force && samples > 0 {
		why = "the read gave up during playback"
	}
	m.show(path, s, sh, noRealPast(sh.from), why+fmt.Sprintf(": notice from %d MB", sh.from>>20))
}

// leadLeft: the reads that wait are still ahead of the estimated playback position, so the player
// has film to show and the verdict can wait for the swarm to ramp up (The Stunt Driver: 5 slow
// seconds 13 s after the start with ~40 s of the SSD head ahead, then 4 MB/s; Nocturne: Infuse
// filling minutes of cache). The verdict comes when the player is about to run dry.
func (m *Manager) leadLeft(s *session, bitrate int64) bool {
	m.mu.Lock()
	anchorOff, anchorAt, frontier := s.anchorOff, s.anchorAt, s.frontier
	v := int64(-1)
	for br := range s.blocked {
		if br.off < s.size-m.d.TailZone(s.size) && (v < 0 || br.off < v) {
			v = br.off
		}
	}
	m.mu.Unlock()
	if anchorAt.IsZero() || bitrate <= 0 {
		return false
	}
	if v < 0 {
		v = frontier
	}
	pos := float64(anchorOff) + float64(bitrate)*time.Since(anchorAt).Seconds()
	return float64(v)-pos > float64(bitrate)*m.d.Tick.Seconds() // more than one tick of film left
}

// waits logs, once per reason and session, why a verdict below the bitrate waits.
func (m *Manager) waits(path string, s *session, why string) {
	m.mu.Lock()
	again := s.deferred == why
	s.deferred = why
	m.mu.Unlock()
	if !again {
		m.d.Logf("[Notice] %s: below the bitrate, waiting: %s", filepath.Base(path), why)
	}
}

// noRealPast: no real byte at or past from reached a reader; the notice would rewrite film the
// player already holds (kernel page cache included).
func noRealPast(from int64) func(*session) bool {
	return func(s *session) bool { return s.frontier <= from }
}

// show activates sh unless the session ended or still(s) no longer holds (real bytes past the
// notice's start reached the player while it was built: the next tick decides again).
func (m *Manager) show(path string, s *session, sh *shown, still func(*session) bool, msg string) {
	m.mu.Lock()
	if m.sessions[path] != s || s.notice != nil || !still(s) {
		m.mu.Unlock()
		return
	}
	s.notice = sh
	m.playing.Add(1)
	m.mu.Unlock()
	m.notices.Add(1)
	if m.d.StopFilm != nil {
		m.d.StopFilm(path, s.hash)
	}
	if m.d.Failed != nil {
		m.d.Failed(path)
	}
	m.d.Logf("[Notice] %s: %s", filepath.Base(path), msg)
}

// deadSwarm: nothing delivered while the read waited, no peer connected, and a forced announce
// got no peer from trackers that answered. A swarm still connecting waits for the read path.
func (m *Manager) deadSwarm(s *session, samples int, path string) bool {
	if samples > 0 || m.peers(s.hash) > 0 || m.d.Announce == nil {
		return false
	}
	m.mu.Lock()
	state, peers, trackers := s.announce, s.peersAnn, s.trackers
	if state == 0 {
		s.announce = 1
	}
	m.mu.Unlock()
	switch state {
	case 0:
		m.d.Announce(s.hash, func(p, answered int) {
			m.mu.Lock()
			s.announce, s.peersAnn, s.trackers = 2, p, answered
			m.mu.Unlock()
		})
	case 2:
		if trackers > 0 && peers == 0 {
			m.d.Logf("[Notice] %s: no peer connected and %d trackers answered with none", filepath.Base(path), trackers)
			return true
		}
	}
	return false
}

func (m *Manager) peers(hash string) int {
	if m.d.Peers == nil {
		return 0
	}
	return m.d.Peers(hash)
}

// fail: a format the bank cannot play ends the session's chances; anything else (bytes not local
// yet) is retried at the next tick and at the next give-up, logged once.
func (m *Manager) fail(path string, s *session, force bool, err error) {
	if errors.Is(err, notice.ErrUnsupported) {
		m.refuse(path, err)
		return
	}
	m.mu.Lock()
	again := s.lastErr == err.Error()
	s.lastErr = err.Error()
	m.mu.Unlock()
	if !again {
		m.d.Logf("[Notice] %s: notice not possible yet: %v", filepath.Base(path), err)
	}
}

// refuse ends the session's chances: the reads go on as today.
func (m *Manager) refuse(path string, err error) {
	m.mu.Lock()
	if s := m.sessions[path]; s != nil {
		s.tried = true
	}
	m.mu.Unlock()
	m.d.Logf("[Notice] %s: notice not possible: %v", filepath.Base(path), err)
}

// local reads the film's bytes on this machine: SSD head and read-ahead cache (Local), then the
// SSD tail window (a moov-last MP4's moov, the Cues).
func (m *Manager) local(path string, s *session) func(off int64, p []byte) int {
	return func(off int64, p []byte) int {
		if off < 0 || off >= s.size {
			return 0
		}
		p = p[:min(int64(len(p)), s.size-off)]
		n := 0
		if m.d.Local != nil {
			n = m.d.Local(path, s.hash, s.fileID, off, p)
		}
		if n < len(p) && m.d.Tail != nil && off+int64(n) >= s.size-m.d.TailSize {
			if m.d.Tail(s.hash, s.fileID, s.size, p[n:], off+int64(n)) == len(p)-n {
				n = len(p)
			}
		}
		return n
	}
}

var (
	errHeadNotLocal = errors.New("the film's header is not on this machine")
	errNotFilm      = errors.New("the first bytes are not a film's header")
)

// film reads the structure and the bitrate (size over the Info duration) from the film's first
// bytes on this machine, once per session.
func (m *Manager) film(path string, s *session) (*notice.Film, error) {
	m.mu.Lock()
	f, mv, frontier, last := s.film, s.mp4, s.frontier, s.headAt
	m.mu.Unlock()
	if f != nil || mv != nil {
		return f, nil
	}
	if last != 0 && last == frontier {
		return nil, errHeadNotLocal
	}
	head := make([]byte, min(int64(headProbe), s.size))
	head = head[:m.local(path, s)(0, head)]
	if len(head) >= 8 && !notice.IsMatroska(head) && !notice.IsMP4(head) {
		return nil, errNotFilm
	}
	if notice.IsMP4(head) {
		return nil, m.loadMP4(path, s, frontier)
	}
	f, err := notice.ParseHead(head)
	if err != nil {
		m.mu.Lock()
		s.headAt = max(frontier, 1)
		m.mu.Unlock()
		if errors.Is(err, notice.ErrUnsupported) {
			return nil, err
		}
		return nil, errHeadNotLocal
	}
	durNs := f.Duration * float64(f.TimestampScale)
	if !(durNs > 1e9) {
		return nil, fmt.Errorf("%w: no usable duration", notice.ErrStructure)
	}
	m.mu.Lock()
	s.film, s.head, s.bitrate = f, head, int64(float64(s.size)/(durNs/1e9))
	m.mu.Unlock()
	return f, nil
}

// loadMP4 reads an MP4's moov from the local bytes (the head, or the tail a moov-last file
// keeps there) and the bitrate from its duration.
func (m *Manager) loadMP4(path string, s *session, frontier int64) error {
	local := m.local(path, s)
	notLocal := func() error {
		m.mu.Lock()
		s.headAt = max(frontier, 1)
		m.mu.Unlock()
		return errHeadNotLocal
	}
	moov, mdats, err := notice.MP4Top(local, s.size)
	if errors.Is(err, notice.ErrUnsupported) {
		return err
	}
	if err != nil {
		return notLocal()
	}
	body := make([]byte, moov[1]-moov[0])
	if local(moov[0], body) < len(body) {
		return notLocal()
	}
	mv, err := notice.ParseMoov(body, mdats)
	if err != nil {
		return fmt.Errorf("%w: moov: %v", notice.ErrUnsupported, err)
	}
	if !(mv.Duration > 1) {
		return fmt.Errorf("%w: no usable duration", notice.ErrUnsupported)
	}
	m.mu.Lock()
	s.mp4, s.bitrate = mv, int64(float64(s.size)/mv.Duration)
	m.mu.Unlock()
	return nil
}

// buildMP4 plans an MP4's notice from the lowest blocked read inside an mdat, else from where
// the served bytes end.
func (m *Manager) buildMP4(s *session, mv *notice.Movie) (*shown, error) {
	m.mu.Lock()
	frontier := s.frontier
	var v int64 = -1
	for br := range s.blocked {
		for _, d := range mv.Mdats {
			if br.off >= d[0] && br.off < d[1] && (v < 0 || br.off < v) {
				v = br.off
			}
		}
	}
	m.mu.Unlock()
	if v < 0 {
		v = max(frontier, mv.Mdats[0][0])
	}
	x, err := notice.FormatOfMP4(mv)
	if err != nil {
		return nil, fmt.Errorf("clip format: %w", err)
	}
	c, err := notice.ClipFor(x)
	if err != nil {
		return nil, fmt.Errorf("clip: %w", err)
	}
	l, err := notice.NewMP4Layout(mv, v, x, c)
	if err != nil {
		return nil, err
	}
	return &shown{r: l, from: v, v: v, e: l.E}, nil
}

// build plans the notice: from the lowest blocked read in the film's Clusters, else (only
// header or tail reads wait) from where the served bytes end. The Cluster holding V is completed
// from local bytes; when they are missing (a resume jumped there) and a Cue lies inside the read,
// the notice starts at that Cue and the read's bytes before it are zeros.
func (m *Manager) build(path string, s *session, f *notice.Film) (*shown, error) {
	m.mu.Lock()
	if mv := s.mp4; mv != nil {
		m.mu.Unlock()
		return m.buildMP4(s, mv)
	}
	head, bitrate, frontier := s.head, s.bitrate, s.frontier
	var v blockedRead
	for br := range s.blocked {
		if br.off >= f.FirstCluster && br.off < s.size-m.d.TailZone(s.size) && (v.n == 0 || br.off < v.off) {
			v = *br
		}
	}
	m.mu.Unlock()
	if v.n == 0 {
		v.off = max(frontier, f.FirstCluster)
	}
	x, err := notice.FormatOf(f, head)
	if err != nil {
		return nil, fmt.Errorf("clip format: %w", err)
	}
	c, err := notice.ClipFor(x)
	if err != nil {
		return nil, fmt.Errorf("clip: %w", err)
	}
	cues := m.cues(path, s, f)
	read := m.local(path, s)
	from := f.FirstCluster
	if i := sort.Search(len(cues), func(i int) bool { return cues[i].Pos > v.off }) - 1; i >= 0 {
		from = cues[i].Pos
	}
	start, ok := v.off, v.off == f.FirstCluster // nothing past the header served: no walk needed
	if !ok {
		start, ok = notice.ClusterContaining(read, from, v.off)
	}
	if !ok && from != f.FirstCluster {
		start, ok = notice.ClusterContaining(read, f.FirstCluster, v.off)
	}
	if ok && start == v.off {
		t0, known := notice.ClusterTime(read, start)
		if !known {
			if t0, known = cueTime(cues, start); !known && start == f.FirstCluster {
				t0, known = 0, true
			}
		}
		if known {
			l, err := notice.NewLayoutAt(f, cues, start, t0, s.size, bitrate, x, c)
			if err != nil {
				return nil, err
			}
			return &shown{r: l, from: v.off, v: l.V, e: l.E}, nil
		}
	}
	if ok && v.off-start <= maxPrefix {
		prefix := make([]byte, v.off-start)
		if read(start, prefix) == len(prefix) {
			l, err := notice.NewLayout(f, cues, prefix, start, s.size, bitrate, x, c)
			if err != nil {
				return nil, err
			}
			return &shown{r: l, from: v.off, v: l.V, e: l.E}, nil
		}
	}
	if i := sort.Search(len(cues), func(i int) bool { return cues[i].Pos >= v.off }); i < len(cues) && cues[i].Pos < v.off+int64(v.n) {
		l, err := notice.NewLayout(f, cues, nil, cues[i].Pos, s.size, bitrate, x, c)
		if err != nil {
			return nil, err
		}
		return &shown{r: l, from: v.off, v: l.V, e: l.E}, nil
	}
	return nil, fmt.Errorf("the film before %d MB is not local and no Cue lies in the read", v.off>>20)
}

func cueTime(cues []notice.Cue, pos int64) (uint64, bool) {
	i := sort.Search(len(cues), func(i int) bool { return cues[i].Pos >= pos })
	if i < len(cues) && cues[i].Pos == pos {
		return cues[i].Time, true
	}
	return 0, false
}

// cues reads the film's Cues from the SSD tail, else from the local bytes (a tail the player
// read through the read-ahead cache); none is fine, the notice then uses the bitrate.
func (m *Manager) cues(path string, s *session, f *notice.Film) []notice.Cue {
	m.mu.Lock()
	cached, at := s.cues, s.cuesAt
	m.mu.Unlock()
	if cached != nil || time.Since(at) < 10*time.Second {
		return cached
	}
	cues := m.readCues(path, s, f)
	m.mu.Lock()
	s.cues, s.cuesAt = cues, time.Now()
	m.mu.Unlock()
	return cues
}

func (m *Manager) readCues(path string, s *session, f *notice.Film) []notice.Cue {
	tailStart := s.size - m.d.TailSize
	local := m.local(path, s)
	read := func(pos int64) []byte {
		if pos <= 0 || pos >= s.size || s.size-pos > 64<<20 {
			return nil
		}
		b := make([]byte, s.size-pos)
		if m.d.TailSize > 0 && pos >= tailStart && m.d.Tail != nil && m.d.Tail(s.hash, s.fileID, s.size, b, pos) == len(b) {
			return b
		}
		if local(pos, b) == len(b) {
			return b
		}
		return nil
	}
	if f.CuesPos == 0 {
		for _, sh := range f.SeekHeads {
			if b := read(sh); b != nil {
				_ = f.ParseSeekHeadAt(b)
			}
		}
	}
	b := read(f.CuesPos)
	if b == nil {
		return nil
	}
	cues, err := notice.ParseCues(b, f)
	if err != nil {
		return nil
	}
	return cues
}

// Files lists the paths with a session on a torrent file and the file's size (piecedrop).
func (m *Manager) Files(hash string, fileID int) (paths []string, size int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for p, s := range m.sessions {
		if s.hash == hash && s.fileID == fileID {
			paths, size = append(paths, p), s.size
		}
	}
	return paths, size
}

// Rollback forgets what the sessions of a file were served from off on: those bytes belonged to
// a piece that failed its hash. A broken header means no valid byte reached the player.
func (m *Manager) Rollback(hash string, fileID int, off int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.hash != hash || s.fileID != fileID || s.notice != nil {
			continue
		}
		s.frontier = min(s.frontier, off)
		if off == 0 || s.film != nil && off < s.film.FirstCluster { // the header itself
			s.served, s.film, s.mp4, s.head, s.headAt = false, nil, nil, nil, 0
		}
		if s.mp4 != nil && !inMdat(s.mp4, off) { // the moov
			s.mp4, s.headAt = nil, 0
		}
		s.cues, s.cuesAt = nil, time.Time{}
	}
}

func inMdat(mv *notice.Movie, off int64) bool {
	for _, d := range mv.Mdats {
		if off >= d[0] && off < d[1] {
			return true
		}
	}
	return false
}
