package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestActorNamesWhoChanged(t *testing.T) {
	s, _, _ := newTestService(t)
	_, tok, _ := s.CreateToken("Agent", ScopeAPI)
	var got [2]string
	h := Wrap(s, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got[0], got[1] = s.Actor(r)
	}))
	call := func(hdr map[string]string) [2]string {
		r := httptest.NewRequest("POST", "/api/config", nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		return got
	}
	if a := call(map[string]string{"Authorization": "Bearer " + tok}); a != [2]string{"api", "Agent"} {
		t.Fatalf("token with sign-in off: %v", a)
	}
	if a := call(map[string]string{"Authorization": "Bearer " + s.InternalToken()}); a != [2]string{"internal", "engine"} {
		t.Fatalf("internal token: %v", a)
	}
	if a := call(nil); a != [2]string{"ui", ""} {
		t.Fatalf("no credentials, sign-in off: %v", a)
	}
	s.Setup("admin", "correct horse battery", "", "")
	if a := call(map[string]string{"Authorization": "Bearer " + tok}); a != [2]string{"api", "Agent"} {
		t.Fatalf("token with sign-in on: %v", a)
	}
}

func TestHistoryRefusesTokensWithSignInOn(t *testing.T) {
	for _, p := range []string{"/api/config/history", "/api/config/history/3", "/api/config/rollback", "/api/gostorm/history", "/api/gostorm/history/2/restore", "/api/gostorm/rollback", "/api/tentacle/history", "/api/tentacle/history/2/restore", "/api/tentacle/rollback"} {
		if scopeRefusal(ScopeAPI, p) == "" {
			t.Fatalf("%s must refuse API tokens", p)
		}
	}
	if scopeRefusal(ScopeAPI, "/api/config") != "" {
		t.Fatal("POST /api/config stays open to API tokens")
	}
}
