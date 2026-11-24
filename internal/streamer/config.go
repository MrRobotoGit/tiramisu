package streamer

import (
	"errors"
	"log"
	"time"

	"tiramisu/internal/piecestore"
)

// Control is the engine side of one torrent; engine.PieceControl satisfies it. The scheduler
// calls it from its own goroutine, never holding a store lock.
type Control interface {
	SetPiecePriorities(map[int]piecestore.Priority)
	SetStreamDeadlines(first, n int, firstDue time.Time, interval time.Duration)
	ClearStreamDeadlinesRange(first, n int)
	SetStreamCursors(offsets []int64)
	ForgetPartialPiece(i int) bool
	UpdateCompletion(i int)
}

// Tunables are the settings read on every use (engine settings Current()).
type Tunables struct {
	CacheSize        int64         // bytes per torrent
	ReadAheadPct     int           // ReaderReadAHead
	ConnectionsLimit int           // budget base when no master limit is set
	IdleTimeout      time.Duration // TorrentDisconnectTimeout; doubled in STRICT
}

// Config of a Streamer.
type Config struct {
	Tunables func() Tunables
	Logf     func(format string, args ...any)
	Tick     time.Duration // scheduler pass cadence, default 1s
	// OnOpen, when set, is told of every consumer opened (length -1: to the end of the file).
	OnOpen func(hash string, off, length int64, kind Kind)
	// Playing, when set, tells whether a torrent is in playback: only those can be at stall risk.
	// nil: every torrent counts.
	Playing func(hash string) bool
}

// Kind of consumer (design.md §1.2).
type Kind int

const (
	// KindStream is the pump: readahead window, deadlines, counted in the divisor.
	KindStream Kind = iota
	// KindDemand is a read a FUSE request is blocked on (FetchAhead until its head,
	// FetchBlockBlocking): raises its range and pins its first pieces ahead of everything.
	KindDemand
	// KindPrefetch is a bounded read nobody waits on (WarmLanding, predictive prefetch,
	// TailFetch): raises its range at Readahead, never pinned.
	KindPrefetch
	// KindHelp downloads a stride of pieces for a friend's playback: level Normal, never pinned,
	// dropped while another torrent is at risk, evicted behind its window.
	KindHelp
)

var (
	ErrIdle   = errors.New("streamer: no data within the idle timeout")
	ErrClosed = errors.New("streamer: stream closed")
	ErrRange  = errors.New("streamer: offset outside the file")
	// ErrPaused fails a speculative read (KindPrefetch) whose bytes are not ready while a stream
	// is at risk of stalling.
	ErrPaused = errors.New("streamer: speculative read paused by a stall risk")
)

func (c Config) withDefaults() Config {
	if c.Tunables == nil {
		c.Tunables = func() Tunables {
			return Tunables{CacheSize: 128 << 20, ReadAheadPct: 75, ConnectionsLimit: 25, IdleTimeout: 15 * time.Second}
		}
	}
	if c.Logf == nil {
		c.Logf = log.Printf
	}
	if c.Tick <= 0 {
		c.Tick = time.Second
	}
	return c
}
