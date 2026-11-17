package auth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type loginLimiter struct {
	mu   sync.Mutex
	byIP map[string]*limEntry
}

type limEntry struct {
	l    *rate.Limiter
	seen time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{byIP: map[string]*limEntry{}} }

// allow: 5 attempts, then one every 12 s (5/min).
func (l *loginLimiter) allow(ip string) bool {
	ip = limitKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, e := range l.byIP {
		if now.Sub(e.seen) > 10*time.Minute {
			delete(l.byIP, k)
		}
	}
	e, ok := l.byIP[ip]
	if !ok {
		e = &limEntry{l: rate.NewLimiter(rate.Every(12*time.Second), 5)}
		l.byIP[ip] = e
	}
	e.seen = now
	return e.l.Allow()
}

func (l *loginLimiter) retryAfter(ip string) time.Duration {
	ip = limitKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.byIP[ip]; ok {
		r := e.l.Reserve()
		d := r.Delay()
		r.Cancel()
		return d
	}
	return 0
}

// limitKey groups IPv6 by /64: one host owns the whole prefix.
func limitKey(ip string) string {
	if a, err := netip.ParseAddr(ip); err == nil && a.Is6() && !a.Is4In6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return ip
}

// fromTrustedProxy reports whether the connection comes from a configured proxy.
func fromTrustedProxy(r *http.Request, trusted []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, p := range trusted {
		if p.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

// clientIP trusts X-Forwarded-For only when the peer is a configured proxy.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	for _, p := range trusted {
		if p.Contains(addr.Unmap()) {
			parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	return host
}

func ParseTrusted(list []string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range list {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p)
		} else if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}
