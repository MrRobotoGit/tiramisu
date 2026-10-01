package torrent

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/log"
	"github.com/anacrolix/missinggo/v2"
	"golang.org/x/time/rate"

	"github.com/anacrolix/torrent/iplist"
	"github.com/anacrolix/torrent/mse"
	"github.com/anacrolix/torrent/storage"
	"github.com/anacrolix/torrent/version"
)

// Contains config elements that are exclusive to tracker handling. There may be other fields in
// ClientConfig that are also relevant.
type ClientTrackerConfig struct {
	// Don't announce to trackers. This only leaves DHT to discover peers.
	DisableTrackers bool `long:"disable-trackers"`
	// Defines DialContext func to use for HTTP tracker announcements
	TrackerDialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// Defines ListenPacket func to use for UDP tracker announcements
	TrackerListenPacket func(network, addr string) (net.PacketConn, error)
	// Takes a tracker's hostname and requests DNS A and AAAA records.
	// Used in case DNS lookups require a special setup (i.e., dns-over-https)
	LookupTrackerIp func(*url.URL) ([]net.IP, error)
	// How long to wait before retrying an announce that failed. Each further consecutive failure
	// doubles the wait, up to FailedAnnounceMaxInterval. A tracker that has gone away is retried
	// forever, so without a growing interval every torrent keeps knocking at this rate.
	FailedAnnounceMinInterval time.Duration
	// The cap on the doubling described above. Set it to FailedAnnounceMinInterval to retry at a
	// constant interval.
	FailedAnnounceMaxInterval time.Duration
}

// Backported from anacrolix/torrent upstream (4aea1d64b, #1116).
const (
	defaultFailedAnnounceMinInterval = time.Minute
	defaultFailedAnnounceMaxInterval = 30 * time.Minute
)

// failedAnnounceInterval is how long to wait after an announce that failed, given how many
// consecutive failures there have been (1 for the first).
func (cfg *ClientTrackerConfig) failedAnnounceInterval(consecutiveFailures int) time.Duration {
	d := cfg.FailedAnnounceMinInterval
	if d <= 0 {
		d = defaultFailedAnnounceMinInterval
	}
	maxInterval := cfg.FailedAnnounceMaxInterval
	if maxInterval <= 0 {
		maxInterval = defaultFailedAnnounceMaxInterval
	}
	if maxInterval < d {
		maxInterval = d
	}
	for i := 1; i < consecutiveFailures; i++ {
		// Checked before doubling, so a huge configured max cannot overflow into a negative wait.
		if d >= maxInterval/2 {
			return maxInterval
		}
		d *= 2
	}
	return d
}

type ClientDhtConfig struct {
	// Don't create a DHT.
	NoDHT            bool `long:"disable-dht"`
	DhtStartingNodes func(network string) dht.StartingNodesGetter
	// Called for each anacrolix/dht Server created for the Client.
	ConfigureAnacrolixDhtServer       func(*dht.ServerConfig)
	PeriodicallyAnnounceTorrentsToDht bool
	// OnQuery hook func
	DHTOnQuery func(query *krpc.Msg, source net.Addr) (propagate bool)
}

// Probably not safe to modify this after it's given to a Client.
type ClientConfig struct {
	ClientTrackerConfig
	ClientDhtConfig

	// Store torrent file data in this directory unless .DefaultStorage is
	// specified.
	DataDir string `long:"data-dir" description:"directory to store downloaded torrent data"`
	// The address to listen for new uTP and TCP BitTorrent protocol connections. DHT shares a UDP
	// socket with uTP unless configured otherwise.
	ListenHost              func(network string) string
	ListenPort              int
	NoDefaultPortForwarding bool
	UpnpID                  string
	DisablePEX              bool `long:"disable-pex"`

	// Never send chunks to peers.
	NoUpload bool `long:"no-upload"`
	// Disable uploading even when it isn't fair.
	DisableAggressiveUpload bool `long:"disable-aggressive-upload"`
	// Upload even after there's nothing in it for us. By default uploading is
	// not altruistic, we'll only upload to encourage the peer to reciprocate.
	Seed bool `long:"seed"`
	// Only applies to chunks uploaded to peers, to maintain responsiveness
	// communicating local Client state to peers. Each limiter token
	// represents one byte. The Limiter's burst must be large enough to fit a
	// whole chunk, which is usually 16 KiB (see TorrentSpec.ChunkSize).
	UploadRateLimiter *rate.Limiter
	// Rate limits all reads from connections to peers. Each limiter token
	// represents one byte. The Limiter's burst must be bigger than the
	// largest Read performed on a the underlying rate-limiting io.Reader
	// minus one. This is likely to be the larger of the main read loop buffer
	// (~4096), and the requested chunk size (~16KiB, see
	// TorrentSpec.ChunkSize).
	DownloadRateLimiter *rate.Limiter
	// Maximum unverified bytes across all torrents. Not used if zero.
	MaxUnverifiedBytes int64
	// Minimum age an outstanding request must reach before another peer may steal it; zero or
	// negative disables the check. NewDefaultClientConfig sets 250ms (TORRENT_STEAL_REQUEST_GRACE
	// overrides it). When the holder has a warmed mean request-to-chunk latency, the grace is
	// that mean clamped to [250ms, 1s] instead of this fixed value (peerStealGrace), so it can
	// only extend for a slow holder; an urgent request is capped back at the fixed value, never
	// exempted (a zero grace would re-open the duplicate window). A steal is a Cancel on the wire, and a Cancel that
	// reaches the holder after it served the block does nothing: the block arrives twice and one
	// copy is counted as ConnStats.ChunksReadWasted. Backported from upstream 23d8abf90 (#1095).
	StealRequestGrace time.Duration
	// AdaptivePipeline sizes each peer's request queue on its minimum request latency instead of
	// a fixed 2s of data. On by default; TORRENT_ADAPTIVE_PIPELINE=0 disables it for a
	// comparison run.
	AdaptivePipeline bool
	// PeakEwma vetoes a steal when the stealer, at its recent peak request-to-chunk latency and
	// queue, would not finish the block sooner than the holder (peak-EWMA placement). Off by
	// default until measured; TORRENT_PEAK_EWMA=1 enables it.
	PeakEwma bool
	// Gradient2 sizes each peer's request queue with Netflix's closed-loop Gradient2 limiter
	// (average-RTT gradient, app-limited aware) instead of the fixed/adaptive target. On by
	// default; TORRENT_GRADIENT2=0 disables it. It takes precedence over AdaptivePipeline.
	Gradient2 bool
	// Gradient2AIMD backs the Gradient2 limit off on a peer Reject of a request it held
	// (TORRENT_GRADIENT2_AIMD=1). Gradient2Windowed feeds Gradient2 one median per ~1s window
	// instead of every chunk (TORRENT_GRADIENT2_WINDOWED=1). Both act only with Gradient2 on.
	Gradient2AIMD     bool
	Gradient2Windowed bool
	// RequestReserve lets a request due within 2s exceed a full queue by a reserved quarter of
	// the limit (TORRENT_REQUEST_RESERVE=1).
	RequestReserve bool

	// User-provided Client peer ID. If not present, one is generated automatically.
	PeerID string
	// For the bittorrent protocol.
	DisableUTP bool
	// For the bittorrent protocol.
	DisableTCP bool `long:"disable-tcp"`
	// Called to instantiate storage for each added torrent. Builtin backends
	// are in the storage package. If not set, the "file" implementation is
	// used (and Closed when the Client is Closed).
	DefaultStorage storage.ClientImpl

	HeaderObfuscationPolicy HeaderObfuscationPolicy
	// The crypto methods to offer when initiating connections with header obfuscation.
	CryptoProvides mse.CryptoMethod
	// Chooses the crypto method to use when receiving connections with header obfuscation.
	CryptoSelector mse.CryptoSelector

	IPBlocklist      iplist.Ranger
	DisableIPv6      bool `long:"disable-ipv6"`
	DisableIPv4      bool
	DisableIPv4Peers bool
	// Perform logging and any other behaviour that will help debug.
	Debug  bool `help:"enable debugging"`
	Logger log.Logger

	// Used for torrent sources and webseeding if set.
	WebTransport http.RoundTripper
	// Defines proxy for HTTP requests, such as for trackers. It's commonly set from the result of
	// "net/http".ProxyURL(HTTPProxy).
	HTTPProxy func(*http.Request) (*url.URL, error)
	// Defines DialContext func to use for HTTP requests, such as for fetching metainfo and webtorrent seeds
	HTTPDialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// HTTPUserAgent changes default UserAgent for HTTP requests
	HTTPUserAgent string
	// HttpRequestDirector modifies the request before it's sent.
	// Useful for adding authentication headers, for example
	HttpRequestDirector func(*http.Request) error
	// WebsocketTrackerHttpHeader returns a custom header to be used when dialing a websocket connection
	// to the tracker. Useful for adding authentication headers
	WebsocketTrackerHttpHeader func() http.Header
	// Updated occasionally to when there's been some changes to client
	// behaviour in case other clients are assuming anything of us. See also
	// `bep20`.
	ExtendedHandshakeClientVersion string
	// Peer ID client identifier prefix. We'll update this occasionally to
	// reflect changes to client behaviour that other clients may depend on.
	// Also see `extendedHandshakeClientVersion`.
	Bep20 string

	// Peer dial timeout to use when there are limited peers.
	NominalDialTimeout time.Duration
	// Minimum peer dial timeout to use (even if we have lots of peers).
	MinDialTimeout             time.Duration
	EstablishedConnsPerTorrent int
	HalfOpenConnsPerTorrent    int
	TotalHalfOpenConns         int
	// AggressivePeerManagement enables warmup-phase PEX churn (client.go) and tail-hedging
	// (requesting.go) for streaming cold-start latency. Off by default - only GoStorm's
	// live settings set this; the fork itself defaults to false like any other client.
	AggressivePeerManagement bool
	// Maximum number of peer addresses in reserve.
	TorrentPeersHighWater int
	// Minumum number of peers before effort is made to obtain more peers.
	TorrentPeersLowWater int

	// Limit how long handshake can take. This is to reduce the lingering
	// impact of a few bad apples. 4s loses 1% of successful handshakes that
	// are obtained with 60s timeout, and 5% of unsuccessful handshakes.
	HandshakesTimeout time.Duration
	// How long between writes before sending a keep alive message on a peer connection that we want
	// to maintain.
	KeepAliveTimeout time.Duration
	// Maximum bytes to buffer per peer connection for peer request data before it is sent.
	MaxAllocPeerRequestDataPerConn int64

	// The IP addresses as our peers should see them. May differ from the
	// local interfaces due to NAT or other network configurations.
	PublicIp4 net.IP
	PublicIp6 net.IP

	// Accept rate limiting affects excessive connection attempts from IPs that fail during
	// handshakes or request torrents that we don't have.
	DisableAcceptRateLimiting bool
	// Don't add connections that have the same peer ID as an existing
	// connection for a given Torrent.
	DropDuplicatePeerIds bool
	// Drop peers that are complete if we are also complete and have no use for the peer. This is a
	// bit of a special case, since a peer could also be useless if they're just not interested, or
	// we don't intend to obtain all of a torrent's data.
	DropMutuallyCompletePeers bool
	// Whether to accept peer connections at all.
	AcceptPeerConnections bool
	// Whether a Client should want conns without delegating to any attached Torrents. This is
	// useful when torrents might be added dynamically in callbacks for example.
	AlwaysWantConns bool

	Extensions PeerExtensionBits
	// Bits that peers must have set to proceed past handshakes.
	MinPeerExtensions PeerExtensionBits

	DisableWebtorrent bool
	DisableWebseeds   bool

	Callbacks Callbacks

	// ICEServers defines a slice describing servers available to be used by
	// ICE, such as STUN and TURN servers.
	ICEServers []string

	DialRateLimiter *rate.Limiter

	PieceHashersPerTorrent int // default: 2
}

func (cfg *ClientConfig) SetListenAddr(addr string) *ClientConfig {
	host, port, err := missinggo.ParseHostPort(addr)
	if err != nil {
		panic(err)
	}
	cfg.ListenHost = func(string) string { return host }
	cfg.ListenPort = port
	return cfg
}

func NewDefaultClientConfig() *ClientConfig {
	cc := &ClientConfig{
		HTTPUserAgent:                  version.DefaultHttpUserAgent,
		ExtendedHandshakeClientVersion: version.DefaultExtendedHandshakeClientVersion,
		Bep20:                          version.DefaultBep20Prefix,
		UpnpID:                         version.DefaultUpnpId,
		NominalDialTimeout:             20 * time.Second,
		MinDialTimeout:                 3 * time.Second,
		EstablishedConnsPerTorrent:     50,
		HalfOpenConnsPerTorrent:        25,
		TotalHalfOpenConns:             100,
		TorrentPeersHighWater:          500,
		TorrentPeersLowWater:           50,
		HandshakesTimeout:              4 * time.Second,
		KeepAliveTimeout:               time.Minute,
		MaxAllocPeerRequestDataPerConn: 1 << 20,
		ListenHost:                     func(string) string { return "" },
		UploadRateLimiter:              unlimited,
		DownloadRateLimiter:            unlimited,
		DisableAcceptRateLimiting:      true,
		DropMutuallyCompletePeers:      true,
		HeaderObfuscationPolicy: HeaderObfuscationPolicy{
			Preferred:        true,
			RequirePreferred: false,
		},
		CryptoSelector:         mse.DefaultCryptoSelector,
		CryptoProvides:         mse.AllSupportedCrypto,
		ListenPort:             42069,
		Extensions:             defaultPeerExtensionBytes(),
		AcceptPeerConnections:  true,
		MaxUnverifiedBytes:     64 << 20,
		StealRequestGrace:      stealRequestGraceFromEnv(),
		AdaptivePipeline:       adaptivePipelineFromEnv(),
		PeakEwma:               peakEwmaFromEnv(),
		Gradient2:              gradient2FromEnv(),
		Gradient2AIMD:          envFlag(gradient2AIMDEnvKey, gradient2AIMDEffective),
		Gradient2Windowed:      envFlag(gradient2WindowedEnvKey, gradient2WindowedEffective),
		RequestReserve:         envFlag(requestReserveEnvKey, requestReserveEffective),
		DialRateLimiter:        rate.NewLimiter(10, 10),
		PieceHashersPerTorrent: 2,
	}
	cc.DhtStartingNodes = func(network string) dht.StartingNodesGetter {
		return func() ([]dht.Addr, error) { return dht.GlobalBootstrapAddrs(network) }
	}
	cc.PeriodicallyAnnounceTorrentsToDht = true
	// Gradient2 takes precedence in nominalMaxRequests: report the adaptive pipeline as the
	// fallback it then is, not as an active lever.
	if cc.Gradient2 && cc.AdaptivePipeline {
		adaptivePipelineEffective.Set("superseded by gradient2")
	}
	return cc
}

type HeaderObfuscationPolicy struct {
	RequirePreferred bool // Whether the value of Preferred is a strict requirement.
	Preferred        bool // Whether header obfuscation is preferred.
}
