package friends

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"slices"

	"tiramisu/internal/metadb"
)

// Library roles, by our side: as master we send our library to the friend, as slave we get its.
const (
	sideMaster = "master"
	sideSlave  = "slave"

	stProposed = "proposed" // master side: waiting for the friend to accept
	stOffered  = "offered"  // slave side: a proposal we have not answered
	stActive   = "active"
	stReleased = "released" // master side: the slave is told at its next pull

	refusedHasMaster = "has_master"
	refusedHasSlaves = "has_slaves"
	refusedRole      = "library_role"
	refusedWithdrawn = "withdrawn"
	refusedExpired   = "expired"
)

var (
	ErrHasMaster   = errors.New("friends: gets a library from another friend")
	ErrHasSlaves   = errors.New("friends: sends its library to other friends")
	ErrLibraryRole = errors.New("friends: a library role with this friend exists already")
	ErrOfferGone   = errors.New("friends: library proposal withdrawn or expired")
	ErrNoLibrary   = errors.New("friends: library sharing not ready")
)

// LibrarySource is the master's library as the feed sees it.
type LibrarySource interface {
	// Since returns entries with seq > cursor in seq order, packed by fit, plus the last seq
	// included and how many remain after it (-1 while the feed is being built).
	Since(cursor int64, fit func([]LibEntry) bool) (entries []LibEntry, last int64, left int64, err error)
	Info(hash string) ([]byte, error) // metainfo bytes, nil when the master has none
	Head() (int64, error)
	Counts(cursor int64) (entries, total, waiting int64, err error) // live entries up to cursor, all live, rows after it
	Confirmed(minCursor int64) error                                // prune del rows every active slave confirmed
	Start() error                                                   // first slave: build the feed
	Stop() error                                                    // no slave left: drop the feed rows
}

// LibrarySink applies the master's entries on a slave. Errors returned are about the node (disk,
// DB), never about one entry: a bad entry is recorded as failed and gives nil.
type LibrarySink interface {
	Apply(ctx context.Context, e LibEntry, info []byte, sess int64) error
	Remove(ctx context.Context, e LibEntry, sess int64) error
	Defer(e LibEntry, sess int64) error               // record as pending, applied after the session
	Fail(e LibEntry, reason string, sess int64) error // record an entry that cannot be applied
	ApplyPending(ctx context.Context) error           // pending and failed entries, outside any session
	Prune(sess int64) error                           // drop the removals of sessions before sess
	Forget()                                          // the mirror was cleared: drop any state kept about it
	HasInfo(hash string) bool
}

// LibraryHooks tell the node about role changes.
type LibraryHooks struct {
	OnMaster func(master string) // "" when the node no longer has a master
	// BeforePull, when set, runs before each pull session, outside any connection: the syncs the
	// master replaces must have stopped writing stubs. An error postpones the session.
	BeforePull func(ctx context.Context) error
}

type libWiring struct {
	src   LibrarySource
	sink  LibrarySink
	hooks *LibraryHooks
}

// libSnap is the immutable view of the roles.
type libSnap struct {
	master string   // our active master, "" none
	slaves []string // our active slaves
	rows   map[string]metadb.FriendLibrary
}

// SetLibrary enables library sharing. Before it, lib_* messages are dropped unanswered.
func (m *Manager) SetLibrary(src LibrarySource, sink LibrarySink, h *LibraryHooks) {
	if h == nil {
		h = &LibraryHooks{}
	}
	m.feedMu.Lock()
	m.hookMu.Lock()
	m.lib.Store(&libWiring{src: src, sink: sink, hooks: h})
	m.fired = "" // new hooks and source: tell them the current state
	m.feedOn.Store(false)
	m.feedRun = false
	m.hookMu.Unlock()
	m.feedMu.Unlock()
	m.syncRoleHooks()
	m.libraryStarted()
}

// LibraryMaster is the friend we get our library from; "" when none.
func (m *Manager) LibraryMaster() string {
	if m == nil {
		return "" // no friends on this node
	}
	if s := m.roles.Load(); s != nil {
		return s.master
	}
	return ""
}

// LibrarySlaves are the friends we send our library to.
func (m *Manager) LibrarySlaves() []string {
	if s := m.roles.Load(); s != nil {
		return slices.Clone(s.slaves)
	}
	return nil
}

// LibraryRoles returns every stored role.
func (m *Manager) LibraryRoles() map[string]metadb.FriendLibrary {
	out := map[string]metadb.FriendLibrary{}
	if s := m.roles.Load(); s != nil {
		for k, v := range s.rows {
			out[k] = v
		}
	}
	return out
}

// reloadRoles rebuilds the roles snapshot from the store.
func (m *Manager) reloadRoles() error {
	rows, err := m.store.ListFriendLibrary()
	if err != nil {
		return err
	}
	s := &libSnap{rows: map[string]metadb.FriendLibrary{}}
	for _, r := range rows {
		s.rows[r.Name] = r
		if r.State != stActive {
			continue
		}
		if r.Side == sideSlave {
			s.master = r.Name
		} else {
			s.slaves = append(s.slaves, r.Name)
		}
	}
	m.roles.Store(s)
	return nil
}

func (m *Manager) reloadRolesLogged() {
	if err := m.reloadRoles(); err != nil {
		m.logf("library roles: %v", err)
	}
}

// syncRoleHooks reports a new master and starts or stops the feed, in the order roles changed.
func (m *Manager) syncRoleHooks() {
	w := m.lib.Load()
	if w == nil {
		return
	}
	m.hookMu.Lock()
	defer m.hookMu.Unlock()
	s := m.roles.Load()
	if s.master != m.fired {
		m.fired = s.master
		if w.hooks.OnMaster != nil {
			w.hooks.OnMaster(s.master)
		}
	}
	on := len(s.slaves) > 0
	if on == m.feedOn.Load() {
		return
	}
	m.feedOn.Store(on)
	// Building the feed walks every stub, and Stop waits for it: never inside a connection's deadline.
	m.goTask(func(context.Context) { m.convergeFeed() })
}

// convergeFeed starts or stops the feed until it matches the latest role state; one at a time.
func (m *Manager) convergeFeed() {
	m.feedMu.Lock()
	defer m.feedMu.Unlock()
	w := m.lib.Load()
	if w == nil {
		return
	}
	for {
		want := m.feedOn.Load()
		if want == m.feedRun {
			return
		}
		if want {
			if err := w.src.Start(); err != nil {
				m.logf("library feed: %v", err)
				return
			}
			m.feedRun = true
			if m.feedOn.Load() {
				m.LibraryChanged()
			}
			continue
		}
		// Off even when clearing failed: the next slave starts it again, and its build repairs the rows.
		if err := w.src.Stop(); err != nil {
			m.logf("library feed stop: %v", err)
		}
		m.feedRun = false
	}
}

func (m *Manager) roleRow(name string) (metadb.FriendLibrary, bool) {
	r, ok := m.roles.Load().rows[name]
	return r, ok
}

// OfferLibrary proposes to friend that we become its library master.
func (m *Manager) OfferLibrary(ctx context.Context, friend string) error {
	f := m.friendByName(friend)
	if f == nil {
		return ErrNotFound
	}
	if m.lib.Load() == nil {
		return ErrNoLibrary
	}
	now := m.now()
	nonce := hex.EncodeToString(randomBytes(16))
	row := metadb.FriendLibrary{Name: friend, Side: sideMaster, State: stProposed, Nonce: nonce,
		ExpiresAt: now.Add(inviteTTL).Unix(), LeftCount: -1, UpdatedAt: now.Unix()}
	m.libMu.Lock()
	delete(m.refused, friend) // a new proposal replaces the last outcome
	switch _, has := m.roleRow(friend); {
	case m.LibraryMaster() != "" || m.accepts != "":
		m.libMu.Unlock()
		return ErrHasMaster
	case has:
		m.libMu.Unlock()
		return ErrLibraryRole
	}
	err := m.store.PutFriendLibrary(row)
	m.reloadRolesLogged()
	m.libMu.Unlock()
	if err != nil {
		return err
	}
	// The expiry travels in Seq: Expires is not covered by the MAC.
	ans, err := m.exchange(ctx, f, Msg{Type: "lib_offer", Nonce: nonce, Seq: row.ExpiresAt})
	if err == nil && ans.Type == "lib_wait" {
		m.logf("library: proposed to %q", friend)
		return nil
	}
	m.deleteRole(friend, func(r metadb.FriendLibrary) bool { return r.Side == sideMaster && r.Nonce == nonce })
	switch {
	case err != nil:
		m.logf("library proposal to %q: %v", friend, err)
		return ErrUnreachable
	case ans.Type == "lib_refused" && ans.Name == refusedHasMaster:
		m.noteRefused(friend, refusedHasMaster)
		return ErrHasMaster
	case ans.Type == "lib_refused" && ans.Name == refusedHasSlaves:
		m.noteRefused(friend, refusedHasSlaves)
		return ErrHasSlaves
	case ans.Type == "lib_refused":
		return ErrLibraryRole
	}
	return ErrUnexpected
}

// AcceptLibrary accepts friend's proposal: it becomes our library master.
func (m *Manager) AcceptLibrary(ctx context.Context, friend string) error {
	f := m.friendByName(friend)
	if f == nil {
		return ErrNotFound
	}
	if m.lib.Load() == nil {
		return ErrNoLibrary
	}
	m.libMu.Lock()
	r, ok := m.roleRow(friend)
	switch {
	case ok && r.Side == sideSlave && r.State == stActive:
		m.libMu.Unlock()
		return nil
	case !ok || r.Side != sideSlave || r.State != stOffered:
		m.libMu.Unlock()
		return ErrOfferGone
	case r.ExpiresAt <= m.now().Unix():
		_ = m.store.DeleteFriendLibrary(friend)
		m.reloadRolesLogged()
		m.libMu.Unlock()
		return ErrOfferGone
	case m.LibraryMaster() != "" || (m.accepts != "" && m.accepts != friend):
		m.libMu.Unlock()
		return ErrHasMaster
	case len(m.LibrarySlaves()) > 0:
		m.libMu.Unlock()
		return ErrHasSlaves
	}
	m.accepts = friend
	m.libMu.Unlock()

	ans, err := m.exchange(ctx, f, Msg{Type: "lib_accept", Nonce: r.Nonce})

	m.libMu.Lock()
	m.accepts = ""
	if err != nil {
		m.libMu.Unlock()
		m.logf("library acceptance to %q: %v", friend, err)
		return ErrUnreachable
	}
	if ans.Type != "lib_ok" {
		_ = m.store.DeleteFriendLibrary(friend)
		m.reloadRolesLogged()
		m.libMu.Unlock()
		if ans.Type == "lib_refused" && ans.Name == refusedHasMaster {
			return ErrHasMaster
		}
		return ErrOfferGone
	}
	now := m.now().Unix()
	m.clearMirror() // nothing an earlier master left behind is applied for this one
	err = m.store.PutFriendLibrary(metadb.FriendLibrary{Name: friend, Side: sideSlave, State: stActive, Nonce: r.Nonce,
		LeftCount: -1, Since: now, UpdatedAt: now})
	var others []metadb.FriendLibrary
	for _, o := range m.roles.Load().rows {
		if o.Name != friend && o.Side == sideSlave && o.State == stOffered {
			others = append(others, o)
			_ = m.store.DeleteFriendLibrary(o.Name)
		}
	}
	m.reloadRolesLogged()
	m.libMu.Unlock()
	if err != nil {
		return err
	}
	m.logf("library: %q is our master", friend)
	m.syncRoleHooks()
	for _, o := range others {
		if of := m.friendByName(o.Name); of != nil {
			m.goTask(func(ctx context.Context) { m.tell(ctx, of, Msg{Type: "lib_decline", Nonce: o.Nonce}) })
		}
	}
	m.libraryStarted()
	return nil
}

// EndLibrary ends the role with friend by its state: withdraw, decline, leave or release.
func (m *Manager) EndLibrary(ctx context.Context, friend string) error {
	f := m.friendByName(friend)
	m.libMu.Lock()
	r, ok := m.roleRow(friend)
	if !ok {
		m.libMu.Unlock()
		return ErrNotFound
	}
	release := r.Side == sideMaster && r.State == stActive
	if !release {
		_ = m.store.DeleteFriendLibrary(friend)
		if r.Side == sideSlave && r.State == stActive {
			m.endMasterLocked()
		}
		m.reloadRolesLogged()
	}
	m.libMu.Unlock()
	m.syncRoleHooks()
	if f == nil {
		return nil
	}
	switch {
	case r.Side == sideMaster && r.State == stProposed:
		m.tell(ctx, f, Msg{Type: "lib_withdraw", Nonce: r.Nonce})
	case r.Side == sideSlave && r.State == stOffered:
		m.tell(ctx, f, Msg{Type: "lib_decline", Nonce: r.Nonce})
	case r.Side == sideSlave && r.State == stActive:
		m.logf("library: we left %q", friend)
		m.tell(ctx, f, Msg{Type: "lib_leave", Nonce: r.Nonce})
	case release:
		ans, err := m.exchange(ctx, f, Msg{Type: "lib_release", Nonce: r.Nonce})
		told := err == nil && ans.Type == "ok"
		m.libMu.Lock()
		if cur, ok := m.roleRow(friend); ok && cur.Nonce == r.Nonce {
			if told {
				_ = m.store.DeleteFriendLibrary(friend)
			} else {
				cur.State, cur.UpdatedAt = stReleased, m.now().Unix()
				_ = m.store.PutFriendLibrary(cur)
			}
			m.reloadRolesLogged()
		}
		m.libMu.Unlock()
		m.syncRoleHooks()
		if !told {
			m.logf("library: %q released, told at its next pull", friend)
		}
	}
	return nil
}

// tell sends msg to f and ignores the answer: a best-effort notice.
func (m *Manager) tell(ctx context.Context, f *friendEntry, msg Msg) {
	if _, err := m.exchange(ctx, f, msg); err != nil && ctx.Err() == nil {
		m.logf("%s to %q: %v", msg.Type, f.name, err)
	}
}

// deleteRole removes friend's role if match accepts it, and reports whether it did.
func (m *Manager) deleteRole(friend string, match func(metadb.FriendLibrary) bool) bool {
	m.libMu.Lock()
	r, ok := m.roleRow(friend)
	if ok && match(r) {
		_ = m.store.DeleteFriendLibrary(friend)
		if r.Side == sideSlave && r.State == stActive {
			m.endMasterLocked()
		}
		m.reloadRolesLogged()
	}
	m.libMu.Unlock()
	if ok && match(r) {
		m.syncRoleHooks()
		return true
	}
	return false
}

// masterCtx is cancelled when the role with master ends: its sessions and pending work stop.
func (m *Manager) masterCtx(master string) context.Context {
	m.libMu.Lock()
	defer m.libMu.Unlock()
	if m.LibraryMaster() != master {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	if m.mctx == nil || m.mctxFor != master {
		m.mctx, m.mcancel = context.WithCancel(context.Background())
		m.mctxFor = master
	}
	return m.mctx
}

// endMasterLocked stops the work of the master that is going and forgets what it sent (libMu held).
func (m *Manager) endMasterLocked() {
	if m.mcancel != nil {
		m.mcancel()
		m.mctx, m.mcancel, m.mctxFor = nil, nil, ""
	}
	m.clearMirror()
}

// clearMirror forgets what a master sent (the files stay), so a next master starts clean.
func (m *Manager) clearMirror() {
	if err := m.store.ClearMirror(); err != nil {
		m.logf("library mirror: %v", err)
	}
	if w := m.lib.Load(); w != nil {
		w.sink.Forget()
	}
}

// endRoleForRemoval tells friend its role ends before the pairing does (best effort).
func (m *Manager) endRoleForRemoval(ctx context.Context, f *friendEntry) {
	r, ok := m.roleRow(f.name)
	if !ok || r.State != stActive {
		return
	}
	if r.Side == sideSlave {
		m.tell(ctx, f, Msg{Type: "lib_leave", Nonce: r.Nonce})
	} else {
		m.tell(ctx, f, Msg{Type: "lib_release", Nonce: r.Nonce})
	}
}

// dropRole forgets the role with a removed friend, and its refusal.
func (m *Manager) dropRole(name string) {
	m.libMu.Lock()
	delete(m.refused, name)
	m.libMu.Unlock()
	m.deleteRole(name, func(metadb.FriendLibrary) bool { return true })
}

// noteRefused keeps why friend refused our proposal, until the next proposal or its removal.
func (m *Manager) noteRefused(friend, why string) {
	m.libMu.Lock()
	defer m.libMu.Unlock()
	if m.refused == nil {
		m.refused = map[string]string{}
	}
	m.refused[friend] = why
}

// LibraryRefused maps each friend that refused our last proposal to why: "has_master" (it gets a
// library from another friend) or "has_slaves" (it sends its library to other friends). In memory.
func (m *Manager) LibraryRefused() map[string]string {
	m.libMu.Lock()
	defer m.libMu.Unlock()
	out := make(map[string]string, len(m.refused))
	for k, v := range m.refused {
		out[k] = v
	}
	return out
}

// expireRoles drops proposals past their expiry; no store access without one.
func (m *Manager) expireRoles() {
	now := m.now().Unix()
	for _, r := range m.roles.Load().rows {
		if (r.State == stProposed || r.State == stOffered) && r.ExpiresAt <= now {
			m.deleteRole(r.Name, func(cur metadb.FriendLibrary) bool { return cur.Nonce == r.Nonce && cur.State == r.State })
		}
	}
}

// serveLibrary answers a friend's lib_* message on the control channel; an invalid one gets none.
func (m *Manager) serveLibrary(f *friendEntry, msg Msg, rw io.ReadWriter, conn net.Conn) {
	if m.lib.Load() == nil {
		return // not wired yet (startup): the sender retries
	}
	if msg.Type == "lib_pull" {
		m.servePull(f, msg, rw, conn)
		return
	}
	if !CheckPair(f.pair, f.role.Other(), msg, "") {
		return
	}
	if _, ok := decodeNonce(msg.Nonce); !ok {
		return
	}
	m.noteContact(f)
	reply := Msg{Type: "ok", Nonce: msg.Nonce}
	switch msg.Type {
	case "lib_offer":
		reply = m.onOffer(f, msg)
	case "lib_accept":
		reply = m.onAccept(f, msg)
	case "lib_withdraw":
		m.deleteRole(f.name, func(r metadb.FriendLibrary) bool {
			return r.Side == sideSlave && r.State == stOffered && r.Nonce == msg.Nonce
		})
	case "lib_decline":
		m.deleteRole(f.name, func(r metadb.FriendLibrary) bool {
			return r.Side == sideMaster && r.State == stProposed && r.Nonce == msg.Nonce
		})
	case "lib_leave":
		if !m.deleteRole(f.name, func(r metadb.FriendLibrary) bool { return r.Side == sideMaster && r.Nonce == msg.Nonce }) {
			if r, ok := m.roleRow(f.name); ok && r.Nonce != msg.Nonce {
				return // an older role's notice: ignored
			}
		} else {
			m.logf("library: %q left", f.name)
		}
	case "lib_release":
		if !m.deleteRole(f.name, func(r metadb.FriendLibrary) bool {
			return r.Side == sideSlave && r.State == stActive && r.Nonce == msg.Nonce
		}) {
			if r, ok := m.roleRow(f.name); ok && r.Nonce != msg.Nonce {
				return
			}
		} else {
			m.logf("library: %q released us", f.name)
		}
	case "lib_news":
		var ok bool
		if reply, ok = m.onNews(f, msg); !ok {
			return
		}
	default:
		return
	}
	reply.Seq = msg.Seq // exchange matches the answer on Nonce and Seq
	reply.MAC = SignPair(f.pair, f.role, reply, "")
	_ = WriteMsg(rw, reply)
}

func refused(nonce, why string) Msg { return Msg{Type: "lib_refused", Nonce: nonce, Name: why} }

// onOffer stores a friend's proposal, unless a rule refuses it.
func (m *Manager) onOffer(f *friendEntry, msg Msg) Msg {
	now := m.now()
	if msg.Seq <= now.Unix() {
		return refused(msg.Nonce, refusedExpired)
	}
	m.libMu.Lock()
	defer m.libMu.Unlock()
	r, has := m.roleRow(f.name)
	switch {
	case m.LibraryMaster() != "" || m.accepts != "":
		return refused(msg.Nonce, refusedHasMaster)
	case len(m.LibrarySlaves()) > 0:
		return refused(msg.Nonce, refusedHasSlaves)
	case has && r.Side == sideMaster:
		return refused(msg.Nonce, refusedRole)
	}
	exp := min(msg.Seq, now.Add(inviteTTL).Unix())
	if err := m.store.PutFriendLibrary(metadb.FriendLibrary{Name: f.name, Side: sideSlave, State: stOffered, Nonce: msg.Nonce,
		ExpiresAt: exp, LeftCount: -1, UpdatedAt: now.Unix()}); err != nil {
		m.logf("library proposal from %q: %v", f.name, err)
		return refused(msg.Nonce, refusedRole)
	}
	m.reloadRolesLogged()
	m.logf("library: %q proposes to be our master", f.name)
	return Msg{Type: "lib_wait", Nonce: msg.Nonce}
}

// onAccept turns our proposal into an active role; repeated, it answers the same.
func (m *Manager) onAccept(f *friendEntry, msg Msg) Msg {
	m.libMu.Lock()
	r, ok := m.roleRow(f.name)
	now := m.now().Unix()
	switch {
	case !ok || r.Side != sideMaster || r.Nonce != msg.Nonce:
		m.libMu.Unlock()
		return refused(msg.Nonce, refusedWithdrawn)
	case r.State == stActive:
		m.libMu.Unlock()
		return Msg{Type: "lib_ok", Nonce: msg.Nonce}
	case r.State != stProposed:
		m.libMu.Unlock()
		return refused(msg.Nonce, refusedWithdrawn)
	case r.ExpiresAt <= now:
		_ = m.store.DeleteFriendLibrary(f.name)
		m.reloadRolesLogged()
		m.libMu.Unlock()
		return refused(msg.Nonce, refusedExpired)
	case m.LibraryMaster() != "" || m.accepts != "":
		m.libMu.Unlock()
		return refused(msg.Nonce, refusedHasMaster)
	}
	r.State, r.Since, r.Cursor, r.ExpiresAt, r.AlignedAt, r.UpdatedAt = stActive, now, 0, 0, 0, now
	err := m.store.PutFriendLibrary(r)
	m.reloadRolesLogged()
	m.libMu.Unlock()
	if err != nil {
		m.logf("library acceptance of %q: %v", f.name, err)
		return refused(msg.Nonce, refusedWithdrawn)
	}
	m.logf("library: %q accepted", f.name)
	m.syncRoleHooks()
	return Msg{Type: "lib_ok", Nonce: msg.Nonce}
}
