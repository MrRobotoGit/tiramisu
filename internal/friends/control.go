package friends

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// dialFriend opens the pair's control channel, encrypted unless the friend is an older node that
// has never encrypted: then the channel is plain, signed as before.
func (m *Manager) dialFriend(ctx context.Context, f *friendEntry) (io.ReadWriter, net.Conn, error) {
	if m.noHello || m.plainFriend(f) {
		return m.dialPlainChecked(ctx, f)
	}
	rw, conn, err := m.dialPlain(ctx, f)
	if err != nil {
		return nil, nil, err
	}
	srw, old, err := m.helloSecure(ctx, f, rw)
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(m.ioTimeout)) // the opening used part of the first
		return srw, conn, nil
	}
	conn.Close()
	if !old {
		return nil, nil, err
	}
	if f.secure.Load() {
		m.refusePlain(f, fmt.Sprintf("%q closed the encrypted opening, but it encrypted before: plaintext refused. "+
			"An older version, a network fault and a block look the same here: reset its encryption only if you know it went back to an older version", f.name))
		return nil, nil, errSecRefused // never back to plaintext with a friend that encrypts
	}
	m.notePlain(f)
	return m.dialPlainChecked(ctx, f)
}

// dialPlainChecked dials in clear, unless the friend was meanwhile seen encrypting.
func (m *Manager) dialPlainChecked(ctx context.Context, f *friendEntry) (io.ReadWriter, net.Conn, error) {
	rw, conn, err := m.dialPlain(ctx, f)
	if err == nil && f.secure.Load() {
		conn.Close()
		return nil, nil, errSecRefused
	}
	return rw, conn, err
}

// dialPlain opens the pair's control channel and checks the reply ID is the friend's side.
// Only the ControlHash is dialled: a hash the other node knows (no bad-accept penalty).
func (m *Manager) dialPlain(ctx context.Context, f *friendEntry) (io.ReadWriter, net.Conn, error) {
	if f.addr == "" {
		return nil, nil, errors.New("friends: no known address")
	}
	dctx, cancel := context.WithTimeout(ctx, m.ioTimeout)
	defer cancel()
	rw, conn, remote, err := m.client.DialControl(dctx, f.addr, f.control, PeerID(f.pair, f.control, f.role))
	if err != nil {
		return nil, nil, err
	}
	conn = closeOnCancel(ctx, conn)
	if !VerifyPeerID(f.pair, f.control, f.role.Other(), remote) {
		conn.Close()
		return nil, nil, errors.New("friends: control channel answered by someone else")
	}
	_ = conn.SetDeadline(time.Now().Add(m.ioTimeout))
	return rw, conn, nil
}

// cancelConn closes on ctx cancellation, so an exchange in progress stops too.
type cancelConn struct {
	net.Conn
	stop func() bool
}

func (c cancelConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

func closeOnCancel(ctx context.Context, conn net.Conn) net.Conn {
	return cancelConn{conn, context.AfterFunc(ctx, func() { conn.Close() })}
}

func (m *Manager) friendByName(name string) *friendEntry {
	for _, f := range m.snap.Load().friends {
		if f.name == name {
			return f
		}
	}
	return nil
}

// Reannounce makes the next address round contact every friend once, acknowledged or not, asking
// each to answer with its own: after a start or Reconnect both sides learn each other's source.
func (m *Manager) Reannounce() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.snap.Load().friends {
		m.reannounce[f.control] = true
	}
}

// OnAddrChange reads the public address fresh and tells every friend that has not acknowledged it.
func (m *Manager) OnAddrChange(ctx context.Context) error {
	ap, err := m.client.FreshPublicAddr(ctx)
	if err != nil {
		return err
	}
	if !validAddr(ap) {
		return ErrNoPublicAddr
	}
	m.recordAddr(ap)
	m.notifyDue(ctx)
	return nil
}

// notifyDue sends the current address to friends that have not acknowledged it, on backoff.
func (m *Manager) notifyDue(ctx context.Context) {
	p := m.lastIP.Load()
	if p == nil {
		return
	}
	addr := p.String()
	now := m.now()
	var due []*friendEntry
	m.mu.Lock()
	back := map[[20]byte]bool{}
	for _, f := range m.snap.Load().friends {
		back[f.control] = m.reannounce[f.control]
		if m.acked[f.control] == addr && !back[f.control] {
			continue
		}
		if b := m.addrRetry[f.control]; b != nil && b.at.After(now) {
			continue
		}
		inFlightLocked(m.addrRetry, f.control, now)
		due = append(due, f)
	}
	m.mu.Unlock()
	for _, f := range due {
		m.sendAddr(ctx, f, addr, back[f.control])
	}
}

// sendAddrOnce announces our current address to f once, acknowledged or not.
func (m *Manager) sendAddrOnce(ctx context.Context, f *friendEntry, back bool) {
	if p := m.lastIP.Load(); p != nil {
		m.sendAddr(ctx, f, p.String(), back)
	}
}

// sendAddr sends addr{addr, seq, back, mac} and waits for the friend's ok. back asks the friend to
// announce itself in return.
func (m *Manager) sendAddr(ctx context.Context, f *friendEntry, addr string, back bool) {
	err := func() error {
		seq, err := m.nextSeq(f.name)
		if err != nil {
			return err
		}
		msg := Msg{Type: "addr", Addr: addr, Seq: seq, Back: back}
		msg.MAC = SignPair(f.pair, f.role, msg, "")
		rw, conn, err := m.dialFriend(ctx, f)
		if err != nil {
			return err
		}
		defer conn.Close()
		if err := WriteMsg(rw, msg); err != nil {
			return err
		}
		ans, err := ReadMsg(rw)
		if err != nil {
			return err
		}
		switch {
		case ans.Type == "ok" && ans.Seq == seq && CheckPair(f.pair, f.role.Other(), ans, ""):
			f.noteControlSource(conn.RemoteAddr())
			m.noteContact(f)
			m.mu.Lock()
			delete(m.reannounce, f.control)
			if m.snap.Load().control[f.control] == nil {
				m.mu.Unlock()
				return nil // removed meanwhile: no acked entry for a gone pairing
			}
			m.acked[f.control] = addr
			delete(m.addrRetry, f.control)
			m.saveAckedLocked()
			m.mu.Unlock()
			return nil
		case ans.Type == "removed" && CheckPair(f.pair, f.role.Other(), ans, ""):
			m.logf("%q removed us while we were away", f.name)
			m.dropFriend(f, false) // they deleted us and will never dial: no tombstone
			return nil
		}
		return ErrUnexpected
	}()
	if err != nil {
		m.logf("address to %q: %v", f.name, err)
		m.mu.Lock()
		if b := m.addrRetry[f.control]; b != nil {
			m.bump(b)
		}
		m.mu.Unlock()
	}
}

// nextSeq bumps and stores my_seq before sending, so a seq is never reused.
func (m *Manager) nextSeq(name string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fs, err := m.store.ListFriends()
	if err != nil {
		return 0, err
	}
	for _, f := range fs {
		if f.Name == name {
			seq := f.MySeq + 1
			if err := m.store.SetFriendAddrSeq(name, f.AddrSeq, seq); err != nil {
				return 0, err
			}
			return seq, nil
		}
	}
	return 0, ErrNotFound
}

// serveControl handles a friend dialling the pair's control channel.
func (m *Manager) serveControl(f *friendEntry, remote [20]byte, rw io.ReadWriter, conn net.Conn) {
	from := conn.RemoteAddr()
	if !VerifyPeerID(f.pair, f.control, f.role.Other(), remote) {
		return
	}
	msg, err := ReadMsg(rw)
	if err != nil {
		return
	}
	if msg.Type == secHello {
		srw, ok := m.answerSecure(f, msg, rw)
		if !ok {
			return
		}
		rw = srw
		_ = conn.SetDeadline(time.Now().Add(m.ioTimeout)) // the opening used part of the first
		if msg, err = ReadMsg(rw); err != nil {
			return
		}
		m.markSecure(f) // the dialler's first encrypted frame decrypted: not a replayed hello
	} else if f.secure.Load() {
		// Only the friend can sign: unsigned junk from the path earns no advice to reset.
		if CheckPair(f.pair, f.role.Other(), msg, "") {
			m.refusePlain(f, fmt.Sprintf("%q spoke in clear, but it encrypted before: refused. If it went back to an older version, reset its encryption in Friends", f.name))
		}
		return // a friend that encrypts never speaks in clear: a downgrade
	}
	switch msg.Type {
	case "addr":
		if !validAddrString(msg.Addr) || msg.Seq <= 0 || !CheckPair(f.pair, f.role.Other(), msg, "") {
			return
		}
		held, moved := m.applyAddr(f, msg)
		if moved {
			f.ctrlIPs.Store(nil) // sources of the old address no longer apply
		}
		f.noteControlSource(from)
		m.noteContact(f)
		if !held {
			return // not stored: no ok, the friend retries
		}
		ok := Msg{Type: "ok", Seq: msg.Seq}
		ok.MAC = SignPair(f.pair, f.role, ok, "")
		_ = WriteMsg(rw, ok)
		if f.name == m.LibraryMaster() {
			m.startPull(f.name) // the master is back or moved: catch up
		}
		if msg.Back {
			// The friend restarted or reconnected: tell it once where we are, so it learns our source.
			m.goTask(func(ctx context.Context) { m.sendAddrOnce(ctx, f, false) })
		}
	case "removed":
		if CheckPair(f.pair, f.role.Other(), msg, "") {
			m.logf("%q removed us", f.name)
			m.dropFriend(f, false)
		}
	case "watch", "win", "unwatch":
		m.serveHelp(f, msg, rw)
	case "lib_offer", "lib_accept", "lib_withdraw", "lib_decline", "lib_leave", "lib_release", "lib_news", "lib_pull":
		m.serveLibrary(f, msg, rw, conn)
	case "ping":
		m.servePing(f, msg, rw)
	}
}

// applyAddr reports whether we now hold that address or a newer one (stale seq counts as held),
// and whether the friend moved to a new address.
func (m *Manager) applyAddr(f *friendEntry, msg Msg) (held, moved bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fs, err := m.store.ListFriends()
	if err != nil {
		return false, false
	}
	for _, cur := range fs {
		if cur.Name != f.name || string(cur.PairKey) != string(f.pair[:]) {
			continue
		}
		if msg.Seq <= cur.AddrSeq {
			return true, false // replay or stale
		}
		if err := m.store.UpdateFriendAddr(cur.Name, msg.Addr, m.now().Unix()); err != nil {
			m.logf("store address of %q: %v", cur.Name, err)
			return false, false
		}
		if err := m.store.SetFriendAddrSeq(cur.Name, msg.Seq, cur.MySeq); err != nil {
			m.logf("store address seq of %q: %v", cur.Name, err)
			return false, false
		}
		moved = cur.Addr != msg.Addr
		if moved {
			m.logf("%q moved to %s", cur.Name, msg.Addr)
		}
		if err := m.refreshLocked(); err != nil {
			m.logf("refresh: %v", err)
		}
		return true, moved
	}
	return false, false
}

// Remove tells the friend, deletes the pairing and closes its connections. If the friend could
// not be told, a tombstone answers "removed" when it next dials us.
func (m *Manager) Remove(ctx context.Context, name string) error {
	f := m.friendByName(name)
	if f == nil {
		return ErrNotFound
	}
	m.endRoleForRemoval(ctx, f) // the role ends first
	msg := Msg{Type: "removed"}
	msg.MAC = SignPair(f.pair, f.role, msg, "")
	told := false
	if rw, conn, err := m.dialFriend(ctx, f); err == nil {
		if WriteMsg(rw, msg) == nil {
			// The friend closes after handling it: a clean EOF means it was read.
			_, rerr := ReadMsg(rw)
			told = errors.Is(rerr, io.EOF)
		}
		conn.Close()
	}
	if !told {
		m.logf("remove %q: not notified now, told when it next dials", name)
	}
	m.dropFriend(f, !told)
	return nil
}

// dropFriend deletes the pairing if f is still the stored one, optionally leaves a control
// tombstone, and closes its connections.
func (m *Manager) dropFriend(f *friendEntry, tombstone bool) {
	m.mu.Lock()
	var err error
	if fs, lerr := m.store.ListFriends(); lerr != nil {
		err = lerr
	} else {
		for _, cur := range fs {
			if cur.Name == f.name && bytes.Equal(cur.PairKey, f.pair[:]) {
				err = m.store.DeleteFriend(f.name)
			}
		}
	}
	if tombstone {
		m.addControlTombLocked(f)
	}
	delete(m.addrRetry, f.control)
	delete(m.plainUntil, f.control)
	delete(m.refusedPlain, f.control)
	delete(m.acked, f.control)
	delete(m.reannounce, f.control)
	m.saveAckedLocked()
	if rerr := m.refreshLocked(); rerr != nil {
		m.logf("refresh: %v", rerr)
	}
	m.mu.Unlock()
	if err != nil {
		m.logf("delete %q: %v", f.name, err)
	}
	m.dropHelps(f.name)
	m.dropRole(f.name)
	// After the snapshot: the hooks no longer recognise the friend, so no new connection is privileged.
	if cb := m.CloseFriendConns; cb != nil {
		pair, other := f.pair, f.role.Other()
		cb(func(ih, id [20]byte) bool { return VerifyPeerID(pair, ih, other, id) })
	}
}
