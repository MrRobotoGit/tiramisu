package auth

import (
	"strings"
	"testing"
)

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	s, _, _ := newTestService(t)
	h := Wrap(s, nil, okHandler())
	for _, path := range []string{"/control", "/login", "/ui/theme.css"} {
		w := do(h, "GET", path, nil)
		for _, k := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Content-Security-Policy"} {
			if w.Header().Get(k) == "" {
				t.Errorf("%s: missing %s", path, k)
			}
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") {
			t.Errorf("%s: CSP without frame-ancestors", path)
		}
		if w.Header().Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS on HTTP", path)
		}
	}
	s.Setup("admin", "correct horse", "", "")
	w := do(h, "GET", "/api/health", nil) // 401 path
	if w.Code != 401 || w.Header().Get("X-Frame-Options") == "" {
		t.Fatal("headers missing on the 401")
	}
	w = do(h, "GET", "/login", map[string]string{"X-Forwarded-Proto": "https"})
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("HSTS missing behind HTTPS proxy")
	}
}
