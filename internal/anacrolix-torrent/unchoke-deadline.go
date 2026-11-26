package torrent

import (
	"strings"
	"sync"
	"time"
)

// A peer that keeps us choked long past the time its client family usually takes to unchoke may
// give up its connection slot to another peer. The deadline of each client family and role is
// learned from this process's own waits: the first wait after which fewer than half of the peers
// still waiting go on to unchoke us. A connection closed before any unchoke, by us or the peer,
// only says the unchoke had not come yet: it is a censored wait, never a refusal. Until enough
// waits are seen, unchokeFallback applies.

func init() {
	peerUnchokedHook = (*PeerConn).recordUnchoke
	peerClosedHook = (*PeerConn).recordNeverUnchoked
	peerOverdueHook = func(cn *PeerConn, now time.Time) bool {
		return cn.unchokeOverdue(learnedDeadlines.table(now), now)
	}
	peerEvictedHook = func(cn *PeerConn, now time.Time) {
		if cn.unchokeOverdue(learnedDeadlines.table(now), now) {
			cn.t.unchokeCooldown(cn, now)
		}
	}
}

// unchokeFallback is the deadline of a family and role not learned yet; "" covers every other key.
var unchokeFallback = map[string]time.Duration{
	"qbittorrent/seed": 5 * time.Second, "qbittorrent/leech": time.Second,
	"utorrent/seed": time.Second, "utorrent/leech": time.Second,
	"transmission/seed": 60 * time.Second, "transmission/leech": 5 * time.Second,
	"libtorrent/seed": time.Second, "deluge/seed": time.Second,
	"bitcomet/leech": 20 * time.Second, "vuze/leech": 5 * time.Second,
	"": 5 * time.Second,
}

const (
	learnThreshold    = 0.5
	learnMinConns     = 30 // waits at least that long needed to learn a deadline
	learnRefresh      = 30 * time.Second
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

// record files a wait under the connection's key.
func (cn *PeerConn) record(d time.Duration, unchoked bool) {
	unchokeWait.record(cn.unchokeKeyOf(), d, unchoked)
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

// learnDeadlines derives the deadlines from per-key histograms (unchoked by wait, closed without
// an unchoke by wait) with a life table: a wait closed without an unchoke stays at risk until its
// bucket and then leaves, censored. The deadline is the first bound t where the chance of an
// unchoke between t and the last bound, for a peer still waiting at t, falls below learnThreshold.
func learnDeadlines(counts map[string][2][]int64, fallback map[string]time.Duration) map[string]time.Duration {
	out := make(map[string]time.Duration, len(fallback)+len(counts))
	for k, d := range fallback {
		out[k] = d
	}
	for k, c := range counts {
		if d, ok := lifeTableDeadline(c[0], c[1]); ok {
			out[k] = d
		}
	}
	return out
}

func lifeTableDeadline(unchoked, censored []int64) (time.Duration, bool) {
	n := len(unchokeWaitBounds) // buckets up to the last bound; the open one only counts at risk
	if len(unchoked) <= n || len(censored) <= n {
		return 0, false
	}
	// surv[i]: share of waits still without an unchoke at bound i-1 (surv[0] = 1 at 0s).
	surv := make([]float64, n+1)
	surv[0] = 1
	atRisk := make([]int64, n+1)
	for i := 0; i < n; i++ {
		for j := i; j < len(unchoked); j++ {
			atRisk[i] += unchoked[j] + censored[j]
		}
		r := float64(atRisk[i]) - float64(censored[i])/2
		q := 0.0
		if r > 0 {
			q = float64(unchoked[i]) / r
		}
		if q > 1 {
			q = 1
		}
		surv[i+1] = surv[i] * (1 - q)
	}
	for i := 1; i < n; i++ {
		if atRisk[i] < learnMinConns {
			return 0, false // too few waits this long
		}
		if surv[i] <= 0 {
			return 0, false
		}
		if later := 1 - surv[n]/surv[i]; later < learnThreshold {
			return unchokeWaitBounds[i-1], true
		}
	}
	return 0, false
}

// unchokeOverdue: we are interested, the peer never unchoked us, and the wait passed its deadline.
func (cn *PeerConn) unchokeOverdue(table map[string]time.Duration, now time.Time) bool {
	if table == nil || cn.friend || !cn.peerChoking || !cn.requestState.Interested {
		return false
	}
	wait := cn.interestWait(now)
	if wait <= unchokeWaitBounds[0] { // no deadline is shorter: fix the key once the role is known
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
