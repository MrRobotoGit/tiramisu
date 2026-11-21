package piecestore

import (
	"sync"
	"time"
)

const (
	shieldStepSeconds = 30
	shieldCapSeconds  = 1800
)

type shieldConfig struct {
	enabled func() (responsive, adaptive bool)
	logf    func(format string, args ...any)
	now     func() time.Time
	poll    time.Duration
}

// Shield is the AdaptiveShield of one torrent: STRICT (verified data only) on the second
// corruption, FAST again after a clean streak of 30s per cycle, capped at 1800s.
type Shield struct {
	hash string
	cfg  shieldConfig

	mu     sync.Mutex
	last   int64 // unix seconds of the last corruption, 0 = never
	active bool
	count  int
	cycle  int
	stop   chan struct{} // non-nil while the watchdog runs
	closed bool
}

func newShield(hash string, cfg shieldConfig) *Shield {
	return &Shield{hash: hash, cfg: cfg}
}

func requiredStreak(cycle int) int64 {
	return min(max(int64(shieldStepSeconds*cycle), shieldStepSeconds), shieldCapSeconds)
}

// Corruption records a real hash failure (never an eviction artefact).
func (s *Shield) Corruption(piece int) {
	responsive, adaptive := s.cfg.enabled()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.last = s.cfg.now().Unix()
	if responsive && adaptive && !s.active {
		s.count++
		if s.count > 1 {
			s.cycle++
			s.active = true
			s.cfg.logf("[AdaptiveShield] Persistent corruption - Force STRICT mode (Shield: ACTIVE, cycle %d , need %d s clean) torrent=%s",
				s.cycle, requiredStreak(s.cycle), s.hash)
		} else {
			s.cfg.logf("[AdaptiveShield] Single corruption detected for piece %d - FAST mode preserved, monitoring... torrent=%s", piece, s.hash)
		}
	}
	if s.count >= 1 && s.stop == nil {
		s.startWatchdogLocked()
	}
}

// Reset ends STRICT and forgets pending corruptions. The cycle survives on purpose; the
// watchdog keeps running so a later clean streak still resets it.
func (s *Shield) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.active {
		s.cfg.logf("[AdaptiveShield] Shield reset - Restoring FAST mode (Shield: OFF) torrent=%s", s.hash)
	}
	s.active = false
	s.count = 0
}

// Responsive reports whether unverified (written) data may be served.
func (s *Shield) Responsive() bool {
	responsive, _ := s.cfg.enabled()
	s.mu.Lock()
	defer s.mu.Unlock()
	return responsive && !s.active
}

// Status returns STRICT on/off, the last corruption (unix s) and the clean streak required.
func (s *Shield) Status() (active bool, last int64, need int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.last, requiredStreak(s.cycle)
}

// Close stops the watchdog for good; later events are ignored.
func (s *Shield) Close() {
	s.mu.Lock()
	s.closed = true
	s.stopWatchdogLocked()
	s.mu.Unlock()
}

func (s *Shield) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	need := requiredStreak(s.cycle)
	if s.cfg.now().Unix()-s.last <= need {
		return
	}
	if s.active {
		s.cfg.logf("[AdaptiveShield] Clean streak detected ( %d s) - Restoring FAST mode (Shield: OFF) torrent=%s", need, s.hash)
	}
	s.active = false
	s.count = 0
	s.cycle = 0
	s.stopWatchdogLocked()
}

func (s *Shield) watchdogRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop != nil
}

func (s *Shield) startWatchdogLocked() {
	stop := make(chan struct{})
	s.stop = stop
	go func() {
		t := time.NewTicker(s.cfg.poll)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.tick()
			}
		}
	}()
}

func (s *Shield) stopWatchdogLocked() {
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
}
