package torr

import (
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"github.com/anacrolix/torrent"

	"tiramisu/internal/gostorm/torr/state"
)

const (
	peerRateWindow = 2 * time.Second // shorter polls reuse the last sample instead of dividing by ~0
	peerRateMax    = 48              // the swarm view draws at most this many peers
)

// peerRates turns cumulative per-peer byte counters into rates between two status polls.
type peerRates struct {
	mu   sync.Mutex
	at   time.Time
	prev map[string]int64
	last []state.PeerRate
}

// PeerInfo resolves a peer address to its country and network owner; nil (the default)
// disables it. Only the resolved names reach the API: the address itself stays here.
var PeerInfo func(addr string) (cc string, asn uint, org string)

func peerID(addr string) string {
	h := fnv.New32a()
	h.Write([]byte(addr))
	return fmt.Sprintf("%08x", h.Sum32())
}

func (p *peerRates) sample(snaps []torrent.PeerSnapshot, now time.Time) []state.PeerRate {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last != nil && now.Sub(p.at) < peerRateWindow {
		return append([]state.PeerRate(nil), p.last...)
	}
	dt := now.Sub(p.at).Seconds()
	next := make(map[string]int64, len(snaps))
	out := make([]state.PeerRate, 0, len(snaps))
	for _, s := range snaps {
		next[s.Addr] = s.UsefulBytes
		kbps := 0.0
		if prev, ok := p.prev[s.Addr]; ok && dt > 0 && s.UsefulBytes >= prev {
			kbps = float64(s.UsefulBytes-prev) / 1024 / dt
		}
		pr := state.PeerRate{ID: peerID(s.Addr), KBps: float64(int64(kbps*10)) / 10, Seed: s.Seed, UTP: s.UTP}
		if PeerInfo != nil {
			pr.CC, pr.ASN, pr.Org = PeerInfo(s.Addr)
		}
		out = append(out, pr)
	}
	// Ties broken by id: conns come from a map, and a random order would read as churn.
	sort.Slice(out, func(i, j int) bool {
		if out[i].KBps != out[j].KBps {
			return out[i].KBps > out[j].KBps
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > peerRateMax {
		out = out[:peerRateMax]
	}
	p.prev, p.at, p.last = next, now, out
	return append([]state.PeerRate(nil), out...)
}
