package friends

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/torrent"

	"tiramisu/internal/metadb"
)

// pullState is the slave's session towards one master (guarded by mu).
type pullState struct {
	running bool
	again   bool     // news arrived during the session: run another at once
	retry   *backoff // the last session failed
	pending bool     // ApplyPending is running
}

// newsState is the master's notice towards one slave (guarded by mu).
type newsState struct {
	running     bool
	again       bool // the feed changed during the notice
	due         bool // a change not yet told
	retry       *backoff
	failedSince time.Time // first failure of the current run of failures: the slave is offline
}

// sessOther binds a session message to its session and, for a pull, to the cursor it acknowledges.
func sessOther(sess string, seq int64) string { return sess + "/" + strconv.FormatInt(seq, 10) }

// infoChunk is how many metainfo bytes fit one lib_infopart frame.
var infoChunk = func() int {
	h, _ := json.Marshal(Msg{Type: "lib_infopart", Nonce: strings.Repeat("0", 32), Hash: strings.Repeat("0", 40),
		Pos: math.MaxInt64, Left: math.MaxInt64, MAC: strings.Repeat("0", 64)})
	room := maxFrame - len(h) - len(`,"body":""`)
	return room / 4 * 3
}()

func fitsBlock(es []LibEntry) bool { return frameFits(blockMsg(es), maxFrame) }

// LibraryChanged tells the active slaves that the feed has news.
func (m *Manager) LibraryChanged() {
	for _, name := range m.LibrarySlaves() {
		m.sendNews(name)
	}
}

// sendNews tells one slave; one notice in flight per slave, a change meanwhile sends one more.
func (m *Manager) sendNews(name string) {
	m.mu.Lock()
	s := m.news[name]
	if s == nil {
		s = &newsState{}
		m.news[name] = s
	}
	s.due = true
	if s.running {
		s.again = true
		m.mu.Unlock()
		return
	}
	if s.retry != nil && s.retry.at.After(m.now()) {
		m.mu.Unlock()
		return // Tick tries again
	}
	s.running, s.again = true, false
	m.mu.Unlock()
	m.goTask(func(ctx context.Context) { m.runNews(ctx, name) })
}

func (m *Manager) runNews(ctx context.Context, name string) {
	for {
		f := m.friendByName(name)
		r, ok := m.roleRow(name)
		w := m.lib.Load()
		if f == nil || w == nil || !ok || r.Side != sideMaster || r.State != stActive || f.addr == "" {
			m.mu.Lock()
			delete(m.news, name)
			m.mu.Unlock()
			return
		}
		head, err := w.src.Head()
		var ans Msg
		if err == nil {
			ans, err = m.exchange(ctx, f, Msg{Type: "lib_news", Nonce: r.Nonce, Seq: head})
		}
		m.mu.Lock()
		s := m.news[name]
		if s == nil {
			m.mu.Unlock()
			return
		}
		if err != nil {
			if s.retry == nil {
				s.retry = &backoff{}
				s.failedSince = m.now()
			}
			m.bump(s.retry)
			s.running, s.again = false, false
			m.mu.Unlock()
			if ctx.Err() == nil {
				m.logf("library news to %q: %v", name, err)
			}
			return
		}
		s.retry, s.failedSince = nil, time.Time{}
		again := s.again
		s.again, s.due = false, again
		if !again {
			s.running = false
		}
		m.mu.Unlock()
		if ans.Type == "lib_none" {
			if m.deleteRole(name, func(cur metadb.FriendLibrary) bool { return cur.Side == sideMaster && cur.Nonce == r.Nonce }) {
				m.logf("library: %q left while we were away", name)
			}
		}
		if !again {
			return
		}
	}
}

// onNews answers a master's notice and starts a pull.
func (m *Manager) onNews(f *friendEntry, msg Msg) (Msg, bool) {
	r, ok := m.roleRow(f.name)
	switch {
	case ok && r.Side == sideSlave && r.Nonce == msg.Nonce:
		if r.State == stActive {
			m.startPull(f.name)
		} // else our acceptance is in flight: its answer starts the pull
		return Msg{Type: "ok", Nonce: msg.Nonce}, true
	case ok && r.Side == sideSlave && r.State == stActive:
		return Msg{}, false // an older role's notice
	}
	return Msg{Type: "lib_none", Nonce: msg.Nonce}, true
}

// libraryStarted starts the pull session due to our master.
func (m *Manager) libraryStarted() {
	if master := m.LibraryMaster(); master != "" {
		m.startPull(master)
	}
}

// startPull runs a pull session towards master; one at a time, news meanwhile runs one more.
func (m *Manager) startPull(master string) {
	if m.lib.Load() == nil {
		return
	}
	m.mu.Lock()
	p := m.pulls[master]
	if p == nil {
		p = &pullState{}
		m.pulls[master] = p
	}
	if p.running {
		p.again = true
		m.mu.Unlock()
		return
	}
	p.running, p.again = true, false
	m.mu.Unlock()
	m.goTask(func(ctx context.Context) { m.runPulls(ctx, master) })
}

func (m *Manager) runPulls(ctx context.Context, master string) {
	for {
		err := m.pull(ctx, master)
		if err != nil && ctx.Err() == nil {
			m.logf("library pull from %q: %v", master, err)
		}
		m.mu.Lock()
		p := m.pulls[master]
		if err != nil {
			if p.retry == nil {
				p.retry = &backoff{}
			}
			m.bump(p.retry)
		} else {
			p.retry = nil
		}
		again := p.again && err == nil && ctx.Err() == nil
		p.again = false
		if !again {
			p.running = false
		}
		startPending := !again && !p.pending
		if startPending {
			p.pending = true
		}
		m.mu.Unlock()
		if again {
			continue
		}
		if startPending {
			m.applyPending(ctx, master)
		}
		return
	}
}

// applyPending applies the entries a session deferred or could not apply, outside any connection.
func (m *Manager) applyPending(ctx context.Context, master string) {
	defer func() {
		m.mu.Lock()
		if p := m.pulls[master]; p != nil {
			p.pending = false
		}
		m.mu.Unlock()
	}()
	w := m.lib.Load()
	if w == nil || m.LibraryMaster() != master {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(m.masterCtx(master), cancel)()
	if err := w.sink.ApplyPending(ctx); err != nil && ctx.Err() == nil {
		m.logf("library pending entries: %v", err)
	}
}

// pull runs one session: blocks until the feed is exhausted, each acknowledged by the next pull.
func (m *Manager) pull(ctx context.Context, master string) error {
	f := m.friendByName(master)
	w := m.lib.Load()
	r, ok, err := m.store.GetFriendLibrary(master)
	if err != nil {
		return err
	}
	if f == nil || w == nil || !ok || r.Side != sideSlave || r.State != stActive {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(m.masterCtx(master), cancel)() // leaving closes the connection too
	if h := w.hooks.BeforePull; h != nil {
		if err := h(ctx); err != nil {
			return err
		}
	}
	rw, conn, err := m.dialFriend(ctx, f)
	if err != nil {
		return err
	}
	defer conn.Close()
	s := &session{m: m, f: f, rw: rw, conn: conn, nonce: hex.EncodeToString(randomBytes(16))}
	sessID := m.sessionID()
	start, cursor, aligned := r.Cursor, r.Cursor, r.AlignedAt > 0
	blocks, applied := 0, 0
	m.mu.Lock()
	prog := m.progress[master]
	if prog == nil {
		prog = &pullProgress{}
		m.progress[master] = prog
	}
	prog.sessStart, prog.sessEntries, prog.running, prog.left = m.now(), 0, true, r.LeftCount
	if start == 0 {
		prog.blocks, prog.entries = 0, 0 // a new alignment
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		prog.running = false
		m.mu.Unlock()
		m.logf("library: pull from %q at cursor %d: %d blocks, %d entries, cursor now %d", master, start, blocks, applied, cursor)
		m.libMu.Lock()
		m.reloadRolesLogged()
		m.libMu.Unlock()
	}()
	for {
		if m.LibraryMaster() != master {
			return nil // left meanwhile
		}
		other := sessOther(s.nonce, cursor)
		ans, err := s.ask(Msg{Type: "lib_pull", Seq: cursor}, other)
		if err != nil {
			return err
		}
		switch {
		case ans.Type == "lib_reset" && cursor > 0:
			m.logf("library: %q reset our cursor %d", master, cursor)
			cursor = 0
			if err := m.store.SetFriendLibraryCursor(master, 0, -1, m.now().Unix()); err != nil {
				return err
			}
			continue
		case ans.Type == "lib_released":
			if m.deleteRole(master, func(cur metadb.FriendLibrary) bool { return cur.Side == sideSlave && cur.Nonce == r.Nonce }) {
				m.logf("library: %q released us while we were away", master)
			}
			return nil
		case ans.Type != "lib_block" || ans.Seq < cursor:
			return ErrUnexpected
		case ans.Left < 0:
			return nil // the feed is being built: its news starts the next session
		}
		var es []LibEntry
		if ans.Body != "" {
			if err := json.Unmarshal([]byte(ans.Body), &es); err != nil {
				return ErrUnexpected
			}
		}
		if len(es) == 0 && ans.Left == 0 {
			now := m.now().Unix()
			if ans.Seq != cursor {
				cursor = ans.Seq
				if err := m.store.SetFriendLibraryCursor(master, cursor, 0, now); err != nil {
					return err
				}
			}
			if !aligned {
				if err := m.store.SetFriendLibraryAligned(master, now); err != nil {
					return err
				}
				m.logf("library: aligned with %q", master)
			}
			return w.sink.Prune(sessID) // the cursor covers every earlier removal
		}
		for _, e := range es {
			if ctx.Err() != nil {
				return nil // the role ended: nothing more is applied for this master
			}
			if err := s.apply(ctx, w.sink, e, sessID); err != nil {
				return err
			}
		}
		cursor = ans.Seq
		blocks++
		applied += len(es)
		m.mu.Lock()
		prog.blocks++
		prog.sessEntries += len(es)
		prog.entries += len(es)
		prog.left = ans.Left
		m.mu.Unlock()
		now := m.now().Unix()
		if err := m.store.SetFriendLibraryCursor(master, cursor, ans.Left, now); err != nil {
			return err
		}
		if ans.Left == 0 && !aligned {
			aligned = true
			if err := m.store.SetFriendLibraryAligned(master, now); err != nil {
				return err
			}
			m.logf("library: aligned with %q", master)
		}
	}
}

// sessionID orders the pull sessions: wall-clock nanoseconds, strictly increasing.
func (m *Manager) sessionID() int64 {
	for {
		last := m.lastSess.Load()
		next := max(m.now().UnixNano(), last+1)
		if m.lastSess.CompareAndSwap(last, next) {
			return next
		}
	}
}

// session is one library pull connection on the slave side.
type session struct {
	m     *Manager
	f     *friendEntry
	rw    io.ReadWriter
	conn  net.Conn
	nonce string
}

// ask sends msg in the session, signed with other, and reads the answer signed the same way.
func (s *session) ask(msg Msg, other string) (Msg, error) {
	msg.Nonce = s.nonce
	msg.MAC = SignPair(s.f.pair, s.f.role, msg, other)
	_ = s.conn.SetDeadline(time.Now().Add(s.m.ioTimeout))
	if err := WriteMsg(s.rw, msg); err != nil {
		return Msg{}, err
	}
	ans, err := ReadMsg(s.rw)
	if err != nil {
		return Msg{}, err
	}
	if ans.Nonce != s.nonce || !CheckPair(s.f.pair, s.f.role.Other(), ans, other) {
		return Msg{}, ErrUnexpected
	}
	s.m.noteContact(s.f)
	return ans, nil
}

// apply hands one entry to the sink, fetching its metainfo first when the master has it.
func (s *session) apply(ctx context.Context, sink LibrarySink, e LibEntry, sess int64) error {
	switch {
	case !e.valid():
		s.m.logf("library: invalid entry %q from %q", e.Path, s.f.name)
		return sink.Fail(e, "invalid entry", sess)
	case e.Del:
		return sink.Remove(ctx, e, sess)
	case sink.HasInfo(e.Hash):
		return sink.Apply(ctx, e, nil, sess)
	case !e.Info:
		return sink.Defer(e, sess)
	}
	info, reason, err := s.fetchInfo(e.Hash)
	switch {
	case err != nil:
		return err
	case reason != "":
		s.m.logf("library: %s: %s", e.Path, reason)
		return sink.Fail(e, reason, sess)
	case info == nil:
		return sink.Defer(e, sess) // the master no longer has it
	}
	return sink.Apply(ctx, e, info, sess)
}

// fetchInfo reads hash's metainfo in parts and checks it hashes to hash; reason explains a refusal.
func (s *session) fetchInfo(hash string) (info []byte, reason string, err error) {
	total := int64(-1)
	for pos := int64(0); total < 0 || pos < total; {
		ans, err := s.ask(Msg{Type: "lib_info", Hash: hash, Pos: pos}, s.nonce)
		if err != nil {
			return nil, "", err
		}
		if ans.Type != "lib_infopart" || ans.Hash != hash || ans.Pos != pos || ans.Left < 0 || (total >= 0 && ans.Left != total) {
			return nil, "", ErrUnexpected
		}
		if total < 0 {
			total = ans.Left
			if total == 0 {
				return nil, "", nil
			}
			if total > int64(torrent.MaxMetadataSize()) {
				return nil, "metainfo too large", nil
			}
			info = make([]byte, 0, total)
		}
		part, err := base64.StdEncoding.DecodeString(ans.Body)
		if err != nil || len(part) == 0 || pos+int64(len(part)) > total {
			return nil, "", ErrUnexpected
		}
		info = append(info, part...)
		pos += int64(len(part))
	}
	want, _ := hex.DecodeString(hash)
	if sum := sha1.Sum(info); !bytes.Equal(sum[:], want) {
		return nil, "info mismatch", nil
	}
	return info, "", nil
}

// servePull serves a slave's pull session in the connection's goroutine until it closes.
func (m *Manager) servePull(f *friendEntry, msg Msg, rw io.ReadWriter, conn net.Conn) {
	sess := msg.Nonce
	if _, ok := decodeNonce(sess); !ok {
		return
	}
	w := m.lib.Load()
	var cachedHash string
	var cachedInfo []byte
	reply := func(ans Msg, other string) bool {
		ans.Nonce = sess
		ans.MAC = SignPair(f.pair, f.role, ans, other)
		_ = conn.SetDeadline(time.Now().Add(m.ioTimeout))
		return WriteMsg(rw, ans) == nil
	}
	for {
		switch msg.Type {
		case "lib_pull":
			other := sessOther(sess, msg.Seq)
			if !CheckPair(f.pair, f.role.Other(), msg, other) || msg.Seq < 0 {
				return
			}
			m.noteContact(f)
			r, ok := m.roleRow(f.name)
			if !ok || r.Side != sideMaster || r.State != stActive {
				if reply(Msg{Type: "lib_released", Seq: msg.Seq}, other) && ok && r.Side == sideMaster && r.State == stReleased {
					m.deleteRole(f.name, func(cur metadb.FriendLibrary) bool { return cur.Nonce == r.Nonce && cur.State == stReleased })
					m.logf("library: %q told it was released", f.name)
				}
				return
			}
			head, err := w.src.Head()
			if err != nil { // never a reset on a failed read: the slave retries later
				m.logf("library feed head: %v", err)
				return
			}
			if msg.Seq > head { // our feed went back (a restored DB): the slave starts over
				m.logf("library: %q is at %d, beyond our feed: reset", f.name, msg.Seq)
				m.libMu.Lock()
				if err := m.store.SetFriendLibraryCursor(f.name, 0, -1, m.now().Unix()); err != nil {
					m.logf("library cursor of %q: %v", f.name, err)
				}
				m.reloadRolesLogged()
				m.libMu.Unlock()
				if !reply(Msg{Type: "lib_reset", Seq: msg.Seq}, other) {
					return
				}
				break
			}
			m.ackCursor(f.name, msg.Seq)
			es, last, left, err := w.src.Since(msg.Seq, fitsBlock)
			if err != nil {
				m.logf("library feed for %q: %v", f.name, err)
				return
			}
			block := Msg{Type: "lib_block", Seq: last, Left: left}
			if left < 0 {
				block.Seq, es = msg.Seq, nil
			}
			if len(es) > 0 {
				b, _ := json.Marshal(es)
				block.Body = string(b)
			} else if left > 0 {
				return // nothing fits a frame: never spin on the same cursor
			}
			if !reply(block, other) {
				return
			}
		case "lib_info":
			if !CheckPair(f.pair, f.role.Other(), msg, sess) || !isHex40(msg.Hash) || msg.Pos < 0 {
				return
			}
			if cachedHash != msg.Hash {
				info, err := w.src.Info(msg.Hash)
				if err != nil {
					m.logf("library metainfo %s: %v", msg.Hash, err)
					return
				}
				cachedHash, cachedInfo = msg.Hash, info
			}
			info := cachedInfo
			if msg.Pos > int64(len(info)) || (msg.Pos == int64(len(info)) && len(info) > 0) {
				return
			}
			part := info[msg.Pos:min(int64(len(info)), msg.Pos+int64(infoChunk))]
			if !reply(Msg{Type: "lib_infopart", Hash: msg.Hash, Pos: msg.Pos, Left: int64(len(info)),
				Body: base64.StdEncoding.EncodeToString(part)}, sess) {
				return
			}
		default:
			return
		}
		_ = conn.SetDeadline(time.Now().Add(m.ioTimeout))
		next, err := ReadMsg(rw)
		if err != nil || next.Nonce != sess {
			return
		}
		msg = next
	}
}

// ackCursor records the cursor a slave confirmed and prunes what every slave has.
func (m *Manager) ackCursor(name string, seq int64) {
	w := m.lib.Load()
	m.libMu.Lock()
	r, ok := m.roleRow(name)
	if !ok || seq < r.Cursor {
		m.libMu.Unlock()
		return
	}
	now := m.now().Unix()
	if seq > r.Cursor {
		if err := m.store.SetFriendLibraryCursor(name, seq, -1, now); err != nil {
			m.logf("library cursor of %q: %v", name, err)
		}
	}
	if head, err := w.src.Head(); err == nil && r.AlignedAt == 0 && seq >= head {
		if err := m.store.SetFriendLibraryAligned(name, now); err == nil {
			m.logf("library: %q aligned", name)
		}
	}
	m.reloadRolesLogged()
	low := int64(-1)
	for _, s := range m.roles.Load().rows {
		if s.Side == sideMaster && s.State == stActive && (low < 0 || s.Cursor < low) {
			low = s.Cursor
		}
	}
	m.libMu.Unlock()
	if low >= 0 {
		if err := w.src.Confirmed(low); err != nil {
			m.logf("library feed prune: %v", err)
		}
	}
}

// libraryTick retries failed pulls and notices whose backoff ran out.
func (m *Manager) libraryTick() {
	if m.lib.Load() == nil {
		return
	}
	now := m.now()
	var pulls, news []string
	m.mu.Lock()
	if master := m.LibraryMaster(); master != "" {
		if p := m.pulls[master]; p != nil && !p.running && p.retry != nil && !p.retry.at.After(now) {
			pulls = append(pulls, master)
		}
	}
	for name, s := range m.news {
		if s.due && !s.running && (s.retry == nil || !s.retry.at.After(now)) {
			news = append(news, name)
		}
	}
	m.mu.Unlock()
	for _, name := range pulls {
		m.startPull(name)
	}
	for _, name := range news {
		m.sendNews(name)
	}
}
