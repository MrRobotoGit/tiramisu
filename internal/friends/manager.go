package friends

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"tiramisu/internal/metadb"
)

const (
	inviteTTL        = 48 * time.Hour
	deliveryWindow   = 48 * time.Hour
	tombstoneTTL     = inviteTTL + deliveryWindow
	minBackoff       = 15 * time.Minute // acceptLimitClearer period: never redial a node faster
	maxBackoff       = time.Hour
	defaultIOTimeout = 30 * time.Second
	maxNameLen       = 64

	settingName  = "name"
	settingAddr  = "addr"
	settingTombs = "tombstones"
	settingAcked = "addr_acked" // control hash (hex) -> our address that friend acknowledged

	roleInviterStr = "inviter"
	roleJoinerStr  = "joiner"
)

var (
	ErrNoPublicAddr   = errors.New("friends: public address unknown")
	ErrInviteExpired  = errors.New("friends: invitation expired or already used")
	ErrOwnInvite      = errors.New("friends: this invitation was created by this node")
	ErrNotFound       = errors.New("friends: not found")
	ErrBadName        = errors.New("friends: invalid node name")
	ErrUnexpected     = errors.New("friends: unexpected answer from the other node")
	ErrAlreadyDecided = errors.New("friends: request already decided the other way")
	// ErrNoAnswer: the request was sent but its answer was lost; it is retried automatically.
	ErrNoAnswer = errors.New("friends: request sent, answer lost")
	// ErrUnreachable: the other node could not be dialled or did not answer.
	ErrUnreachable = errors.New("friends: other node unreachable")
)

// Store is the persistence the Manager needs; *metadb.DB satisfies it.
type Store interface {
	ListFriends() ([]metadb.Friend, error)
	PutFriend(metadb.Friend) error // error on duplicate name
	DeleteFriend(name string) error
	UpdateFriendAddr(name, addr string, seen int64) error
	PutInvite(metadb.FriendInvite) error
	GetInvite(hash []byte) (metadb.FriendInvite, error)
	ListInvites() ([]metadb.FriendInvite, error)
	SetInviteState(hash []byte, state string) error
	PurgeInvites(now int64) error
	PutRequest(metadb.FriendRequest) (int64, error)
	GetRequest(id int64) (metadb.FriendRequest, error)
	ListRequests() ([]metadb.FriendRequest, error)
	SetRequestState(id int64, state string, pairKey []byte, expiresAt int64) error // erases eph_priv
	SetRequestApproved(id int64, pairKey, nonceTheirs, pubMine, pubTheirs []byte, expiresAt int64) error
	DeleteRequest(id int64) error
	PurgeRequests(now int64) error
	SetFriendAddrSeq(name string, addrSeq, mySeq int64) error
	TouchFriend(name string, seen int64) error
	SetFriendSecure(name string, pairKey []byte) error
	ClearFriendSecure(name string, pairKey []byte) error
	GetFriendSetting(key string) (string, error)
	SetFriendSetting(key, value string) error

	PutFriendLibrary(metadb.FriendLibrary) error
	GetFriendLibrary(name string) (metadb.FriendLibrary, bool, error)
	ListFriendLibrary() ([]metadb.FriendLibrary, error)
	SetFriendLibraryCursor(name string, cursor, left, updated int64) error
	SetFriendLibraryAligned(name string, at int64) error
	DeleteFriendLibrary(name string) error
	ClearMirror() error
	MirrorCounts() (films, episodes, failed int64, err error)
	ListMirrorRecent(n int) ([]metadb.MirrorRow, error)
}

// Client is the part of the BT client the Manager uses (an adapter over the current bt.Client).
type Client interface {
	DialControl(ctx context.Context, addr string, ih [20]byte, id [20]byte) (io.ReadWriter, net.Conn, [20]byte, error)
	// FreshPublicAddr reads the public IPv4 without the process cache, plus the listen port.
	FreshPublicAddr(ctx context.Context) (netip.AddrPort, error)
}

type hashKind byte

const (
	kindInvite hashKind = iota + 1 // our open invite or an in request on it
	kindOut                        // our out request: the inviter may deliver here
	kindTomb                       // closed: answered with "expired" (or "removed" for a control hash)
)

type friendEntry struct {
	name    string
	pair    PairKey
	role    Role // our side
	addr    string
	control [20]byte
	seen    *atomic.Int64                 // last contact seen by the hooks, flushed by Tick
	secure  *atomic.Bool                  // seen on the encrypted channel: plaintext refused, both ways
	ctrlIPs *atomic.Pointer[[]netip.Addr] // sources of authenticated control contacts, in memory
}

// tomb is a closed hash. Control tombs keep the ex-friend's ID, our reply ID and the MAC of
// "removed" (never the pair key); they last until the ex-friend has read it, not for a time.
type tomb struct {
	Exp    int64  `json:"exp"`
	Remote string `json:"remote,omitempty"`
	Reply  string `json:"reply,omitempty"`
	MAC    string `json:"mac,omitempty"`
}

func (t tomb) live(now int64) bool { return t.Remote != "" || t.Exp > now }

type ctrlTomb struct {
	remote, reply [20]byte
	mac           string
}

// snapshot is the immutable view read by the fork hooks.
type snapshot struct {
	friends   []*friendEntry
	control   map[[20]byte]*friendEntry
	hashes    map[[20]byte]hashKind
	ctrlTombs map[[20]byte]ctrlTomb
}

type backoff struct {
	n  int
	at time.Time
}

// Manager holds the friends state and implements the fork's FriendHooks.
type Manager struct {
	store  Store
	client Client
	now    func() time.Time
	warm   atomic.Pointer[warmHooks] // nil: the warmup is not served

	helpHooks atomic.Pointer[HelpHooks] // nil: no help to friends' playbacks
	help      helpState                 // sessions we help
	activity  atomic.Pointer[func() map[string]*Activity]
	lastWatch atomic.Int64 // the last watch we sent (watchSeq)
	lastSess  atomic.Int64 // the last library pull session (sessionID)

	// CloseFriendConns closes open peer connections whose (infohash, peer ID) match; wired by the engine.
	CloseFriendConns func(match func(ih, peerID [20]byte) bool)
	Logf             func(format string, args ...any)
	// OnActive runs under the Manager's lock when Active turns true: it must not call back in.
	OnActive func()
	// OnFriendAddrs runs under the Manager's lock with the addresses friends gained or moved to
	// (new pairing, address update): it must not call back in.
	OnFriendAddrs func(addrs []string)
	// Go starts a background task bound to the engine's lifetime (no task after Close: Tick and
	// Resume pick the stored work up later). Nil: a plain goroutine with context.Background().
	Go func(f func(context.Context))

	want   atomic.Bool
	active atomic.Bool // friends, live invites or requests: the public address matters
	snap   atomic.Pointer[snapshot]
	lastIP atomic.Pointer[netip.AddrPort]
	// addrRead: lastIP came from a read in this process, not only from the DB.
	addrRead atomic.Bool

	mu           sync.Mutex // guards writes; never held across network I/O or calls into client
	seen         map[string]*atomic.Int64
	secure       map[string]*atomic.Bool
	plainUntil   map[[20]byte]time.Time // control hash -> until when the friend is taken for a plaintext node
	refusedPlain map[[20]byte]bool      // control hash -> a refused plaintext contact was logged
	ctrlIPs      map[string]*atomic.Pointer[[]netip.Addr]
	tombs        map[[20]byte]tomb
	acked        map[[20]byte]string   // control hash -> our address the friend acknowledged
	reannounce   map[[20]byte]bool     // control hash -> contact once regardless of the ack
	retry        map[int64]*backoff    // request id -> next delivery or resume
	addrRetry    map[[20]byte]*backoff // control hash -> next address notification

	reach reachability

	ioTimeout time.Duration // per dial and, in a library session, per message
	noHello   bool          // tests: dial as an older binary, which never offers encryption

	lib      atomic.Pointer[libWiring] // nil until SetLibrary: lib_* messages are dropped
	roles    atomic.Pointer[libSnap]
	libMu    sync.Mutex      // serialises role changes; never held across network I/O
	accepts  string          // the master we are accepting right now (guarded by libMu)
	mctx     context.Context // cancelled when the role with mctxFor ends (guarded by libMu)
	mcancel  context.CancelFunc
	mctxFor  string
	refused  map[string]string // friend -> why it refused our last proposal (guarded by libMu)
	hookMu   sync.Mutex        // orders OnMaster and the feed's Start/Stop
	fired    string            // the master OnMaster last reported (guarded by hookMu)
	feedOn   atomic.Bool
	feedMu   sync.Mutex               // serialises the feed's Start and Stop
	feedRun  bool                     // what the source was last told (guarded by feedMu)
	pulls    map[string]*pullState    // per master (guarded by mu)
	news     map[string]*newsState    // per slave (guarded by mu)
	progress map[string]*pullProgress // per master, the first alignment (guarded by mu)

	ping pingState // friends' presence, in memory
}

// New loads the state and builds the first snapshot.
func New(store Store, client Client, now func() time.Time) (*Manager, error) {
	if now == nil {
		now = time.Now
	}
	m := &Manager{
		store: store, client: client, now: now,
		Logf:         log.Printf,
		seen:         map[string]*atomic.Int64{},
		secure:       map[string]*atomic.Bool{},
		plainUntil:   map[[20]byte]time.Time{},
		refusedPlain: map[[20]byte]bool{},
		ctrlIPs:      map[string]*atomic.Pointer[[]netip.Addr]{},
		tombs:        map[[20]byte]tomb{},
		acked:        map[[20]byte]string{},
		reannounce:   map[[20]byte]bool{},
		retry:        map[int64]*backoff{},
		addrRetry:    map[[20]byte]*backoff{},
		ioTimeout:    defaultIOTimeout,
		pulls:        map[string]*pullState{},
		news:         map[string]*newsState{},
		progress:     map[string]*pullProgress{},
	}
	if err := m.loadTombs(); err != nil {
		return nil, err
	}
	if v, err := store.GetFriendSetting(settingAcked); err != nil {
		return nil, err
	} else if v != "" {
		var raw map[string]string
		if err := json.Unmarshal([]byte(v), &raw); err != nil {
			m.logf("acknowledged addresses unreadable, all friends will be told again: %v", err)
		}
		for k, a := range raw {
			if h, ok := decode20(k); ok {
				m.acked[h] = a
			}
		}
	}
	if v, _ := store.GetFriendSetting(settingAddr); v != "" {
		if ap, err := netip.ParseAddrPort(v); err == nil && validAddr(ap) {
			m.lastIP.Store(&ap)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshLocked(); err != nil {
		return nil, err
	}
	if err := m.reloadRoles(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) loadTombs() error {
	v, err := m.store.GetFriendSetting(settingTombs)
	if err != nil || v == "" {
		return err
	}
	var raw map[string]tomb
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		// Older format: hash -> expiry.
		var old map[string]int64
		if err2 := json.Unmarshal([]byte(v), &old); err2 != nil {
			m.logf("tombstones unreadable, closed hashes forgotten: %v", err)
			return nil
		}
		raw = map[string]tomb{}
		for k, exp := range old {
			raw[k] = tomb{Exp: exp}
		}
	}
	for k, t := range raw {
		if h, ok := decode20(k); ok {
			m.tombs[h] = t
		} else {
			m.logf("tombstone %q unreadable", k)
		}
	}
	return nil
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf("[Friends] "+format, args...)
	}
}

func decode20(s string) ([20]byte, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 20 {
		return [20]byte{}, false
	}
	return [20]byte(b), true
}

// refreshLocked rebuilds the snapshot and want from the store. Expired invites and requests
// count as tombstones until expireLocked records them, so there is no window for penalties.
func (m *Manager) refreshLocked() error {
	fs, err := m.store.ListFriends()
	if err != nil {
		return err
	}
	invs, err := m.store.ListInvites()
	if err != nil {
		return err
	}
	reqs, err := m.store.ListRequests()
	if err != nil {
		return err
	}
	now := m.now().Unix()
	s := &snapshot{control: map[[20]byte]*friendEntry{}, hashes: map[[20]byte]hashKind{}, ctrlTombs: map[[20]byte]ctrlTomb{}}
	for h, t := range m.tombs {
		if !t.live(now) {
			continue
		}
		s.hashes[h] = kindTomb
		remote, ok1 := decode20(t.Remote)
		reply, ok2 := decode20(t.Reply)
		if ok1 && ok2 {
			s.ctrlTombs[h] = ctrlTomb{remote: remote, reply: reply, mac: t.MAC}
		}
	}
	for _, in := range invs {
		if len(in.Hash) == 20 {
			s.hashes[[20]byte(in.Hash)] = kindTomb
		}
	}
	for _, r := range reqs {
		if len(r.InviteHash) == 20 {
			s.hashes[[20]byte(r.InviteHash)] = kindTomb
		}
	}
	// Live rows override; our own invite wins over an out request on the same hash.
	for _, r := range reqs {
		if len(r.InviteHash) == 20 && r.ExpiresAt > now && r.Direction == "out" {
			s.hashes[[20]byte(r.InviteHash)] = kindOut
		}
	}
	for _, in := range invs {
		if len(in.Hash) == 20 && in.ExpiresAt > now && (in.State == "open" || in.State == "requested") {
			s.hashes[[20]byte(in.Hash)] = kindInvite
		}
	}
	for _, r := range reqs {
		if len(r.InviteHash) == 20 && r.ExpiresAt > now && r.Direction == "in" {
			s.hashes[[20]byte(r.InviteHash)] = kindInvite
		}
	}
	live := map[string]bool{}
	for _, f := range fs {
		if len(f.PairKey) != len(PairKey{}) {
			continue
		}
		e := &friendEntry{name: f.Name, pair: PairKey(f.PairKey), role: roleFromString(f.Role), addr: f.Addr}
		e.control = ControlHash(e.pair)
		if m.seen[f.Name] == nil {
			m.seen[f.Name] = new(atomic.Int64)
		}
		e.seen = m.seen[f.Name]
		if m.secure[f.Name] == nil {
			m.secure[f.Name] = new(atomic.Bool)
		}
		if f.Secure {
			m.secure[f.Name].Store(true)
		}
		e.secure = m.secure[f.Name]
		if m.ctrlIPs[f.Name] == nil {
			m.ctrlIPs[f.Name] = new(atomic.Pointer[[]netip.Addr])
		}
		e.ctrlIPs = m.ctrlIPs[f.Name]
		live[f.Name] = true
		s.friends = append(s.friends, e)
		s.control[e.control] = e
	}
	for name := range m.seen {
		if !live[name] {
			delete(m.seen, name)
			delete(m.ctrlIPs, name)
			delete(m.secure, name)
		}
	}
	old := m.snap.Swap(s)
	if a := newFriendAddrs(old, s); len(a) > 0 && m.OnFriendAddrs != nil {
		m.OnFriendAddrs(a)
	}
	// A pending control tombstone keeps accepting connections until the ex-friend dials.
	m.want.Store(len(s.friends) > 0 || len(s.hashes) > 0)
	active := len(s.friends) > 0
	for _, k := range s.hashes {
		active = active || k != kindTomb
	}
	was := m.active.Swap(active)
	if !active {
		m.addrRead.Store(false) // not kept fresh while inactive: the next invite reads it again
	}
	if !was && active && m.OnActive != nil {
		m.OnActive()
	}
	return nil
}

// newFriendAddrs lists the friend addresses of s that old did not have for that friend.
func newFriendAddrs(old, s *snapshot) (out []string) {
	known := map[PairKey]string{}
	if old != nil {
		for _, f := range old.friends {
			known[f.pair] = f.addr
		}
	}
	for _, f := range s.friends {
		if a, ok := known[f.pair]; f.addr != "" && (!ok || a != f.addr) {
			out = append(out, f.addr)
		}
	}
	return out
}

// Active reports whether there is a friend, a live invite or a live request: only then is the
// public address worth reading.
func (m *Manager) Active() bool { return m.active.Load() }

func roleFromString(s string) Role {
	if s == roleInviterStr {
		return RoleInviter
	}
	return RoleJoiner
}

// addTombLocked keeps h answerable with "expired" for tombstoneTTL.
func (m *Manager) addTombLocked(h []byte) {
	if len(h) != 20 {
		return
	}
	if t, ok := m.tombs[[20]byte(h)]; ok && t.Remote != "" {
		return // a control tomb already answers this hash
	}
	m.tombs[[20]byte(h)] = tomb{Exp: m.now().Add(tombstoneTTL).Unix()}
	m.saveTombsLocked()
}

// pruneTombsLocked drops dead tombstones and reports whether any went.
func (m *Manager) pruneTombsLocked() bool {
	now := m.now().Unix()
	changed := false
	for h, t := range m.tombs {
		if !t.live(now) {
			delete(m.tombs, h)
			changed = true
		}
	}
	return changed
}

// addControlTombLocked answers the ex-friend's future control dials with an authenticated "removed".
func (m *Manager) addControlTombLocked(f *friendEntry) {
	remote := PeerID(f.pair, f.control, f.role.Other())
	reply := PeerID(f.pair, f.control, f.role)
	m.tombs[f.control] = tomb{
		Remote: hex.EncodeToString(remote[:]),
		Reply:  hex.EncodeToString(reply[:]),
		MAC:    SignPair(f.pair, f.role, Msg{Type: "removed"}, ""),
	}
	m.saveTombsLocked()
}

func (m *Manager) saveTombsLocked() {
	m.pruneTombsLocked()
	raw := map[string]tomb{}
	for h, t := range m.tombs {
		raw[hex.EncodeToString(h[:])] = t
	}
	b, _ := json.Marshal(raw)
	if err := m.store.SetFriendSetting(settingTombs, string(b)); err != nil {
		m.logf("save tombstones: %v", err)
	}
}

func (m *Manager) saveAckedLocked() {
	raw := map[string]string{}
	for h, a := range m.acked {
		raw[hex.EncodeToString(h[:])] = a
	}
	b, _ := json.Marshal(raw)
	if err := m.store.SetFriendSetting(settingAcked, string(b)); err != nil {
		m.logf("save acknowledged addresses: %v", err)
	}
}

// SelfName is this node's name as friends see it; the hostname until set.
func (m *Manager) SelfName() string {
	if v, err := m.store.GetFriendSetting(settingName); err == nil && validName(v) {
		return v
	}
	if h, err := os.Hostname(); err == nil {
		h = strings.TrimSpace(h)
		if i := strings.IndexByte(h, '.'); i > 0 {
			h = h[:i]
		}
		if validName(h) {
			return h
		}
	}
	return "tiramisu"
}

// SetSelfName changes the name announced in future invites and joins.
func (m *Manager) SetSelfName(name string) error {
	if !validName(name) {
		return ErrBadName
	}
	return m.store.SetFriendSetting(settingName, name)
}

func validName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > maxNameLen || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// uniqueNameLocked turns a declared name into a free local label: "raspberrypi (2)".
func (m *Manager) uniqueNameLocked(name string) (string, error) {
	fs, err := m.store.ListFriends()
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, f := range fs {
		taken[f.Name] = true
	}
	if !taken[name] {
		return name, nil
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf(" (%d)", i)
		base := name
		for len(base)+len(suffix) > maxNameLen {
			_, size := utf8.DecodeLastRuneInString(base)
			base = base[:len(base)-size]
		}
		if c := strings.TrimSpace(base) + suffix; !taken[c] {
			return c, nil
		}
	}
}

// friendByPairLocked finds a stored friend with this pair key (re-entry after a lost answer).
func (m *Manager) friendByPairLocked(pair []byte) bool {
	fs, err := m.store.ListFriends()
	if err != nil {
		return false
	}
	for _, f := range fs {
		if bytes.Equal(f.PairKey, pair) {
			return true
		}
	}
	return false
}

func validAddrString(s string) bool {
	ap, err := netip.ParseAddrPort(s)
	return err == nil && validAddr(ap) && ap.String() == s
}

func decodeNonce(s string) ([16]byte, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return [16]byte{}, false
	}
	return [16]byte(b), true
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// randomPeerID is a plain qBittorrent-looking ID for invite-channel dials.
func randomPeerID() (id [20]byte) {
	copy(id[:], peerIDPrefix)
	for i, b := range randomBytes(12) {
		id[8+i] = b32Alphabet[b&31]
	}
	return id
}

// publicAddr returns the last known public address, reading (and recording) one if none is known.
// With fresh, an address only seeded from the DB (no successful read since start) is re-read.
func (m *Manager) publicAddr(ctx context.Context, fresh bool) (netip.AddrPort, error) {
	if p := m.lastIP.Load(); p != nil && (!fresh || m.addrRead.Load()) {
		return *p, nil
	}
	ap, err := m.client.FreshPublicAddr(ctx)
	if err != nil || !validAddr(ap) {
		return netip.AddrPort{}, ErrNoPublicAddr
	}
	m.recordAddr(ap)
	return ap, nil
}

// recordAddr stores the current public address; a change resets reachability and address backoff.
func (m *Manager) recordAddr(ap netip.AddrPort) bool {
	m.lastIP.Store(&ap)
	m.addrRead.Store(true)
	m.mu.Lock()
	defer m.mu.Unlock()
	if known, _ := m.store.GetFriendSetting(settingAddr); known == ap.String() {
		return false
	}
	if err := m.store.SetFriendSetting(settingAddr, ap.String()); err != nil {
		m.logf("save public address: %v", err)
	}
	clear(m.addrRetry)
	m.ResetReachability()
	m.logf("public address now %s", ap)
	return true
}

func (m *Manager) findRequestLocked(ih []byte, direction string) (metadb.FriendRequest, bool) {
	reqs, err := m.store.ListRequests()
	if err != nil {
		return metadb.FriendRequest{}, false
	}
	now := m.now().Unix()
	for _, r := range reqs {
		if r.Direction == direction && bytes.Equal(r.InviteHash, ih) && r.ExpiresAt > now {
			return r, true
		}
	}
	return metadb.FriendRequest{}, false
}

// bump moves b to its next backoff step: 15 min doubling up to 1 h.
func (m *Manager) bump(b *backoff) {
	b.n++
	b.at = m.now().Add(RetryDelay(b.n))
}

// RetryDelay is the wait before attempt n+1 after n failures: 15 min doubling up to 1 h.
func RetryDelay(n int) time.Duration {
	d := minBackoff << (max(n, 1) - 1)
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	return d
}

func (m *Manager) scheduleLocked(id int64) {
	b := m.retry[id]
	if b == nil {
		b = &backoff{}
		m.retry[id] = b
	}
	m.bump(b)
}

// inFlightLocked marks a retry as running so a concurrent Tick does not dial it again.
func inFlightLocked[K comparable](table map[K]*backoff, k K, now time.Time) {
	b := table[k]
	if b == nil {
		b = &backoff{}
		table[k] = b
	}
	b.at = now.Add(minBackoff)
}

// Tick expires invites and requests, retries deliveries, resumes joins, re-sends unacknowledged
// addresses, flushes last contact and checks which friends are online.
func (m *Manager) Tick(ctx context.Context) {
	now := m.now()
	var deliveries, resumes []int64
	m.mu.Lock()
	if !m.want.Load() && len(m.tombs) == 0 {
		m.mu.Unlock()
		return // friends unused: no store access at all
	}
	m.expireLocked()
	if reqs, err := m.store.ListRequests(); err == nil {
		for _, r := range reqs {
			b := m.retry[r.ID]
			if b == nil && r.Direction == "out" && r.State == "pending" {
				m.scheduleLocked(r.ID)
				continue
			}
			if b != nil && b.at.After(now) {
				continue
			}
			switch {
			case r.Direction == "in" && (r.State == "approved" || r.State == "declined"):
				deliveries = append(deliveries, r.ID)
			case r.Direction == "out" && r.State == "pending":
				resumes = append(resumes, r.ID)
			default:
				continue
			}
			inFlightLocked(m.retry, r.ID, now)
		}
	}
	for _, f := range m.snap.Load().friends {
		if t := f.seen.Swap(0); t != 0 {
			_ = m.store.TouchFriend(f.name, t)
		}
	}
	m.mu.Unlock()
	m.expireRoles()
	m.libraryTick()

	for _, id := range deliveries {
		m.tryDeliver(ctx, id)
	}
	for _, id := range resumes {
		m.tryResume(ctx, id)
	}
	m.notifyDue(ctx)
	m.pingRound(ctx)
}

// expireLocked turns expired invites and requests into tombstones and purges them; it writes
// only when something changed.
func (m *Manager) expireLocked() {
	now := m.now().Unix()
	exp := m.now().Add(tombstoneTTL).Unix()
	purgeInv, purgeReq := false, false
	if invs, err := m.store.ListInvites(); err == nil {
		for _, in := range invs {
			if in.ExpiresAt <= now && len(in.Hash) == 20 {
				m.tombs[[20]byte(in.Hash)] = tomb{Exp: exp}
			}
			purgeInv = purgeInv || in.ExpiresAt <= now || in.State == "used" || in.State == "cancelled"
		}
	}
	if reqs, err := m.store.ListRequests(); err == nil {
		for _, r := range reqs {
			if r.ExpiresAt <= now {
				if len(r.InviteHash) == 20 {
					m.tombs[[20]byte(r.InviteHash)] = tomb{Exp: exp}
				}
				// Approved but never confirmed: the joiner may hold the pairing, so its control
				// dial must read an authenticated "removed".
				if r.Direction == "in" && r.State == "approved" && len(r.PairKey) == len(PairKey{}) && !m.hasPairLocked(r.PairKey) {
					pair := PairKey(r.PairKey)
					m.addControlTombLocked(&friendEntry{pair: pair, control: ControlHash(pair), role: RoleInviter})
				}
				delete(m.retry, r.ID)
				purgeReq = true
			}
		}
	}
	if purgeInv {
		_ = m.store.PurgeInvites(now)
	}
	if purgeReq {
		_ = m.store.PurgeRequests(now)
	}
	if m.pruneTombsLocked() || purgeInv || purgeReq {
		m.saveTombsLocked()
	}
	if err := m.refreshLocked(); err != nil {
		m.logf("refresh: %v", err)
	}
}

// hasPairLocked reports whether a stored friend has this pair key.
func (m *Manager) hasPairLocked(key []byte) bool {
	fs, err := m.store.ListFriends()
	if err != nil {
		return true // unknown: no tombstone that could cut a live pairing
	}
	for _, f := range fs {
		if bytes.Equal(f.PairKey, key) {
			return true
		}
	}
	return false
}

// Run calls Tick every minute until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Tick(ctx)
		}
	}
}

// Resume (at startup only) retries every undecided out request and pending delivery now.
func (m *Manager) Resume(ctx context.Context) {
	m.mu.Lock()
	if reqs, err := m.store.ListRequests(); err == nil {
		for _, r := range reqs {
			if r.Direction == "out" && r.State == "pending" || r.Direction == "in" && r.State != "pending" {
				m.retry[r.ID] = &backoff{at: m.now()}
			}
		}
	}
	m.mu.Unlock()
	m.Tick(ctx)
}

// answerWith sends one closing frame while draining the dialler's frames (both may write first).
// It reports whether the frame was written and the dialler then closed cleanly.
func answerWith(rw io.ReadWriter, msg Msg) bool {
	werr := make(chan error, 1)
	go func() { werr <- WriteMsg(rw, msg) }()
	var rerr error
	for rerr == nil {
		_, rerr = ReadMsg(rw)
	}
	return <-werr == nil && errors.Is(rerr, io.EOF)
}

// dropTomb forgets a control tombstone once the ex-friend has read "removed".
func (m *Manager) dropTomb(h [20]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tombs, h)
	m.saveTombsLocked()
	if err := m.refreshLocked(); err != nil {
		m.logf("refresh: %v", err)
	}
}

func answerExpired(rw io.ReadWriter) { _ = answerWith(rw, Msg{Type: "expired"}) }
