package torrent

import "time"

// Optional observers of connection events, nil unless set from an init function before
// any connection exists.
var (
	peerUnchokedHook func(*PeerConn, time.Time) // the peer unchoked us
	peerClosedHook   func(*PeerConn, time.Time) // the connection is closing
	// peerOverdueHook reports a connection that may give up its slot before the usual grace.
	peerOverdueHook func(*PeerConn, time.Time) bool
	peerEvictedHook func(*PeerConn, time.Time) // the connection is closed to make room
)
