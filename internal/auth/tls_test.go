package auth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHSTSAndSecureCookie(t *testing.T) {
	s, _, _ := newTestService(t)
	h := Wrap(s, nil, okHandler())
	w := do(h, "GET", "/login", map[string]string{"X-Forwarded-Proto": "https"})
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("HSTS missing behind HTTPS proxy (as before native HTTPS)")
	}
	r := httptest.NewRequest("GET", "/login", nil)
	r.TLS = &tls.ConnectionState{}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS on direct TLS with SetHSTS(false) (local CA)")
	}
	s.SetHSTS(true)
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("HSTS missing on direct TLS with the user's certificate")
	}
}

func TestSessionCookieNamePerScheme(t *testing.T) {
	rw := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.TLS = &tls.ConnectionState{}
	setSessionCookie(rw, r, "v")
	if c := rw.Result().Cookies()[0]; c.Name != secureCookieName || !c.Secure {
		t.Fatalf("HTTPS: %s secure=%v", c.Name, c.Secure)
	}
	rw = httptest.NewRecorder()
	setSessionCookie(rw, httptest.NewRequest("GET", "/", nil), "v")
	if c := rw.Result().Cookies()[0]; c.Name != cookieName || c.Secure {
		t.Fatalf("HTTP: %s secure=%v", c.Name, c.Secure)
	}
	both := httptest.NewRequest("GET", "/", nil)
	both.AddCookie(&http.Cookie{Name: cookieName, Value: "plain"})
	both.AddCookie(&http.Cookie{Name: secureCookieName, Value: "tls"})
	if c, _ := sessionCookie(both); c.Value != "tls" {
		t.Fatal("with both, the HTTPS one wins")
	}
	rw = httptest.NewRecorder()
	clearSessionCookie(rw)
	if len(rw.Result().Cookies()) != 2 {
		t.Fatal("logout clears both cookies")
	}
}

func TestCleartextUses(t *testing.T) {
	s, _, _ := newTestService(t)
	_, tok, err := s.CreateToken("backup script", ScopeAPI)
	if err != nil {
		t.Fatal(err)
	}
	s.NoteCleartext(tok, "192.168.1.70")
	s.NoteCleartext(tok, "192.168.1.70")
	s.NoteCleartext("tmr_madeup", "192.168.1.70")
	u := s.CleartextUses()
	if len(u) != 1 || u[0].Name != "backup script" || u[0].IP != "192.168.1.70" || u[0].Count != 2 {
		t.Fatalf("%+v", u)
	}
}

func TestCleartextLookupBudget(t *testing.T) {
	s, _, _ := newTestService(t)
	_, tok, _ := s.CreateToken("menubar", ScopeAPI)
	fake := func(i int) string { return tok[:len(tok)-4] + string(rune('a'+i/26)) + string(rune('a'+i%26)) + "zz" }
	for i := 0; i < cleartextLookups+10; i++ {
		s.NoteCleartext(fake(i), "192.168.1.70")
	}
	s.NoteCleartext(tok, "192.168.1.70") // budget spent: not looked up
	if len(s.CleartextUses()) != 0 {
		t.Fatal("past the per-minute lookup cap the DB is not queried")
	}
}

func TestCleartextMadeUpTokensCannotBlind(t *testing.T) {
	s, _, _ := newTestService(t)
	id, tok, _ := s.CreateToken("menubar", ScopeAPI)
	// Well formed but with a prefix no token has: dropped without spending the budget.
	for i := 0; i < cleartextLookups*3; i++ {
		s.NoteCleartext("tmr_"+string(rune('a'+i/26%26))+string(rune('a'+i%26))+tok[6:], "192.168.1.66")
	}
	s.NoteCleartext(tok, "192.168.1.70")
	if u := s.CleartextUses(); len(u) != 1 || u[0].IP != "192.168.1.70" {
		t.Fatalf("the real token must be recorded: %+v", u)
	}
	if err := s.RevokeToken(id); err != nil {
		t.Fatal(err)
	}
	if len(s.CleartextUses()) != 0 {
		t.Fatal("a revoked token disappears from the record")
	}
	s.NoteCleartext(tok, "192.168.1.70")
	if len(s.CleartextUses()) != 0 {
		t.Fatal("a revoked token must no longer be recorded")
	}
}
