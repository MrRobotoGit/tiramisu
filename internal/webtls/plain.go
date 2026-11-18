package webtls

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"tiramisu/internal/auth"
)

// PlainPolicy guards the plain-HTTP side while HTTPS runs. accessOn is the sign-in state
// at start-up; trusted proxies terminate TLS themselves and pass through, like loopback;
// onBearer sees every token sent in clear from another host.
func PlainPolicy(next http.Handler, accessOn bool, trusted []netip.Prefix, onBearer func(tok, ip string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := remoteIP(r)
		if thisHost(ip) || inPrefixes(ip, trusted) {
			next.ServeHTTP(w, r)
			return
		}
		if tok := bearer(r); tok != "" && onBearer != nil {
			onBearer(tok, ip.String())
		}
		p := auth.CleanPath(r.URL.Path)
		if p == "/plex/webhook" || p == "/webhook" {
			next.ServeHTTP(w, r)
			return
		}
		// Not cached: if HTTPS is turned off later, the browser must come back to http://.
		w.Header().Set("Cache-Control", "no-store")
		page := (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.Contains(r.Header.Get("Accept"), "text/html")
		if page && r.Host != "" {
			http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return
		}
		if accessOn || page {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"use https"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func remoteIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// hostAddrs lists this machine's own addresses. With some VPN NAT rules a connection to
// 127.0.0.1 arrives from the host's LAN address: it is still a local call.
var hostAddrs = func() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out
}

var local struct {
	sync.Mutex
	at    time.Time
	addrs []netip.Addr
}

// thisHost: loopback, or one of this machine's addresses (refreshed at most every 30s).
func thisHost(ip netip.Addr) bool {
	if ip.IsLoopback() {
		return true
	}
	local.Lock()
	defer local.Unlock()
	if time.Since(local.at) > 30*time.Second {
		local.addrs, local.at = hostAddrs(), time.Now()
	}
	return slices.Contains(local.addrs, ip)
}
