package streamer

import (
	"sync"
	"sync/atomic"
	"time"

	"tiramisu/internal/piecestore"
)

// HelpWindow is the part of a friend's playback a help downloads: pieces First..Last with
// k mod N == I, kept resident; earlier ones from Guard (the watcher's position) only while it lacks them.
type HelpWindow struct{ Guard, First, Last, I, N int }

func (w HelpWindow) share(i int) bool { return i%w.N == w.I }

// Empty reports a window with no piece to download.
func (w HelpWindow) Empty() bool { return w.Last < w.First }

// Fit bounds w to the torrent and to capacity bytes from First to Last. The pass bounds it again
// to the help's part of the cache, less the earlier pieces it keeps (fitHelps).
func (w HelpWindow) Fit(numPieces int, pieceLen, capacity int64) HelpWindow {
	w.N = min(max(w.N, 1), 2)
	w.I = min(max(w.I, 0), w.N-1)
	w.First = max(w.First, 0)
	w.Guard = min(max(w.Guard, 0), w.First)
	room := int(capacity / max(pieceLen, 1))
	w.Last = min(w.Last, numPieces-1, w.First+room-1)
	return w
}

// Help is a KindHelp consumer of one torrent.
type Help struct {
	t       *Torrent
	mu      sync.Mutex
	w       HelpWindow
	onPiece func(i int)
	closed  atomic.Bool

	reported map[int]bool // pass only: share pieces already handed to onPiece

	taken atomic.Pointer[func(pieces []int) []bool] // which pieces the watcher has; nil: none known
}

// SetTaken tells the help which pieces the watcher has (its Haves on the friend link): earlier
// pieces it has are no longer kept for it.
func (h *Help) SetTaken(f func(pieces []int) []bool) { h.taken.Store(&f) }

// OpenHelp registers a help over w; onPiece (may be nil) is told, from the scheduler and without
// blocking, of every verified piece of the share. It returns the window the next pass applies.
func (t *Torrent) OpenHelp(w HelpWindow, onPiece func(i int)) (*Help, HelpWindow, error) {
	h := &Help{t: t, onPiece: onPiece, reported: map[int]bool{}}
	h.w = w.Fit(t.ts.NumPieces(), t.ts.PieceLen(), t.ts.Capacity())
	applied := t.fitNow(h.w, h)
	t.mu.Lock()
	if t.dead || t.ts.Closed() {
		t.mu.Unlock()
		return nil, HelpWindow{}, ErrClosed
	}
	t.helps[h] = struct{}{}
	t.mu.Unlock()
	if f := t.s.cfg.OnOpen; f != nil {
		f(t.ts.Hash(), int64(h.w.First)*t.ts.PieceLen(), int64(h.w.Last-h.w.First+1)*t.ts.PieceLen(), KindHelp)
	}
	t.trigger()
	return h, applied, nil
}

// Update moves the help to a new window: its levels move in the next pass. It returns the window
// that pass applies, as the consumers registered now leave room for it.
func (h *Help) Update(w HelpWindow) HelpWindow {
	w = w.Fit(h.t.ts.NumPieces(), h.t.ts.PieceLen(), h.t.ts.Capacity())
	h.mu.Lock()
	h.w = w
	h.mu.Unlock()
	h.t.trigger()
	return h.t.fitNow(w, h) // outside h.mu: the snapshot takes t.mu, then h.mu
}

// Window is the window asked, bounded to the cache; each pass may bound it further (fitHelps).
func (h *Help) Window() HelpWindow {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.w
}

// Close ends the help: its levels are cleared on the next pass, its pieces become evictable.
func (h *Help) Close() {
	if !h.closed.CompareAndSwap(false, true) {
		return
	}
	t := h.t
	t.mu.Lock()
	delete(t.helps, h)
	t.mu.Unlock()
	t.trigger()
}

func (h *Help) span() span {
	w := h.Window()
	pl := h.t.ts.PieceLen()
	return span{kind: KindHelp, pos: int64(w.First) * pl, end: int64(w.Last+1) * pl, help: h, hw: w}
}

// reserved is what the planned helps will still download: their share's missing bytes.
func (t *Torrent) reserved(plan []span) int64 {
	var n int64
	np := t.ts.NumPieces()
	for _, sp := range plan {
		if sp.kind != KindHelp {
			continue
		}
		for i := sp.hw.First; i <= sp.hw.Last && i < np; i++ {
			if p := t.ts.Piece(i); sp.hw.share(i) && !p.Complete() {
				n += max(p.Length()-p.Allocated(), 0)
			}
		}
	}
	return n
}

// reportHelped hands every newly verified share piece of each help to its onPiece.
func (t *Torrent) reportHelped(spans []span) {
	np := t.ts.NumPieces()
	for _, sp := range spans {
		h := sp.help
		if sp.kind != KindHelp || h == nil || h.onPiece == nil {
			continue
		}
		for i := range h.reported { // out of the window (passed, or ahead of a seek back)
			if i < sp.hw.Guard || i > sp.hw.Last {
				delete(h.reported, i)
			}
		}
		for i := sp.hw.Guard; i <= sp.hw.Last && i < np; i++ {
			if sp.hw.share(i) && !h.reported[i] && t.ts.Piece(i).Complete() {
				h.reported[i] = true
				h.onPiece(i)
			}
		}
	}
}

// Playback is the playing stream of a torrent: torrent offsets of its position, of the end of its
// read-ahead (R) and of its file, its rate (film bitrate, else measured drain) and its measured
// drain (0: unknown).
type Playback struct {
	Pos, ReadEnd, FileEnd int64
	Rate, Drain           int64
	PieceLen              int64
	NumPieces             int
}

// PlaybackWindow reports the playing stream of hash, the one that moved last; false without one.
func (s *Streamer) PlaybackWindow(hash string) (Playback, bool) {
	s.mu.Lock()
	var t *Torrent
	for ts, tt := range s.tors {
		if ts.Hash() == hash && !ts.Closed() {
			t = tt
			break
		}
	}
	s.mu.Unlock()
	if t == nil {
		return Playback{}, false
	}
	t.mu.Lock()
	var best *Stream
	for st := range t.streams {
		if st.kind == KindStream && !st.closed.Load() && (best == nil || st.touch.Load() > best.touch.Load()) {
			best = st
		}
	}
	t.mu.Unlock()
	if best == nil {
		return Playback{}, false
	}
	sp := best.span()
	ra := best.readahead.Load()
	if ra <= 0 { // before its first pass
		ra = computeShares(s.cfg.Tunables(), t.ts.PieceLen(), 1, false, 0, 0).readahead
	}
	drain := int64(best.dl.consumption())
	rate := best.bitrate.Load()
	if rate <= 0 {
		rate = drain
	}
	return Playback{Pos: sp.pos, ReadEnd: min(sp.pos+ra, sp.fileEnd), FileEnd: sp.fileEnd, Rate: rate, Drain: drain,
		PieceLen: t.ts.PieceLen(), NumPieces: t.ts.NumPieces()}, true
}

// Tick is the scheduler's pass cadence.
func (s *Streamer) Tick() time.Duration { return s.cfg.Tick }

// helpRoom is each help's part of the cache: what the streams' protected and edge windows and the
// demand and prefetch windows leave (overlaps counted twice: never more), split among the helps.
func helpRoom(spans []span, sh shares, numPieces int) int64 {
	helps, used := 0, int64(0)
	size := func(w piecestore.Window) int64 { return int64(max(w.Last-w.First+1, 0)) * sh.pieceLen }
	for _, sp := range spans {
		switch sp.kind {
		case KindHelp:
			helps++
		case KindStream:
			used += sh.protected
			for _, w := range edgeWindows(sp, sh.pieceLen, numPieces) {
				used += size(w)
			}
		case KindDemand, KindPrefetch:
			used += size(demandWindow(sp, sh.pieceLen, numPieces))
		}
	}
	return max(sh.capacity-used, 0) / int64(max(helps, 1))
}

// fitHelps bounds every help span to its part of the cache: the earlier share pieces the watcher
// still lacks first, then the window from First with what is left.
func (t *Torrent) fitHelps(spans []span, sh shares, numPieces int) {
	room := helpRoom(spans, sh, numPieces)
	for i := range spans {
		sp := &spans[i]
		if sp.kind != KindHelp {
			continue
		}
		var cand []int
		for k := sp.hw.Guard; k < sp.hw.First && k < numPieces; k++ {
			if sp.hw.share(k) && t.ts.Piece(k).Complete() {
				cand = append(cand, k)
			}
		}
		var lacks []bool
		if f := sp.help.takenFunc(); f != nil && len(cand) > 0 {
			lacks = f(cand)
		}
		left := room
		for k, piece := range cand {
			if lacks != nil && lacks[k] {
				continue // the watcher has it
			}
			if left < sh.pieceLen {
				break
			}
			sp.early = append(sp.early, piece)
			left -= sh.pieceLen
		}
		sp.hw = sp.hw.Fit(numPieces, sh.pieceLen, left)
		sp.end = int64(sp.hw.Last+1) * sh.pieceLen
	}
}

func (h *Help) takenFunc() func([]int) []bool {
	if h == nil {
		return nil
	}
	if p := h.taken.Load(); p != nil {
		return *p
	}
	return nil
}

// fitNow fits w as the next pass will, from the consumers registered now.
func (t *Torrent) fitNow(w HelpWindow, self *Help) HelpWindow {
	pl, np := t.ts.PieceLen(), t.ts.NumPieces()
	var spans []span
	streams := 0
	t.mu.Lock()
	for s := range t.streams {
		spans = append(spans, s.span())
		if s.kind == KindStream {
			streams++
		}
	}
	for h := range t.helps {
		if h != self {
			spans = append(spans, span{kind: KindHelp})
		}
	}
	t.mu.Unlock()
	spans = append(spans, span{kind: KindHelp})
	sh := computeShares(t.s.cfg.Tunables(), pl, streams, false, 0, 0)
	return w.Fit(np, pl, helpRoom(spans, sh, np))
}
