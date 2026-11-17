package mcp

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// rebindGuard replaces the SDK's localhost protection. With sign-in on every request
// already carries a token or a session a rebinding page cannot have, so the guard steps
// aside and /mcp works behind a reverse proxy that keeps Host. With sign-in off it is the
// SDK check: a loopback connection must name a loopback Host.
func rebindGuard(signedIn func() bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if signedIn == nil || !signedIn() {
			if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && la != nil &&
				isLoopback(la.String()) && !isLoopback(r.Host) {
				http.Error(w, "forbidden Host "+strconv.Quote(r.Host)+
					": /mcp behind a reverse proxy needs sign-in on (Control Panel › 07 Access)", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopback reports whether addr ("host", "host:port", "[v6]:port") is localhost or a
// loopback IP, as the SDK's own check does.
func isLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
