package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tiramisu/internal/metadb"
)

func TestSessionValueIsNotAToken(t *testing.T) {
	s, _, _ := newTestService(t)
	sess, _ := s.Setup("admin", "correct horse", "", "")
	s.AuthenticateSession(sess) // now cached
	if _, ok := s.AuthenticateToken(sess, ""); ok {
		t.Fatal("session value accepted as a token")
	}
	if _, ok := s.AuthenticateToken("tmr_"+sess, ""); ok {
		t.Fatal("session cookie accepted as Bearer")
	}
	_, tok, _ := s.CreateToken("x", ScopeMCP)
	s.AuthenticateToken(tok, "")
	if ok, _ := s.AuthenticateSession(tok); ok {
		t.Fatal("token accepted as cookie")
	}
}

func TestTypedNilStore(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, markerName), nil, 0600)
	var db *metadb.DB
	s := New(db, dir, false) // must not panic
	if !s.State().FailClosed {
		t.Fatal("expected fail-closed")
	}
}

func TestMarkerWithEmptyDBFailsClosed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, markerName), nil, 0600)
	db, _ := metadb.New(filepath.Join(dir, "t.db"), nil) // recreated, no user
	defer db.Close()
	s := New(db, dir, false)
	if !s.FailClosed() {
		t.Fatal("empty DB under marker reopened access")
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); err != nil {
		t.Fatal("marker removed")
	}
}

func TestStaleReadIsNotCached(t *testing.T) {
	s, _, _ := newTestService(t)
	now := time.Now()
	_, gen := s.lookup("t:x", now)
	s.clearCache() // revoke happened while the DB was read
	s.store_("t:x", &cacheEntry{at: now}, gen, now)
	if e, _ := s.lookup("t:x", now); e != nil {
		t.Fatal("read from before the revoke ended up cached")
	}
}

func TestCachedSessionExpires(t *testing.T) {
	s, db, _ := newTestService(t)
	now := time.Unix(2_000_000, 0)
	s.now = func() time.Time { return now }
	sess, _ := s.Setup("admin", "correct horse", "", "")
	db.SQL().Exec(`UPDATE auth_sessions SET expires_at = ?`, now.Unix()+60)
	s.clearCache()
	if ok, _ := s.AuthenticateSession(sess); !ok {
		t.Fatal("valid")
	}
	now = now.Add(2 * time.Minute) // past expiry, still inside cacheTTL
	if ok, _ := s.AuthenticateSession(sess); ok {
		t.Fatal("expired but served from cache")
	}
}

func TestDisableDropsSessions(t *testing.T) {
	s, _, _ := newTestService(t)
	sess, _ := s.Setup("admin", "correct horse", "", "")
	s.Disable("correct horse")
	s.Enable("correct horse", "", "")
	if ok, _ := s.AuthenticateSession(sess); ok {
		t.Fatal("cookie from the previous period valid again")
	}
}

func TestResetWithoutDBKeepsDBMarkerLogic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, markerName), nil, 0600)
	var db *metadb.DB
	if err := Reset(db, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); !os.IsNotExist(err) {
		t.Fatal("marker")
	}
}

func TestResetAtUnreadableDBRemovesMarker(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, markerName), nil, 0600)
	bad := filepath.Join(dir, "tiramisu.db")
	os.WriteFile(bad, []byte("not a sqlite database, definitely not"), 0600)
	err := ResetAt(bad, dir)
	if err == nil || !errors.Is(err, ErrDBUntouched) {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); !os.IsNotExist(err) {
		t.Fatal("marker left behind: 503 forever")
	}
}

func TestResetAtMissingDBDoesNotCreateOne(t *testing.T) {
	dir := t.TempDir()
	if err := ResetAt(filepath.Join(dir, "tiramisu.db"), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tiramisu.db")); !os.IsNotExist(err) {
		t.Fatal("DB created")
	}
}
