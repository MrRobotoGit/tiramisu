package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"tiramisu/internal/metadb"
)

func newTestService(t *testing.T) (*Service, *metadb.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := metadb.New(filepath.Join(dir, "t.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, dir, false), db, dir
}

func TestFreshInstallIsOpen(t *testing.T) {
	s, _, _ := newTestService(t)
	st := s.State()
	if st.Enabled || st.Configured || !st.Available || st.FailClosed {
		t.Fatalf("%+v", st)
	}
}

func TestSetupEnablesAndWritesMarker(t *testing.T) {
	s, _, dir := newTestService(t)
	sess, err := s.Setup("admin", "correct horse", "ua", "1.1.1.1")
	if err != nil || sess == "" {
		t.Fatal(err)
	}
	if !s.State().Enabled {
		t.Fatal("not enabled")
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); err != nil {
		t.Fatal("marker missing")
	}
	if _, err := s.Setup("x", "correct horse", "", ""); err != ErrConflict {
		t.Fatalf("second setup: %v", err)
	}
	if ok, _ := s.AuthenticateSession(sess); !ok {
		t.Fatal("setup session not valid")
	}
}

func TestLoginLogoutDisableEnable(t *testing.T) {
	s, _, dir := newTestService(t)
	s.Setup("admin", "correct horse", "", "")
	if _, err := s.Login("admin", "wrong password", "", ""); err != ErrBadCredentials {
		t.Fatal("wrong password accepted")
	}
	if _, err := s.Login("nobody", "correct horse", "", ""); err != ErrBadCredentials {
		t.Fatal("wrong user accepted")
	}
	sess, err := s.Login("admin", "correct horse", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.Logout(sess)
	if ok, _ := s.AuthenticateSession(sess); ok {
		t.Fatal("session valid after logout (cache not cleared?)")
	}
	if err := s.Disable("wrong password"); err != ErrBadCredentials {
		t.Fatal("disable without password")
	}
	if err := s.Disable("correct horse"); err != nil || s.State().Enabled {
		t.Fatal("disable")
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); !os.IsNotExist(err) {
		t.Fatal("marker left behind")
	}
	if _, err := s.Setup("admin", "correct horse", "", ""); err != ErrConflict {
		t.Fatal("setup with an existing user")
	}
	if _, err := s.Enable("correct horse", "", ""); err != nil || !s.State().Enabled {
		t.Fatal("enable")
	}
}

func TestSessionExpiresAndSlides(t *testing.T) {
	s, db, _ := newTestService(t)
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }
	sess, _ := s.Setup("admin", "correct horse", "", "")
	now = now.Add(2 * time.Hour)
	s.clearCache()
	ok, renewed := s.AuthenticateSession(sess)
	if !ok || !renewed {
		t.Fatal("renewal expected after 2h")
	}
	got, _, _ := db.GetAuthSession(hashSecret(sess))
	if got.ExpiresAt != now.Add(sessionTTL).Unix() {
		t.Fatal("expiry not moved forward")
	}
	now = now.Add(sessionTTL + time.Second)
	s.clearCache()
	if ok, _ := s.AuthenticateSession(sess); ok {
		t.Fatal("expired session accepted")
	}
}

func TestPasswordChangeKeepsOnlyCurrentSession(t *testing.T) {
	s, _, _ := newTestService(t)
	a, _ := s.Setup("admin", "correct horse", "", "")
	b, _ := s.Login("admin", "correct horse", "", "")
	if err := s.ChangePassword(a, "correct horse", "battery staple"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AuthenticateSession(a); !ok {
		t.Fatal("current session lost")
	}
	if ok, _ := s.AuthenticateSession(b); ok {
		t.Fatal("other session still valid")
	}
}

func TestFailClosedWithoutDB(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, markerName), nil, 0600)
	s := New(nil, dir, false)
	if st := s.State(); !st.FailClosed || st.Available {
		t.Fatalf("%+v", st)
	}
	if s := New(nil, t.TempDir(), false); s.State().FailClosed || s.State().Enabled {
		t.Fatal("without a marker it must stay open")
	}
}

func TestResetPendingBlocksSetup(t *testing.T) {
	dir := t.TempDir()
	db, _ := metadb.New(filepath.Join(dir, "t.db"), nil)
	defer db.Close()
	s := New(db, dir, true)
	if _, err := s.Setup("admin", "correct horse", "", ""); err != ErrResetPending {
		t.Fatal(err)
	}
}

func TestLoginNeedsUsername(t *testing.T) {
	s, _, _ := newTestService(t)
	s.Setup("admin", "correct horse", "", "")
	if _, err := s.Login("", "correct horse", "", ""); err != ErrBadCredentials {
		t.Fatal("login without username accepted")
	}
}

func TestSessionNotRenewedOnEveryRequest(t *testing.T) {
	s, db, _ := newTestService(t)
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }
	sess, _ := s.Setup("admin", "correct horse", "", "")
	for i := 0; i < 3; i++ {
		now = now.Add(time.Minute)
		if ok, renewed := s.AuthenticateSession(sess); !ok || renewed {
			t.Fatalf("request %d: ok=%v renewed=%v", i, ok, renewed)
		}
	}
	got, _, _ := db.GetAuthSession(hashSecret(sess))
	if got.LastSeen != 1_000_000 {
		t.Fatal("session rewritten before an hour")
	}
}
