package webtls

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func nolog(string, ...any) {}

func fakeHost(t *testing.T, name string, container bool, ips ...string) {
	stableIPv4 = func() []string { return ips }
	hostname = func() (string, error) { return name, nil }
	inContainer = func() bool { return container }
	t.Cleanup(func() {
		stableIPv4, hostname = interfaceIPv4, os.Hostname
		inContainer = func() bool { _, err := os.Stat("/.dockerenv"); return err == nil }
	})
}

func TestHostsAndRequired(t *testing.T) {
	fakeHost(t, "tiramisu", false, "192.168.1.20")
	got := Hosts([]string{" 10.0.0.5 ", "", "nas.lan"})
	want := []string{"tiramisu", "tiramisu.local", "localhost", "127.0.0.1", "::1", "192.168.1.20", "10.0.0.5", "nas.lan"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if r := Required(nil); !slices.Equal(r, []string{"192.168.1.20"}) {
		t.Fatalf("without extras: %v", r)
	}
	if r := Required([]string{" nas.lan ", ""}); !slices.Equal(r, []string{"nas.lan", "192.168.1.20"}) {
		t.Fatalf("outside a container both extras and IPv4 count: %v", r)
	}
	fakeHost(t, "c0ffee", true, "172.17.0.3")
	if r := Required([]string{"192.168.1.20"}); !slices.Equal(r, []string{"192.168.1.20"}) {
		t.Fatalf("in a container only the extras: %v", r)
	}
}

func TestInterfaceIPv4OnlyStable(t *testing.T) {
	for _, s := range interfaceIPv4() {
		ip := net.ParseIP(s)
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			t.Fatalf("address not allowed in the SAN: %s", s)
		}
	}
}

func testCA(t *testing.T, now time.Time) authority {
	t.Helper()
	c, k, err := generateCA("tiramisu", now)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	return authority{pair.Leaf, pair.PrivateKey.(*ecdsa.PrivateKey)}
}

func TestCAAndLeaf(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	ca := testCA(t, now)
	if !ca.cert.IsCA || ca.cert.KeyUsage&x509.KeyUsageCertSign == 0 || !ca.cert.NotAfter.Equal(now.Add(caValidity)) {
		t.Fatalf("CA: IsCA=%v usage=%v NotAfter=%v", ca.cert.IsCA, ca.cert.KeyUsage, ca.cert.NotAfter)
	}
	c, k, err := generateLeaf(ca, []string{"tiramisu", "127.0.0.1", "::1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := tls.X509KeyPair(c, k)
	leaf := pair.Leaf
	if leaf.IsCA || !leaf.NotAfter.Equal(now.Add(leafValidity)) {
		t.Fatalf("leaf: IsCA=%v NotAfter=%v", leaf.IsCA, leaf.NotAfter)
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Fatal("key usages missing (iOS requires them)")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "tiramisu", CurrentTime: now}); err != nil {
		t.Fatalf("the leaf does not verify with the CA as root: %v", err)
	}
}

func TestNeedsRenewal(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	ca := testCA(t, now)
	c, k, _ := generateLeaf(ca, []string{"tiramisu", "192.168.1.20"}, now)
	pair, _ := tls.X509KeyPair(c, k)
	leaf := pair.Leaf
	if needsRenewal(leaf, ca.cert, []string{"192.168.1.20"}, now) {
		t.Fatal("just generated")
	}
	if !needsRenewal(leaf, ca.cert, nil, now.Add(336*24*time.Hour)) {
		t.Fatal("29 days before expiry it must be renewed")
	}
	if !needsRenewal(leaf, ca.cert, []string{"192.168.1.9"}, now) {
		t.Fatal("required IP changed: must be renewed")
	}
	if !needsRenewal(leaf, testCA(t, now).cert, nil, now) {
		t.Fatal("signed by another CA: must be renewed")
	}
}

func TestEnsureLocal(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	hosts, req := []string{"tiramisu", "127.0.0.1"}, []string{"127.0.0.1"}
	a, caA, caNew, err := ensureLocal(dir, hosts, req, now, nolog)
	if err != nil || !caNew {
		t.Fatalf("first generation: %v %v", err, caNew)
	}
	for _, f := range []string{"ca.key", "key.pem"} {
		st, _ := os.Stat(filepath.Join(dir, f))
		if st.Mode().Perm() != 0600 {
			t.Fatalf("permissions %s %v", f, st.Mode().Perm())
		}
	}
	if d, _ := os.Stat(dir); d.Mode().Perm() != 0700 {
		t.Fatalf("dir permissions %v", d.Mode().Perm())
	}
	b, caB, caNew, err := ensureLocal(dir, hosts, req, now.Add(time.Hour), nolog)
	if err != nil || caNew || string(a.Certificate[0]) != string(b.Certificate[0]) {
		t.Fatal("the second start must reuse everything")
	}
	// IP changed: new leaf, same CA. Clients need to do nothing.
	c, caC, caNew, err := ensureLocal(dir, []string{"tiramisu", "192.168.1.9"}, []string{"192.168.1.9"}, now, nolog)
	if err != nil || caNew || string(c.Certificate[0]) == string(b.Certificate[0]) || string(caC) != string(caB) || string(caA) != string(caB) {
		t.Fatal("IP change: new leaf, CA unchanged")
	}
	// Broken leaf: regenerated with the same CA.
	os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("broken"), 0600)
	if _, caD, caNew, err := ensureLocal(dir, hosts, req, now, nolog); err != nil || caNew || string(caD) != string(caB) {
		t.Fatal("broken leaf: regenerated, CA unchanged")
	}
	// Broken CA: new CA.
	os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("broken"), 0600)
	if _, caE, caNew, err := ensureLocal(dir, hosts, req, now, nolog); err != nil || !caNew || string(caE) == string(caB) {
		t.Fatal("broken CA: must be regenerated")
	}
}

func TestEnsureLocalReadOnlyKeepsValidLeaf(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	a, _, _, err := ensureLocal(dir, []string{"tiramisu", "127.0.0.1"}, []string{"127.0.0.1"}, now, nolog)
	if err != nil {
		t.Fatal(err)
	}
	writeFile = func(string, []byte) error { return os.ErrPermission } // read-only STATE
	t.Cleanup(func() { writeFile = writeAtomic })
	var warned bool
	b, _, _, err := ensureLocal(dir, []string{"tiramisu", "10.0.0.9"}, []string{"10.0.0.9"}, now, func(string, ...any) { warned = true })
	if err != nil || !warned || string(a.Certificate[0]) != string(b.Certificate[0]) {
		t.Fatalf("read-only STATE: the valid certificate must stay (%v, warned=%v)", err, warned)
	}
	os.Remove(filepath.Join(dir, "cert.pem"))
	if _, _, _, err := ensureLocal(dir, []string{"tiramisu"}, nil, now, nolog); err == nil {
		t.Fatal("without a certificate to keep, the error must be returned")
	}
}
