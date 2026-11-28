package torrent

import (
	"strings"
	"time"
)

// A peer that keeps us choked long past the time its client family usually takes to unchoke may
// give up its connection slot to another peer. The deadline of each client family and role comes
// from unchokeDeadline, a table computed offline from observed waits.

func init() {
	peerUnchokedHook = func(cn *PeerConn, _ time.Time) { cn.unchokedOnce = true }
	peerOverdueHook = func(cn *PeerConn, now time.Time) bool {
		return cn.unchokeOverdue(unchokeDeadline, now)
	}
	peerEvictedHook = func(cn *PeerConn, now time.Time) {
		if cn.unchokeOverdue(unchokeDeadline, now) {
			cn.t.unchokeCooldown(cn, now)
		}
	}
}

// unchokeDeadline is, per client family and role, the first wait after which fewer than half of
// the peers still waiting go on to unchoke us; "" covers every other key. A connection closed
// before any unchoke counts as a censored wait, never as a refusal.
var unchokeDeadline = map[string]time.Duration{
	"qbittorrent/seed": 10 * time.Second, "qbittorrent/leech": time.Second,
	"utorrent/seed": 5 * time.Second, "utorrent/leech": 5 * time.Second,
	"transmission/seed": 10 * time.Second, "transmission/leech": time.Second,
	"libtorrent/seed": 5 * time.Second, "deluge/seed": 10 * time.Second,
	"bittorrent/seed": 5 * time.Second, "vuze/seed": 5 * time.Second,
	"bitcomet/leech": 20 * time.Second, "vuze/leech": 10 * time.Second,
	"": 10 * time.Second,
}

// activeUnchokeTable is the deadline profile in use (nil: off), for UnchokeSettle.
var activeUnchokeTable = unchokeDeadline

// UnchokeSettle is how long until every connected peer that keeps us choked, while we want its
// pieces, reaches its client's usual unchoke wait (the deadline table) from when it started to choke
// us: 0 when none is waiting.
// Until then a low download rate says little about what the swarm will deliver.
func (t *Torrent) UnchokeSettle() time.Duration {
	table := activeUnchokeTable
	if table == nil {
		return 0
	}
	t.cl.rLock()
	defer t.cl.rUnlock()
	now := time.Now()
	var left time.Duration
	for cn := range t.conns {
		if cn.friend || !cn.peerChoking || !cn.requestState.Interested {
			continue
		}
		key := cn.unchokeKey
		if key == "" {
			key = cn.unchokeWaitKey()
		}
		d, ok := table[key]
		if !ok {
			d = table[""]
		}
		// A peer that unchoked us and choked us again (seed rotation, tit-for-tat while the player
		// read nothing) gets its usual wait again from that choke.
		waited := cn.interestWait(now)
		if cn.unchokedOnce {
			waited = now.Sub(cn.rechokedAt)
		}
		if r := d - waited; r > left {
			left = r
		}
	}
	return left
}

// unchokeMinWait is the shortest deadline: past it the role is known, so the key can be fixed.
const unchokeMinWait = time.Second

// clientFamily names the remote client from its Azureus-style peer ID ("-qB4650-"), else from
// the extended handshake's "v" field, else "other".
func clientFamily(id PeerID, extV string) string {
	if id[0] == '-' && id[7] == '-' {
		switch code := string(id[1:3]); code {
		case "qB":
			return "qbittorrent"
		case "TR":
			return "transmission"
		case "UT", "UM", "UW":
			return "utorrent"
		case "BT":
			return "bittorrent"
		case "lt", "LT":
			return "libtorrent"
		case "DE":
			return "deluge"
		case "BC":
			return "bitcomet"
		case "AZ":
			return "vuze"
		case "XL", "SD":
			return "xunlei"
		}
	}
	// The extended handshake's name is chosen by the peer: only known families make a key.
	v := strings.ToLower(strings.TrimSpace(extV))
	for _, f := range []string{"qbittorrent", "transmission", "utorrent", "deluge", "libtorrent", "bitcomet", "vuze"} {
		if strings.HasPrefix(v, f) {
			return f
		}
	}
	return "other"
}

// unchokeKeyOf is the connection's key, fixed the first time it is asked: early on the role is
// unknown until the bitfield arrives and the family until the extended handshake does.
func (cn *PeerConn) unchokeKeyOf() string {
	if cn.unchokeKey == "" {
		cn.unchokeKey = cn.unchokeWaitKey()
	}
	return cn.unchokeKey
}

// unchokeWaitKey is the table key of a connection: client family and role.
func (cn *PeerConn) unchokeWaitKey() string {
	if cn.friend {
		return "friend"
	}
	v, _ := cn.PeerClientName.Load().(string)
	role := "leech"
	if cn.t != nil {
		if all, known := cn.peerHasAllPieces(); known && all {
			role = "seed"
		}
	}
	return clientFamily(cn.PeerID, v) + "/" + role
}

// interestWait is how long we have wanted this peer: only the time we were interested counts,
// since peers unchoke only those that are.
func (cn *PeerConn) interestWait(now time.Time) time.Duration {
	d := cn.priorInterest
	if cn.requestState.Interested {
		d += now.Sub(cn.lastBecameInterested)
	}
	return d
}

// unchokeOverdue: we are interested, the peer never unchoked us, and the wait passed its deadline.
func (cn *PeerConn) unchokeOverdue(table map[string]time.Duration, now time.Time) bool {
	if table == nil || cn.friend || cn.unchokedOnce || !cn.peerChoking || !cn.requestState.Interested {
		return false
	}
	wait := cn.interestWait(now)
	if wait <= unchokeMinWait {
		return false
	}
	d, ok := table[cn.unchokeKeyOf()]
	if !ok {
		d = table[""]
	}
	return wait > d
}

// unchokeCooldownDur keeps a peer closed for an overdue unchoke from being dialled straight back:
// it would start again at the back of the same full queue.
const unchokeCooldownDur = 30 * time.Second

func (t *Torrent) unchokeCooldown(cn *PeerConn, now time.Time) {
	if t == nil || cn.RemoteAddr == nil {
		return
	}
	if t.churnCooldown == nil {
		t.churnCooldown = make(map[string]churnCooldownEntry)
	}
	t.churnCooldown[churnCooldownKey(cn.RemoteAddr)] = churnCooldownEntry{until: now.Add(unchokeCooldownDur), probed: true}
}
