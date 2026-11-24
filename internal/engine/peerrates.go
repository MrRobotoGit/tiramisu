package engine

import (
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"tiramisu/internal/engine/bt"
)

const (
	peerRateWindow = 2 * time.Second // shorter polls reuse the last sample instead of dividing by ~0
	peerRateMax    = 48              // the swarm view draws at most this many peers
)

// PeerRate is one peer in the swarm view; the address never leaves the engine.
type PeerRate struct {
	ID     string  `json:"id"`
	KBps   float64 `json:"kbps"`
	UpKBps float64 `json:"up_kbps,omitempty"`
	Seed   bool    `json:"seed"`
	UTP    bool    `json:"utp,omitempty"`
	CC     string  `json:"cc,omitempty"`
	ASN    uint    `json:"asn,omitempty"`
	Org    string  `json:"org,omitempty"`
	Friend string  `json:"friend,omitempty"` // the friend's name on a proven friend link
	// FriendRole is the help running with that friend on this torrent: helping_you or you_help.
	FriendRole string `json:"friend_role,omitempty"`
}

// PeerInfoFunc resolves an address to country and network owner (GeoIP).
type PeerInfoFunc func(addr string) (cc string, asn uint, org string)

// peerRates turns cumulative per-peer byte counters into rates between two samples.
type peerRates struct {
	mu   sync.Mutex
	at   time.Time
	prev map[string]int64
	sent map[string]int64
	last []PeerRate
}

func peerID(addr string) string {
	h := fnv.New32a()
	h.Write([]byte(addr))
	return fmt.Sprintf("%08x", h.Sum32())
}

// friend names a proven friend link's peer ID; nil leaves Friend empty.
func (p *peerRates) sample(snaps []bt.PeerSnapshot, now time.Time, info PeerInfoFunc, friend func(id [20]byte) string) []PeerRate {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last != nil && now.Sub(p.at) < peerRateWindow {
		return append([]PeerRate(nil), p.last...)
	}
	dt := now.Sub(p.at).Seconds()
	next := make(map[string]int64, len(snaps))
	nextSent := make(map[string]int64, len(snaps))
	out := make([]PeerRate, 0, len(snaps))
	for _, s := range snaps {
		next[s.Addr], nextSent[s.Addr] = s.UsefulBytes, s.SentBytes
		kbps, up := 0.0, 0.0
		if prev, ok := p.prev[s.Addr]; ok && dt > 0 && s.UsefulBytes >= prev {
			kbps = float64(s.UsefulBytes-prev) / 1024 / dt
		}
		if prev, ok := p.sent[s.Addr]; ok && dt > 0 && s.SentBytes >= prev {
			up = float64(s.SentBytes-prev) / 1024 / dt
		}
		pr := PeerRate{ID: peerID(s.Addr), KBps: float64(int64(kbps*10)) / 10, UpKBps: float64(int64(up*10)) / 10, Seed: s.Seed, UTP: s.UTP}
		if info != nil {
			pr.CC, pr.ASN, pr.Org = info(s.Addr)
		}
		if s.Friend && friend != nil {
			pr.Friend = friend(s.PeerID)
		}
		out = append(out, pr)
	}
	// Busiest first both ways; ties by id, or a map-ordered list would read as churn.
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].KBps+out[i].UpKBps, out[j].KBps+out[j].UpKBps; a != b {
			return a > b
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > peerRateMax { // friends past the cut stay listed
		kept := out[:peerRateMax:peerRateMax]
		for _, pr := range out[peerRateMax:] {
			if pr.Friend != "" {
				kept = append(kept, pr)
			}
		}
		out = kept
	}
	p.prev, p.sent, p.at, p.last = next, nextSent, now, out
	return append([]PeerRate(nil), out...)
}
