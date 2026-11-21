package bt

import (
	"context"
	"net"
	"sync"
	"time"
)

// DHTBootstrapHosts are the fixed starting nodes (engine.md §1.2).
var DHTBootstrapHosts = []string{
	"router.bittorrent.com:6881",
	"router.utorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"dht.libtorrent.org:25401",
	"dht.aelitis.com:6881",
	"router.bitcomet.com:6881",
	"dht.vuze.com:6881",
}

// LookupIP is net.DefaultResolver.LookupIP, replaceable in tests.
type LookupIP func(ctx context.Context, network, host string) ([]net.IP, error)

// ResolveDHTBootstrap resolves hosts to IPv4 ip:port, each lookup bounded by perHost.
func ResolveDHTBootstrap(hosts []string, perHost time.Duration, lookup LookupIP) []string {
	var out []string
	for _, hp := range hosts {
		host, port, err := net.SplitHostPort(hp)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), perHost)
		ips, err := lookup(ctx, "ip4", host)
		cancel()
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil {
				out = append(out, net.JoinHostPort(v4.String(), port))
			}
		}
	}
	return out
}

var (
	dhtOnce  sync.Once
	dhtNodes []string
)

// DHTNodes resolves the bootstrap hosts once per process (IPv4 only, 3s per host).
func DHTNodes() []string {
	dhtOnce.Do(func() {
		dhtNodes = ResolveDHTBootstrap(DHTBootstrapHosts, 3*time.Second, net.DefaultResolver.LookupIP)
	})
	return dhtNodes
}
