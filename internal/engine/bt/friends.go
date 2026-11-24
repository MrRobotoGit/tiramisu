package bt

import (
	"context"
	"io"
	"net"
	"net/netip"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// FriendHooks is the fork's interface for trusted friend nodes.
type FriendHooks = torrent.FriendHooks

// DialControl opens a friends control connection to addr on the control infohash ih.
func (c *Client) DialControl(ctx context.Context, addr string, ih, id [20]byte) (io.ReadWriter, net.Conn, [20]byte, error) {
	return c.cl.DialControl(ctx, addr, ih, id)
}

// CloseFriendConns closes the friend connections whose (infohash, peer ID) match.
func (c *Client) CloseFriendConns(match func(ih, peerID [20]byte) bool) {
	c.cl.CloseFriendConns(func(ih metainfo.Hash, id [20]byte) bool { return match(ih, id) })
}

// FriendAddrs lists the remote addresses of the connected friend peers.
func (t *Torrent) FriendAddrs() []string {
	var out []string
	for _, p := range t.t.PeerSnapshots() {
		if p.Friend {
			out = append(out, p.Addr)
		}
	}
	return out
}

// DialFriend dials a friend at addr (ip:port) outside the connection limit; see the fork's
// Torrent.DialFriend.
func (t *Torrent) DialFriend(addr string, id [20]byte) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return
	}
	t.t.DialFriend(net.TCPAddrFromAddrPort(ap), id)
}

// FriendsHave reports, for each piece, whether a proven friend link announces it.
func (t *Torrent) FriendsHave(pieces []int) []bool { return t.t.FriendsHave(pieces) }

// SetNoRequestsFrom stops (on) or resumes downloading from the friend presenting id; see the fork.
func (t *Torrent) SetNoRequestsFrom(id [20]byte, on bool) { t.t.SetNoRequestsFrom(id, on) }
