package friends

import (
	"context"
	"encoding/hex"
	"io"
	"sync"
	"time"
)

// MaxHelpers is how many friends help one playback at most: the window splits k mod N, N ≤ 2.
const MaxHelpers = 2

// Window is a watcher's help window: pieces First..Last with k mod N == I, the watcher reading at
// torrent offset Pos at Bps bytes/s. Seq grows with every window of a session.
type Window struct {
	Hash              [20]byte
	Nonce             string
	Seq               int64
	Pos, Bps          int64
	First, Last, I, N int
}

// valid applies the checks every received window must pass.
func (w Window) valid() bool {
	return w.N >= 1 && w.N <= MaxHelpers && w.I >= 0 && w.I < w.N && w.First >= 0 && w.First <= w.Last &&
		w.Pos >= 0 && w.Bps >= 0 && w.Seq > 0
}

// HelpHooks serve a friend's playback; the engine sets them. They run in the connection goroutine
// of the friend's control dial and must return quickly.
type HelpHooks struct {
	// OnWatch decides whether to help friend's playback of hash in session nonce.
	OnWatch func(friend string, hash [20]byte, nonce string) bool
	// OnWindow applies a window of an accepted session; false ends the help.
	OnWindow func(friend string, w Window) bool
	// OnUnwatch ends a session, accepted or refused: the watcher stopped, or the friend was removed.
	OnUnwatch func(friend string, hash [20]byte, nonce string)
}

type helpKey struct {
	friend string
	hash   [20]byte
}

// helpSess is a session we accepted to help: the windows must carry its nonce and a newer seq.
type helpSess struct {
	nonce   string
	seq     int64
	refused bool // we said no: only its unwatch is expected
}

type helpState struct {
	mu   sync.Mutex
	sess map[helpKey]*helpSess
	last map[helpKey]int64 // per friend and film, while its accepted session lives: an older watch is a replay
}

// SetHelp enables (nil disables) helping friends' playbacks.
func (m *Manager) SetHelp(h *HelpHooks) { m.helpHooks.Store(h) }

func (m *Manager) helpFriends() []*friendEntry {
	var out []*friendEntry
	for _, f := range m.snap.Load().friends {
		if f.addr != "" {
			out = append(out, f)
		}
	}
	return out
}

// exchange sends msg to f on the pair's control channel and reads one answer.
func (m *Manager) exchange(ctx context.Context, f *friendEntry, msg Msg) (Msg, error) {
	msg.MAC = SignPair(f.pair, f.role, msg, "")
	rw, conn, err := m.dialFriend(ctx, f)
	if err != nil {
		return Msg{}, err
	}
	defer conn.Close()
	if err := WriteMsg(rw, msg); err != nil {
		return Msg{}, err
	}
	ans, err := ReadMsg(rw)
	if err != nil {
		return Msg{}, err
	}
	if !CheckPair(f.pair, f.role.Other(), ans, "") || ans.Hash != msg.Hash || ans.Nonce != msg.Nonce || ans.Seq != msg.Seq {
		return Msg{}, ErrUnexpected
	}
	f.noteControlSource(conn.RemoteAddr())
	m.noteContact(f)
	return ans, nil
}

// NotifyWatch tells every friend with a known address that we play hash in session nonce and
// returns, in friend order, those that answered they help.
func (m *Manager) NotifyWatch(ctx context.Context, hash [20]byte, nonce string) []string {
	helpers, _ := m.NotifyWatchAll(ctx, hash, nonce)
	return helpers
}

// NotifyWatchAll is NotifyWatch that also lists every friend notified, helping or not: the end of
// the playback is told to all of them.
func (m *Manager) NotifyWatchAll(ctx context.Context, hash [20]byte, nonce string) (helpers, notified []string) {
	seq := m.watchSeq()
	fs := m.helpFriends()
	ok := make([]bool, len(fs))
	var wg sync.WaitGroup
	for i, f := range fs {
		wg.Go(func() {
			ans, err := m.exchange(ctx, f, Msg{Type: "watch", Hash: hex.EncodeToString(hash[:]), Nonce: nonce, Seq: seq})
			ok[i] = err == nil && ans.Type == "help" && ans.OK
			if err != nil && ctx.Err() == nil {
				m.logf("watch to %q: %v", f.name, err)
			}
		})
	}
	wg.Wait()
	for i, f := range fs {
		notified = append(notified, f.name)
		if ok[i] {
			helpers = append(helpers, f.name)
		}
	}
	return helpers, notified
}

// SendWindow sends w to friend; ok=false when the friend no longer helps that session.
func (m *Manager) SendWindow(ctx context.Context, friend string, w Window) (bool, error) {
	f := m.friendByName(friend)
	if f == nil {
		return false, ErrNotFound
	}
	ans, err := m.exchange(ctx, f, Msg{Type: "win", Hash: hex.EncodeToString(w.Hash[:]), Nonce: w.Nonce, Seq: w.Seq,
		Pos: w.Pos, Bps: w.Bps, First: w.First, Last: w.Last, I: w.I, N: w.N})
	if err != nil {
		return false, err
	}
	return ans.Type == "help" && ans.OK, nil
}

// NotifyUnwatch tells the helpers that session nonce of hash ended.
func (m *Manager) NotifyUnwatch(ctx context.Context, helpers []string, hash [20]byte, nonce string) {
	var wg sync.WaitGroup
	for _, name := range helpers {
		f := m.friendByName(name)
		if f == nil {
			continue
		}
		wg.Go(func() {
			if _, err := m.exchange(ctx, f, Msg{Type: "unwatch", Hash: hex.EncodeToString(hash[:]), Nonce: nonce}); err != nil && ctx.Err() == nil {
				m.logf("unwatch to %q: %v", f.name, err)
			}
		})
	}
	wg.Wait()
}

// serveHelp answers a watcher's help message; an invalid one gets no answer.
func (m *Manager) serveHelp(f *friendEntry, msg Msg, rw io.ReadWriter) {
	if !CheckPair(f.pair, f.role.Other(), msg, "") {
		return
	}
	var hash [20]byte
	if b, err := hex.DecodeString(msg.Hash); err != nil || len(b) != 20 {
		return
	} else {
		hash = [20]byte(b)
	}
	if _, ok := decodeNonce(msg.Nonce); !ok {
		return
	}
	h := m.helpHooks.Load()
	key := helpKey{f.name, hash}
	reply := Msg{Type: "help", Hash: msg.Hash, Nonce: msg.Nonce, Seq: msg.Seq}
	switch msg.Type {
	case "watch":
		// Stored before the engine sees it: an end racing the acceptance removes it.
		sess := &helpSess{nonce: msg.Nonce}
		m.help.mu.Lock()
		if msg.Seq <= m.help.last[key] {
			m.help.mu.Unlock()
			return // a replayed watch: no answer, no takeover
		}
		if m.help.sess == nil {
			m.help.sess, m.help.last = map[helpKey]*helpSess{}, map[helpKey]int64{}
		}
		m.help.last[key] = msg.Seq
		m.help.sess[key] = sess
		m.help.mu.Unlock()
		reply.OK = h != nil && h.OnWatch != nil && h.OnWatch(f.name, hash, msg.Nonce)
		m.help.mu.Lock()
		if !reply.OK && m.help.sess[key] == sess {
			sess.refused = true // kept until its unwatch, so the end of that playback is known
			delete(m.help.last, key)
			for k, s := range m.help.sess { // only the newest refusal per friend: it is all the status shows
				if k.friend == f.name && k != key && s.refused {
					delete(m.help.sess, k)
				}
			}
		}
		reply.OK = reply.OK && m.help.sess[key] == sess
		m.help.mu.Unlock()
	case "win":
		w := Window{Hash: hash, Nonce: msg.Nonce, Seq: msg.Seq, Pos: msg.Pos, Bps: msg.Bps, First: msg.First, Last: msg.Last, I: msg.I, N: msg.N}
		if !w.valid() {
			return
		}
		m.help.mu.Lock()
		s := m.help.sess[key]
		fresh := s != nil && !s.refused && s.nonce == msg.Nonce && msg.Seq > s.seq
		if fresh {
			s.seq = msg.Seq
		}
		m.help.mu.Unlock()
		if !fresh {
			if s == nil || s.refused || s.nonce != msg.Nonce {
				reply.OK = false // not a session we help (any more)
				break
			}
			return // a replayed or stale window
		}
		reply.OK = h != nil && h.OnWindow != nil && h.OnWindow(f.name, w)
		if !reply.OK {
			m.endHelp(key, msg.Nonce)
		}
	case "unwatch":
		if msg.Seq != 0 {
			return
		}
		if m.endHelp(key, msg.Nonce) && h != nil && h.OnUnwatch != nil {
			h.OnUnwatch(f.name, hash, msg.Nonce)
		}
		reply.Type = "ok"
	default:
		return
	}
	reply.MAC = SignPair(f.pair, f.role, reply, "")
	_ = WriteMsg(rw, reply)
}

// endHelp forgets session nonce of key; false if it was not the current one.
func (m *Manager) endHelp(key helpKey, nonce string) bool {
	m.help.mu.Lock()
	defer m.help.mu.Unlock()
	if s := m.help.sess[key]; s != nil && s.nonce == nonce {
		delete(m.help.sess, key)
		delete(m.help.last, key) // only a live session orders the watches: a clock gone back is fine after
		return true
	}
	return false
}

// EndHelp forgets a session the engine stopped helping: its next window is answered "no".
func (m *Manager) EndHelp(friend string, hash [20]byte, nonce string) {
	m.endHelp(helpKey{friend, hash}, nonce)
}

// dropHelps ends every session helped for friend (removed).
func (m *Manager) dropHelps(friend string) {
	m.help.mu.Lock()
	var gone []helpKey
	var nonces []string
	for k, s := range m.help.sess {
		if k.friend == friend {
			gone = append(gone, k)
			nonces = append(nonces, s.nonce)
			delete(m.help.sess, k)
		}
	}
	for k := range m.help.last {
		if k.friend == friend {
			delete(m.help.last, k)
		}
	}
	m.help.mu.Unlock()
	if h := m.helpHooks.Load(); h != nil && h.OnUnwatch != nil {
		for i, k := range gone {
			h.OnUnwatch(k.friend, k.hash, nonces[i])
		}
	}
}

// FriendPeerID is the peer ID friend presents on ih.
func (m *Manager) FriendPeerID(friend string, ih [20]byte) ([20]byte, bool) {
	if f := m.friendByName(friend); f != nil {
		return PeerID(f.pair, ih, f.role.Other()), true
	}
	return [20]byte{}, false
}

// FriendAddr is friend's last known address; "" if unknown or not a friend.
func (m *Manager) FriendAddr(friend string) string {
	if f := m.friendByName(friend); f != nil {
		return f.addr
	}
	return ""
}

// watchSeq orders our watches: wall-clock nanoseconds, strictly increasing, so a restart still
// counts on from where the helper last saw us.
func (m *Manager) watchSeq() int64 {
	for {
		last := m.lastWatch.Load()
		next := max(time.Now().UnixNano(), last+1)
		if m.lastWatch.CompareAndSwap(last, next) {
			return next
		}
	}
}
