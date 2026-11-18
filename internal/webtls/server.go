package webtls

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

type Options struct {
	Settings  Settings // at start-up
	StateDir  string
	AccessOn  bool                 // sign-in state at start-up (on or fail-closed)
	Trusted   []netip.Prefix       // reverse proxies: plain HTTP from them passes
	Saved     func() Settings      // the https block in config.json now
	AccessNow func() bool          // sign-in state now
	OnBearer  func(tok, ip string) // token sent in clear from another host
	Logf      func(string, ...any)
}

type Server struct {
	o        Options
	store    *store // nil when HTTPS is off or could not start
	startErr error
}

// New prepares the certificate; it does not listen. If HTTPS cannot start, the server
// stays plain and StartErr says why: :9080 must never become unreachable.
func New(o Options) *Server {
	s := &Server{o: o}
	if !o.Settings.Enabled {
		return s
	}
	st, err := newStore(o.Settings, o.StateDir, o.Logf)
	if err != nil {
		s.startErr = err
		o.Logf("[TLS] ERROR %v — serving plain HTTP only", err)
		return s
	}
	s.store = st
	return s
}

func (s *Server) Running() bool   { return s.store != nil }
func (s *Server) StartErr() error { return s.startErr }

// OwnCertificate: HSTS is allowed only with the user's own certificate.
func (s *Server) OwnCertificate() bool { return s.store != nil && s.store.OwnFiles() }

// CertPEM is the local CA, the one certificate clients trust; nil with the user's own files.
func (s *Server) CertPEM() []byte {
	if s.store == nil || s.store.CADER() == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.store.CADER()})
}

// ListenAndServe on addr. With HTTPS off it is the plain server of before, unchanged.
func (s *Server) ListenAndServe(addr string, h http.Handler) error {
	if s.store == nil {
		return http.ListenAndServe(addr, h)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.o.Logf("[TLS] HTTPS on %s (%s)", addr, map[bool]string{true: "own certificate", false: "local CA"}[s.OwnCertificate()])
	return s.Serve(ln, h, nil)
}

// Serve runs the two servers on ln; plainWrap defaults to PlainPolicy.
func (s *Server) Serve(ln net.Listener, h http.Handler, plainWrap func(http.Handler) http.Handler) error {
	if s.store == nil {
		return http.Serve(ln, h)
	}
	if plainWrap == nil {
		plainWrap = func(n http.Handler) http.Handler { return PlainPolicy(n, s.o.AccessOn, s.o.Trusted, s.o.OnBearer) }
	}
	tl, pl := split(ln, 10*time.Second, s.o.Logf)
	errLog := log.New(handshakeFilter{s.o.Logf}, "", 0)
	ts := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ErrorLog: errLog,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.store.GetCertificate}}
	ps := &http.Server{Handler: plainWrap(h), ReadHeaderTimeout: 10 * time.Second, ErrorLog: errLog}
	// One port: when either server stops, the other stops too, and a closed listener is
	// the normal end, not an error.
	errc := make(chan error, 2)
	go func() { errc <- ps.Serve(pl) }()
	go func() { errc <- ts.ServeTLS(tl, "", "") }()
	err := <-errc
	ts.Close()
	ps.Close()
	<-errc
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// handshakeFilter drops "TLS handshake error" lines: every browser that has not trusted
// the self-signed certificate yet produces one per visit.
type handshakeFilter struct{ logf func(string, ...any) }

func (f handshakeFilter) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte("TLS handshake error")) {
		f.logf("[TLS] %s", strings.TrimSpace(string(p)))
	}
	return len(p), nil
}

// Register adds the state endpoints. They live under /api/auth/, so API tokens never reach them.
func (s *Server) Register(mux *http.ServeMux, cleartext func() any) {
	mux.HandleFunc("GET /api/auth/tls", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{"running": s.Running(), "access_on_at_start": s.o.AccessOn}
		reason := ""
		switch {
		case !s.o.Saved().Equal(s.o.Settings):
			reason = "the https settings changed"
		case s.Running() && s.o.AccessNow() != s.o.AccessOn:
			reason = "sign-in was turned " + map[bool]string{true: "on", false: "off"}[s.o.AccessNow()]
		}
		out["restart_required"], out["restart_reason"] = reason != "", reason
		if s.startErr != nil {
			out["start_error"] = s.startErr.Error()
		}
		if s.store != nil {
			out["certificate"] = s.store.Info()
			out["cleartext_uses"] = cleartext()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /api/auth/tls/cert", func(w http.ResponseWriter, r *http.Request) {
		pemb := s.CertPEM()
		if pemb == nil {
			http.NotFound(w, r)
			return
		}
		// The CA type: iOS offers to install it as a profile; with a generic PEM type it only saves a file.
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="tiramisu-ca.crt"`)
		w.Write(pemb)
	})
}
