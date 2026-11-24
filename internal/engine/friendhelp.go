package engine

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"tiramisu/internal/engine/bt"
	"tiramisu/internal/friends"
	"tiramisu/internal/streamer"
)

const dialHelp = "help"

// SetStreamer gives the engine the piece scheduler: with it the node helps its friends' playbacks
// and asks them to help its own. nil disables both.
func (e *Engine) SetStreamer(st *streamer.Streamer) {
	if e == nil {
		return
	}
	e.streamer.Store(st)
	if e.friends == nil {
		return
	}
	if st == nil {
		e.friends.SetHelp(nil)
		return
	}
	e.friends.SetHelp(&friends.HelpHooks{OnWatch: e.onWatch, OnWindow: e.onWindow, OnUnwatch: e.onUnwatch, OnBack: e.onFriendBack})
}

// maxHelps is FriendsMaxHelps: 0 (unset) is the default, negative turns helping off.
func (c Config) maxHelps() int {
	switch {
	case c.FriendsMaxHelps == 0:
		return defaultMaxHelps
	case c.FriendsMaxHelps < 0:
		return 0
	}
	return c.FriendsMaxHelps
}

const defaultMaxHelps = 2

type helpKey struct{ friend, hash string }

// helping is one friend's playback we help: a background slot from the watch on, then the
// hydrated torrent, a KindHelp span and its consumer from the first window on.
type helping struct {
	friend, hash, nonce string
	title               string
	pieceLen            int64
	numPieces           int
	slot                func()

	mu         sync.Mutex
	win        friends.Window // the last window asked
	started    bool
	ended      bool
	tor        *Torrent
	release    func()
	help       *streamer.Help
	unskip     func() // downloads from the watcher resume
	timer      *time.Timer
	done       chan struct{}
	logState   string
	windows    int       // windows received
	logWindows int       // windows when the last line was logged
	lastLog    time.Time // last "helping" line
}

type helpRegistry struct {
	mu      sync.Mutex
	byKey   map[helpKey]*helping
	refused map[string]refusal // friend -> its last watch refused, for the status view
}

// refusal is a refused watch and its session: the watcher's unwatch of that session clears it.
type refusal struct {
	friends.Refusal
	nonce string
}

// helpCount is the number of helps held (tests and logs).
func (e *Engine) helpCount() int {
	e.helps.mu.Lock()
	defer e.helps.mu.Unlock()
	return len(e.helps.byKey)
}

// onWatch decides whether to help friend's playback of hash: a row in the library, a help under
// the cap, a background slot free (behind the owner's playback and scans) and upload allowed.
// A refusal of a film in the library is kept for the status view, with its reason.
func (e *Engine) onWatch(friend string, ih [20]byte, nonce string) bool {
	hash := hex.EncodeToString(ih[:])
	cfg := e.config()
	row := e.row(hash)
	if row == nil || len(row.InfoBytes) == 0 {
		return false
	}
	refuse := func(why string) bool {
		e.helps.mu.Lock()
		e.noteRefusalLocked(friend, hash, nonce, row.Title, why)
		e.helps.mu.Unlock()
		return false
	}
	switch {
	case e.streamer.Load() == nil || e.currentClient() == nil:
		return refuse("unavailable")
	case cfg.maxHelps() == 0:
		return refuse("off")
	case cfg.DisableUpload:
		return refuse("upload_off")
	}
	_, pieceLen, numPieces, err := bt.InfoFiles(row.InfoBytes)
	if err != nil || pieceLen <= 0 {
		return refuse("unavailable")
	}
	key := helpKey{friend, hash}
	e.helps.mu.Lock()
	defer e.helps.mu.Unlock()
	if old := e.helps.byKey[key]; old != nil { // a new session of the same playback: it takes over
		old.mu.Lock()
		ending := old.ended
		if !ending {
			old.nonce = nonce
		}
		old.mu.Unlock()
		if !ending {
			e.armFirstWindow(old, cfg)
			delete(e.helps.refused, friend)
			return true
		}
		delete(e.helps.byKey, key) // ending: its slot goes back on its own, a new help starts
	}
	if len(e.helps.byKey) >= cfg.maxHelps() {
		e.noteRefusalLocked(friend, hash, nonce, row.Title, "cap")
		return false
	}
	slot, ok := e.takeBackgroundSlot()
	if !ok {
		e.noteRefusalLocked(friend, hash, nonce, row.Title, "busy")
		return false
	}
	hp := &helping{friend: friend, hash: hash, title: row.Title, nonce: nonce, pieceLen: pieceLen, numPieces: numPieces, slot: slot, done: make(chan struct{})}
	if e.helps.byKey == nil {
		e.helps.byKey = map[helpKey]*helping{}
	}
	e.helps.byKey[key] = hp
	delete(e.helps.refused, friend)
	e.armFirstWindow(hp, cfg)
	e.deps.logf("[Friends] help accepted for %s on %s", friend, hash8(hash))
	return true
}

// noteRefusalLocked keeps friend's last refused watch; under helps.mu.
func (e *Engine) noteRefusalLocked(friend, hash, nonce, title, why string) {
	if e.helps.refused == nil {
		e.helps.refused = map[string]refusal{}
	}
	e.helps.refused[friend] = refusal{friends.Refusal{Hash: hash, Title: title, Reason: why, At: e.now().Unix()}, nonce}
}

// armFirstWindow ends hp if no window arrives within the torrent expiry timeout.
func (e *Engine) armFirstWindow(hp *helping, cfg Config) {
	hp.mu.Lock()
	defer hp.mu.Unlock()
	if hp.timer != nil {
		hp.timer.Stop()
	}
	hp.timer = time.AfterFunc(cfg.timeout(), func() { e.endHelp(hp, "no window") })
}

func (e *Engine) helpOf(friend, hash, nonce string) *helping {
	e.helps.mu.Lock()
	hp := e.helps.byKey[helpKey{friend, hash}]
	e.helps.mu.Unlock()
	if hp == nil {
		return nil
	}
	hp.mu.Lock()
	defer hp.mu.Unlock()
	if hp.ended || hp.nonce != nonce {
		return nil
	}
	return hp
}

// onWindow applies a window of a helped session: the first starts the help, the next move it. The
// help ends past the window's horizon unless another window comes.
func (e *Engine) onWindow(friend string, w friends.Window) bool {
	hash := hex.EncodeToString(w.Hash[:])
	hp := e.helpOf(friend, hash, w.Nonce)
	if hp == nil {
		return false
	}
	hp.mu.Lock()
	if hp.ended { // ended since the lookup
		hp.mu.Unlock()
		return false
	}
	hp.win = w
	start := !hp.started
	hp.started = true
	h := hp.help
	hp.mu.Unlock()
	// The window as applied: the streamer's, or before the span opens our whole cache.
	applied := helpWindowOf(w, hp.pieceLen).Fit(hp.numPieces, hp.pieceLen, e.config().CacheSize)
	if h != nil {
		applied = h.Update(helpWindowOf(w, hp.pieceLen))
		e.logHelp(hp, applied, w)
	}
	e.armHorizon(hp, w, applied)
	if start {
		e.goFriends(func() { e.startHelp(hp) })
	}
	return true
}

// armHorizon ends hp when the watcher, at its pace, reaches the end of the applied window w.
func (e *Engine) armHorizon(hp *helping, w friends.Window, applied streamer.HelpWindow) {
	rate := max(w.Bps, streamer.MinRate)
	horizon := time.Duration(float64(max(int64(applied.Last+1)*hp.pieceLen-w.Pos, 0)) / float64(rate) * float64(time.Second))
	hp.mu.Lock()
	defer hp.mu.Unlock()
	if hp.ended || hp.win != w { // a newer window arms its own
		return
	}
	if hp.timer != nil {
		hp.timer.Stop()
	}
	hp.timer = time.AfterFunc(horizon, func() { e.endHelp(hp, "past the window's horizon") })
}

func helpWindowOf(w friends.Window, pieceLen int64) streamer.HelpWindow {
	return streamer.HelpWindow{Guard: int(w.Pos / pieceLen), First: w.First, Last: w.Last, I: w.I, N: w.N}
}

// logHelp logs the help when its share or the bound of its cache changes.
func (e *Engine) logHelp(hp *helping, applied streamer.HelpWindow, asked friends.Window) {
	clamp := ""
	if applied.Last < asked.Last {
		clamp = fmt.Sprintf(" (cache bound: %d pieces from R)", applied.Last-applied.First+1)
	}
	state := fmt.Sprintf("%d/%d%s", applied.I+1, applied.N, clamp)
	hp.mu.Lock()
	changed := state != hp.logState
	hp.logState = state
	hp.mu.Unlock()
	hp.mu.Lock()
	hp.windows++
	now := time.Now()
	due := now.Sub(hp.lastLog) >= watchLogEvery
	n := hp.windows - hp.logWindows
	if changed || due {
		hp.lastLog, hp.logWindows = now, hp.windows
	}
	hp.mu.Unlock()
	if changed || due {
		e.deps.logf("[Friends] helping %s on %s: share %d/%d, pieces %d-%d of %d-%d asked%s; %d windows since the last line",
			hp.friend, hash8(hp.hash), applied.I+1, applied.N, applied.First, applied.Last, asked.First, asked.Last, clamp, n)
	}
}

// startHelp hydrates the torrent as a background consumer, dials the watcher and opens the span.
func (e *Engine) startHelp(hp *helping) {
	fail := func(why string) { e.endHelp(hp, why) }
	t, err := e.Add(hp.hash, Meta{})
	if err != nil {
		fail("add: " + err.Error())
		return
	}
	select {
	case <-t.working:
	case <-t.closed:
		fail("torrent closed")
		return
	case <-hp.done:
		return
	case <-e.friendsCtx.Done():
		fail("engine closed")
		return
	case <-time.After(e.infoWait(e.config())):
		fail("no metadata")
		return
	}
	st := e.streamer.Load()
	ts, ctl := t.Store(), t.Pieces()
	if st == nil || ts == nil || ctl == nil {
		fail("no store")
		return
	}
	if e.helpControl != nil {
		ctl = e.helpControl(ctl)
	}
	release, ok := t.Acquire() // the torrent does not expire while the help runs
	if !ok {
		fail("torrent closed")
		return
	}
	hp.mu.Lock()
	w := hp.win
	hp.mu.Unlock()
	pieces := make(chan int, int(ts.Capacity()/max(ts.PieceLen(), 1))+1)
	var onPiece func(int)
	if d := e.serve.warm.Load(); d != nil {
		onPiece = func(i int) {
			select {
			case pieces <- i:
			default: // the SSD is behind: this piece stays in RAM only
			}
		}
		e.goFriends(func() { e.writeHelped(hp, t, pieces) })
	}
	h, applied, err := st.Torrent(ts, ctl).OpenHelp(helpWindowOf(w, hp.pieceLen), onPiece)
	if err != nil {
		release()
		fail("streamer: " + err.Error())
		return
	}
	h.SetTaken(t.bt.FriendsHave) // pieces the watcher already has are not kept for it
	unskip := e.skipWatcher(hp, t)
	hp.mu.Lock()
	if hp.ended { // ended while starting
		hp.mu.Unlock()
		h.Close()
		unskip()
		release()
		return
	}
	hp.tor, hp.release, hp.help, hp.unskip = t, release, h, unskip
	hp.mu.Unlock()
	for { // the windows that came meanwhile; onWindow applies the later ones itself
		hp.mu.Lock()
		w = hp.win
		hp.mu.Unlock()
		applied = h.Update(helpWindowOf(w, hp.pieceLen))
		hp.mu.Lock()
		same := hp.win == w
		hp.mu.Unlock()
		if same {
			break
		}
	}
	e.logHelp(hp, applied, w)
	e.armHorizon(hp, w, applied)
	if m := e.friends; m != nil {
		if addr := m.FriendAddr(hp.friend); addr != "" {
			e.dialFriends(t, dialHelp, addr)
		}
	}
	select {
	case <-t.closed:
		e.endHelp(hp, "torrent closed")
	case <-e.friendsCtx.Done():
		e.endHelp(hp, "engine closed")
	case <-hp.done:
	}
}

// skipWatcher stops downloading this torrent from the watcher during the help: whatever it holds,
// it needs no help with. The returned func resumes; other torrents are not affected.
func (e *Engine) skipWatcher(hp *helping, t *Torrent) func() {
	ih, err := hex.DecodeString(hp.hash)
	if e.friends == nil || err != nil || len(ih) != 20 || t.bt == nil {
		return func() {}
	}
	id, ok := e.friends.FriendPeerID(hp.friend, [20]byte(ih))
	if !ok {
		return func() {}
	}
	t.bt.SetNoRequestsFrom(id, true)
	return func() { t.bt.SetNoRequestsFrom(id, false) }
}

// writeHelped keeps every verified piece of the share in the SSD warmup as mid pieces.
func (e *Engine) writeHelped(hp *helping, t *Torrent, pieces <-chan int) {
	d := e.serve.warm.Load()
	ts := t.Store()
	if d == nil || ts == nil {
		return
	}
	files := t.Files()
	buf := make([]byte, ts.PieceLen()) // WriteMidPiece copies or writes it before returning
	for {
		var i int
		select {
		case i = <-pieces:
		case <-hp.done:
			return
		case <-e.friendsCtx.Done():
			return
		}
		p := ts.Piece(i)
		if p == nil {
			continue
		}
		buf := buf[:p.Length()]
		if n, err := p.Read(buf, 0, true); err != nil || int64(n) != p.Length() {
			continue // evicted or not verified: nothing unverified reaches the SSD
		}
		off := int64(i) * ts.PieceLen()
		for _, f := range files {
			lo, hi := max(off, f.Offset), min(off+int64(len(buf)), f.Offset+f.Length)
			if lo < hi {
				d.WriteMidPiece(hp.hash, f.ID, lo-f.Offset, buf[lo-off:hi-off])
			}
		}
	}
}

// onUnwatch ends the help of a session the watcher closed, or forgets its refusal.
func (e *Engine) onUnwatch(friend string, ih [20]byte, nonce string) {
	hash := hex.EncodeToString(ih[:])
	e.helps.mu.Lock()
	if r, ok := e.helps.refused[friend]; ok && r.Hash == hash && r.nonce == nonce { // a refused session ends too
		delete(e.helps.refused, friend)
	}
	e.helps.mu.Unlock()
	if hp := e.helpOf(friend, hash, nonce); hp != nil {
		e.endHelp(hp, "the watcher stopped")
	}
}

// endHelp closes the span, gives back the consumer and the slot; the torrent then expires as usual.
func (e *Engine) endHelp(hp *helping, why string) {
	hp.mu.Lock()
	if hp.ended {
		hp.mu.Unlock()
		return
	}
	hp.ended = true
	close(hp.done)
	if hp.timer != nil {
		hp.timer.Stop()
	}
	h, release, unskip, nonce := hp.help, hp.release, hp.unskip, hp.nonce
	hp.mu.Unlock()
	if h != nil {
		h.Close()
	}
	if unskip != nil {
		unskip()
	}
	if release != nil {
		release()
	}
	e.helps.mu.Lock()
	if e.helps.byKey[helpKey{hp.friend, hp.hash}] == hp {
		delete(e.helps.byKey, helpKey{hp.friend, hp.hash})
	}
	e.helps.mu.Unlock()
	hp.slot()
	if m := e.friends; m != nil {
		if ih, err := hex.DecodeString(hp.hash); err == nil && len(ih) == 20 {
			m.EndHelp(hp.friend, [20]byte(ih), nonce)
		}
	}
	e.deps.logf("[Friends] help for %s on %s ended: %s", hp.friend, hash8(hp.hash), why)
}

// endHelps ends every help (Close, Reconnect).
func (e *Engine) endHelps(why string) {
	e.helps.mu.Lock()
	var all []*helping
	for _, hp := range e.helps.byKey {
		all = append(all, hp)
	}
	e.helps.mu.Unlock()
	for _, hp := range all {
		e.endHelp(hp, why)
	}
}
