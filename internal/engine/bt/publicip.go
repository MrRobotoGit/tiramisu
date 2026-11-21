package bt

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Public-IP services, plain-text answers.
var (
	PublicIP4URL = "https://api.ipify.org"
	PublicIP6URL = "https://api6.ipify.org"
)

// FetchPublicIP asks url for our address, bounded by timeout. It keeps only a public address
// of the wanted family (v4 true = IPv4).
func FetchPublicIP(client *http.Client, url string, v4 bool, timeout time.Duration) net.IP {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return nil
	}
	ip := net.ParseIP(strings.TrimSpace(string(b)))
	if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
		return nil
	}
	if (ip.To4() != nil) != v4 {
		return nil
	}
	return ip
}

// publicIPCache keeps the addresses found; a failed lookup (nil) is tried again next time.
type publicIPCache struct {
	mu     sync.Mutex
	v4, v6 net.IP
	fetch  func(url string, v4 bool) net.IP
}

func newPublicIPCache() *publicIPCache {
	c := &http.Client{}
	return &publicIPCache{fetch: func(url string, v4 bool) net.IP { return FetchPublicIP(c, url, v4, 3*time.Second) }}
}

var publicIPs = newPublicIPCache()

func (c *publicIPCache) get(ipv6 bool) (net.IP, net.IP) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.v4 == nil {
		c.v4 = c.fetch(PublicIP4URL, true)
	}
	if !ipv6 {
		return c.v4, nil
	}
	if c.v6 == nil {
		c.v6 = c.fetch(PublicIP6URL, false)
	}
	return c.v4, c.v6
}

// PublicIPs looks the addresses up, 3s each, IPv6 only when asked. A success is kept for the
// process; a failure is retried at the next call.
func PublicIPs(ipv6 bool) (net.IP, net.IP) { return publicIPs.get(ipv6) }
