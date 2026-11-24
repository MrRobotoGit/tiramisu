package engine

import (
	"context"
	"encoding/hex"
	"io"
	"net"
	"net/netip"
	"slices"
	"time"

	"tiramisu/internal/engine/bt"
	"tiramisu/internal/friends"
)

// friendsClient is the Manager's view of the BT client: always the current one, which Reconnect
// replaces.
type friendsClient struct{ e *Engine }

func (f friendsClient) DialControl(ctx context.Context, addr string, ih, id [20]byte) (io.ReadWriter, net.Conn, [20]byte, error) {
	c := f.e.currentClient()
	if c == nil {
		return nil, nil, [20]byte{}, ErrNotConnected
	}
	return c.DialControl(ctx, addr, ih, id)
}

func (f friendsClient) FreshPublicAddr(ctx context.Context) (netip.AddrPort, error) {
	if f.e.currentClient() == nil { // no lookup between clients
		return netip.AddrPort{}, ErrNotConnected
	}
	ip, _ := f.e.freshPublicIPs(ctx, false)
	c := f.e.currentClient() // after the lookup: the port of the client in use now
	if c == nil {
		return netip.AddrPort{}, ErrNotConnected
	}
	a, ok := netip.AddrFromSlice(ip.To4())
	if !ok {
		return netip.AddrPort{}, friends.ErrNoPublicAddr
	}
	return netip.AddrPortFrom(a, uint16(c.LocalPort())), nil
}

// initFriends builds the friends Manager over the engine's DB; without a DB there is none.
func (e *Engine) initFriends() {
	if e.deps.DB == nil {
		return
	}
	m, err := friends.New(e.deps.DB, friendsClient{e}, func() time.Time { return e.now() })
	if err != nil {
		e.deps.logf("[Friends] disabled: %v", err)
		return
	}
	m.Logf = e.deps.logf
	m.CloseFriendConns = func(match func(ih, peerID [20]byte) bool) {
		if c := e.currentClient(); c != nil {
			c.CloseFriendConns(match)
		}
	}
	// The first friend, invite or request starts the timers and address reads (none for installs
	// without). Under the Manager's lock: only goroutines start here.
	m.Go = func(f func(context.Context)) { e.goFriends(func() { f(e.friendsCtx) }) }
	// A friend's new address: dial it on the torrents being played.
	m.OnFriendAddrs = func(addrs []string) {
		e.goFriends(func() {
			for _, t := range e.ListHydrated() {
				e.dialFriends(t, dialPlay, addrs...)
			}
		})
	}
	m.OnActive = func() {
		if !(e.friendsStarted.Load() && e.launchFriends()) {
			e.goFriends(e.friendsAddrChange)
		}
	}
	m.SetActivity(e.friendActivity)
	e.friends = m
	e.friendsCtx, e.friendsStop = context.WithCancel(context.Background())
}

// Friends is the friends Manager; nil when the engine has no DB.
func (e *Engine) Friends() *friends.Manager {
	if e == nil {
		return nil
	}
	return e.friends
}

func (e *Engine) friendOptions(o *bt.Options) {
	if e.friends != nil {
		o.Friends, o.OnHandshake = e.friends, e.friends.OnHandshake
		o.OnFriendLinkClosed = func(ih [20]byte) {
			hash := hex.EncodeToString(ih[:])
			e.goFriends(func() { e.dialFriends(e.Get(hash), dialPlay) })
		}
		o.OnFriendHave = e.friendHave
	}
}

// friendHave counts a friend's Have on a torrent we watch with helpers (client lock held).
func (e *Engine) friendHave(ih [20]byte, piece int) {
	if v, ok := e.meters.Load(ih); ok {
		v.(*deliveryMeter).have(piece, time.Now())
	}
}

// FriendsPlayback opens (on) or closes a playback session of hash: a webhook confirmation keyed by
// path, or a first player read past the header keyed by its handle. The first open session dials
// the friends; while any is open, a closing friend link or a friend's new address dials again.
// Scans hydrate and probe headers, and a friend refuses torrents it has not hydrated: only
// playback is worth the dials (sockets, DH on both sides). The sessions of a hash end at the
// latest when its torrent expires or is removed, so a lost stop cannot keep it wanted.
func (e *Engine) FriendsPlayback(hash, session, reason string, on bool) {
	if e == nil || e.friends == nil {
		return
	}
	hash, err := ParseHash(hash)
	if err != nil {
		return
	}
	e.friendsPlayMu.Lock()
	ss := e.friendsPlaying[hash]
	first := on && len(ss) == 0
	if on {
		if ss == nil {
			ss = map[string]struct{}{}
			e.friendsPlaying[hash] = ss
		}
		ss[session] = struct{}{}
		if first {
			e.startWatchLocked(hash)
		}
	} else if ss != nil {
		delete(ss, session)
		if len(ss) == 0 {
			delete(e.friendsPlaying, hash)
			e.endWatchLocked(hash)
		}
	}
	e.friendsPlayMu.Unlock()
	if first && e.friends.FriendCount() > 0 {
		e.deps.logf("[Friends] dial on %s: %s", hash8(hash), reason)
		e.goFriends(func() { e.dialFriends(e.Get(hash), dialPlay) })
	}
}

// coldIntentMax bounds the opens remembered for torrents not registered yet (hydrations that
// never register); past it the oldest one goes.
const coldIntentMax = 256

// FriendsColdOpen dials the friends once per hydration of hash: a file opened without local
// warmup, in parallel with the swarm and never waited for.
func (e *Engine) FriendsColdOpen(hash string) {
	if e == nil || e.friends == nil || e.friends.FriendCount() == 0 {
		return
	}
	hash, err := ParseHash(hash)
	if err != nil {
		return
	}
	e.coldMu.Lock()
	t := e.lookup(hash)
	if t == nil { // consumed when the torrent registers (addSpec)
		if e.friendsColdIntent == nil {
			e.friendsColdIntent = map[string]uint64{}
		}
		if _, ok := e.friendsColdIntent[hash]; !ok && len(e.friendsColdIntent) >= coldIntentMax {
			oldest, at := "", uint64(0)
			for h, n := range e.friendsColdIntent {
				if oldest == "" || n < at {
					oldest, at = h, n
				}
			}
			delete(e.friendsColdIntent, oldest)
		}
		e.coldSeq++
		e.friendsColdIntent[hash] = e.coldSeq
		e.coldMu.Unlock()
		return
	}
	e.coldMu.Unlock()
	t.friendsCold.Store(true)
	if t.State() == StateWorking { // else watchInfo dials on the move to Working
		e.friendsColdDial(t)
	}
}

// friendsColdIntentTake consumes an open made before t registered.
func (e *Engine) friendsColdIntentTake(t *Torrent) {
	e.coldMu.Lock()
	defer e.coldMu.Unlock()
	if _, ok := e.friendsColdIntent[t.hash]; ok {
		delete(e.friendsColdIntent, t.hash)
		t.friendsCold.Store(true)
	}
}

// friendsColdDial dials once per hydration.
func (e *Engine) friendsColdDial(t *Torrent) {
	if !t.friendsColdDialed.CompareAndSwap(false, true) {
		return
	}
	e.deps.logf("[Friends] dial on %s: cold open", hash8(t.hash))
	e.goFriends(func() { e.dialFriends(t, dialCold) })
}

// FriendsPlaying reports whether a playback session of hash is open.
func (e *Engine) FriendsPlaying(hash string) bool {
	h, err := ParseHash(hash)
	return err == nil && e != nil && e.friends != nil && e.friendsWanted(h)
}

// friendsWanted reports whether a playback session of hash is open.
func (e *Engine) friendsWanted(hash string) bool {
	e.friendsPlayMu.Lock()
	defer e.friendsPlayMu.Unlock()
	return len(e.friendsPlaying[hash]) > 0
}

// friendsPlaybackEnd drops every session of hash: its torrent expired or was removed.
func (e *Engine) friendsPlaybackEnd(hash string) {
	e.friendsPlayMu.Lock()
	delete(e.friendsPlaying, hash)
	e.endWatchLocked(hash)
	e.friendsPlayMu.Unlock()
}

func hash8(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// Why friends are dialled: a playback dials again on events, the others once.
const (
	dialPlay = "play"
	dialCold = "cold"
)

// dialFriends dials the friends with a known address (only those at addrs, if given) for t,
// outside the connection limit, while t is working (and, for dialPlay, being played). Called on
// events only: the first player read, a cold open, a friend's new address, a friend link closing.
func (e *Engine) dialFriends(t *Torrent, reason string, addrs ...string) {
	m := e.friends
	if m == nil || t == nil || t.State() != StateWorking || m.FriendCount() == 0 || reason == dialPlay && !e.friendsWanted(t.hash) {
		return
	}
	ih, err := hex.DecodeString(t.hash)
	if err != nil || len(ih) != 20 || t.bt == nil {
		return
	}
	var dialled []string
	for _, p := range m.FriendPeers([20]byte(ih)) {
		if len(addrs) == 0 || slices.Contains(addrs, p.Addr) {
			e.friendDial(t.bt, p.Addr, p.ID)
			dialled = append(dialled, p.Addr)
		}
	}
	if len(dialled) > 0 {
		e.deps.logf("[Friends] %s: dialling %v (a link, if any, logs as proven)", hash8(t.hash), dialled)
	}
}

// startFriends runs at Start: the friends timers begin now if there is anything to serve (a
// friend, invite, request or tombstone), otherwise at the first one (OnActive).
func (e *Engine) startFriends() {
	if e.friends == nil {
		return
	}
	e.friendsStarted.Store(true)
	if e.friends.WantConns() {
		e.launchFriends()
	}
}

// launchFriends, once per process, reads the address, resumes pending requests and runs the
// Manager's timers until Close. It reports whether this call started them.
func (e *Engine) launchFriends() bool {
	if !e.friendsLaunched.CompareAndSwap(false, true) {
		return false
	}
	m, ctx := e.friends, e.friendsCtx
	e.goFriends(func() {
		m.Reannounce() // sources are in memory only: relearn them both ways (not under OnActive's lock)
		e.friendsAddrChange()
		m.Resume(ctx)
		tk := time.NewTicker(e.friendsAddrEvery)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				e.friendsAddrChange()
			}
		}
	})
	e.goFriends(func() { m.Run(ctx) })
	return true
}

// friendsAddrChange reads the public address, only when the Manager has something to announce.
// Reads run one at a time, so an older read never lands after a newer one.
func (e *Engine) friendsAddrChange() {
	e.friendsAddrMu.Lock()
	defer e.friendsAddrMu.Unlock()
	if !e.friends.Active() || e.friendsCtx.Err() != nil {
		return
	}
	if err := e.friends.OnAddrChange(e.friendsCtx); err != nil && e.friendsCtx.Err() == nil {
		e.deps.logf("[Friends] public address: %v", err)
	}
}

// friendsReconnected starts a new reachability check and re-reads the address after a Reconnect.
func (e *Engine) friendsReconnected() {
	if e.friends == nil {
		return
	}
	e.friends.ResetReachability()
	e.friends.Reannounce() // our source may have changed: friends answer with theirs
	e.goFriends(e.friendsAddrChange)
}

// goFriends runs f in a goroutine that Close waits for; none after Close.
func (e *Engine) goFriends(f func()) {
	e.friendsMu.Lock()
	defer e.friendsMu.Unlock()
	if e.friendsClosed {
		return
	}
	e.friendsWG.Add(1)
	go func() {
		defer e.friendsWG.Done()
		f()
	}()
}

// stopFriends stops the Manager's goroutines and waits for them.
func (e *Engine) stopFriends() {
	e.friendsMu.Lock()
	e.friendsClosed = true
	e.friendsMu.Unlock()
	if e.friendsStop != nil {
		e.friendsStop()
	}
	e.friendsWG.Wait()
}

func (e *Engine) clearColdIntents() {
	e.coldMu.Lock()
	e.friendsColdIntent = nil
	e.coldMu.Unlock()
}
