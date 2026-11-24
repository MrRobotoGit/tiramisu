package bt

import (
	"crypto/rand"
	"encoding/base32"
	"net"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/mse"
	pp "github.com/anacrolix/torrent/peer_protocol"
	"golang.org/x/time/rate"
)

// Identity masquerades as qBittorrent (engine.md §1.2).
const (
	Bep20Prefix   = "-qB5030-"
	ClientVersion = "qBittorrent/5.0.3"
)

// Options are the client knobs, read once per client.
type Options struct {
	Debug                    bool
	EnableIPv6               bool
	DisableTCP               bool
	DisableUTP               bool
	DisableDHT               bool
	DisablePEX               bool
	DisableUpload            bool
	ForceEncrypt             bool
	AggressivePeerManagement bool
	ConnectionsLimit         int
	DownloadRateLimit        int  // kB/s, 0 = unlimited
	UploadRateLimit          int  // kB/s, 0 = unlimited
	ListenPort               int  // 0 = random
	PortForwarding           bool // the library's UPnP maps ListenPort
	DisableTrackers          bool // tests only

	PublicIP4 net.IP
	PublicIP6 net.IP
	DHTNodes  []string // resolved ip:port; empty = the library's global bootstrap

	Blocklist *Blocklist // nil = no filtering

	Friends     torrent.FriendHooks                 // nil = no friend nodes
	OnHandshake func(incoming bool, network string) // every completed handshake; nil = none
	// OnFriendLinkClosed runs under the client lock when a proven friend link closes.
	OnFriendLinkClosed func(ih [20]byte)
	// OnFriendHave runs under the client lock for every Have a proven friend link announces: it
	// must only count.
	OnFriendHave func(ih [20]byte, piece int)
}

func peerID() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	s := Bep20Prefix + base32.StdEncoding.EncodeToString(b)
	return s[:20]
}

func limiter(kB int) *rate.Limiter {
	if kB <= 0 {
		return rate.NewLimiter(rate.Inf, 0)
	}
	r := kB * 1024
	return rate.NewLimiter(rate.Limit(r), max(r, 16<<10))
}

// libConfig builds the library config; every value here is a decision in engine.md §1.2.
func libConfig(o Options, st *Storage) *torrent.ClientConfig {
	c := torrent.NewDefaultClientConfig()
	c.DefaultStorage = st
	c.Debug = o.Debug
	c.DisableIPv6 = !o.EnableIPv6
	c.DisableTCP = o.DisableTCP
	c.DisableUTP = o.DisableUTP
	c.DisableWebtorrent = true // pion/dtls CVE; a wss:// tracker would start it
	c.NoDefaultPortForwarding = !o.PortForwarding
	c.NoDHT = o.DisableDHT
	c.DisablePEX = o.DisablePEX
	c.NoUpload = o.DisableUpload
	c.DisableTrackers = o.DisableTrackers
	if o.Blocklist != nil {
		c.IPBlocklist = o.Blocklist.ranger()
	}
	c.Bep20 = Bep20Prefix
	c.PeerID = peerID()
	c.HTTPUserAgent = ClientVersion
	c.ExtendedHandshakeClientVersion = ClientVersion
	c.UpnpID = ClientVersion
	c.EstablishedConnsPerTorrent = o.ConnectionsLimit
	if c.EstablishedConnsPerTorrent <= 0 {
		c.EstablishedConnsPerTorrent = 25
	}
	c.AggressivePeerManagement = o.AggressivePeerManagement
	c.TotalHalfOpenConns = 500
	c.HalfOpenConnsPerTorrent = 60
	c.DisableAcceptRateLimiting = false
	c.PieceHashersPerTorrent = 2
	c.DialRateLimiter = rate.NewLimiter(40, 40)
	c.KeepAliveTimeout = 30 * time.Second
	c.HandshakesTimeout = 5 * time.Second
	c.TorrentPeersHighWater = 200
	c.TorrentPeersLowWater = 50
	c.DropMutuallyCompletePeers = true
	c.NominalDialTimeout = 6 * time.Second
	c.MinDialTimeout = 2 * time.Second
	c.HeaderObfuscationPolicy.Preferred = o.ForceEncrypt
	c.HeaderObfuscationPolicy.RequirePreferred = o.ForceEncrypt
	if o.ForceEncrypt {
		c.CryptoProvides = mse.CryptoMethodRC4
	} else {
		c.CryptoProvides = mse.AllSupportedCrypto
	}
	c.DownloadRateLimiter = limiter(o.DownloadRateLimit)
	c.UploadRateLimiter = limiter(o.UploadRateLimit)
	c.ListenPort = max(o.ListenPort, 0)
	c.PublicIp4 = o.PublicIP4
	c.PublicIp6 = o.PublicIP6
	c.Friends = o.Friends
	if f := o.OnHandshake; f != nil {
		c.Callbacks.CompletedHandshake = func(pc *torrent.PeerConn, _ torrent.InfoHash) {
			f(pc.Discovery == torrent.PeerSourceIncoming, pc.Network)
		}
	}
	if f := o.OnFriendLinkClosed; f != nil {
		c.Callbacks.PeerConnClosed = func(pc *torrent.PeerConn) {
			if pc.Friend() {
				f(pc.Torrent().InfoHash())
			}
		}
	}
	if f := o.OnFriendHave; f != nil {
		c.Callbacks.ReadMessage = func(pc *torrent.PeerConn, m *pp.Message) {
			if m.Type == pp.Have && pc.Friend() {
				f(pc.Torrent().InfoHash(), int(m.Index))
			}
		}
	}
	if !o.DisableDHT && len(o.DHTNodes) > 0 {
		nodes := o.DHTNodes
		c.DhtStartingNodes = func(network string) dht.StartingNodesGetter {
			return func() ([]dht.Addr, error) {
				var out []dht.Addr
				for _, n := range nodes {
					if ua, err := net.ResolveUDPAddr("udp4", n); err == nil {
						out = append(out, dht.NewAddr(ua))
					}
				}
				return out, nil
			}
		}
	}
	return c
}
