package bt

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"regexp"
	"sort"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/iplist"
)

// ErrEmptyBlocklist: zero ranges is an error, never "no filtering" (e228d68).
var ErrEmptyBlocklist = errors.New("bt: blocklist has no ranges")

// Blocklist is a parsed P2P blocklist, sorted by first IP.
type Blocklist struct{ list *iplist.IPList }

func (b *Blocklist) NumRanges() int {
	if b == nil {
		return 0
	}
	return b.list.NumRanges()
}

// Blocked reports whether ip falls in a range (tests and diagnostics).
func (b *Blocklist) Blocked(ip net.IP) bool {
	if b == nil {
		return false
	}
	_, ok := b.list.Lookup(ip)
	return ok
}

func parseRanges(r io.Reader, filter *regexp.Regexp) []iplist.Range {
	var out []iplist.Range
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		rg, ok, err := iplist.ParseBlocklistP2PLine(sc.Bytes())
		if err != nil || !ok {
			continue // malformed lines are skipped
		}
		if filter != nil && !filter.MatchString(rg.Description) {
			continue
		}
		out = append(out, rg)
	}
	return out
}

// LoadBlocklist parses P2P lines, keeps the ranges whose description matches filter (nil keeps
// all) and sorts them by first IP: lookups binary-search (68b2ce4).
func LoadBlocklist(r io.Reader, filter *regexp.Regexp) (*Blocklist, error) {
	ranges := parseRanges(r, filter)
	if len(ranges) == 0 {
		return nil, ErrEmptyBlocklist
	}
	sort.Slice(ranges, func(i, j int) bool {
		a, b := ranges[i].First, ranges[j].First
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return bytes.Compare(a, b) < 0
	})
	return &Blocklist{list: iplist.New(ranges)}, nil
}

// CountRanges counts parseable ranges: the sanity gate before installing a download (a
// captcha page parses to 0).
func CountRanges(r io.Reader) int { return len(parseRanges(r, nil)) }

// SetBlocklist live-swaps the client's list; nil disables filtering.
func (c *Client) SetBlocklist(b *Blocklist) {
	if b == nil {
		c.cl.SetIPBlocklist(nil)
		return
	}
	c.cl.SetIPBlocklist(b.list)
}

func (b *Blocklist) ranger() iplist.Ranger {
	if b == nil {
		return nil
	}
	return b.list
}

// BlockedRange is a blocklist range and the distinct addresses it rejected.
type BlockedRange = torrent.BlockedRange

// BlocklistCounters are the process-wide rejection counters of the fork.
func BlocklistCounters() (rejections, distinctIPs uint64) {
	return torrent.IPBlocklistRejections(), torrent.IPBlocklistDistinctIPs()
}

// BlocklistTopRanges lists the ranges that rejected the most addresses, at most n.
func BlocklistTopRanges(n int) []BlockedRange { return torrent.IPBlocklistTopRanges(n) }
