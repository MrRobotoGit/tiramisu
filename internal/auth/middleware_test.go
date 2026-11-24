package auth

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
}

func do(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestMiddlewareOpenPassesEverything(t *testing.T) {
	s, _, _ := newTestService(t)
	h := Wrap(s, nil, okHandler())
	if w := do(h, "POST", "/api/config", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestMiddlewareEnabled(t *testing.T) {
	s, _, _ := newTestService(t)
	sess, _ := s.Setup("admin", "correct horse", "", "")
	_, tok, _ := s.CreateToken("t", ScopeMCP)
	_, apiTok, _ := s.CreateToken("a", ScopeAPI)
	bear := func(t string) map[string]string { return map[string]string{"Authorization": "Bearer " + t} }
	h := Wrap(s, nil, okHandler())
	html := map[string]string{"Accept": "text/html"}
	cases := []struct {
		name, method, path string
		hdr                map[string]string
		want               int
	}{
		{"webhook plex", "POST", "/plex/webhook", nil, 200},
		{"webhook", "POST", "/webhook", nil, 200},
		{"asset ui", "GET", "/ui/theme.css", nil, 200},
		{"login page", "GET", "/login", html, 200},
		{"status", "GET", "/api/auth/status", nil, 200},
		{"traversal from /ui/", "GET", "/ui/../api/health", nil, 401},
		{"double slash", "GET", "//api/health", nil, 401},
		{"well-known", "GET", "/.well-known/oauth-protected-resource", nil, 404},
		{"page without cookie", "GET", "/control", html, 302},
		{"api without credentials", "GET", "/api/health", nil, 401},
		{"mcp without credentials", "POST", "/mcp", nil, 401},
		{"valid bearer", "POST", "/mcp", map[string]string{"Authorization": "Bearer " + tok}, 200},
		{"MCP token on the API", "GET", "/api/health", bear(tok), 403},
		{"MCP token on config", "POST", "/api/config", bear(tok), 403},
		{"API token on the API", "GET", "/api/health", bear(apiTok), 200},
		{"API token on config", "POST", "/api/config", bear(apiTok), 200},
		{"API token on /mcp", "POST", "/mcp", bear(apiTok), 403},
		{"API token revokes a token", "DELETE", "/api/auth/tokens/1", bear(apiTok), 403},
		{"API token creates a token", "POST", "/api/auth/tokens", bear(apiTok), 403},
		{"MCP token revokes a token", "DELETE", "/api/auth/tokens/1", bear(tok), 403},
		{"API token turns sign-in off", "POST", "/api/auth/disable", bear(apiTok), 403},
		{"API token on sessions", "GET", "/api/auth/sessions", bear(apiTok), 403},
		{"wrong bearer", "GET", "/metrics", map[string]string{"Authorization": "Bearer tmr_nope"}, 401},
		{"internal token", "GET", "/metrics", map[string]string{"Authorization": "Bearer " + s.InternalToken()}, 200},
		{"cookie", "GET", "/api/health", map[string]string{"Cookie": cookieName + "=" + sess}, 200},
		{"cross-site with cookie", "POST", "/api/config", map[string]string{"Cookie": cookieName + "=" + sess, "Sec-Fetch-Site": "cross-site"}, 403},
		{"same-origin with cookie", "POST", "/api/config", map[string]string{"Cookie": cookieName + "=" + sess, "Sec-Fetch-Site": "same-origin"}, 200},
	}
	for _, c := range cases {
		if w := do(h, c.method, c.path, c.hdr); w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
	}
	w := do(h, "GET", "/control?x=1", html)
	if loc := w.Header().Get("Location"); loc != "/login?next=%2Fcontrol%3Fx%3D1" {
		t.Errorf("redirect %q", loc)
	}
	if w := do(h, "GET", "/api/health", nil); w.Header().Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate")
	}
}

func TestMiddlewareCrossOriginOnSetupWhileOpen(t *testing.T) {
	s, _, _ := newTestService(t)
	h := Wrap(s, nil, okHandler())
	if w := do(h, "POST", "/api/auth/setup", map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	// With sign-in off, a page on another site must not be able to write or restart.
	for _, p := range []string{"/api/config", "/api/restart", "/api/library/remove"} {
		if w := do(h, "POST", p, map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != 403 {
			t.Fatalf("%s cross-site with sign-in off: %d", p, w.Code)
		}
		if w := do(h, "POST", p, map[string]string{"Origin": "http://evil.example"}); w.Code != 403 {
			t.Fatalf("%s with foreign Origin and sign-in off: %d", p, w.Code)
		}
	}
	// curl, scripts and webhooks send neither Origin nor Sec-Fetch-Site: they pass.
	for _, p := range []string{"/api/config", "/plex/webhook", "/webhook"} {
		if w := do(h, "POST", p, nil); w.Code != 200 {
			t.Fatalf("%s without browser headers: %d", p, w.Code)
		}
	}
	if w := do(h, "GET", "/api/config", map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != 200 {
		t.Fatal("reads do not go through the cross-origin check")
	}
}

func TestMiddlewareFailClosed(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir)
	s := New(nil, dir, false)
	h := Wrap(s, nil, okHandler())
	if w := do(h, "GET", "/api/health", nil); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if w := do(h, "POST", "/plex/webhook", nil); w.Code != 200 {
		t.Fatal("webhook blocked")
	}
}

func writeMarker(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, markerName), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

// A plain-HTTP proxy that rewrites Host: the browser sends Origin and no Sec-Fetch-Site.
func TestCrossOriginBehindHostRewritingProxy(t *testing.T) {
	s, _, _ := newTestService(t)
	proxy := netip.MustParsePrefix("10.0.0.5/32")
	h := Wrap(s, []netip.Prefix{proxy}, okHandler())
	req := func(remote, fwdHost, origin, site string) int {
		r := httptest.NewRequest("POST", "http://192.168.1.20:9080/api/config", nil)
		r.RemoteAddr = remote + ":40000"
		if fwdHost != "" {
			r.Header.Set("X-Forwarded-Host", fwdHost)
		}
		r.Header.Set("Origin", origin)
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := req("10.0.0.5", "tiramisu.lan", "http://tiramisu.lan", ""); c != 200 {
		t.Errorf("trusted proxy naming the page's host: %d", c)
	}
	if c := req("10.0.0.5", "", "http://tiramisu.lan", ""); c != 403 {
		t.Errorf("no X-Forwarded-Host: %d", c)
	}
	if c := req("10.0.0.9", "tiramisu.lan", "http://tiramisu.lan", ""); c != 403 {
		t.Errorf("untrusted peer claiming a forwarded host: %d", c)
	}
	if c := req("10.0.0.5", "tiramisu.lan", "http://evil.example", ""); c != 403 {
		t.Errorf("another site through the trusted proxy: %d", c)
	}
	if c := req("10.0.0.5", "tiramisu.lan", "http://tiramisu.lan", "cross-site"); c != 403 {
		t.Errorf("browser says cross-site: %d", c)
	}
}

func TestScopeRefusal(t *testing.T) {
	cases := []struct {
		scope, path string
		refused     bool
	}{
		{ScopeMCP, "/mcp", false},
		{ScopeMCP, "/api/health", true},
		{ScopeAPI, "/api/health", false},
		{ScopeAPI, "/mcp", true},
		{ScopeAPI, "/api/auth/tokens", true},
		{"", "/api/health", true},
		{"admin", "/api/config", true},
	}
	for _, c := range cases {
		if got := scopeRefusal(c.scope, c.path) != ""; got != c.refused {
			t.Errorf("scope %q on %s: refused=%v", c.scope, c.path, got)
		}
	}
}
