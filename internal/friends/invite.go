package friends

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"tiramisu/internal/metadb"
)

// InviteInfo is a freshly created invite: ID is the hex of its InviteHash.
type InviteInfo struct {
	ID      string
	Code    string
	Expires time.Time
}

// PreviewInfo is what the inviter declares in hello (not authenticated).
type PreviewInfo struct {
	Name    string
	Expires time.Time
}

// CreateInvite issues a one-time code with the last known public address.
func (m *Manager) CreateInvite(ctx context.Context) (InviteInfo, error) {
	addr, err := m.publicAddr(ctx, true)
	if err != nil {
		return InviteInfo{}, err
	}
	var inv Invite
	copy(inv.Secret[:], randomBytes(16))
	inv.Addr = addr
	code, err := EncodeCode(inv)
	if err != nil {
		return InviteInfo{}, err
	}
	ih := InviteHash(inv.Secret)
	now := m.now()
	exp := now.Add(inviteTTL)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.store.PutInvite(metadb.FriendInvite{Hash: ih[:], Secret: inv.Secret[:], ExpiresAt: exp.Unix(), State: "open", CreatedAt: now.Unix()}); err != nil {
		return InviteInfo{}, err
	}
	if err := m.refreshLocked(); err != nil {
		return InviteInfo{}, err
	}
	return InviteInfo{ID: hex.EncodeToString(ih[:]), Code: code, Expires: exp}, nil
}

// CancelInvite withdraws an unused invite and any undecided request on it.
func (m *Manager) CancelInvite(id string) error {
	h, err := hex.DecodeString(id)
	if err != nil || len(h) != 20 {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	in, err := m.store.GetInvite(h)
	if err != nil || (in.State != "open" && in.State != "requested") {
		return ErrNotFound
	}
	if r, ok := m.findRequestLocked(h, "in"); ok {
		if r.State != "pending" {
			return ErrNotFound
		}
		_ = m.store.DeleteRequest(r.ID)
	}
	if err := m.store.SetInviteState(h, "cancelled"); err != nil {
		return err
	}
	m.addTombLocked(h)
	_ = m.store.PurgeInvites(m.now().Unix())
	return m.refreshLocked()
}

// Preview reads the inviter's declared name and expiry without using the code.
func (m *Manager) Preview(ctx context.Context, code string) (PreviewInfo, error) {
	inv, err := DecodeCode(code)
	if err != nil {
		return PreviewInfo{}, err
	}
	rw, conn, err := m.dialInvite(ctx, inv)
	if err != nil {
		return PreviewInfo{}, err
	}
	defer conn.Close()
	hello, err := readHello(rw)
	if err != nil {
		return PreviewInfo{}, err
	}
	return PreviewInfo{Name: hello.Name, Expires: time.Unix(hello.Expires, 0)}, nil
}

func (m *Manager) dialInvite(ctx context.Context, inv Invite) (io.ReadWriter, net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, m.ioTimeout)
	defer cancel()
	rw, conn, _, err := m.client.DialControl(dctx, inv.Addr.String(), InviteHash(inv.Secret), randomPeerID())
	if err != nil {
		return nil, nil, fmt.Errorf("%w: inviting node: %w", ErrUnreachable, err)
	}
	conn = closeOnCancel(ctx, conn)
	_ = conn.SetDeadline(time.Now().Add(m.ioTimeout))
	return rw, conn, nil
}

// readHello reads the first frame of the invite channel: hello, or a definite refusal.
func readHello(rw io.Reader) (Msg, error) {
	m, err := ReadMsg(rw)
	if err != nil {
		return Msg{}, fmt.Errorf("%w: no answer from the inviting node: %w", ErrUnreachable, err)
	}
	switch m.Type {
	case "hello":
		if !validName(m.Name) || m.Expires <= 0 {
			return Msg{}, ErrUnexpected
		}
		return m, nil
	case "expired":
		return Msg{}, ErrInviteExpired
	}
	return Msg{}, ErrUnexpected
}

// Join sends the request for a code and returns its id; (id, ErrNoAnswer) means it was saved and
// is retried. Any other error leaves no request behind.
func (m *Manager) Join(ctx context.Context, code string) (int64, error) {
	inv, err := DecodeCode(code)
	if err != nil {
		return 0, err
	}
	ih := InviteHash(inv.Secret)
	if _, err := m.store.GetInvite(ih[:]); err == nil {
		return 0, ErrOwnInvite // also guards self-pairing through a hairpin NAT
	}
	m.mu.Lock()
	if r, ok := m.findRequestLocked(ih[:], "out"); ok {
		m.mu.Unlock()
		return r.ID, nil
	}
	m.mu.Unlock()
	if _, err := m.publicAddr(ctx, false); err != nil {
		return 0, err
	}
	rw, conn, err := m.dialInvite(ctx, inv)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	hello, err := readHello(rw)
	if err != nil {
		return 0, err
	}
	now := m.now()
	if hello.Expires <= now.Unix() {
		return 0, ErrInviteExpired
	}
	exp := min(hello.Expires, now.Add(inviteTTL).Unix()) + int64(deliveryWindow/time.Second)
	eph, err := NewEphemeral()
	if err != nil {
		return 0, err
	}
	r := metadb.FriendRequest{
		InviteHash: ih[:], Direction: "out", Name: hello.Name, Addr: inv.Addr.String(),
		NonceMine: randomBytes(16), Secret: inv.Secret[:], State: "pending",
		EphPriv: eph.Bytes(), PubMine: eph.PublicKey().Bytes(),
		ExpiresAt: exp, CreatedAt: now.Unix(),
	}
	m.mu.Lock()
	r.ID, err = m.store.PutRequest(r)
	if err == nil {
		err = m.refreshLocked()
	}
	m.mu.Unlock()
	if err != nil {
		return 0, err
	}
	// Stored before sending: once join is out, a lost answer is resumed with the same nonce.
	if err := m.joinExchange(ctx, rw, r); err != nil {
		var definite definiteErr
		if errors.As(err, &definite) {
			m.closeRequest(r, false)
			return 0, definite.error
		}
		m.mu.Lock()
		m.scheduleLocked(r.ID)
		m.mu.Unlock()
		return r.ID, fmt.Errorf("%w: %v", ErrNoAnswer, err)
	}
	m.mu.Lock()
	if cur, err := m.store.GetRequest(r.ID); err == nil && cur.State == "pending" {
		m.scheduleLocked(r.ID) // ask again until a decision arrives
	}
	m.mu.Unlock()
	return r.ID, nil
}

// definiteErr marks an answer that closes the request (refused or expired).
type definiteErr struct{ error }

// joinExchange sends join after hello and handles the inviter's answer.
func (m *Manager) joinExchange(ctx context.Context, rw io.ReadWriter, r metadb.FriendRequest) error {
	addr, err := m.publicAddr(ctx, false)
	if err != nil {
		return err
	}
	join := Msg{Type: "join", Name: m.SelfName(), Nonce: hex.EncodeToString(r.NonceMine), Addr: addr.String(), Pub: hex.EncodeToString(r.PubMine)}
	join.MAC = SignInvite([16]byte(r.Secret), RoleJoiner, join, "")
	if err := WriteMsg(rw, join); err != nil {
		return err
	}
	ans, err := ReadMsg(rw)
	if err != nil {
		return fmt.Errorf("friends: no answer to the request: %w", err)
	}
	switch ans.Type {
	case "pending":
		return nil
	case "approved", "declined":
		reply, err := m.acceptDecision(r, ans)
		if err != nil {
			return err
		}
		return WriteMsg(rw, reply)
	case "expired":
		return definiteErr{ErrInviteExpired}
	case "refused":
		if ans.Name == refusedSelf {
			return definiteErr{ErrOwnInvite}
		}
		return definiteErr{ErrUnexpected}
	}
	return ErrUnexpected
}

// refusedSelf answers a join on our own invite (self-pairing through a hairpin NAT).
const refusedSelf = "self"

// tryResume redials the inviter for an undecided out request.
func (m *Manager) tryResume(ctx context.Context, id int64) {
	r, err := m.store.GetRequest(id)
	if err != nil || r.Direction != "out" || r.State != "pending" || len(r.Secret) != 16 {
		return
	}
	inv := Invite{Secret: [16]byte(r.Secret)}
	var perr error
	if inv.Addr, perr = parseAddr(r.Addr); perr != nil {
		return
	}
	err = func() error {
		rw, conn, err := m.dialInvite(ctx, inv)
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, err := readHello(rw); err != nil {
			if errors.Is(err, ErrInviteExpired) {
				return definiteErr{err}
			}
			return err
		}
		return m.joinExchange(ctx, rw, r)
	}()
	var definite definiteErr
	switch {
	case errors.As(err, &definite):
		m.closeRequest(r, false)
	case err != nil:
		m.logf("resume request %d: %v", id, err)
		fallthrough
	default:
		m.mu.Lock()
		if cur, e := m.store.GetRequest(id); e == nil && cur.State == "pending" {
			m.scheduleLocked(id)
		} else {
			delete(m.retry, id)
		}
		m.mu.Unlock()
	}
}

// acceptDecision (joiner) verifies approved/declined and returns the reply: confirm or expired.
func (m *Manager) acceptDecision(r metadb.FriendRequest, msg Msg) (Msg, error) {
	secret := [16]byte(r.Secret)
	if !CheckInvite(secret, RoleInviter, msg, hex.EncodeToString(r.NonceMine)) {
		return Msg{}, ErrUnexpected
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := m.store.GetRequest(r.ID)
	if err != nil {
		return Msg{}, ErrNotFound
	}
	if msg.Type == "declined" {
		if cur.State == "approved" {
			return Msg{}, ErrUnexpected
		}
		if cur.State == "pending" {
			if err := m.store.SetRequestState(cur.ID, "declined", nil, cur.ExpiresAt); err != nil {
				return Msg{}, err
			}
			delete(m.retry, cur.ID)
			m.logf("request to %q declined", cur.Name)
		}
		return Msg{Type: "expired"}, nil
	}
	nonceInv, ok := decodeNonce(msg.Nonce)
	pub, okPub := hex.DecodeString(msg.Pub)
	if !ok || okPub != nil || !validName(msg.Name) {
		return Msg{}, ErrUnexpected
	}
	var pair PairKey
	switch cur.State {
	case "approved":
		// A repeat after a lost confirm: the private key is gone, so answer from the stored key.
		if len(cur.PairKey) != len(pair) || !bytes.Equal(cur.PubTheirs, pub) || !bytes.Equal(cur.NonceTheirs, nonceInv[:]) {
			return Msg{}, ErrUnexpected
		}
		pair = PairKey(cur.PairKey)
	case "pending":
		if cur.Direction != "out" || len(cur.EphPriv) != 32 || len(cur.NonceMine) != 16 {
			return Msg{}, ErrUnexpected
		}
		eph, err := ecdh.X25519().NewPrivateKey(cur.EphPriv)
		if err != nil {
			return Msg{}, ErrUnexpected
		}
		if pair, err = DerivePairKey(secret, nonceInv, [16]byte(cur.NonceMine), eph, RoleJoiner, pub); err != nil {
			return Msg{}, ErrUnexpected
		}
		now := m.now()
		// The name is a local label: a clash gets a suffix, never a stalled pairing.
		name, err := m.uniqueNameLocked(msg.Name)
		if err != nil {
			return Msg{}, err
		}
		if !m.friendByPairLocked(pair[:]) {
			if err := m.store.PutFriend(metadb.Friend{Name: name, PairKey: pair[:], Role: roleJoinerStr, Addr: cur.Addr, LastSeen: now.Unix(), CreatedAt: now.Unix()}); err != nil {
				return Msg{}, fmt.Errorf("friends: save friend %q: %w", name, err)
			}
		}
		// Kept until expiry: a repeated approved gets the same confirm.
		if err := m.store.SetRequestApproved(cur.ID, pair[:], nonceInv[:], cur.PubMine, pub, now.Add(deliveryWindow).Unix()); err != nil {
			return Msg{}, err
		}
		delete(m.retry, cur.ID)
		if err := m.refreshLocked(); err != nil {
			return Msg{}, err
		}
		m.logf("paired with %q", name)
	default:
		return Msg{}, ErrUnexpected
	}
	confirm := Msg{Type: "confirm"}
	confirm.MAC = SignPair(pair, RoleJoiner, confirm, msg.Nonce)
	return confirm, nil
}

// closeRequest drops a request and keeps its hash answerable as expired.
func (m *Manager) closeRequest(r metadb.FriendRequest, consumeInvite bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.store.DeleteRequest(r.ID)
	if consumeInvite {
		_ = m.store.SetInviteState(r.InviteHash, "used")
		_ = m.store.PurgeInvites(m.now().Unix())
	}
	delete(m.retry, r.ID)
	m.addTombLocked(r.InviteHash)
	if err := m.refreshLocked(); err != nil {
		m.logf("refresh: %v", err)
	}
}

// Approve accepts an incoming request: derives the pair key, then delivers the decision.
func (m *Manager) Approve(ctx context.Context, id int64) error {
	return m.decide(ctx, id, true, false)
}

// Decline refuses an incoming request; the invite is consumed.
func (m *Manager) Decline(ctx context.Context, id int64) error {
	return m.decide(ctx, id, false, false)
}

// ApproveAsync and DeclineAsync record the decision and return; the first delivery (up to 30 s)
// runs through m.Go, bound to the engine's lifetime, and is retried on backoff.
func (m *Manager) ApproveAsync(id int64) error { return m.decide(context.Background(), id, true, true) }
func (m *Manager) DeclineAsync(id int64) error {
	return m.decide(context.Background(), id, false, true)
}

func (m *Manager) goTask(f func(context.Context)) {
	if m.Go != nil {
		m.Go(f)
		return
	}
	go f(context.Background())
}

func (m *Manager) decide(ctx context.Context, id int64, approve, background bool) error {
	m.mu.Lock()
	r, err := m.store.GetRequest(id)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (r.Direction != "in" || r.ExpiresAt <= m.now().Unix()) {
		m.mu.Unlock()
		return ErrNotFound
	}
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if r.State != "pending" {
		m.mu.Unlock()
		if (r.State == "approved") != approve {
			return ErrAlreadyDecided
		}
		return nil // same decision: delivery is under way
	}
	if len(r.Secret) != 16 || len(r.NonceMine) != 16 || len(r.NonceTheirs) != 16 || approve && len(r.PubTheirs) != 32 {
		m.mu.Unlock()
		return ErrUnexpected
	}
	exp := m.now().Add(deliveryWindow).Unix()
	if approve {
		err = m.approveLocked(r, exp)
	} else {
		err = m.store.SetRequestState(id, "declined", nil, exp)
	}
	if err == nil {
		_ = m.store.SetInviteState(r.InviteHash, "used")
		_ = m.store.PurgeInvites(m.now().Unix())
		inFlightLocked(m.retry, id, m.now()) // Tick and Resume leave this delivery alone
		err = m.refreshLocked()
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if background {
		m.goTask(func(ctx context.Context) { m.tryDeliver(ctx, id) })
	} else {
		m.tryDeliver(ctx, id)
	}
	return nil
}

// approveLocked derives the pair key with a fresh ephemeral whose private half is never stored.
func (m *Manager) approveLocked(r metadb.FriendRequest, exp int64) error {
	eph, err := NewEphemeral()
	if err != nil {
		return err
	}
	k, err := DerivePairKey([16]byte(r.Secret), [16]byte(r.NonceMine), [16]byte(r.NonceTheirs), eph, RoleInviter, r.PubTheirs)
	if err != nil {
		return ErrUnexpected
	}
	return m.store.SetRequestApproved(r.ID, k[:], r.NonceTheirs, eph.PublicKey().Bytes(), r.PubTheirs, exp)
}

// tryDeliver dials the joiner with the decision; on failure it retries on backoff until expiry.
func (m *Manager) tryDeliver(ctx context.Context, id int64) {
	r, err := m.store.GetRequest(id)
	if err != nil || r.Direction != "in" || r.State == "pending" {
		return
	}
	inv := Invite{Secret: [16]byte(r.Secret)}
	if inv.Addr, err = parseAddr(r.Addr); err != nil {
		m.closeRequest(r, true)
		return
	}
	done := func() bool {
		rw, conn, err := m.dialInvite(ctx, inv)
		if err != nil {
			m.logf("deliver decision to %q: %v", r.Name, err)
			return false
		}
		defer conn.Close()
		return m.sendDecision(rw, r, nil) // our dial: the source is its known address
	}()
	if !done {
		m.mu.Lock()
		if _, err := m.store.GetRequest(id); err == nil {
			m.scheduleLocked(id)
		}
		m.mu.Unlock()
	}
}

func parseAddr(s string) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil || !validAddr(ap) {
		return netip.AddrPort{}, fmt.Errorf("friends: bad address %q", s)
	}
	return ap, nil
}

// sendDecision (inviter) writes the decision and handles confirm or expired. True when finished.
func (m *Manager) sendDecision(rw io.ReadWriter, r metadb.FriendRequest, from net.Addr) bool {
	secret := [16]byte(r.Secret)
	theirs := hex.EncodeToString(r.NonceTheirs)
	var msg Msg
	switch r.State {
	case "pending":
		_ = WriteMsg(rw, Msg{Type: "pending"})
		return false
	case "approved":
		msg = Msg{Type: "approved", Name: m.SelfName(), Nonce: hex.EncodeToString(r.NonceMine), Pub: hex.EncodeToString(r.PubMine)}
	case "declined":
		msg = Msg{Type: "declined"}
	default:
		return false
	}
	msg.MAC = SignInvite(secret, RoleInviter, msg, theirs)
	if err := WriteMsg(rw, msg); err != nil {
		return false
	}
	ans, err := ReadMsg(rw)
	if err != nil {
		return false
	}
	switch ans.Type {
	case "confirm":
		if r.State == "approved" && len(r.PairKey) == len(PairKey{}) && CheckPair(PairKey(r.PairKey), RoleJoiner, ans, msg.Nonce) {
			m.completeInvite(r)
			if f := m.snap.Load().friendByPair(r.PairKey); f != nil {
				f.noteControlSource(from)
			}
			return true
		}
	case "expired":
		// The joiner closed its request: declined acknowledged, or it gave up before approval.
		m.closeRequest(r, true)
		return true
	}
	return false
}

// completeInvite registers the friend after a valid confirm.
func (m *Manager) completeInvite(r metadb.FriendRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.store.GetRequest(r.ID); err != nil {
		return // already completed by a concurrent exchange
	}
	now := m.now().Unix()
	name, err := m.uniqueNameLocked(r.Name)
	if err != nil {
		m.logf("save friend %q: %v", r.Name, err)
		return
	}
	if !m.friendByPairLocked(r.PairKey) {
		if err := m.store.PutFriend(metadb.Friend{Name: name, PairKey: r.PairKey, Role: roleInviterStr, Addr: r.Addr, LastSeen: now, CreatedAt: now}); err != nil {
			m.logf("save friend %q: %v", name, err)
			return
		}
	}
	_ = m.store.DeleteRequest(r.ID)
	_ = m.store.SetInviteState(r.InviteHash, "used")
	_ = m.store.PurgeInvites(now)
	delete(m.retry, r.ID)
	m.addTombLocked(r.InviteHash)
	if err := m.refreshLocked(); err != nil {
		m.logf("refresh: %v", err)
	}
	m.logf("paired with %q", name)
}

// serveInvite (inviter) runs the invite channel for a dialling joiner.
func (m *Manager) serveInvite(ih [20]byte, rw io.ReadWriter, from net.Addr) {
	m.mu.Lock()
	r, isReq := m.findRequestLocked(ih[:], "in")
	in, err := m.store.GetInvite(ih[:])
	m.mu.Unlock()
	now := m.now().Unix()
	openInv := err == nil && (in.State == "open" || in.State == "requested") && in.ExpiresAt > now
	var secret [16]byte
	var expires int64
	switch {
	case isReq && len(r.Secret) == 16:
		secret, expires = [16]byte(r.Secret), r.ExpiresAt
	case openInv && len(in.Secret) == 16:
		secret, expires = [16]byte(in.Secret), in.ExpiresAt
	default:
		answerExpired(rw)
		return
	}
	if err := WriteMsg(rw, Msg{Type: "hello", Name: m.SelfName(), Expires: expires}); err != nil {
		return
	}
	join, err := ReadMsg(rw)
	if err != nil || join.Type != "join" {
		return // preview, or garbage
	}
	nonce, ok := decodeNonce(join.Nonce)
	pub, okPub := parsePub(join.Pub)
	if !ok || !okPub || !validName(join.Name) || !validAddrString(join.Addr) || !CheckInvite(secret, RoleJoiner, join, "") {
		return
	}
	if !usablePub(pub) { // after the MAC: unauthenticated joins cost no scalar multiplication
		return
	}
	m.mu.Lock()
	r, isReq = m.findRequestLocked(ih[:], "in")
	switch {
	case isReq && (!bytes.Equal(r.NonceTheirs, nonce[:]) || !bytes.Equal(r.PubTheirs, pub)):
		m.mu.Unlock()
		_ = WriteMsg(rw, Msg{Type: "expired"}) // consumed by another join
		return
	case isReq:
		// Same nonce and key: a repeat, answered with the decision as it stands.
	default:
		in, err = m.store.GetInvite(ih[:])
		if err != nil || in.State != "open" || in.ExpiresAt <= m.now().Unix() {
			m.mu.Unlock()
			_ = WriteMsg(rw, Msg{Type: "expired"})
			return
		}
		// Our own out request on this hash: we are dialling ourselves (hairpin NAT). A shared name
		// is no proof of that (two nodes keep the default hostname), so names are never refused.
		if _, self := m.findRequestLocked(ih[:], "out"); self {
			m.mu.Unlock()
			_ = WriteMsg(rw, Msg{Type: "refused", Name: refusedSelf})
			return
		}
		r = metadb.FriendRequest{
			InviteHash: ih[:], Direction: "in", Name: join.Name, Addr: join.Addr,
			NonceMine: randomBytes(16), NonceTheirs: nonce[:], PubTheirs: pub, Secret: in.Secret,
			State: "pending", ExpiresAt: in.ExpiresAt, CreatedAt: m.now().Unix(),
		}
		if r.ID, err = m.store.PutRequest(r); err != nil {
			m.mu.Unlock()
			return
		}
		_ = m.store.SetInviteState(ih[:], "requested")
		if err := m.refreshLocked(); err != nil {
			m.logf("refresh: %v", err)
		}
		m.logf("request from %q", join.Name)
	}
	m.mu.Unlock()
	m.sendDecision(rw, r, from)
}

// serveDelivery (joiner) receives the inviter's decision on an out request.
func (m *Manager) serveDelivery(ih [20]byte, rw io.ReadWriter) {
	m.mu.Lock()
	r, ok := m.findRequestLocked(ih[:], "out")
	m.mu.Unlock()
	if !ok || len(r.Secret) != 16 || len(r.NonceMine) != 16 {
		answerExpired(rw)
		return
	}
	msg, err := ReadMsg(rw)
	if err != nil || (msg.Type != "approved" && msg.Type != "declined") {
		return
	}
	reply, err := m.acceptDecision(r, msg)
	if err != nil {
		m.logf("decision from %q: %v", r.Name, err)
		return
	}
	_ = WriteMsg(rw, reply)
}
