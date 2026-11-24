package webtls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func startTest(t *testing.T, st Settings, accessOn bool, saved Settings, accessNow bool) (*Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	s := New(Options{Settings: st, StateDir: t.TempDir(), AccessOn: accessOn,
		Saved: func() Settings { return saved }, AccessNow: func() bool { return accessNow },
		Logf: func(string, ...any) {}})
	if s.StartErr() != nil {
		t.Fatal(s.StartErr())
	}
	s.Register(mux, func() any { return []string{} })
	go s.Serve(ln, mux, nil)
	t.Cleanup(func() { ln.Close() })
	return s, ln.Addr().String()
}

func TestServerTLSAndPlainSamePort(t *testing.T) {
	s, addr := startTest(t, Settings{Enabled: true}, true, Settings{Enabled: true}, true)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(s.CertPEM())
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}}}
	r, err := c.Get("https://" + addr + "/api/health")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("https: %v %v", err, r)
	}
	r, err = http.Get("http://" + addr + "/api/health") // cleartext loopback
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("http loopback: %v %v", err, r)
	}
}

func TestStatusEndpoint(t *testing.T) {
	_, addr := startTest(t, Settings{Enabled: true}, false, Settings{Enabled: true}, true)
	r, err := http.Get("http://" + addr + "/api/auth/tls")
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Running         bool   `json:"running"`
		RestartRequired bool   `json:"restart_required"`
		Reason          string `json:"restart_reason"`
		Cert            Info   `json:"certificate"`
	}
	json.NewDecoder(r.Body).Decode(&st)
	if !st.Running || !st.RestartRequired || !strings.Contains(st.Reason, "sign-in") || st.Cert.Fingerprint == "" {
		t.Fatalf("%+v", st)
	}
	r, _ = http.Get("http://" + addr + "/api/auth/tls/cert")
	if ct := r.Header.Get("Content-Type"); ct != "application/x-x509-ca-cert" {
		t.Fatalf("Content-Type %q: iOS installs a profile only for the CA type", ct)
	}
	body, _ := io.ReadAll(r.Body)
	if !strings.HasPrefix(string(body), "-----BEGIN CERTIFICATE-----") || strings.Contains(string(body), "PRIVATE") {
		t.Fatal("only the public certificate may be served")
	}
}

func TestStartErrorNoFalseRestart(t *testing.T) {
	file := t.TempDir() + "/not-a-dir"
	os.WriteFile(file, nil, 0600) // STATE/tls cannot be created
	s := New(Options{Settings: Settings{Enabled: true}, StateDir: file,
		Saved: func() Settings { return Settings{Enabled: true} }, AccessNow: func() bool { return false },
		Logf: func(string, ...any) {}})
	if s.StartErr() == nil || s.Running() {
		t.Fatal("expected an error, server in cleartext")
	}
	mux := http.NewServeMux()
	s.Register(mux, func() any { return nil })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/auth/tls", nil))
	var st map[string]any
	json.NewDecoder(w.Body).Decode(&st)
	if st["restart_required"] != false || st["start_error"] == nil {
		t.Fatalf("%v", st)
	}
}

func TestRestartRequiredOnConfigChange(t *testing.T) {
	_, addr := startTest(t, Settings{}, false, Settings{Enabled: true}, false)
	r, _ := http.Get("http://" + addr + "/api/auth/tls")
	var st map[string]any
	json.NewDecoder(r.Body).Decode(&st)
	if st["running"] != false || st["restart_required"] != true {
		t.Fatalf("%v", st)
	}
}

func TestServeEndsCleanlyOnClose(t *testing.T) {
	s := New(Options{Settings: Settings{Enabled: true}, StateDir: t.TempDir(),
		Saved: func() Settings { return Settings{Enabled: true} }, AccessNow: func() bool { return false }, Logf: nolog})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	done := make(chan error, 1)
	go func() { done <- s.Serve(ln, http.NewServeMux(), nil) }()
	time.Sleep(100 * time.Millisecond)
	ln.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("normal close reported as an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve does not return after the port is closed")
	}
}
