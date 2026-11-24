package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"sync"
	"time"

	"tiramisu/internal/friends"
	"tiramisu/internal/streamer"
)

// watchLogEvery spaces the watcher's window log lines; the events themselves are not throttled.
const watchLogEvery = time.Minute

// watchSession is our playback of hash with up to friends.MaxHelpers friends helping it.
type watchSession struct {
	hash  string
	ih    [20]byte
	nonce string
	stop  chan struct{}
	once  sync.Once
	wake  chan struct{} // cap 1: a friend is back and can be asked again

	notified map[string]string // friend -> the session nonce it was told (runWatch only)

	mu      sync.Mutex
	helpers []string        // the friends helping now, for the status view
	back    map[string]bool // friends that restarted, moved or answer again since the last wake
}

func (w *watchSession) end() { w.once.Do(func() { close(w.stop) }) }

func (w *watchSession) setHelpers(h []string) {
	w.mu.Lock()
	w.helpers = append([]string(nil), h...)
	w.mu.Unlock()
}

// markBack queues friend for runWatch; never blocks.
func (w *watchSession) markBack(friend string) {
	w.mu.Lock()
	if w.back == nil {
		w.back = map[string]bool{}
	}
	w.back[friend] = true
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *watchSession) takeBack() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := w.back
	w.back = nil
	return b
}

func (w *watchSession) helping() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.helpers
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// startWatch asks the friends to help the first playback session of hash; under friendsPlayMu.
func (e *Engine) startWatchLocked(hash string) {
	if e.streamer.Load() == nil || e.friends.FriendCount() == 0 || e.watching[hash] != nil {
		return
	}
	ih, err := hex.DecodeString(hash)
	if err != nil || len(ih) != 20 {
		return
	}
	ws := &watchSession{hash: hash, ih: [20]byte(ih), nonce: newNonce(), stop: make(chan struct{}), wake: make(chan struct{}, 1)}
	if e.watching == nil {
		e.watching = map[string]*watchSession{}
	}
	e.watching[hash] = ws
	e.goFriends(func() { e.runWatch(ws) })
}

// endWatchLocked ends the watch of hash: the helpers are told; under friendsPlayMu.
func (e *Engine) endWatchLocked(hash string) {
	if ws := e.watching[hash]; ws != nil {
		ws.end()
		delete(e.watching, hash)
	}
}

// onFriendBack lets every playback ask friend again (it restarted, moved or answers again).
func (e *Engine) onFriendBack(friend string) {
	e.friendsPlayMu.Lock()
	defer e.friendsPlayMu.Unlock()
	for _, ws := range e.watching {
		ws.markBack(friend)
	}
}

// runWatch notifies the friends, then sends the helpers a window on events only: the first one,
// the read-ahead entering the window, a seek, a change of helpers. A free place is offered to a
// friend that comes back, and after a lost helper to the friends not helping. At the end it unwatches.
func (e *Engine) runWatch(ws *watchSession) {
	m, ctx, st := e.friends, e.friendsCtx, e.streamer.Load()
	if st == nil {
		return
	}
	defer e.unwatchAll(ctx, ws)
	helpers := e.askHelpers(ctx, ws, ws.nonce)
	meter := &deliveryMeter{}
	e.meters.Store(ws.ih, meter)
	defer e.meters.CompareAndDelete(ws.ih, meter) // a newer watch of the same film keeps its own

	var (
		sent        bool
		first, last int
		pos         int64
		seq         int64
		changed     bool
		rewatched   bool
		lastLog     time.Time
		windows     int
		asked       int                 // pieces in all the windows sent
		lead        int                 // pieces between R and the window: grows while the watcher outruns the helpers
		logWindows  int                 // windows sent when the last line was logged
		logPieces   int64               // pieces past R from the helpers then
		failed      = map[string]bool{} // the last window to this helper did not get through
	)
	tick := time.NewTicker(max(st.Tick(), 10*time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-ws.stop:
			if windows == 0 {
				return
			}
			e.deps.logf("[Friends] watching %s ended: %d windows, %d pieces past the read-ahead from %v; %.1f pieces a window",
				hash8(ws.hash), windows, meter.total.Load(), helpers, float64(asked)/float64(max(windows, 1)))
			return
		case <-ctx.Done():
			return
		case <-ws.wake:
			// A helper that comes back may have restarted and forgotten the session: asked again too.
			back := ws.takeBack()
			stay := slices.DeleteFunc(slices.Clone(helpers), func(h string) bool { return back[h] })
			next := e.recallHelpers(ctx, ws, stay, func(f string) bool { return back[f] })
			if !slices.Equal(next, helpers) || len(stay) != len(helpers) {
				helpers, changed = next, true
				ws.setHelpers(helpers)
				for f := range back {
					delete(failed, f)
				}
			}
			continue
		case <-tick.C:
		}
		if len(helpers) == 0 {
			continue // until a friend comes back
		}
		pb, ok := st.PlaybackWindow(ws.hash)
		if !ok || pb.PieceLen <= 0 {
			continue
		}
		seek := sent && (pb.Pos < pos || pb.Pos >= int64(last+1)*pb.PieceLen)
		entered := sent && pb.ReadEnd > int64(first)*pb.PieceLen
		pos = pb.Pos
		if sent && !seek && !entered && !changed {
			continue
		}
		now := time.Now()
		if sent {
			got, pending := meter.delivered()
			lead = nextLead(lead, seek || entered, got, pending, int(e.config().CacheSize/pb.PieceLen))
		}
		c := meter.estimate(pb.PieceLen, now)
		lastPiece := int((pb.FileEnd - 1) / pb.PieceLen)
		f, l := helpWindow(pb.Pos, pb.ReadEnd+int64(lead)*pb.PieceLen, pb.Rate, int64(c), e.config().CacheSize, pb.PieceLen, lastPiece, len(helpers))
		if l < f {
			continue // the read-ahead reached the end of the file
		}
		var held func(int) bool
		if t := e.Get(ws.hash); t != nil {
			if ts := t.Store(); ts != nil {
				held = func(i int) bool { return ts.Piece(i).Complete() }
			}
		}
		meter.next(f, l, pb.PieceLen, now, held)
		first, last, sent, changed = f, l, true, false
		seq++
		windows++
		asked += l - f + 1
		kept, lost := e.sendWindows(ctx, helpers, failed, friends.Window{Hash: ws.ih, Nonce: ws.nonce, Seq: seq, Pos: pb.Pos, Bps: horizonRate(pb), First: f, Last: l})
		if len(lost) > 0 { // unreachable twice: told best-effort that the session is over for them
			m.NotifyUnwatch(ctx, lost, ws.ih, ws.nonce)
		}
		if now.Sub(lastLog) >= watchLogEvery || len(kept) != len(helpers) {
			got := meter.total.Load()
			e.deps.logf("[Friends] watching %s: window %d-%d (R at piece %d, film %.2f MB/s, helpers deliver %.2f MB/s) to %v; %d windows and %d pieces from the helpers since %s",
				hash8(ws.hash), f, l, pb.ReadEnd/pb.PieceLen, float64(pb.Rate)/(1<<20), c/(1<<20), kept,
				windows-logWindows, got-logPieces, since(lastLog, now))
			lastLog, logWindows, logPieces = now, windows, got
		}
		if len(kept) != len(helpers) {
			gone := map[string]bool{}
			for _, h := range helpers {
				gone[h] = !slices.Contains(kept, h)
			}
			helpers, changed = kept, true
			ws.setHelpers(helpers)
			if len(helpers) == 0 && !rewatched { // lost them all (a pause past the horizon): ask once more
				rewatched = true
				ws.nonce = newNonce()
				helpers = e.askHelpers(ctx, ws, ws.nonce)
			} else { // a free place: the others, not those just lost
				helpers = e.recallHelpers(ctx, ws, helpers, func(f string) bool { return !gone[f] })
			}
		}
	}
}

// askHelpers notifies the friends and keeps at most friends.MaxHelpers of those that accepted;
// the others are told the session is over.
func (e *Engine) askHelpers(ctx context.Context, ws *watchSession, nonce string) []string {
	m := e.friends
	helpers, notified := m.NotifyWatchAll(ctx, ws.ih, nonce)
	if ws.notified == nil {
		ws.notified = map[string]string{}
	}
	for _, name := range notified {
		ws.notified[name] = nonce
	}
	if len(helpers) > friends.MaxHelpers {
		m.NotifyUnwatch(ctx, helpers[friends.MaxHelpers:], ws.ih, nonce)
		helpers = helpers[:friends.MaxHelpers]
	}
	e.deps.logf("[Friends] watching %s: helpers %v", hash8(ws.hash), helpers)
	ws.setHelpers(helpers)
	return helpers
}

// recallHelpers asks the friends want accepts and not helping now to help session ws.nonce; it
// returns helpers grown to at most friends.MaxHelpers, the extra ones told the session is over.
func (e *Engine) recallHelpers(ctx context.Context, ws *watchSession, helpers []string, want func(string) bool) []string {
	room := friends.MaxHelpers - len(helpers)
	if room <= 0 {
		return helpers
	}
	got, notified := e.friends.NotifyWatchSome(ctx, ws.ih, ws.nonce, func(f string) bool {
		return want(f) && !slices.Contains(helpers, f)
	})
	for _, name := range notified {
		ws.notified[name] = ws.nonce
	}
	if len(got) > room {
		e.friends.NotifyUnwatch(ctx, got[room:], ws.ih, ws.nonce)
		got = got[:room]
	}
	if len(got) == 0 {
		return helpers
	}
	helpers = append(slices.Clip(helpers), got...)
	e.deps.logf("[Friends] watching %s: %v join, helpers %v", hash8(ws.hash), got, helpers)
	ws.setHelpers(helpers)
	return helpers
}

// sendWindows sends w to every helper, helper i taking the pieces k mod N == i. It returns those
// still helping and those dropped after a second failed send in a row; a helper that answers it
// no longer helps is dropped at once. failed carries the failures from one window to the next.
func (e *Engine) sendWindows(ctx context.Context, helpers []string, failed map[string]bool, w friends.Window) (kept, lost []string) {
	ok := make([]bool, len(helpers))
	errs := make([]error, len(helpers))
	var wg sync.WaitGroup
	for i, name := range helpers {
		wg.Go(func() {
			hw := w
			hw.I, hw.N = i, len(helpers)
			ok[i], errs[i] = e.friends.SendWindow(ctx, name, hw)
			if errs[i] != nil && ctx.Err() == nil {
				e.deps.logf("[Friends] window to %s: %v", name, errs[i])
			}
		})
	}
	wg.Wait()
	for i, name := range helpers {
		switch {
		case ok[i]:
			delete(failed, name)
			kept = append(kept, name)
		case errs[i] != nil && !failed[name]: // retried at the next event
			failed[name] = true
			kept = append(kept, name)
		case errs[i] != nil:
			delete(failed, name)
			lost = append(lost, name)
		}
	}
	return kept, lost
}

// unwatchAll tells every friend notified of the watch, helping or not, that the playback ended
// (best-effort; never after Close).
func (e *Engine) unwatchAll(ctx context.Context, ws *watchSession) {
	if ctx.Err() != nil {
		return
	}
	byNonce := map[string][]string{}
	for name, nonce := range ws.notified {
		byNonce[nonce] = append(byNonce[nonce], name)
	}
	for nonce, names := range byNonce {
		e.friends.NotifyUnwatch(ctx, names, ws.ih, nonce)
	}
}

// since names the interval a log line covers: from the previous line, or the start.
func since(last, now time.Time) string {
	if last.IsZero() {
		return "the start"
	}
	return now.Sub(last).Truncate(time.Second).String() + " ago"
}

// horizonRate is the pace the helper's horizon assumes: the measured drain, but never above the
// film's rate, since a player that buffered ahead stops reading while it plays.
func horizonRate(pb streamer.Playback) int64 {
	if pb.Drain > 0 && pb.Rate > 0 {
		return min(pb.Drain, pb.Rate)
	}
	return pb.Drain
}
