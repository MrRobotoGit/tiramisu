package auth

import (
	"crypto/subtle"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"tiramisu/internal/metadb"
)

const (
	markerName   = "auth.enabled"
	sessionTTL   = 30 * 24 * time.Hour
	sessionTouch = time.Hour
	tokenTouch   = time.Minute
	cacheTTL     = 5 * time.Minute
	cachePrune   = 1024
)

var (
	ErrBadCredentials = errors.New("wrong username or password")
	ErrConflict       = errors.New("conflict")
	ErrUnavailable    = errors.New("state database unavailable")
	ErrResetPending   = errors.New("TIRAMISU_RESET_AUTH is set: remove it and restart")
	ErrNotFound       = errors.New("not found")
	ErrBadUsername    = errors.New("username must be 1-64 characters, no surrounding spaces")
	ErrBadName        = errors.New("name must be 1-64 characters")
	ErrBadScope       = errors.New(`scope must be "mcp" or "api"`)
)

// bcryptSlots bounds concurrent bcrypt work: each hash costs a Pi 4 core for ~0.3 s.
var bcryptSlots = make(chan struct{}, 2)

func withBcrypt[T any](f func() T) T {
	bcryptSlots <- struct{}{}
	defer func() { <-bcryptSlots }()
	return f()
}

// Store is the subset of *metadb.DB the service uses.
type Store interface {
	GetAuthUser() (metadb.AuthUser, bool, error)
	CreateAuthUser(username, passwordHash string) error
	SetAuthEnabled(on bool) error
	SetAuthPassword(passwordHash string) error
	CreateAuthSession(s metadb.AuthSession) error
	GetAuthSession(idHash string) (metadb.AuthSession, bool, error)
	TouchAuthSession(idHash string, lastSeen, expiresAt int64) (bool, error)
	DeleteAuthSession(idHash string) error
	DeleteAuthSessionsExcept(idHash string) error
	PurgeExpiredAuthSessions(now int64) error
	ListAuthSessions() ([]metadb.AuthSession, error)
	CreateAuthToken(name, prefix, tokenHash, scope string, createdAt int64) (int64, error)
	GetAuthTokenByHash(tokenHash string) (metadb.AuthToken, bool, error)
	TouchAuthToken(id, lastUsed int64, ip string) error
	DeleteAuthToken(id int64) (bool, error)
	ListAuthTokens() ([]metadb.AuthToken, error)
	ResetAuth() error
}

type State struct {
	Enabled      bool
	Configured   bool
	Available    bool
	FailClosed   bool
	ResetPending bool
	Username     string
}

// cacheEntry is a secret already validated against the DB. Keys carry the kind
// ("s:"/"t:"), so a session value never passes as a token or the reverse.
type cacheEntry struct {
	at        time.Time
	expiresAt int64 // sessions only
	tokenID   int64  // tokens only
	scope     string // tokens only
	lastTouch time.Time
}

// Token scopes: an MCP token opens /mcp and nothing else, an API token the HTTP API
// and not /mcp. Neither manages tokens or the sign-in: that needs a browser session.
const (
	ScopeMCP = "mcp"
	ScopeAPI = "api"
)

type Service struct {
	store         Store
	marker        string
	internalToken string
	resetPending  bool
	failClosed    bool
	enabled       atomic.Bool
	now           func() time.Time
	logf          func(string, ...any)

	mu    sync.Mutex
	cache map[string]*cacheEntry
	gen   uint64 // bumped by every invalidation; a DB read older than it is not cached

	opMu sync.Mutex // serialises setup/enable/disable/password
}

// New builds the service. store may be nil (StateDB off or failed).
func New(store Store, stateDir string, resetPending bool) *Service {
	if db, ok := store.(*metadb.DB); ok && db == nil {
		store = nil // typed nil from a failed StateDB
	}
	s := &Service{
		store:         store,
		marker:        filepath.Join(stateDir, markerName),
		internalToken: randomString(),
		resetPending:  resetPending,
		now:           time.Now,
		logf:          func(string, ...any) {},
		cache:         map[string]*cacheEntry{},
	}
	_, markerErr := os.Stat(s.marker)
	markerPresent := markerErr == nil
	if markerErr != nil && !os.IsNotExist(markerErr) {
		markerPresent = true // unreadable marker: assume sign-in was on
	}
	if store == nil {
		s.failClosed = markerPresent
		return s
	}
	u, ok, err := store.GetAuthUser()
	switch {
	case err != nil:
		s.failClosed = markerPresent
		s.store = nil
	case !ok && markerPresent:
		// The user vanished under a live marker (DB recreated or replaced): only Reset may open.
		s.failClosed = true
	default:
		s.enabled.Store(ok && u.Enabled)
		if err := s.syncMarker(); err != nil && s.enabled.Load() {
			s.failClosed = true
		}
		_ = store.PurgeExpiredAuthSessions(s.now().Unix())
	}
	return s
}

// SetLogger routes the service's diagnostics (marker errors, 500 details).
func (s *Service) SetLogger(f func(string, ...any)) {
	if f != nil {
		s.logf = f
	}
}

func (s *Service) syncMarker() error {
	if s.enabled.Load() {
		return os.WriteFile(s.marker, nil, 0600)
	}
	if err := os.Remove(s.marker); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Service) State() State {
	st := State{Enabled: s.enabled.Load(), Available: s.store != nil && !s.failClosed, FailClosed: s.failClosed, ResetPending: s.resetPending}
	if s.store != nil {
		if u, ok, _ := s.store.GetAuthUser(); ok {
			st.Configured, st.Username = true, u.Username
		}
	}
	return st
}

func (s *Service) Enabled() bool         { return s.enabled.Load() }
func (s *Service) FailClosed() bool      { return s.failClosed }
func (s *Service) InternalToken() string { return s.internalToken }

func (s *Service) IsInternal(tok string) bool {
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.internalToken)) == 1
}

func (s *Service) clearCache() {
	s.mu.Lock()
	s.cache = map[string]*cacheEntry{}
	s.gen++
	s.mu.Unlock()
}

// lookup returns a fresh cache entry, or nil plus the generation to pass to store.
func (s *Service) lookup(key string, now time.Time) (*cacheEntry, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.cache[key]; ok && now.Sub(e.at) <= cacheTTL {
		return e, 0
	}
	return nil, s.gen
}

func (s *Service) store_(key string, e *cacheEntry, gen uint64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen {
		return // invalidated while the DB was read: do not resurrect a revoked secret
	}
	if len(s.cache) >= cachePrune {
		for k, v := range s.cache {
			if now.Sub(v.at) > cacheTTL {
				delete(s.cache, k)
			}
		}
	}
	s.cache[key] = e
}

func (s *Service) forget(key string) {
	s.mu.Lock()
	delete(s.cache, key)
	s.mu.Unlock()
}

// claimTouch reports whether this caller should write the touch; it marks it at once
// so concurrent requests do not all write.
func (s *Service) claimTouch(e *cacheEntry, every time.Duration, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(e.lastTouch) < every {
		return false
	}
	e.lastTouch = now
	return true
}

func (s *Service) newSession(ua, ip string) (string, error) {
	tok := randomString()
	now := s.now().Unix()
	err := s.store.CreateAuthSession(metadb.AuthSession{IDHash: hashSecret(tok), CreatedAt: now, LastSeen: now,
		ExpiresAt: now + int64(sessionTTL/time.Second), UserAgent: truncate(ua, 200), IP: ip})
	return tok, err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Service) guard() error {
	if s.store == nil || s.failClosed {
		return ErrUnavailable
	}
	if s.resetPending {
		return ErrResetPending
	}
	return nil
}

func validUsername(u string) bool {
	if u == "" || utf8.RuneCountInString(u) > 64 || strings.TrimSpace(u) != u {
		return false
	}
	for _, r := range u {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Setup creates the single user and turns sign-in on. Only when no user exists.
func (s *Service) Setup(username, password, ua, ip string) (string, error) {
	if err := s.guard(); err != nil {
		return "", err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if !validUsername(username) {
		return "", ErrBadUsername
	}
	if _, exists, err := s.store.GetAuthUser(); err != nil {
		return "", err
	} else if exists {
		return "", ErrConflict // before bcrypt: an open setup endpoint must not burn CPU
	}
	h, err := withBcrypt(func() hashResult { v, e := hashPassword(password); return hashResult{v, e} }).get()
	if err != nil {
		return "", err
	}
	if err := s.store.CreateAuthUser(username, h); err == metadb.ErrAuthUserExists {
		return "", ErrConflict
	} else if err != nil {
		return "", err
	}
	return s.turnOn(ua, ip)
}

func (s *Service) turnOn(ua, ip string) (string, error) {
	// Sessions left from an earlier on-period must not come back with it.
	if err := s.store.DeleteAuthSessionsExcept(""); err != nil {
		return "", err
	}
	if err := s.store.SetAuthEnabled(true); err != nil {
		return "", err
	}
	s.enabled.Store(true)
	if err := s.syncMarker(); err != nil {
		// Without the marker a later DB failure would reopen access: refuse to turn on.
		s.enabled.Store(false)
		_ = s.store.SetAuthEnabled(false)
		s.logf("[Auth] cannot write %s: %v", s.marker, err)
		return "", err
	}
	s.clearCache()
	return s.newSession(ua, ip)
}

// verify checks the password, and the username too when checkUser is set.
func (s *Service) verify(checkUser bool, username, password string) (bool, error) {
	u, ok, err := s.store.GetAuthUser()
	if err != nil {
		return false, err
	}
	if !ok || len(password) > 72 {
		withBcrypt(func() bool { return checkPassword(string(dummyHash()), password) })
		return false, nil
	}
	pwOK := withBcrypt(func() bool { return checkPassword(u.PasswordHash, password) })
	userOK := !checkUser || subtle.ConstantTimeCompare([]byte(username), []byte(u.Username)) == 1
	return pwOK && userOK, nil
}

func (s *Service) mustVerify(checkUser bool, username, password string) error {
	ok, err := s.verify(checkUser, username, password)
	if err != nil {
		return err
	}
	if !ok {
		return ErrBadCredentials
	}
	return nil
}

// Enable turns sign-in back on for the existing user.
func (s *Service) Enable(password, ua, ip string) (string, error) {
	if err := s.guard(); err != nil {
		return "", err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.enabled.Load() {
		return "", ErrConflict
	}
	if err := s.mustVerify(false, "", password); err != nil {
		return "", err
	}
	return s.turnOn(ua, ip)
}

func (s *Service) Disable(password string) error {
	if s.store == nil || s.failClosed {
		return ErrUnavailable
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.mustVerify(false, "", password); err != nil {
		return err
	}
	if err := s.store.SetAuthEnabled(false); err != nil {
		return err
	}
	s.enabled.Store(false)
	if err := s.syncMarker(); err != nil {
		s.logf("[Auth] cannot remove %s: %v", s.marker, err)
	}
	_ = s.store.DeleteAuthSessionsExcept("")
	s.clearCache()
	return nil
}

func (s *Service) Login(username, password, ua, ip string) (string, error) {
	if s.store == nil || s.failClosed {
		return "", ErrUnavailable
	}
	if username == "" {
		return "", ErrBadCredentials
	}
	if err := s.mustVerify(true, username, password); err != nil {
		return "", err
	}
	return s.newSession(ua, ip)
}

func (s *Service) Logout(session string) {
	if s.store != nil && session != "" {
		_ = s.store.DeleteAuthSession(hashSecret(session))
	}
	s.clearCache()
}

// LogoutOthers keeps only session (all of them when session is empty, e.g. a Bearer caller).
func (s *Service) LogoutOthers(session string) error {
	if s.store == nil {
		return ErrUnavailable
	}
	keep := ""
	if session != "" {
		keep = hashSecret(session)
	}
	err := s.store.DeleteAuthSessionsExcept(keep)
	s.clearCache()
	return err
}

func (s *Service) ChangePassword(session, oldPw, newPw string) error {
	if s.store == nil {
		return ErrUnavailable
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.mustVerify(false, "", oldPw); err != nil {
		return err
	}
	h, err := withBcrypt(func() hashResult { v, e := hashPassword(newPw); return hashResult{v, e} }).get()
	if err != nil {
		return err
	}
	if err := s.store.SetAuthPassword(h); err != nil {
		return err
	}
	return s.LogoutOthers(session)
}

// AuthenticateSession reports whether the cookie value is a live session, and whether
// its expiry slid (the caller then re-issues the cookie).
func (s *Service) AuthenticateSession(session string) (ok, renewed bool) {
	if s.store == nil || session == "" {
		return false, false
	}
	h := hashSecret(session)
	key := "s:" + h
	now := s.now()
	e, gen := s.lookup(key, now)
	if e == nil {
		sess, found, err := s.store.GetAuthSession(h)
		if err != nil || !found || sess.ExpiresAt < now.Unix() {
			return false, false
		}
		e = &cacheEntry{at: now, expiresAt: sess.ExpiresAt, lastTouch: time.Unix(sess.LastSeen, 0)}
		s.store_(key, e, gen, now)
	}
	s.mu.Lock()
	expired := e.expiresAt < now.Unix()
	s.mu.Unlock()
	if expired {
		s.forget(key)
		return false, false
	}
	if !s.claimTouch(e, sessionTouch, now) {
		return true, false
	}
	exp := now.Add(sessionTTL).Unix()
	updated, err := s.store.TouchAuthSession(h, now.Unix(), exp)
	if err != nil {
		return true, false // the session is valid; the slide is retried next hour
	}
	if !updated {
		s.forget(key) // deleted since it was cached
		return false, false
	}
	s.mu.Lock()
	e.expiresAt = exp
	s.mu.Unlock()
	return true, true
}

// AuthenticateToken reports whether tok is a valid API token, and its scope.
func (s *Service) AuthenticateToken(tok, ip string) (scope string, ok bool) {
	if s.store == nil || !strings.HasPrefix(tok, tokenPrefixLiteral) || len(tok) != len(tokenPrefixLiteral)+43 {
		return "", false
	}
	h := hashSecret(tok)
	key := "t:" + h
	now := s.now()
	e, gen := s.lookup(key, now)
	if e == nil {
		t, found, err := s.store.GetAuthTokenByHash(h)
		if err != nil || !found {
			return "", false
		}
		e = &cacheEntry{at: now, tokenID: t.ID, scope: t.Scope, lastTouch: time.Unix(t.LastUsed, 0)}
		s.store_(key, e, gen, now)
	}
	if s.claimTouch(e, tokenTouch, now) {
		_ = s.store.TouchAuthToken(e.tokenID, now.Unix(), ip)
	}
	return e.scope, true
}

func (s *Service) Sessions() ([]metadb.AuthSession, error) {
	if s.store == nil {
		return nil, ErrUnavailable
	}
	_ = s.store.PurgeExpiredAuthSessions(s.now().Unix())
	return s.store.ListAuthSessions()
}

func (s *Service) RevokeSessionByHash(idHash string) error {
	if s.store == nil {
		return ErrUnavailable
	}
	err := s.store.DeleteAuthSession(idHash)
	s.clearCache()
	return err
}

func (s *Service) CreateToken(name, scope string) (int64, string, error) {
	if s.store == nil {
		return 0, "", ErrUnavailable
	}
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return 0, "", ErrBadName
	}
	if scope != ScopeMCP && scope != ScopeAPI {
		return 0, "", ErrBadScope
	}
	tok := newToken()
	id, err := s.store.CreateAuthToken(name, tokenPrefix(tok), hashSecret(tok), scope, s.now().Unix())
	return id, tok, err
}

func (s *Service) Tokens() ([]metadb.AuthToken, error) {
	if s.store == nil {
		return nil, ErrUnavailable
	}
	return s.store.ListAuthTokens()
}

func (s *Service) RevokeToken(id int64) error {
	if s.store == nil {
		return ErrUnavailable
	}
	ok, err := s.store.DeleteAuthToken(id)
	s.clearCache()
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// Reset clears user, sessions and tokens and removes the marker. Used by --reset-auth.
func Reset(store Store, stateDir string) error {
	if db, ok := store.(*metadb.DB); ok && db == nil {
		store = nil
	}
	if store != nil {
		if err := store.ResetAuth(); err != nil {
			return err // keep the marker: the DB still says sign-in is on
		}
	}
	if err := os.Remove(filepath.Join(stateDir, markerName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

type hashResult struct {
	v   string
	err error
}

func (r hashResult) get() (string, error) { return r.v, r.err }
