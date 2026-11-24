package bt

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/anacrolix/torrent"

	"tiramisu/internal/piecestore"
)

// Client is one BitTorrent client over one piecestore.Store.
type Client struct {
	cl      *torrent.Client
	store   *piecestore.Store
	storage *Storage
	logf    func(string, ...any)

	dropMu sync.Mutex // the library panics on a drop of a torrent it no longer holds
	closed bool
}

// NewClient builds a client with every knob of o. Construction fails, for instance, while the
// previous client still holds the peer port.
func NewClient(o Options, store *piecestore.Store, logf func(string, ...any)) (*Client, error) {
	if logf == nil {
		logf = log.Printf
	}
	st := NewStorage(store)
	cl, err := torrent.NewClient(libConfig(o, st))
	if err != nil {
		return nil, err
	}
	return &Client{cl: cl, store: store, storage: st, logf: logf}, nil
}

func (c *Client) Store() *piecestore.Store { return c.store }
func (c *Client) LocalPort() int           { return c.cl.LocalPort() }

// Close drops every torrent (closing their stores) and the client. Idempotent.
func (c *Client) Close() {
	c.dropMu.Lock()
	if c.closed {
		c.dropMu.Unlock()
		return
	}
	c.closed = true
	c.dropMu.Unlock()
	c.cl.Close()
}

// Add adds s, or merges it into the torrent already holding its hash (new=false; onStore is
// then not used). onStore runs under the client lock when the torrent's store opens.
func (c *Client) Add(s Spec, onStore func(*piecestore.TorrentStore)) (*Torrent, bool, error) {
	ls, err := s.lib()
	if err != nil {
		return nil, false, err
	}
	if t, ok := c.cl.Torrent(ls.InfoHash); ok && t.FriendsOnly() {
		return nil, false, ErrFriendsOnlyHeld
	}
	ls.Storage = forTorrent{s: c.storage, onOpen: onStore}
	t, isNew, err := c.cl.AddTorrentSpec(ls)
	if err != nil {
		return nil, false, err
	}
	bt := &Torrent{t: t, c: c}
	if isNew {
		// A write after Close must not disable the download for good (2a review).
		t.SetOnWriteChunkError(func(err error) {
			if errors.Is(err, piecestore.ErrClosed) {
				return
			}
			c.logf("[Tentacle] chunk write error on %s: %v; download disabled", s.InfoHash, err)
			t.DisallowDataDownload()
		})
	}
	return bt, isNew, nil
}

// drop drops t only while this client still holds that exact torrent.
func (c *Client) drop(t *torrent.Torrent) error {
	c.dropMu.Lock()
	defer c.dropMu.Unlock()
	if c.closed {
		return nil
	}
	cur, ok := c.cl.Torrent(t.InfoHash())
	if !ok || cur != t {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			c.logf("[Tentacle] drop %s: %v", t.InfoHash().HexString(), r)
		}
	}()
	t.Drop()
	return nil
}

// Holds reports whether the client still holds t (tests and the stale-wrapper check).
func (c *Client) Holds(t *Torrent) bool {
	if t == nil || t.c != c {
		return false
	}
	cur, ok := c.cl.Torrent(t.t.InfoHash())
	return ok && cur == t.t
}

func (c *Client) String() string { return fmt.Sprintf("bt.Client(port %d)", c.LocalPort()) }
