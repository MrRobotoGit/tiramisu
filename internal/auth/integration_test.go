package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The whole :9080 shape: real routes registered next to the auth API, wrapped once.
func TestIntegrationScenario(t *testing.T) {
	s, _, _ := newTestService(t)
	mux := http.NewServeMux()
	Register(mux, s, nil)
	enabledSeenByConfig := false
	for _, p := range []string{"/mcp", "/metrics", "/plex/webhook", "/api/health"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	}
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		enabledSeenByConfig = s.Enabled() // a stale Control Panel save must not touch sign-in
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(Wrap(s, nil, mux))
	defer srv.Close()
	c := srv.Client()

	get := func(path, auth string) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if get("/mcp", "") != 200 || get("/metrics", "") != 200 {
		t.Fatal("with auth off everything must pass")
	}
	_, tok, _ := s.CreateToken("Agent MCP", ScopeMCP)
	r, _ := c.Post(srv.URL+"/api/auth/setup", "application/json", strings.NewReader(`{"username":"admin","password":"correct horse"}`))
	if r.StatusCode != 200 {
		t.Fatalf("setup %d", r.StatusCode)
	}
	cookie := r.Cookies()[0]
	if get("/mcp", "") != 401 || get("/mcp", tok) != 200 || get("/metrics", s.InternalToken()) != 200 {
		t.Fatal("bearer")
	}
	if get("/.well-known/oauth-authorization-server", "") != 404 {
		t.Fatal("well-known")
	}
	if r, _ := c.Post(srv.URL+"/plex/webhook", "text/plain", nil); r.StatusCode != 200 {
		t.Fatal("webhook")
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/config", strings.NewReader(`{}`))
	req.AddCookie(cookie)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if r, _ := c.Do(req); r.StatusCode != 200 || !enabledSeenByConfig || !s.Enabled() {
		t.Fatal("config save with auth on")
	}
}
