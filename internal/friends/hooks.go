package friends

import (
	"crypto/hmac"
	"io"
	"net"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

var _ torrent.FriendHooks = (*Manager)(nil)

// friendAt is the friend whose last known address is addr.
func (s *snapshot) friendAt(addr torrent.PeerRemoteAddr) *friendEntry {
	if addr == nil || len(s.friends) == 0 {
		return nil
	}
	a := addr.String()
	for _, f := range s.friends {
		if f.addr != "" && f.addr == a {
			return f
		}
	}
	return nil
}

// OutgoingPeerID presents the pair ID when dialling a friend's known address.
func (m *Manager) OutgoingPeerID(ih metainfo.Hash, addr torrent.PeerRemoteAddr) ([20]byte, bool) {
	f := m.snap.Load().friendAt(addr)
	if f == nil {
		return [20]byte{}, false
	}
	return PeerID(f.pair, ih, f.role), true
}

// ConfirmOutgoing accepts only the other side's ID: the address alone proves nothing.
func (m *Manager) ConfirmOutgoing(ih metainfo.Hash, addr torrent.PeerRemoteAddr, reply [20]byte) (friend, inviter bool) {
	f := m.snap.Load().friendAt(addr)
	if f == nil || !VerifyPeerID(f.pair, ih, f.role.Other(), reply) {
		return false, false
	}
	return true, f.role == RoleInviter
}

// IncomingPeerID recognises a friend by the ID of its side and answers with ours.
func (m *Manager) IncomingPeerID(ih metainfo.Hash, remote [20]byte) (id [20]byte, friend, inviter, ok bool) {
	s := m.snap.Load()
	for _, f := range s.friends {
		if VerifyPeerID(f.pair, ih, f.role.Other(), remote) {
			// A bare ID proves nothing on a torrent; the control channel MACs what follows.
			if ih == f.control {
				f.seen.Store(m.now().Unix())
			}
			return PeerID(f.pair, ih, f.role), true, f.role == RoleInviter, true
		}
	}
	// An ex-friend dialling our control hash: answer with our old ID so it can read "removed".
	if t, ok := s.ctrlTombs[ih]; ok && t.remote == remote {
		return t.reply, false, false, true
	}
	return [20]byte{}, false, false, false
}

// friendByID is the friend whose other side presents remote on ih.
func (s *snapshot) friendByID(ih, remote [20]byte) *friendEntry {
	for _, f := range s.friends {
		if VerifyPeerID(f.pair, ih, f.role.Other(), remote) {
			return f
		}
	}
	return nil
}

// FriendProof is our proof on the connection with MSE secret s; nil for a stranger.
func (m *Manager) FriendProof(ih metainfo.Hash, remote [20]byte, s []byte, myNonce, theirNonce [16]byte) []byte {
	f := m.snap.Load().friendByID(ih, remote)
	if f == nil {
		return nil
	}
	return ConnProof(f.pair, s, ih, f.role, theirNonce, myNonce)
}

// CheckFriendProof verifies the friend's proof on the current snapshot: a revoked friend fails.
func (m *Manager) CheckFriendProof(ih metainfo.Hash, remote [20]byte, s []byte, myNonce, theirNonce [16]byte, proof []byte) (inviter, ok bool) {
	f := m.snap.Load().friendByID(ih, remote)
	if f == nil {
		return false, false
	}
	want := ConnProof(f.pair, s, ih, f.role.Other(), myNonce, theirNonce)
	if want == nil || !hmac.Equal(want, proof) {
		return false, false
	}
	f.seen.Store(m.now().Unix())
	return f.role == RoleInviter, true
}

// FriendAddrMatch reports whether addr has the IP of the friend's last known address.
func (m *Manager) FriendAddrMatch(ih metainfo.Hash, remote [20]byte, addr torrent.PeerRemoteAddr) bool {
	f := m.snap.Load().friendByID(ih, remote)
	if f == nil || addr == nil {
		return false
	}
	known, err1 := netip.ParseAddrPort(f.addr)
	got, err2 := netip.ParseAddrPort(addr.String())
	return err1 == nil && err2 == nil && known.Addr().Unmap() == got.Addr().Unmap()
}

// FriendPeer is where to dial a friend for one torrent and the peer ID it answers with.
type FriendPeer struct {
	Addr string
	ID   [20]byte
}

// FriendPeers lists the friends with a known address, for dialling them on ih.
func (m *Manager) FriendPeers(ih [20]byte) []FriendPeer {
	var out []FriendPeer
	for _, f := range m.snap.Load().friends {
		if f.addr != "" {
			out = append(out, FriendPeer{Addr: f.addr, ID: PeerID(f.pair, ih, f.role.Other())})
		}
	}
	return out
}

// FriendIP reports whether addr has the IP of a friend's known address or of one of its
// authenticated control contacts.
func (m *Manager) FriendIP(addr torrent.PeerRemoteAddr) bool {
	s := m.snap.Load()
	if addr == nil || len(s.friends) == 0 {
		return false
	}
	got, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return false
	}
	ip := got.Addr().Unmap()
	for _, f := range s.friends {
		if known, err := netip.ParseAddrPort(f.addr); err == nil && known.Addr().Unmap() == ip {
			return true
		}
		if p := f.ctrlIPs.Load(); p != nil && slices.Contains(*p, ip) {
			return true
		}
	}
	return false
}

// noteControlSource remembers the source IP of an authenticated control contact from f: behind a
// loopback NAT, CGNAT or VPN its dials arrive from there, not from its announced address.
func (f *friendEntry) noteControlSource(addr interface{ String() string }) {
	if addr == nil || f.ctrlIPs == nil {
		return
	}
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return
	}
	ip := ap.Addr().Unmap()
	for {
		old := f.ctrlIPs.Load()
		if old != nil && len(*old) > 0 && (*old)[len(*old)-1] == ip {
			return
		}
		var next []netip.Addr
		if old != nil {
			next = slices.DeleteFunc(slices.Clone(*old), func(a netip.Addr) bool { return a == ip })
		}
		next = append(next, ip) // newest last; the oldest go past the bound
		if len(next) > maxCtrlIPs {
			next = next[len(next)-maxCtrlIPs:]
		}
		if f.ctrlIPs.CompareAndSwap(old, &next) {
			return
		}
	}
}

// maxCtrlIPs bounds a friend's remembered sources: a node has a few paths (NAT, CGNAT, VPN,
// IPv4 and IPv6), not one per address it can rotate through.
const maxCtrlIPs = 8

// friendByPair is the friend with this pair key.
func (s *snapshot) friendByPair(key []byte) *friendEntry {
	for _, f := range s.friends {
		if string(f.pair[:]) == string(key) {
			return f
		}
	}
	return nil
}

// FriendProven records the source of a proven friend link: the proof is bound to its MSE secret.
func (m *Manager) FriendProven(ih metainfo.Hash, remote [20]byte, addr torrent.PeerRemoteAddr) {
	if f := m.snap.Load().friendByID(ih, remote); f != nil {
		f.noteControlSource(addr)
	}
}

// FriendCount is the number of friends.
func (m *Manager) FriendCount() int { return len(m.snap.Load().friends) }

// TakeUnknown serves the invite and control channels; anything else goes back to the client.
func (m *Manager) TakeUnknown(ih metainfo.Hash, remote [20]byte, friend bool, rw io.ReadWriter, conn net.Conn) bool {
	s := m.snap.Load()
	f := s.control[ih]
	kind := s.hashes[ih]
	if f == nil && kind == 0 {
		return false
	}
	_ = conn.SetDeadline(time.Now().Add(m.ioTimeout))
	switch {
	case f != nil:
		m.serveControl(f, remote, rw, conn)
	case kind == kindInvite:
		m.serveInvite(ih, rw, conn.RemoteAddr())
	case kind == kindOut:
		m.serveDelivery(ih, rw)
	default:
		if t, ok := s.ctrlTombs[ih]; ok && t.remote == remote {
			if answerWith(rw, Msg{Type: "removed", MAC: t.mac}) {
				m.dropTomb(ih)
			}
		} else {
			answerExpired(rw)
		}
	}
	return true
}

// WantConns keeps accepting while friends, invites or requests exist (reads an atomic only).
func (m *Manager) WantConns() bool { return m.want.Load() }

// warmHooks serve the SSD warmup to friends: open loads a friends-only torrent for a friend's
// name, keys yields the warm infohashes.
type warmHooks struct {
	open func(friend string, ih [20]byte) bool
	keys func(f func([]byte) bool) bool
}

// SetWarm enables (or, with nils, disables) serving the warmup to friends.
func (m *Manager) SetWarm(open func(friend string, ih [20]byte) bool, keys func(f func([]byte) bool) bool) {
	if open == nil && keys == nil {
		m.warm.Store(nil)
		return
	}
	m.warm.Store(&warmHooks{open: open, keys: keys})
}

// OpenFriendTorrent hands a friend's candidate connection for an unknown infohash to the warmup;
// never for control or invite hashes.
func (m *Manager) OpenFriendTorrent(ih metainfo.Hash, remote [20]byte) bool {
	w := m.warm.Load()
	if w == nil || w.open == nil {
		return false
	}
	s := m.snap.Load()
	if s.control[ih] != nil || s.hashes[ih] != 0 {
		return false
	}
	f := s.friendByID(ih, remote)
	if f == nil {
		return false
	}
	return w.open(f.name, ih)
}

// Skeys offers the invite, request, tombstone and control hashes as MSE keys, and the warm
// infohashes to a friend's address only.
func (m *Manager) Skeys(addr torrent.PeerRemoteAddr, f func([]byte) bool) bool {
	if !m.skeys(f) {
		return false
	}
	if w := m.warm.Load(); w != nil && w.keys != nil && m.FriendIP(addr) {
		return w.keys(f)
	}
	return true
}

func (m *Manager) skeys(f func([]byte) bool) bool {
	s := m.snap.Load()
	for h := range s.hashes {
		if !f(h[:]) {
			return false
		}
	}
	for h := range s.control {
		if !f(h[:]) {
			return false
		}
	}
	return true
}

type reachability struct {
	mu       sync.Mutex
	proven   bool
	last     time.Time
	networks map[string]bool
}

// Reach is the measured reachability: Checking until an incoming handshake proves the port open.
type Reach struct {
	Checking  bool      `json:"checking"`
	Reachable bool      `json:"reachable"`
	Last      time.Time `json:"last,omitzero"` // last proof, kept across resets
	Networks  []string  `json:"networks"`      // networks with incoming handshakes since the last reset
}

// OnHandshake records completed handshakes; incoming ones prove the port is open (LAN and hairpin
// connections count too).
func (m *Manager) OnHandshake(incoming bool, network string) {
	if !incoming {
		return
	}
	now := m.now()
	r := &m.reach
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proven, r.last = true, now
	if r.networks == nil {
		r.networks = map[string]bool{}
	}
	r.networks[network] = true
}

// ResetReachability starts a new check; called on a public address change and on Reconnect.
func (m *Manager) ResetReachability() {
	r := &m.reach
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proven, r.networks = false, nil
}

// Reach reports whether an incoming handshake arrived since start or the last reset.
func (m *Manager) Reach() Reach {
	r := &m.reach
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Reach{Checking: !r.proven, Reachable: r.proven, Last: r.last, Networks: []string{}}
	for n := range r.networks {
		out.Networks = append(out.Networks, n)
	}
	sort.Strings(out.Networks)
	return out
}
