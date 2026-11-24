package torrent

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/anacrolix/log"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/mse"
	pp "github.com/anacrolix/torrent/peer_protocol"
)

// FriendHooks lets the application recognise trusted nodes. WantConns runs under the Client lock
// (atomic reads only); the other hooks take no locks and do no I/O.
//
// A matching handshake ID only makes a candidate. Privileges need a tm_fp proof bound to the
// connection's MSE secret, checked under the Client lock on the hooks' current state: once a
// revocation is visible to the hooks no connection becomes a friend, and CloseFriendConns closes
// the proven ones.
type FriendHooks interface {
	// OutgoingPeerID is the ID to present when dialling addr for ih; ok=false uses the client ID.
	// Such dials always run MSE.
	OutgoingPeerID(ih metainfo.Hash, addr PeerRemoteAddr) (id [20]byte, ok bool)
	// ConfirmOutgoing reports whether the reply ID is the friend's (a candidate), and whether we
	// are the inviter side of that pair.
	ConfirmOutgoing(ih metainfo.Hash, addr PeerRemoteAddr, reply [20]byte) (friend, inviter bool)
	// IncomingPeerID sees the remote handshake before we reply; friend marks a candidate.
	IncomingPeerID(ih metainfo.Hash, remote [20]byte) (id [20]byte, friend, inviter, ok bool)
	// FriendProof is our proof for the connection with MSE secret s; nil if remote is no friend.
	FriendProof(ih metainfo.Hash, remote [20]byte, s []byte, myNonce, theirNonce [16]byte) []byte
	// CheckFriendProof verifies the remote's proof; s must be the 96-byte MSE secret.
	CheckFriendProof(ih metainfo.Hash, remote [20]byte, s []byte, myNonce, theirNonce [16]byte, proof []byte) (inviter, ok bool)
	// FriendAddrMatch reports whether addr has the IP last known for the friend presenting remote.
	FriendAddrMatch(ih metainfo.Hash, remote [20]byte, addr PeerRemoteAddr) bool
	// FriendCount is the number of friends.
	FriendCount() int
	// FriendIP reports whether addr has the IP of a friend's known address or authenticated sources.
	FriendIP(addr PeerRemoteAddr) bool
	// FriendProven reports a proven link (client lock held): its source is authenticated.
	FriendProven(ih metainfo.Hash, remote [20]byte, addr PeerRemoteAddr)
	// TakeUnknown serves, synchronously, a connection for an infohash no torrent holds; conn (for
	// deadlines only) keeps the handshake deadline. True = handled, no bad-accept penalty.
	TakeUnknown(ih metainfo.Hash, remote [20]byte, friend bool, rw io.ReadWriter, conn net.Conn) bool
	WantConns() bool
	// Skeys adds MSE secret keys for a connection from addr: invite and control infohashes, and
	// warm infohashes only for a friend's address.
	Skeys(addr PeerRemoteAddr, f func([]byte) bool) bool
	// OpenFriendTorrent may load a friends-only torrent for ih, which no torrent holds, for the MSE
	// candidate presenting remote; it runs synchronously in the connection goroutine and may do I/O.
	OpenFriendTorrent(ih metainfo.Hash, remote [20]byte) bool
}

// DialControl dials addr and completes the BT handshake for a control infohash with the client's
// obfuscation policy; no PeerConn, no rate limiter or blocklist. The caller closes conn.
func (cl *Client) DialControl(ctx context.Context, addr string, ih metainfo.Hash, id [20]byte) (rw io.ReadWriter, conn net.Conn, remote [20]byte, err error) {
	policy := cl.config.HeaderObfuscationPolicy
	rw, conn, remote, err = cl.dialControlOnce(ctx, addr, ih, id, policy.Preferred)
	if err == nil || policy.RequirePreferred || ctx.Err() != nil {
		return
	}
	return cl.dialControlOnce(ctx, addr, ih, id, !policy.Preferred)
}

func (cl *Client) dialControlOnce(ctx context.Context, addr string, ih metainfo.Hash, id [20]byte, encrypt bool) (rw io.ReadWriter, conn net.Conn, remote [20]byte, err error) {
	for _, d := range cl.dialers {
		c, e := d.Dial(ctx, addr)
		if e == nil {
			conn, err = c, nil
			break
		}
		// Keep the error of a dialer that could reach addr, not "no suitable address" from one that can't.
		var ae *net.AddrError
		if err == nil || !errors.As(e, &ae) {
			err = e
		}
	}
	if conn == nil {
		if err == nil {
			err = errors.New("no dialers")
		}
		return
	}
	dl := time.Now().Add(cl.config.HandshakesTimeout)
	if cd, ok := ctx.Deadline(); ok && cd.Before(dl) {
		dl = cd
	}
	if err = conn.SetDeadline(dl); err != nil {
		conn.Close()
		return nil, nil, remote, err
	}
	// Cancellation without a deadline still aborts the handshake.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer func() {
		if !stop() && err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = conn.SetDeadline(time.Time{})
		}
		if err != nil {
			conn.Close()
			conn, rw = nil, nil
		}
	}()
	rw = conn
	if encrypt {
		rw, _, err = mse.InitiateHandshake(conn, ih[:], nil, cl.config.CryptoProvides)
		if err != nil {
			err = fmt.Errorf("header obfuscation handshake: %w", err)
			return
		}
	}
	res, err := pp.Handshake(rw, &ih, id, cl.config.Extensions)
	if err != nil {
		err = fmt.Errorf("bittorrent protocol handshake: %w", err)
		return
	}
	if res.Hash != ih {
		err = errors.New("bittorrent protocol handshake: peer infohash didn't match")
		return
	}
	remote = res.PeerID
	return
}

// CloseFriendConns closes every proven friend connection whose torrent infohash and peer ID match
// (revocation: the privileges follow the connection, not the friendship).
func (cl *Client) CloseFriendConns(match func(ih metainfo.Hash, peerID [20]byte) bool) {
	cl.lock()
	defer cl.unlock()
	for _, t := range cl.torrents {
		for c := range t.conns {
			if c.friend && match(t.infoHash, c.PeerID) {
				t.dropConnection(c)
			}
		}
	}
}

// friendProofExt is the BEP 10 extension carrying the per-connection proof; only candidates
// announce it.
const friendProofExt pp.ExtensionName = "tm_fp"

const (
	fpNonce byte = 'n'
	fpProof byte = 'p'
)

// friendProofState is one-shot: one nonce each way, one proof each way, anything else ignored.
type friendProofState struct {
	myNonce, theirNonce [16]byte
	sent, got, done     bool
	extID               pp.ExtensionNumber // the remote's tm_fp ID when our nonce went out
}

// addFriendProofProtocol announces tm_fp on this connection only, on a copy of the shared map.
func (c *PeerConn) addFriendProofProtocol() {
	m := c.LocalLtepProtocolMap
	c.LocalLtepProtocolMap = &LocalLtepProtocolMap{
		Index:      append(slices.Clone(m.Index), friendProofExt),
		NumBuiltin: m.NumBuiltin,
	}
}

// friendProofWindowAt reports whether a candidate may still sit above the limit waiting for its
// proof: the handshake timeout, counted from the BT handshake.
func (c *PeerConn) friendProofWindowAt(now time.Time) bool {
	return now.Sub(c.completedHandshake) < c.t.cl.config.HandshakesTimeout
}

// outsideLimitAt: proven friends, and candidates admitted above the limit within their window.
func (c *PeerConn) outsideLimitAt(now time.Time) bool {
	return c.friend || (c.friendSlot && c.friendProofWindowAt(now))
}

// sendFriendNonce answers the remote's extended handshake; client lock held, so no
// WriteExtendedMessage.
func (c *PeerConn) sendFriendNonce() {
	if !c.friendCandidate || c.fp.sent {
		return
	}
	id := c.PeerExtensionIDs[friendProofExt]
	if id == 0 {
		return
	}
	c.fp.sent, c.fp.extID = true, id
	crand.Read(c.fp.myNonce[:])
	c.write(pp.Message{Type: pp.Extended, ExtendedID: id, ExtendedPayload: append([]byte{fpNonce}, c.fp.myNonce[:]...)})
}

func (c *PeerConn) onFriendProofMsg(payload []byte) error {
	h := c.t.cl.config.Friends
	if h == nil || !c.friendCandidate || c.friend || c.fp.done || !c.fp.sent || len(payload) == 0 {
		return nil
	}
	logf := c.logger.WithDefaultLevel(log.Debug).Printf
	switch payload[0] {
	case fpNonce:
		if c.fp.got {
			return nil
		}
		if len(payload) != 17 {
			logf("friend proof: malformed nonce (%d bytes)", len(payload))
			return nil
		}
		c.fp.got = true
		copy(c.fp.theirNonce[:], payload[1:])
		proof := h.FriendProof(c.t.infoHash, c.PeerID, c.mseSecret, c.fp.myNonce, c.fp.theirNonce)
		if proof == nil {
			logf("friend proof: none to send (not or no longer a friend)")
			return nil
		}
		c.write(pp.Message{Type: pp.Extended, ExtendedID: c.fp.extID, ExtendedPayload: append([]byte{fpProof}, proof...)})
	case fpProof:
		if !c.fp.got {
			return nil
		}
		// A late proof still counts: the window only bounds the time spent above the limit.
		c.fp.done = true
		inviter, ok := h.CheckFriendProof(c.t.infoHash, c.PeerID, c.mseSecret, c.fp.myNonce, c.fp.theirNonce, payload[1:])
		c.mseSecret = nil
		if !ok {
			c.logger.WithDefaultLevel(log.Warning).Printf("friend proof: rejected (invalid, or friend revoked)")
			if c.t.friendsOnly {
				return errors.New("friends-only torrent: proof rejected")
			}
			return nil
		}
		return c.t.friendProven(c, inviter)
	default:
		logf("friend proof: malformed message type %q", payload[0])
	}
	return nil
}

// friendProven grants the privileges; client lock held. Of two proven links of a pair the one the
// inviter side opened stays, else the existing one; the ID's other candidates are closed unless
// they are the inviter side's link.
func (t *Torrent) friendProven(c *PeerConn, inviter bool) error {
	if _, ok := t.conns[c]; !ok {
		return nil // already dropped while the proof was in flight
	}
	var drop []*PeerConn
	for c0 := range t.conns {
		if c0 == c || c0.PeerID != c.PeerID || !(c0.friend || c0.friendCandidate) {
			continue
		}
		if c0.friend && (c.outgoing == c0.outgoing || c.outgoing != inviter) {
			return errors.New("existing friend connection preferred")
		}
		// An unproven link opened by the inviter side may still prove and win: both ends keep it.
		if !c0.friend && c0.outgoing != c.outgoing && c0.outgoing == inviter {
			continue
		}
		drop = append(drop, c0)
	}
	for _, c0 := range drop {
		t.dropConnection(c0)
	}
	if !c.friend && !c.friendSlot {
		t.friendLinks++
	}
	c.friend, c.friendInviter, c.friendSlot = true, inviter, false
	if h := t.cl.config.Friends; h != nil {
		h.FriendProven(t.infoHash, c.PeerID, c.RemoteAddr)
	}
	c.logger.WithDefaultLevel(log.Warning).Printf("friend proof: link proven with %v", c.RemoteAddr)
	if t.friendsOnly {
		t._completedPieces.Iterate(func(i uint32) bool { c.have(pieceIndex(i)); return true })
		if err := c.friendsOnlyCheckDone(); err != nil {
			return err
		}
	}
	c.tickleWriter()
	c.updateRequests("friend proven")
	t.openNewConns()
	return nil
}

// admitFriendCandidate lets a candidate into a full torrent without evicting anyone, at most one
// per friend ID and as many as there are friends. A candidate at the friend's known IP is never
// replaced by one elsewhere; otherwise the newer one wins. A candidate past its window is ordinary.
func (t *Torrent) admitFriendCandidate(c *PeerConn, now time.Time) bool {
	h := t.cl.config.Friends
	if h == nil || !c.friendCandidate || !c.friendProofWindowAt(now) {
		return false
	}
	match := h.FriendAddrMatch(t.infoHash, c.PeerID, c.RemoteAddr)
	var old *PeerConn
	n := 0
	for c0 := range t.conns {
		if c0.friend || !c0.outsideLimitAt(now) {
			continue
		}
		if c0.PeerID == c.PeerID {
			if !match && h.FriendAddrMatch(t.infoHash, c0.PeerID, c0.RemoteAddr) {
				return false
			}
			old = c0
			continue
		}
		n++
	}
	if n >= h.FriendCount() {
		return false
	}
	if old != nil {
		old.close()
		t.deletePeerConn(old)
	}
	c.friendSlot = true
	return true
}

// Friend reports whether c is a proven friend link; client lock held (as in callbacks).
func (c *PeerConn) Friend() bool { return c.friend }

// DialFriend dials a friend at addr outside the connection limit (MSE is forced by initiateConn),
// once the info is known. It does nothing while a link or candidate presenting id, a connection
// to addr or a dial to addr exists.
func (t *Torrent) DialFriend(addr PeerRemoteAddr, id [20]byte) {
	t.cl.lock()
	defer t.cl.unlock()
	if t.closed.IsSet() || t.info == nil || t.connectingToPeerAddr(addr.String()) {
		return
	}
	for c := range t.conns {
		if c.PeerID == id && (c.friend || c.friendCandidate) {
			return
		}
	}
	initiateConn(outgoingConnOpts{
		peerInfo:                PeerInfo{Addr: addr, Source: PeerSourceDirect, Trusted: true},
		t:                       t,
		HeaderObfuscationPolicy: t.cl.config.HeaderObfuscationPolicy,
	}, true)
}

// FriendsOnly reports whether t serves proven friends only.
func (t *Torrent) FriendsOnly() bool { return t.friendsOnly }

var errNothingToServe = errors.New("friends-only torrent: the friend has every piece we hold")

// friendsOnlyCheckDone ends a friend's link to a friends-only torrent once there is nothing left to
// give: the friend is not interested and has every piece we hold. Client lock held.
func (c *PeerConn) friendsOnlyCheckDone() error {
	t := c.t
	if !t.friendsOnly || !c.friend || c.peerInterested || !t.haveInfo() {
		return nil
	}
	done := true
	t._completedPieces.Iterate(func(i uint32) bool {
		done = c.peerHasPiece(pieceIndex(i))
		return done
	})
	if done {
		return errNothingToServe
	}
	return nil
}

// dropUnprovenLater drops c from a friends-only torrent if it is still unproven once its proof
// window is over twice. Client lock held.
func (t *Torrent) dropUnprovenLater(c *PeerConn) {
	time.AfterFunc(2*t.cl.config.HandshakesTimeout, func() {
		t.cl.lock()
		defer t.cl.unlock()
		if _, ok := t.conns[c]; ok && !c.friend {
			t.dropConnection(c)
		}
	})
}

// dropIfIdle drops a friends-only torrent left without connections.
func (t *Torrent) dropIfIdle() {
	var wg sync.WaitGroup
	defer wg.Wait()
	t.cl.lock()
	defer t.cl.unlock()
	if !t.friendsOnly || t.closed.IsSet() || len(t.conns) != 0 || t.cl.torrents[t.infoHash] != t {
		return
	}
	t.cl.dropTorrent(t.infoHash, &wg)
}

// FriendsHave reports, for each piece, whether a proven friend link announces it; one read lock.
func (t *Torrent) FriendsHave(pieces []int) []bool {
	out := make([]bool, len(pieces))
	t.cl.rLock()
	defer t.cl.rUnlock()
	for c := range t.conns {
		if !c.friend {
			continue
		}
		for k, i := range pieces {
			out[k] = out[k] || c.peerHasPiece(i)
		}
	}
	return out
}
