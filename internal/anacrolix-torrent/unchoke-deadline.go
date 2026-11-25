package torrent

import (
	"strings"
	"sync"
	"time"
	"unsafe"
)

// A peer that keeps us choked long past the time its client family usually takes to unchoke may
// give up its connection slot to another peer. The deadline of each client family and role is
// learned from this process's own waits: the first wait after which fewer than half of the peers
// still waiting go on to unchoke us. Until enough waits are seen, unchokeFallback applies.

func init() {
	peerUnchokedHook = (*PeerConn).recordUnchoke
	peerClosedHook = func(cn *PeerConn, now time.Time) {
		cn.recordNeverUnchoked(now)
		unchokeSpared.CompareAndDelete(cn.t, cn)
	}
	peerOverdueHook = func(cn *PeerConn, now time.Time) bool {
		return cn.unchokeOverdue(learnedDeadlines.table(now), now)
	}
	peerEvictedHook = func(cn *PeerConn, now time.Time) {
		if cn.unchokeOverdue(learnedDeadlines.table(now), now) {
			cn.overdueEvicted(now)
		}
	}
}

// unchokeFallback is the deadline of a family and role not learned yet; "" covers every other key.
var unchokeFallback = map[string]time.Duration{
	"qbittorrent/seed": time.Second, "qbittorrent/leech": time.Second,
	"utorrent/seed": time.Second, "utorrent/leech": 5 * time.Second,
	"transmission/seed": 10 * time.Second, "transmission/leech": 5 * time.Second,
	"libtorrent/seed": time.Second, "deluge/seed": time.Second,
	"bitcomet/leech": 20 * time.Second, "vuze/leech": 5 * time.Second,
	"": 5 * time.Second,
}

const (
	learnThreshold    = 0.5
	learnMinConns     = 30 // explorer waits at least that long needed to learn a deadline
	learnExploreEvery = 10 // one connection in ten is never evicted, see unchokeExplorer
	learnRefresh      = 30 * time.Second
	learnedKey        = "#learned" // marks a learned table; no family/role key starts with "#"
)

// unchokeWaitBounds are the histogram bucket upper bounds; the last bucket is open.
var unchokeWaitBounds = []time.Duration{
	time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 35 * time.Second, 60 * time.Second,
}

type unchokeBucket struct {
	Conns       int64   `json:"conns"`
	Unchoked    int64   `json:"unchoked"`
	Never       int64   `json:"never"`
	WaitHist    []int64 `json:"wait_hist"`  // unchoked, by wait
	NeverWaited []int64 `json:"never_hist"` // never unchoked, by time we waited
}

type unchokeWaitStats struct {
	mu sync.Mutex
	m  map[string]*unchokeBucket
}

// unchokeWait holds, per client family and role, how long peers we wanted took to unchoke us and
// how long we waited on those that never did.
var unchokeWait = &unchokeWaitStats{m: map[string]*unchokeBucket{}}

// unchokeLearn holds the waits of explorer connections only: what the learned deadlines read.
var unchokeLearn = &unchokeWaitStats{m: map[string]*unchokeBucket{}}

// unchokeRecorded holds the connections already counted, until they close.
var unchokeRecorded sync.Map // *PeerConn -> struct{}

func unchokeWaitBucket(d time.Duration) int {
	for i, b := range unchokeWaitBounds {
		if d < b {
			return i
		}
	}
	return len(unchokeWaitBounds)
}

func (s *unchokeWaitStats) record(key string, d time.Duration, unchoked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.m[key]
	if b == nil {
		n := len(unchokeWaitBounds) + 1
		b = &unchokeBucket{WaitHist: make([]int64, n), NeverWaited: make([]int64, n)}
		s.m[key] = b
	}
	b.Conns++
	if unchoked {
		b.Unchoked++
		b.WaitHist[unchokeWaitBucket(d)]++
	} else {
		b.Never++
		b.NeverWaited[unchokeWaitBucket(d)]++
	}
}

// counts returns, per key, the unchoked-by-wait and never-by-wait histograms.
func (s *unchokeWaitStats) counts() map[string][2][]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][2][]int64, len(s.m))
	for k, b := range s.m {
		out[k] = [2][]int64{append([]int64(nil), b.WaitHist...), append([]int64(nil), b.NeverWaited...)}
	}
	return out
}

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
	// The extended handshake's name is chosen by the peer: only known families make a key, so
	// the stats cannot grow without bound.
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

// unchokeWaitKey is the stats key of a connection: client family and role.
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

// recordUnchoke counts the first unchoke of a connection, timed by our interest.
func (cn *PeerConn) recordUnchoke(now time.Time) {
	if _, done := unchokeRecorded.LoadOrStore(cn, struct{}{}); done {
		return
	}
	cn.record(cn.interestWait(now), true)
}

// recordNeverUnchoked counts a closing connection we were interested in that never unchoked us.
func (cn *PeerConn) recordNeverUnchoked(now time.Time) {
	if _, done := unchokeRecorded.LoadAndDelete(cn); done || cn.lastBecameInterested.IsZero() {
		return
	}
	cn.record(cn.interestWait(now), false)
}

// record files a wait under every connection's stats, and under the learning stats only for
// explorers: the others are closed at their deadline, so their waits past it would read as
// peers that never unchoke and the learned deadline would confirm itself.
func (cn *PeerConn) record(d time.Duration, unchoked bool) {
	key := cn.unchokeKeyOf()
	unchokeWait.record(key, d, unchoked)
	if _, censored := unchokeCensored.LoadAndDelete(cn); cn.unchokeExplorer() && !censored {
		unchokeLearn.record(key, d, unchoked)
	}
}

// unchokeCensored holds explorers closed at their deadline because another explorer held the
// spare slot: their waits stop at the deadline, so they stay out of the learning.
var unchokeCensored sync.Map // *PeerConn -> struct{}

// unchokeSpared holds, per torrent, the explorer kept past its deadline. One at a time: each
// spared explorer holds a slot that a peer likely to unchoke us could use.
var unchokeSpared sync.Map // *Torrent -> *PeerConn

// unchokeExploreOne limits the spared explorers to one per torrent; false spares them all.
var unchokeExploreOne = true

func (cn *PeerConn) spareExplorer() bool {
	if !unchokeExploreOne {
		return true
	}
	if cur, ok := unchokeSpared.Load(cn.t); ok {
		if c := cur.(*PeerConn); c != cn && !c.closed.IsSet() {
			if _, unchoked := unchokeRecorded.Load(c); !unchoked {
				return false
			}
		}
	}
	unchokeSpared.Store(cn.t, cn)
	return true
}

// overdueEvicted: the connection was closed for a new peer because its unchoke was overdue.
func (cn *PeerConn) overdueEvicted(now time.Time) {
	if cn.unchokeExplorer() {
		unchokeCensored.Store(cn, struct{}{})
	}
	cn.t.unchokeCooldown(cn, now)
}

var learnedDeadlines learnedTable

type learnedTable struct {
	mu sync.Mutex
	at time.Time
	t  map[string]time.Duration
}

func (l *learnedTable) table(now time.Time) map[string]time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.t == nil || now.Sub(l.at) > learnRefresh {
		l.t, l.at = learnDeadlines(unchokeLearn.counts(), unchokeFallback), now
	}
	return l.t
}

// learnDeadlines derives the deadlines from per-key histograms (unchoked by wait, never by wait).
func learnDeadlines(counts map[string][2][]int64, fallback map[string]time.Duration) map[string]time.Duration {
	out := make(map[string]time.Duration, len(fallback)+len(counts)+1)
	for k, d := range fallback {
		out[k] = d
	}
	out[learnedKey] = 1
	for k, c := range counts {
		u, n := c[0], c[1]
		for i := 1; i < len(u); i++ {
			var later, risk int64
			for j := i; j < len(u); j++ {
				later += u[j]
				risk += u[j] + n[j]
			}
			if risk < learnMinConns {
				break
			}
			if float64(later)/float64(risk) < learnThreshold {
				out[k] = unchokeWaitBounds[i-1]
				break
			}
		}
	}
	return out
}

// unchokeOverdue: we are interested, the peer never unchoked us, and the wait passed its deadline.
func (cn *PeerConn) unchokeOverdue(table map[string]time.Duration, now time.Time) bool {
	if table == nil || cn.friend || !cn.peerChoking || !cn.requestState.Interested {
		return false
	}
	wait := cn.interestWait(now)
	if wait <= unchokeWaitBounds[0] { // no deadline is shorter: spare the key until the role is known
		return false
	}
	if table[learnedKey] != 0 && cn.unchokeExplorer() && cn.spareExplorer() {
		return false
	}
	if _, seen := unchokeRecorded.Load(cn); seen {
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

// unchokeExplorer: a connection exempt from the learned deadline. Evicting every connection at
// its deadline would hide whether peers unchoke later, and the deadline could only shrink.
func (cn *PeerConn) unchokeExplorer() bool {
	h := uint64(uintptr(unsafe.Pointer(cn))) * 0x9E3779B97F4A7C15 // spread allocator-aligned addresses
	return (h>>32)%learnExploreEvery == 0
}
