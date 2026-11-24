package webtls

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func leafPEM(t *testing.T, hosts []string, now time.Time) ([]byte, []byte, error) {
	t.Helper()
	return generateLeaf(testCA(t, now), hosts, now)
}

func ownPair(t *testing.T, dir, name string, hosts []string, now time.Time) (string, string) {
	t.Helper()
	c, k, err := leafPEM(t, hosts, now)
	if err != nil {
		t.Fatal(err)
	}
	cp, kp := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	os.WriteFile(cp, c, 0600)
	os.WriteFile(kp, k, 0600)
	return cp, kp
}

func TestStoreSelfSigned(t *testing.T) {
	s, err := newStore(Settings{}, t.TempDir(), func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	in := s.Info()
	if in.Source != "self-signed" || s.OwnFiles() || s.CADER() == nil || in.Fingerprint != fingerprint(s.CADER()) || in.CANotAfter.IsZero() {
		t.Fatalf("the fingerprint shown must be the CA's: %+v", in)
	}
}

func TestStoreReloadOnKeyOnlyChange(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	cp, kp := ownPair(t, dir, "a", []string{"a.lan"}, now)
	s, err := newStore(Settings{CertFile: cp, KeyFile: kp}, filepath.Join(dir, "state"), nolog)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Minute)
	os.Chtimes(kp, later, later) // only the key's mtime changes
	s.now = func() time.Time { return later }
	before := s.mtime
	s.GetCertificate(nil)
	if s.mtime == before {
		t.Fatal("the key's mtime must count for the reload")
	}
}

func TestStoreOneFieldLogged(t *testing.T) {
	var logged bool
	s, err := newStore(Settings{CertFile: "/x.pem"}, t.TempDir(), func(string, ...any) { logged = true })
	if err != nil || s.OwnFiles() || !logged {
		t.Fatalf("only one field: self-signed and a log (%v, %v)", err, logged)
	}
}

func TestStoreOwnFilesAndReload(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	cp, kp := ownPair(t, dir, "a", []string{"a.lan"}, now)
	s, err := newStore(Settings{CertFile: cp, KeyFile: kp}, filepath.Join(dir, "state"), func(string, ...any) {})
	if err != nil || !s.OwnFiles() || s.Info().SAN[0] != "a.lan" {
		t.Fatalf("%v %+v", err, s.Info())
	}
	// Replace the files and move the mtime forward: the reload must see it.
	c2, k2, _ := leafPEM(t, []string{"b.lan"}, now)
	os.WriteFile(cp, c2, 0600)
	os.WriteFile(kp, k2, 0600)
	later := now.Add(2 * time.Minute)
	os.Chtimes(cp, later, later)
	s.now = func() time.Time { return later }
	if _, err := s.GetCertificate(nil); err != nil {
		t.Fatal(err)
	}
	if s.Info().SAN[0] != "b.lan" {
		t.Fatalf("not reloaded: %v", s.Info().SAN)
	}
	// File broken while running: the previous one stays.
	os.WriteFile(cp, []byte("broken"), 0600)
	evenLater := later.Add(2 * time.Minute)
	os.Chtimes(cp, evenLater, evenLater)
	s.now = func() time.Time { return evenLater }
	s.GetCertificate(nil)
	if s.Info().SAN[0] != "b.lan" {
		t.Fatal("a broken file must not replace the good certificate")
	}
}

func TestStoreOwnFilesBrokenFallsBack(t *testing.T) {
	dir := t.TempDir()
	var logged string
	s, err := newStore(Settings{CertFile: filepath.Join(dir, "nope.crt"), KeyFile: filepath.Join(dir, "nope.key")},
		filepath.Join(dir, "state"), func(f string, a ...any) { logged = f })
	if err != nil || s.OwnFiles() || s.Info().Source != "self-signed" {
		t.Fatalf("%v %+v", err, s.Info())
	}
	if logged == "" {
		t.Fatal("the fallback must be logged")
	}
}
