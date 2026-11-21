package engine

import (
	"container/list"
	"encoding/json"
	"sync"

	"tiramisu/internal/metadb"
)

// storedSpec is the spec column: the library's spec JSON as today's binary writes it, with
// Go field names and the metainfo in its own column.
type storedSpec struct {
	Trackers                 [][]string
	InfoHash                 string
	InfoBytes                []byte // always null here
	DisplayName              string
	Webseeds                 []string
	DhtNodes                 []string
	PeerAddrs                []string
	Sources                  []string
	ChunkSize                int
	Storage                  any // always null
	DisableInitialPieceCheck bool
	DisallowDataUpload       bool
	DisallowDataDownload     bool
}

func decodeSpec(b []byte) storedSpec {
	var s storedSpec
	_ = json.Unmarshal(b, &s)
	s.InfoBytes, s.Storage = nil, nil
	return s
}

// rowCacheLimit caps the bytes of cached rows (a Peek of a DB-only torrent reads one).
const rowCacheLimit = 32 << 20

type rowEntry struct {
	hash string
	row  *metadb.GoStormTorrent // nil caches a miss
	size int64
}

// rowCache is a byte-capped LRU of single-row reads. A read that raced a write is never
// cached: every write bumps gen (a removed torrent came back in 23 of 40 runs without it).
type rowCache struct {
	mu    sync.Mutex
	limit int64
	bytes int64
	gen   uint64
	order *list.List
	items map[string]*list.Element
}

func newRowCache(limit int64) *rowCache {
	return &rowCache{limit: limit, order: list.New(), items: map[string]*list.Element{}}
}

func rowSize(r *metadb.GoStormTorrent) int64 {
	if r == nil {
		return 64
	}
	return int64(64 + len(r.Hash) + len(r.Title) + len(r.Category) + len(r.Poster) + len(r.Data) + len(r.Spec) + len(r.InfoBytes))
}

// get returns the cached row and true, or the generation to pass to put after a DB read.
func (c *rowCache) get(hash string) (*metadb.GoStormTorrent, bool, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[hash]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*rowEntry).row, true, c.gen
	}
	return nil, false, c.gen
}

func (c *rowCache) put(hash string, row *metadb.GoStormTorrent, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	c.removeLocked(hash)
	e := &rowEntry{hash: hash, row: row, size: rowSize(row)}
	c.items[hash] = c.order.PushFront(e)
	c.bytes += e.size
	for c.bytes > c.limit && c.order.Len() > 1 {
		c.removeLocked(c.order.Back().Value.(*rowEntry).hash)
	}
}

// invalidate runs after every write or delete of hash.
func (c *rowCache) invalidate(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.removeLocked(hash)
}

func (c *rowCache) removeLocked(hash string) {
	if el, ok := c.items[hash]; ok {
		c.bytes -= el.Value.(*rowEntry).size
		c.order.Remove(el)
		delete(c.items, hash)
	}
}

func (c *rowCache) size() (int64, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes, c.order.Len()
}
