package streamer

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"tiramisu/internal/piecestore"
)

// File locates one file inside the torrent.
type File struct {
	Offset int64 // first byte in the torrent
	Length int64
}

// Stream is one ordered consumer of a file range. Its position moves only as Read returns
// bytes, so priorities, windows and deadlines follow consumption. One goroutine reads.
type Stream struct {
	t     *Torrent
	kind  Kind
	file  File
	start int64 // file-relative offset of the open
	end   int64 // file-relative, exclusive
	pos   atomic.Int64
	seq   atomic.Int64 // when the current wait began: open, or the first Await after Demote
	busy  atomic.Int32 // Reads in flight: a busy stream is never parked
	// demoted: the demand's caller has its bytes; waiters: readers blocked on it since
	demoted atomic.Bool
	waiters atomic.Int32
	touch   atomic.Int64 // unix nanos of the last progress (or open)
	// bitrate is the film's bytes per second of content (size / duration); 0: unknown
	bitrate atomic.Int64
	// waitSeq: when the reader began waiting on piece waitAt-1 (0: never); it holds until the
	// position leaves that piece, so a reader woken block by block keeps its place
	waitSeq atomic.Int64
	waitAt  atomic.Int64

	closed atomic.Bool
	done   chan struct{}

	// lead: bytes the consumer already holds past Pos (the pump's lead over the player)
	lead atomic.Int64

	modePiece  int // piece whose mode is fixed, reader goroutine only
	responsive bool

	dl deadlineState
}

// Open starts a consumer over [off, off+length) of f (length < 0: to the end of the file).
// A stream's window and priorities apply at once, not at the next tick.
func (t *Torrent) Open(f File, off, length int64, kind Kind) (*Stream, error) {
	if f.Offset < 0 || f.Length < 0 || f.Offset+f.Length > t.ts.Length() || off < 0 || off > f.Length {
		return nil, ErrRange
	}
	end := f.Length
	if length >= 0 && off+length < end {
		end = off + length
	}
	s := &Stream{t: t, kind: kind, file: f, start: off, end: end, done: make(chan struct{}), modePiece: -1}
	s.seq.Store(t.s.seq.Add(1))
	s.pos.Store(off)
	s.touch.Store(time.Now().UnixNano())
	t.mu.Lock()
	if t.dead || t.ts.Closed() {
		t.mu.Unlock()
		return nil, ErrClosed
	}
	t.streams[s] = struct{}{}
	if kind == KindStream {
		t.aggrOn = true // a new stream boosts early buffering (V231); a demand read does not
	}
	t.mu.Unlock()
	if f := t.s.cfg.OnOpen; f != nil {
		f(t.ts.Hash(), off, length, kind)
	}
	t.trigger()
	return s, nil
}

// SetLead records the bytes the consumer holds past Pos, ready for its player (negative: 0).
func (s *Stream) SetLead(n int64) { s.lead.Store(max(n, 0)) }

// SetBitrate records the film's bytes per second of content: the stall risk measures the
// buffer at that pace instead of the pump's drain.
func (s *Stream) SetBitrate(n int64) { s.bitrate.Store(max(n, 0)) }

// Lead is the last SetLead value.
func (s *Stream) Lead() int64 { return s.lead.Load() }

// Pos is the file offset of the next byte Read returns.
func (s *Stream) Pos() int64 { return s.pos.Load() }

// Read returns the bytes from Pos, waiting until at least one is ready; then it takes what
// is contiguous without waiting more. io.EOF only at the end of the range. A wait during
// which no piece of the torrent receives data for the idle timeout (doubled in STRICT) is
// ErrIdle.
func (s *Stream) Read(ctx context.Context, p []byte) (int, error) {
	if s.closed.Load() {
		return 0, ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	s.busy.Add(1)
	defer s.busy.Add(-1)
	ts := s.t.ts
	pl := ts.PieceLen()
	n := 0
	var idle *time.Timer
	var lastWrites uint64
	var lastState piecestore.State
	defer func() {
		if idle != nil {
			idle.Stop()
		}
	}()
	for {
		pos := s.pos.Load()
		if pos >= s.end || n == len(p) {
			if n == 0 {
				return 0, io.EOF
			}
			return n, nil
		}
		abs := s.file.Offset + pos
		pi := int(abs / pl)
		piece := ts.Piece(pi)
		ch := piece.Changed() // before the read: a write in between is not missed
		if ts.Closed() {
			return n, closedErr(n)
		}
		if pi != s.modePiece {
			s.modePiece, s.responsive = pi, ts.Responsive() // FAST/STRICT fixed per piece
		}
		po := abs - int64(pi)*pl
		want := min(int64(len(p)-n), piece.Length()-po, s.end-pos)
		m, err := piece.Read(p[n:n+int(want)], po, !s.responsive)
		if m > 0 {
			n += m
			s.advance(abs, int64(m))
			continue
		}
		if n > 0 {
			return n, nil
		}
		if err != nil && !errors.Is(err, piecestore.ErrNotReady) {
			if ts.Closed() {
				return 0, ErrClosed
			}
			return 0, err
		}
		var paused <-chan struct{} // nil: never fires
		if s.kind == KindPrefetch {
			paused = s.t.s.pausedCh()
			select {
			case <-paused:
				if s.t.othersAtRisk() {
					return 0, ErrPaused
				}
				paused = nil // only this torrent is at risk: its own prefetch goes on
			default:
			}
		}
		if idle == nil {
			idle = time.NewTimer(s.idleTimeout())
			lastWrites, lastState = piece.WriteCount(), piece.State()
			if s.waitAt.Load() != int64(pi)+1 {
				s.waitSeq.Store(s.t.s.seq.Add(1))
				s.waitAt.Store(int64(pi) + 1)
				s.t.trigger() // its piece now queues for the swarm
			}
		}
		select {
		case <-ch:
			if w, st := piece.WriteCount(), piece.State(); w != lastWrites || st != lastState {
				lastWrites, lastState = w, st
				idle.Reset(s.idleTimeout())
			}
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-s.done:
			return 0, ErrClosed
		case <-paused:
			if s.t.othersAtRisk() {
				return 0, ErrPaused
			}
		case <-idle.C:
			// the swarm is stopped only if the whole torrent got nothing (it may be serving
			// older waiters' pieces first)
			if d := s.idleTimeout() - time.Since(ts.LastData()); d > 0 {
				idle.Reset(d)
				continue
			}
			return 0, ErrIdle
		}
	}
}

func closedErr(n int) error {
	if n > 0 {
		return nil
	}
	return ErrClosed
}

func (s *Stream) idleTimeout() time.Duration {
	d := s.t.s.cfg.Tunables().IdleTimeout
	if d <= 0 {
		d = 15 * time.Second
	}
	if !s.responsive {
		d *= 2 // verified pieces take longer (8f6eaa9)
	}
	return d
}

func (s *Stream) advance(abs, m int64) {
	s.pos.Add(m)
	s.touch.Store(time.Now().UnixNano())
	s.dl.sample(m, time.Now())
	pl := s.t.ts.PieceLen()
	if (abs+m)/pl != abs/pl || s.pos.Load() >= s.end {
		s.t.trigger() // the window moves with the consumer
	}
}

// Close ends the consumer: its priorities and the deadlines it installed are cleared on the
// next pass.
func (s *Stream) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	close(s.done)
	t := s.t
	t.mu.Lock()
	delete(t.streams, s)
	if r := s.dl.clear(); r.n > 0 {
		t.clears = append(t.clears, r)
	}
	t.mu.Unlock()
	t.trigger()
}

func (s *Stream) span() span {
	sp := span{kind: s.Kind(), fileStart: s.file.Offset, fileEnd: s.file.Offset + s.file.Length,
		pos: s.file.Offset + s.pos.Load(), end: s.file.Offset + s.end, seq: s.seq.Load()}
	if pl := s.t.ts.PieceLen(); pl > 0 && s.waitAt.Load() == sp.pos/pl+1 {
		sp.waiting = s.waitSeq.Load()
	}
	return sp
}

func (s *Stream) idle(now time.Time) bool {
	return s.busy.Load() == 0 && now.Sub(time.Unix(0, s.touch.Load())) > parkAfter
}

// fileWindow is every piece of the stream's file.
func (s *Stream) fileWindow() piecestore.Window {
	pl, np := s.t.ts.PieceLen(), s.t.ts.NumPieces()
	sp := s.span()
	return piecestore.Window{First: pieceOf(sp.fileStart, pl), Last: lastPiece(max(sp.fileEnd, sp.fileStart+1), sp.fileEnd, pl, np)}
}

// Demote ends a demand once its caller has its bytes: the rest of the range is a prefetch
// (Readahead, unpinned) until a reader blocks on it again (Await).
func (s *Stream) Demote() {
	if s.kind == KindDemand && s.demoted.CompareAndSwap(false, true) {
		s.t.trigger()
	}
}

// Await marks a reader blocked on byte off of the range: a demoted stream is a demand again,
// its wait beginning now, until done is called; a demand still waiting keeps its place. off
// may be behind Pos (bytes still in the pipe).
func (s *Stream) Await(off int64) (done func()) {
	if off < s.start || off >= s.end || s.closed.Load() {
		return func() {}
	}
	if s.waiters.Add(1) == 1 && s.demoted.Load() {
		s.seq.Store(s.t.s.seq.Add(1))
	}
	s.t.trigger()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.waiters.Add(-1)
			s.t.trigger()
		})
	}
}

// Kind is the consumer kind the scheduler applies now.
func (s *Stream) Kind() Kind {
	if s.kind == KindDemand && s.demoted.Load() && s.waiters.Load() == 0 {
		return KindPrefetch
	}
	return s.kind
}
