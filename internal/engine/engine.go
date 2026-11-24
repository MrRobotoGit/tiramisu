package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"tiramisu/internal/engine/bt"
	"tiramisu/internal/friends"
	"tiramisu/internal/piecestore"
	"tiramisu/internal/streamer"
)

var ErrNotConnected = errors.New("engine: BT client not connected")

// Engine owns the BitTorrent client, the piece store and the registry of hydrated torrents.
type Engine struct {
	deps Deps
	cfg  atomic.Pointer[Config]
	rows *rowCache

	mu       sync.RWMutex
	client   *bt.Client
	torrents map[string]*Torrent
	stopTick chan struct{}
	tickDone chan struct{} // closed when the tick goroutine has returned

	addMu sync.Mutex // serialises add and close; lock order: setMu, Torrent.life, addMu, mu, client
	setMu sync.Mutex // serialises reconnects

	ready     chan struct{}
	readyOnce sync.Once

	// Seams, replaced in tests.
	now        func() time.Time
	newClient  func(bt.Options, *piecestore.Store, func(string, ...any)) (*bt.Client, error)
	publicIPs  func(ipv6 bool) (net.IP, net.IP)
	dhtNodes   func() []string
	trackers   func() []string // default tracker list
	tuneBT     func(*bt.Options)
	retries    int
	retryDelay time.Duration
	tickEvery  time.Duration
	tickFn     func()
	settle     time.Duration
	infoWait   func(Config) time.Duration

	block blocklistState

	loader *trackerLoader

	peerInfoFn atomic.Pointer[PeerInfoFunc] // GeoIP hook of the swarm view

	friends          *friends.Manager // nil without a DB
	freshPublicIPs   func(ctx context.Context, ipv6 bool) (net.IP, net.IP)
	friendsAddrEvery time.Duration // re-read period: the shared public IP lookup caches forever
	friendsCtx       context.Context
	friendsStop      context.CancelFunc
	friendsStarted   atomic.Bool // Start has run: OnActive may launch the timers
	friendsLaunched  atomic.Bool
	friendsPlayMu    sync.Mutex
	friendsPlaying   map[string]map[string]struct{}                // infohash -> open playback sessions
	watching         map[string]*watchSession                      // infohash -> playback asking help (friendsPlayMu)
	friendDial       func(t *bt.Torrent, addr string, id [20]byte) // (*bt.Torrent).DialFriend; tests observe it
	friendsWG        sync.WaitGroup
	friendsMu        sync.Mutex // guards friendsClosed and friendsWG.Add against Close
	friendsClosed    bool
	friendsAddrMu    sync.Mutex // one public-address read at a time

	coldMu            sync.Mutex
	friendsColdIntent map[string]uint64 // cold opens of torrents not registered yet, by order
	coldSeq           uint64

	bg     atomic.Pointer[background] // data slots shared with the FUSE pumps; nil = none
	serve  friendServe                // friends-only torrents over the SSD warmup
	meters sync.Map                   // infohash -> *deliveryMeter of a playback with helpers

	streamer    atomic.Pointer[streamer.Streamer] // piece scheduler: helps need it
	helps       helpRegistry                      // friends' playbacks we help
	helpControl func(PieceControl) PieceControl   // tests observe a help's levels
}

// New builds an engine; nothing connects until Start.
func New(cfg Config, d Deps) *Engine {
	e := &Engine{
		deps: d, rows: newRowCache(rowCacheLimit), torrents: map[string]*Torrent{},
		ready: make(chan struct{}), now: time.Now, newClient: bt.NewClient, friendDial: (*bt.Torrent).DialFriend,
		friendsPlaying: map[string]map[string]struct{}{},
		publicIPs:      bt.PublicIPs, freshPublicIPs: bt.FreshPublicIPs, dhtNodes: bt.DHTNodes,
		trackers: func() []string { return BuiltinTrackers },
		retries:  30, retryDelay: time.Second, tickEvery: time.Second, settle: 200 * time.Millisecond,
		infoWait: func(c Config) time.Duration { return 60*time.Second + c.timeout() },
	}
	c := cfg.normalized()
	e.cfg.Store(&c)
	e.block.binDir = binaryDir()
	e.tickFn = e.tick
	e.loader = newTrackerLoader(d.logf)
	e.trackers = e.loader.List
	e.friendsAddrEvery = 15 * time.Minute
	e.initFriends()
	return e
}

func (e *Engine) config() Config { return *e.cfg.Load() }

// Config returns the configuration in use.
func (e *Engine) Config() Config { return e.config() }

// Ready is closed after the first successful connect: it replaces main's 2s sleep.
func (e *Engine) Ready() <-chan struct{} { return e.ready }

// Up reports whether a client is connected. Get, Peek, Up and ListHydrated are nil-safe:
// tests and simulators run the FUSE code without an engine.
func (e *Engine) Up() bool {
	if e == nil {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.client != nil
}

func (e *Engine) currentClient() *bt.Client {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.client
}

// Start connects once; a failure is fatal for the caller, as today.
func (e *Engine) Start() error {
	cfg := e.config()
	if err := e.connect(cfg, e.clientOptions(cfg)); err != nil {
		return err
	}
	e.loader.SetOnLoad(e.fanOutTrackers)
	e.loader.Start()
	e.startFriends()
	e.readyOnce.Do(func() { close(e.ready) })
	return nil
}

func (e *Engine) pieceSettings() (bool, bool) {
	c := e.config()
	return c.ResponsiveMode, c.AdaptiveShield
}

// clientOptions resolves the network inputs once per Start or Reconnect, not per port retry:
// a failed public-IP lookup costs up to 3s.
func (e *Engine) clientOptions(cfg Config) bt.Options {
	o := cfg.btOptions()
	o.PortForwarding = cfg.portForwarding(e.deps.NATPMP)
	if !cfg.DisableDHT {
		o.DHTNodes = e.dhtNodes()
	}
	o.PublicIP4, o.PublicIP6 = e.publicIPs(cfg.EnableIPv6)
	o.Blocklist = e.connectBlocklist()
	e.friendOptions(&o)
	if e.tuneBT != nil {
		e.tuneBT(&o)
	}
	return o
}

// connect builds a new store and client; the ticker starts only on success.
func (e *Engine) connect(cfg Config, o bt.Options) error {
	store := piecestore.New(piecestore.Config{Capacity: cfg.CacheSize, Settings: e.pieceSettings, Logf: e.deps.Logf})
	c, err := e.newClient(o, store, e.deps.Logf)
	if err != nil {
		return fmt.Errorf("engine: connect: %w", err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	e.mu.Lock()
	e.client, e.torrents, e.stopTick, e.tickDone = c, map[string]*Torrent{}, stop, done
	e.mu.Unlock()
	go e.tickLoop(stop, done)
	e.deps.logf("[Tentacle] connected, port %d, cache %d MiB, conns %d", c.LocalPort(), cfg.CacheSize>>20, cfg.ConnectionsLimit)
	return nil
}

// disconnect stops the ticker, waiting for a tick in flight, then closes the client; the
// torrent map is replaced on connect.
func (e *Engine) disconnect() {
	e.mu.Lock()
	c, stop, done := e.client, e.stopTick, e.tickDone
	e.client, e.stopTick, e.tickDone = nil, nil, nil
	e.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	if c != nil {
		c.Close()
	}
	runtime.GC()
	debug.FreeOSMemory()
}

// Close stops the engine for good.
func (e *Engine) Close() {
	e.setMu.Lock()
	defer e.setMu.Unlock()
	e.stopFriends()
	e.loader.Stop()
	e.endHelps("engine closed")
	e.dropServed("")
	e.clearColdIntents()
	for _, t := range e.ListHydrated() {
		e.close(t)
	}
	e.disconnect()
}

// Reconnect applies cfg with a new client: every torrent is dropped (playback stops), then the
// connect is retried while the old client still holds the peer port.
func (e *Engine) Reconnect(cfg Config) error {
	e.setMu.Lock()
	defer e.setMu.Unlock()
	c := cfg.normalized()
	e.endHelps("reconnect")
	e.dropServed("")
	e.clearColdIntents()
	for _, t := range e.ListHydrated() {
		e.close(t)
	}
	time.Sleep(e.settle)
	e.disconnect()
	e.cfg.Store(&c)
	o := e.clientOptions(c)
	var err error
	for i := 0; i < e.retries; i++ {
		if err = e.connect(c, o); err == nil {
			break
		}
		e.deps.logf("[Tentacle] reconnect attempt %d/%d: %v", i+1, e.retries, err)
		time.Sleep(e.retryDelay)
	}
	if err != nil {
		return err
	}
	e.friendsReconnected()
	time.Sleep(e.settle)
	e.readyOnce.Do(func() { close(e.ready) })
	return nil
}

func (e *Engine) tickLoop(stop, done chan struct{}) {
	defer close(done)
	tk := time.NewTicker(e.tickEvery)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			e.tickFn()
		}
	}
}

// tick runs every second for every hydrated torrent.
func (e *Engine) tick() {
	e.updateStats()
	e.expire()
}
