package auth

import (
	"testing"
)

func TestTokensLifecycle(t *testing.T) {
	s, _, _ := newTestService(t)
	id, tok, err := s.CreateToken("Agent MCP", ScopeMCP)
	if err != nil || id == 0 {
		t.Fatal(err)
	}
	if sc, ok := s.AuthenticateToken(tok, "1.2.3.4"); !ok || sc != ScopeMCP {
		t.Fatal("valid token refused")
	}
	list, _ := s.Tokens()
	if len(list) != 1 || list[0].Prefix != tok[:8] || list[0].LastIP != "1.2.3.4" {
		t.Fatalf("%+v", list)
	}
	if err := s.RevokeToken(id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.AuthenticateToken(tok, ""); ok {
		t.Fatal("revoked token accepted (cache?)")
	}
	if err := s.RevokeToken(id); err != ErrNotFound {
		t.Fatal("double revoke")
	}
	if _, _, err := s.CreateToken("", ScopeMCP); err == nil {
		t.Fatal("empty name")
	}
}

func TestTokensWorkWhileOpen(t *testing.T) {
	s, _, _ := newTestService(t) // auth off
	if _, _, err := s.CreateToken("menubar", ScopeAPI); err != nil {
		t.Fatal(err)
	}
}

func TestReset(t *testing.T) {
	s, db, dir := newTestService(t)
	s.Setup("admin", "correct horse", "", "")
	s.CreateToken("x", ScopeMCP)
	if err := Reset(db, dir); err != nil {
		t.Fatal(err)
	}
	s2 := New(db, dir, false)
	if st := s2.State(); st.Enabled || st.Configured {
		t.Fatalf("%+v", st)
	}
	if l, _ := s2.Tokens(); len(l) != 0 {
		t.Fatal("tokens survived")
	}
}

func TestCreateTokenScope(t *testing.T) {
	s, _, _ := newTestService(t)
	if _, _, err := s.CreateToken("x", "admin"); err != ErrBadScope {
		t.Fatalf("unknown scope: %v", err)
	}
	_, tok, err := s.CreateToken("menubar", ScopeAPI)
	if err != nil {
		t.Fatal(err)
	}
	if sc, ok := s.AuthenticateToken(tok, ""); !ok || sc != ScopeAPI {
		t.Fatalf("scope read %q", sc)
	}
	list, _ := s.Tokens()
	if len(list) != 1 || list[0].Scope != ScopeAPI {
		t.Fatalf("list %+v", list)
	}
}
