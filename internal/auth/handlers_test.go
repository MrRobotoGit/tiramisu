package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/control#access": "/control#access", "": "/control", "//evil.com": "/control",
		"/\\evil.com": "/control", "https://evil.com": "/control", "control": "/control",
		"/\t/evil.com": "/control", "/\n/evil.com": "/control", "/\r/evil.com": "/control",
		"/control?x=1": "/control?x=1", "/.//evil.com": "/control",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
}

func TestHandlersFlow(t *testing.T) {
	s, _, _ := newTestService(t)
	mux := http.NewServeMux()
	Register(mux, s, nil)
	h := Wrap(s, nil, mux)

	post := func(path, body string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// token created with auth off
	w := post("/api/auth/tokens", `{"name":"Agent MCP"}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"token":"tmr_`) {
		t.Fatalf("token: %d %s", w.Code, w.Body)
	}
	w = post("/api/auth/setup", `{"username":"admin","password":"correct horse"}`, nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Set-Cookie"), cookieName+"=") {
		t.Fatalf("setup: %d", w.Code)
	}
	if w := post("/api/auth/login", `{"username":"admin","password":"nope nope nope"}`, nil); w.Code != 401 {
		t.Fatalf("wrong login: %d", w.Code)
	}
	if w := post("/api/auth/tokens", `{"name":"x"}`, nil); w.Code != 401 {
		t.Fatal("token created without credentials with auth on")
	}
}

func TestLoginRateLimited(t *testing.T) {
	s, _, _ := newTestService(t)
	s.Setup("admin", "correct horse", "", "")
	mux := http.NewServeMux()
	Register(mux, s, nil)
	var last int
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"a","password":"bbbbbbbbbbbb"}`))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		last = w.Code
	}
	if last != 429 {
		t.Fatal(last)
	}
}

func TestPasswordEndpointsRateLimited(t *testing.T) {
	s, _, _ := newTestService(t)
	s.Setup("admin", "correct horse", "", "")
	s.Disable("correct horse")
	mux := http.NewServeMux()
	Register(mux, s, nil)
	var last int
	for i := 0; i < 6; i++ {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/auth/enable", strings.NewReader(`{"password":"guess guess guess"}`)))
		last = w.Code
	}
	if last != 429 {
		t.Fatalf("enable not rate limited: %d", last)
	}
}
