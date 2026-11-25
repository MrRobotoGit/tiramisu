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
	peerClosedHook = (*PeerConn).recordNeverUnchoked
	peerOverdueHook = func(cn *PeerConn, now time.Time) bool {
		return cn.unchokeOverdue(learnedDeadlines.table(now), now)
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
	learnMinConns     = 30 // waits at least that long needed to learn a deadline
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
		default:
			return "id:" + code
		}
	}
	if v := strings.TrimSpace(extV); v != "" {
		if i := strings.IndexAny(v, " /"); i > 0 {
			v = v[:i]
		}
		return "v:" + strings.ToLower(v)
	}
	return "other"
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

// recordUnchoke counts the first unchoke of a connection, timed from our first interest.
func (cn *PeerConn) recordUnchoke(now time.Time) {
	if _, done := unchokeRecorded.LoadOrStore(cn, struct{}{}); done {
		return
	}
	var d time.Duration
	if !cn.firstInterestedAt.IsZero() {
		d = now.Sub(cn.firstInterestedAt)
	}
	unchokeWait.record(cn.unchokeWaitKey(), d, true)
}

// recordNeverUnchoked counts a closing connection we were interested in that never unchoked us.
func (cn *PeerConn) recordNeverUnchoked(now time.Time) {
	if _, done := unchokeRecorded.LoadAndDelete(cn); done || cn.firstInterestedAt.IsZero() {
		return
	}
	unchokeWait.record(cn.unchokeWaitKey(), now.Sub(cn.firstInterestedAt), false)
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
		l.t, l.at = learnDeadlines(unchokeWait.counts(), unchokeFallback), now
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
	if table == nil || cn.friend || !cn.peerChoking || cn.firstInterestedAt.IsZero() {
		return false
	}
	if table[learnedKey] != 0 && cn.unchokeExplorer() {
		return false
	}
	if _, seen := unchokeRecorded.Load(cn); seen {
		return false
	}
	d, ok := table[cn.unchokeWaitKey()]
	if !ok {
		d = table[""]
	}
	return now.Sub(cn.firstInterestedAt) > d
}

// unchokeExplorer: a connection exempt from the learned deadline. Evicting every connection at
// its deadline would hide whether peers unchoke later, and the deadline could only shrink.
func (cn *PeerConn) unchokeExplorer() bool {
	h := uint64(uintptr(unsafe.Pointer(cn))) * 0x9E3779B97F4A7C15 // spread allocator-aligned addresses
	return (h>>32)%learnExploreEvery == 0
}
