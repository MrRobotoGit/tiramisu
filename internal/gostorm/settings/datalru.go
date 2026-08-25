package settings

import "container/list"

// DataCacheLimit caps the bytes DBReadCache keeps for torrent records. ListTorrent walks every
// key in the bucket, so an uncapped map ends up holding the whole library's piece hashes - 232MB
// on a 800-torrent library, over half the heap at rest. Evicting is safe: Get falls back to the
// DB on a miss and Set is write-through, so a dropped entry costs one BoltDB read and nothing
// else. The hot path is GetTorrent on the few torrents being played, which fits many times over.
var DataCacheLimit int64 = 32 << 20

type dataEntry struct {
	key [2]string
	val []byte
}

// dataLRU is a byte-bounded LRU. Not safe for concurrent use: DBReadCache holds its own lock.
type dataLRU struct {
	limit int64
	bytes int64
	order *list.List // front = most recently used
	items map[[2]string]*list.Element
}

func newDataLRU(limit int64) *dataLRU {
	return &dataLRU{
		limit: limit,
		order: list.New(),
		items: make(map[[2]string]*list.Element),
	}
}

func (c *dataLRU) get(key [2]string) ([]byte, bool) {
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*dataEntry).val, true
}

// put stores val, replacing any previous value for key, then evicts from the back until the
// cache is within its limit. A single value larger than the limit is kept: refusing it would
// make the entry uncacheable forever, and the next put trims back to size anyway.
func (c *dataLRU) put(key [2]string, val []byte) {
	if el, ok := c.items[key]; ok {
		e := el.Value.(*dataEntry)
		c.bytes += int64(len(val)) - int64(len(e.val))
		e.val = val
		c.order.MoveToFront(el)
	} else {
		c.items[key] = c.order.PushFront(&dataEntry{key: key, val: val})
		c.bytes += int64(len(val))
	}
	for c.bytes > c.limit && c.order.Len() > 1 {
		c.evictOldest()
	}
}

func (c *dataLRU) evictOldest() {
	el := c.order.Back()
	if el == nil {
		return
	}
	e := el.Value.(*dataEntry)
	c.order.Remove(el)
	delete(c.items, e.key)
	c.bytes -= int64(len(e.val))
}

func (c *dataLRU) remove(key [2]string) {
	el, ok := c.items[key]
	if !ok {
		return
	}
	e := el.Value.(*dataEntry)
	c.order.Remove(el)
	delete(c.items, key)
	c.bytes -= int64(len(e.val))
}

// removePath drops every entry under xPath, mirroring Clear on the underlying DB.
func (c *dataLRU) removePath(xPath string) {
	var next *list.Element
	for el := c.order.Front(); el != nil; el = next {
		next = el.Next()
		e := el.Value.(*dataEntry)
		if e.key[0] == xPath {
			c.order.Remove(el)
			delete(c.items, e.key)
			c.bytes -= int64(len(e.val))
		}
	}
}

func (c *dataLRU) size() int64 { return c.bytes }
func (c *dataLRU) len() int    { return c.order.Len() }
