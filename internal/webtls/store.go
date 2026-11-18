package webtls

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"tiramisu/internal/config"
)

// Settings is the https block of config.json.
type Settings = config.HTTPSConfig

const reloadEvery = 60 * time.Second

type loaded struct {
	pair  *tls.Certificate
	caDER []byte // the local CA, nil with the user's own files
	info  Info
}

type store struct {
	cur      atomic.Pointer[loaded]
	own      bool
	certFile string
	keyFile  string
	logf     func(string, ...any)
	now      func() time.Time

	mu        sync.Mutex
	lastCheck time.Time
	mtime     [2]time.Time // cert, key
}

func newStore(st Settings, stateDir string, logf func(string, ...any)) (*store, error) {
	s := &store{certFile: st.CertFile, keyFile: st.KeyFile, logf: logf, now: time.Now}
	if (st.CertFile == "") != (st.KeyFile == "") {
		logf("[TLS] ERROR cert_file and key_file go together, only one is set — using the self-signed certificate")
	}
	if st.CertFile != "" && st.KeyFile != "" {
		if err := s.loadOwn(); err == nil {
			s.own = true
			return s, nil
		} else {
			logf("[TLS] ERROR cannot load %s / %s: %v — using the self-signed certificate", st.CertFile, st.KeyFile, err)
		}
	}
	dir := filepath.Join(stateDir, "tls")
	pair, caDER, caNew, err := ensureLocal(dir, Hosts(st.ExtraHostnames), Required(st.ExtraHostnames), s.now(), logf)
	if err != nil {
		return nil, err
	}
	if caNew {
		logf("[TLS] new local CA in %s: trust ca.pem once on each client (Control Panel › Access › HTTPS)", dir)
	}
	s.cur.Store(&loaded{&pair, caDER, infoOf(&pair, caDER, "self-signed", filepath.Join(dir, "cert.pem"))})
	return s, nil
}

func (s *store) mtimes() ([2]time.Time, error) {
	var m [2]time.Time
	for i, f := range []string{s.certFile, s.keyFile} {
		fi, err := os.Stat(f)
		if err != nil {
			return m, err
		}
		m[i] = fi.ModTime()
	}
	return m, nil
}

func (s *store) loadOwn() error {
	m, err := s.mtimes()
	if err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return err
	}
	s.mtime = m
	s.cur.Store(&loaded{&pair, nil, infoOf(&pair, nil, "file", s.certFile)})
	return nil
}

// GetCertificate reloads the user's files at most once a minute, when their mtime changes.
func (s *store) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if s.own {
		s.mu.Lock()
		if now := s.now(); now.Sub(s.lastCheck) >= reloadEvery {
			s.lastCheck = now
			if m, err := s.mtimes(); err == nil && m != s.mtime {
				if err := s.loadOwn(); err != nil {
					s.logf("[TLS] ERROR reloading %s: %v — keeping the previous certificate", s.certFile, err)
				} else {
					s.logf("[TLS] certificate reloaded from %s", s.certFile)
				}
			}
		}
		s.mu.Unlock()
	}
	return s.cur.Load().pair, nil
}

func (s *store) Info() Info     { return s.cur.Load().info }
func (s *store) CADER() []byte  { return s.cur.Load().caDER }
func (s *store) OwnFiles() bool { return s.own }
