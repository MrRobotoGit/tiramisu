package friends

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// pingState tracks, in memory only, whether each friend answered in the last maintenance periods.
type pingState struct {
	mu      sync.Mutex
	start   time.Time // first use in this process: older pings are replays
	peers   map[[20]byte]*pingPeer
	running atomic.Bool
	lastSeq atomic.Int64
	served  atomic.Int64 // pings answered
}

type pingPeer struct {
	contacts, counted uint64 // authenticated control contacts, and how many the last round saw
	misses            int    // consecutive periods without an answer
	known, online     bool
	checked           int64 // unix time of the last verdict
	lastSeq           int64 // newest ping seq received from the friend
	retry             backoff
	oldNoted          bool
}

type pingResult int

const (
	pingFailed  pingResult = iota
	pingOK                 // authenticated pong
	pingOld                // the friend accepted the channel but predates ping
	pingRemoved            // the friend removed us
)

func (p *pingState) peerLocked(h [20]byte, now time.Time) *pingPeer {
	if p.start.IsZero() {
		p.start = now
	}
	if p.peers == nil {
		p.peers = map[[20]byte]*pingPeer{}
	}
	pp := p.peers[h]
	if pp == nil {
		pp = &pingPeer{}
		p.peers[h] = pp
	}
	return pp
}

// noteContact records an authenticated control exchange with f: no ping is needed this period.
func (m *Manager) noteContact(f *friendEntry) {
	m.ping.mu.Lock()
	m.ping.peerLocked(f.control, m.now()).contacts++
	m.ping.mu.Unlock()
}

func (p *pingState) contactsOf(h [20]byte) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pp := p.peers[h]; pp != nil {
		return pp.contacts
	}
	return 0
}

// presence is the verdict on the friend with control hash h; ok is false before the first one.
func (m *Manager) presence(h [20]byte) (online bool, checked int64, ok bool) {
	m.ping.mu.Lock()
	defer m.ping.mu.Unlock()
	if pp := m.ping.peers[h]; pp != nil && pp.known {
		return pp.online, pp.checked, true
	}
	return false, 0, false
}

// pingSeq orders our pings: nanoseconds on our clock, strictly increasing.
func (m *Manager) pingSeq() int64 {
	for {
		last := m.ping.lastSeq.Load()
		next := max(m.now().UnixNano(), last+1)
		if m.ping.lastSeq.CompareAndSwap(last, next) {
			return next
		}
	}
}

// servePing answers an authenticated ping; only a fresh one counts as contact.
func (m *Manager) servePing(f *friendEntry, msg Msg, rw io.ReadWriter) {
	if msg.Seq <= 0 || !CheckPair(f.pair, f.role.Other(), msg, "") {
		return
	}
	if _, ok := decodeNonce(msg.Nonce); !ok {
		return
	}
	m.ping.served.Add(1)
	m.ping.mu.Lock()
	pp := m.ping.peerLocked(f.control, m.now())
	if msg.Seq > pp.lastSeq && !time.Unix(0, msg.Seq).Before(m.ping.start) {
		pp.lastSeq = msg.Seq
		pp.contacts++
	}
	m.ping.mu.Unlock()
	pong := Msg{Type: "pong", Nonce: msg.Nonce, Seq: msg.Seq}
	pong.MAC = SignPair(f.pair, f.role, pong, "")
	_ = WriteMsg(rw, pong)
}

// sendPing asks f whether it is up.
func (m *Manager) sendPing(ctx context.Context, f *friendEntry) pingResult {
	msg := Msg{Type: "ping", Nonce: hex.EncodeToString(randomBytes(16)), Seq: m.pingSeq()}
	msg.MAC = SignPair(f.pair, f.role, msg, "")
	rw, conn, err := m.dialFriend(ctx, f)
	if err != nil {
		return pingFailed
	}
	defer conn.Close()
	if err := WriteMsg(rw, msg); err != nil {
		return pingFailed
	}
	ans, err := ReadMsg(rw)
	switch {
	case errors.Is(err, io.EOF):
		// The handshake carried the friend's pair ID, then it closed without a word: an older binary.
		return pingOld
	case err != nil:
		return pingFailed
	case ans.Type == "pong" && ans.Nonce == msg.Nonce && ans.Seq == msg.Seq && CheckPair(f.pair, f.role.Other(), ans, ""):
		f.noteControlSource(conn.RemoteAddr())
		return pingOK
	case ans.Type == "removed" && CheckPair(f.pair, f.role.Other(), ans, ""):
		return pingRemoved
	}
	return pingFailed
}

func activityLive(a *Activity) bool {
	return a != nil && len(a.Links)+len(a.HelpingYou)+len(a.YouHelp) > 0
}

// pingRound closes one maintenance period: friends with live traffic are online, the others are
// pinged unless they are on backoff, and two silent periods in a row make a friend offline.
func (m *Manager) pingRound(ctx context.Context) {
	if !m.ping.running.CompareAndSwap(false, true) {
		return
	}
	defer m.ping.running.Store(false)
	now := m.now()
	fs := m.snap.Load().friends
	var act map[string]*Activity
	if a := m.activity.Load(); a != nil {
		act = (*a)()
	}
	unreachable := map[[20]byte]bool{}
	m.mu.Lock()
	for _, f := range fs {
		if b := m.addrRetry[f.control]; b != nil && b.n > 0 && b.at.After(now) {
			unreachable[f.control] = true
		}
	}
	m.mu.Unlock()

	var due, back []*friendEntry
	m.ping.mu.Lock()
	live := map[[20]byte]bool{}
	for _, f := range fs {
		live[f.control] = true
		pp := m.ping.peerLocked(f.control, now)
		contact := pp.contacts != pp.counted
		pp.counted = pp.contacts
		switch {
		case contact || activityLive(act[f.name]):
			if m.settleLocked(f, pp, true, false, now) {
				back = append(back, f)
			}
		case f.addr == "" || unreachable[f.control] || pp.retry.at.After(now):
			m.settleLocked(f, pp, false, false, now)
		default:
			due = append(due, f)
		}
	}
	for h := range m.ping.peers {
		if !live[h] {
			delete(m.ping.peers, h)
		}
	}
	m.ping.mu.Unlock()

	res := make([]pingResult, len(due))
	var wg sync.WaitGroup
	for i, f := range due {
		wg.Go(func() { res[i] = m.sendPing(ctx, f) })
	}
	wg.Wait()
	if ctx.Err() != nil {
		return // shutting down: no verdict from cancelled dials
	}
	for i, f := range due {
		switch res[i] {
		case pingRemoved:
			m.logf("%q removed us while we were away", f.name)
			m.dropFriend(f, false)
			continue
		case pingOK, pingOld:
			f.seen.Store(m.now().Unix())
		}
		m.ping.mu.Lock()
		if pp := m.ping.peers[f.control]; pp != nil {
			if res[i] == pingOld && !pp.oldNoted {
				pp.oldNoted = true
				m.logf("%q runs an older version without presence checks", f.name)
			}
			if m.settleLocked(f, pp, res[i] != pingFailed, true, now) {
				back = append(back, f)
			}
		}
		m.ping.mu.Unlock()
	}
	for _, f := range back {
		m.friendBack(f.name)
	}
}

// settleLocked records one period's outcome; tried marks a ping that failed, which backs off once
// the friend is offline. It reports a friend that answers again after being offline.
func (m *Manager) settleLocked(f *friendEntry, pp *pingPeer, answered, tried bool, now time.Time) (back bool) {
	if answered {
		back = pp.known && !pp.online
		if back {
			m.logf("%q answers again", f.name)
		}
		pp.known, pp.online, pp.misses, pp.checked = true, true, 0, now.Unix()
		pp.retry = backoff{}
		return back
	}
	pp.misses++
	if pp.misses < 2 {
		return false
	}
	if pp.known && pp.online {
		m.logf("%q no longer answers", f.name)
	}
	pp.known, pp.online, pp.checked = true, false, now.Unix()
	if tried {
		m.bump(&pp.retry)
	}
	return false
}
