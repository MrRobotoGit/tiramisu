package auth

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"tiramisu/internal/metadb"
)

//go:embed login.html
var loginHTML []byte

type handlers struct {
	s       *Service
	trusted []netip.Prefix
	lim     *loginLimiter
}

// Register mounts /api/auth/* and /login on mux.
func Register(mux *http.ServeMux, s *Service, trusted []netip.Prefix) {
	h := &handlers{s: s, trusted: trusted, lim: newLoginLimiter()}
	mux.HandleFunc("GET /login", h.loginPage)
	mux.HandleFunc("GET /api/auth/status", h.status)
	mux.HandleFunc("POST /api/auth/setup", h.setup)
	mux.HandleFunc("POST /api/auth/enable", h.enable)
	mux.HandleFunc("POST /api/auth/login", h.login)
	mux.HandleFunc("POST /api/auth/logout", h.logout)
	mux.HandleFunc("POST /api/auth/logout-others", h.logoutOthers)
	mux.HandleFunc("POST /api/auth/password", h.password)
	mux.HandleFunc("POST /api/auth/disable", h.disable)
	mux.HandleFunc("GET /api/auth/sessions", h.sessions)
	mux.HandleFunc("DELETE /api/auth/sessions/{id}", h.revokeSession)
	mux.HandleFunc("GET /api/auth/tokens", h.tokens)
	mux.HandleFunc("POST /api/auth/tokens", h.createToken)
	mux.HandleFunc("DELETE /api/auth/tokens/{id}", h.revokeToken)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (h *handlers) fail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrBadCredentials):
		code = http.StatusUnauthorized
	case errors.Is(err, ErrConflict), errors.Is(err, ErrResetPending):
		code = http.StatusConflict
	case errors.Is(err, ErrUnavailable):
		code = http.StatusServiceUnavailable
	case errors.Is(err, ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, ErrWeakPassword), errors.Is(err, ErrLongPassword), errors.Is(err, ErrBadUsername), errors.Is(err, ErrBadName), errors.Is(err, ErrBadScope):
		code = http.StatusBadRequest
	}
	msg := err.Error()
	if code == http.StatusInternalServerError {
		h.s.logf("[Auth] %v", err)
		msg = "internal error"
	}
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 4096)
	return json.NewDecoder(r.Body).Decode(v)
}

// safeNext keeps a post-login redirect on this origin. Browsers drop tabs and
// newlines and read "\\" as "/", so "/\t/evil" or "/\\evil" would leave the site.
func safeNext(n string) string {
	const fallback = "/control"
	for i := 0; i < len(n); i++ {
		if c := n[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return fallback
		}
	}
	if !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") {
		return fallback
	}
	u, err := url.Parse(n)
	// Only canonical paths: browsers normalise "/.//evil" to "//evil", a host.
	if err != nil || u.Scheme != "" || u.Host != "" || cleanPath(u.Path) != u.Path {
		return fallback
	}
	return n
}

func (h *handlers) sessionOK(r *http.Request) bool {
	if !h.s.Enabled() {
		return true
	}
	c, err := sessionCookie(r)
	if err != nil || bearer(r) != "" {
		return false
	}
	ok, _ := h.s.AuthenticateSession(c.Value)
	return ok
}

func (h *handlers) authenticated(r *http.Request) bool {
	if !h.s.Enabled() {
		return true
	}
	if tok := bearer(r); tok != "" {
		if h.s.IsInternal(tok) {
			return true
		}
		_, ok := h.s.AuthenticateToken(tok, clientIP(r, h.trusted))
		return ok
	}
	if c, err := sessionCookie(r); err == nil {
		ok, _ := h.s.AuthenticateSession(c.Value)
		return ok
	}
	return false
}

func (h *handlers) loginPage(w http.ResponseWriter, r *http.Request) {
	if !h.s.Enabled() {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(loginHTML)
}

func (h *handlers) status(w http.ResponseWriter, r *http.Request) {
	st := h.s.State()
	out := map[string]any{"enabled": st.Enabled, "configured": st.Configured, "available": st.Available,
		"fail_closed": st.FailClosed, "reset_pending": st.ResetPending, "authenticated": h.authenticated(r)}
	// The username only to a browser session: a token holder has no use for it, and it
	// would be half of a password guess.
	if out["authenticated"] == true && st.Configured && h.sessionOK(r) {
		out["username"] = st.Username
	}
	writeJSON(w, 200, out)
}

type creds struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Old      string `json:"old"`
	New      string `json:"new"`
	Name     string `json:"name"`
	Scope    string `json:"scope"`
}

func (h *handlers) setup(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r) {
		return
	}
	var c creds
	if decode(r, &c) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	if h.s.Enabled() {
		h.fail(w, ErrConflict)
		return
	}
	sess, err := h.s.Setup(c.Username, c.Password, r.UserAgent(), clientIP(r, h.trusted))
	if err != nil {
		h.fail(w, err)
		return
	}
	setSessionCookie(w, r, sess)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *handlers) enable(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r) {
		return
	}
	var c creds
	if decode(r, &c) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	sess, err := h.s.Enable(c.Password, r.UserAgent(), clientIP(r, h.trusted))
	if err != nil {
		h.fail(w, err)
		return
	}
	setSessionCookie(w, r, sess)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// limited applies the login rate limit to every endpoint that checks the password.
func (h *handlers) limited(w http.ResponseWriter, r *http.Request) bool {
	ip := clientIP(r, h.trusted)
	if h.lim.allow(ip) {
		return false
	}
	secs := int(h.lim.retryAfter(ip)/time.Second) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many attempts", "retry_after": secs})
	return true
}

func (h *handlers) login(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r) {
		return
	}
	ip := clientIP(r, h.trusted)
	if !h.s.Enabled() {
		h.fail(w, ErrConflict)
		return
	}
	var c creds
	if decode(r, &c) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	sess, err := h.s.Login(c.Username, c.Password, r.UserAgent(), ip)
	if err != nil {
		h.fail(w, err)
		return
	}
	setSessionCookie(w, r, sess)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *handlers) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := sessionCookie(r); err == nil {
		h.s.Logout(c.Value)
	}
	clearSessionCookie(w)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *handlers) logoutOthers(w http.ResponseWriter, r *http.Request) {
	if err := h.s.LogoutOthers(SessionFrom(r)); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *handlers) password(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r) {
		return
	}
	var c creds
	if decode(r, &c) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	if err := h.s.ChangePassword(SessionFrom(r), c.Old, c.New); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *handlers) disable(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r) {
		return
	}
	var c creds
	if decode(r, &c) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	if err := h.s.Disable(c.Password); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *handlers) sessions(w http.ResponseWriter, r *http.Request) {
	list, err := h.s.Sessions()
	if err != nil {
		h.fail(w, err)
		return
	}
	cur := ""
	if v := SessionFrom(r); v != "" {
		cur = hashSecret(v)
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		out = append(out, map[string]any{"id": x.IDHash[:16], "created_at": x.CreatedAt, "last_seen": x.LastSeen,
			"expires_at": x.ExpiresAt, "user_agent": x.UserAgent, "ip": x.IP, "current": x.IDHash == cur})
	}
	writeJSON(w, 200, out)
}

func (h *handlers) revokeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	list, err := h.s.Sessions()
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, x := range list {
		if len(id) == 16 && strings.HasPrefix(x.IDHash, id) {
			if err := h.s.RevokeSessionByHash(x.IDHash); err != nil {
				h.fail(w, err)
				return
			}
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
	}
	h.fail(w, ErrNotFound)
}

func (h *handlers) tokens(w http.ResponseWriter, r *http.Request) {
	list, err := h.s.Tokens()
	if err != nil {
		h.fail(w, err)
		return
	}
	if list == nil {
		list = []metadb.AuthToken{}
	}
	writeJSON(w, 200, list)
}

func (h *handlers) createToken(w http.ResponseWriter, r *http.Request) {
	var c creds
	if decode(r, &c) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	if c.Scope == "" {
		c.Scope = ScopeMCP // the narrower one
	}
	name := strings.TrimSpace(c.Name)
	id, tok, err := h.s.CreateToken(name, c.Scope)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.s.logf("[Auth] token id %d %q (%s, %s…) created from %s", id, name, c.Scope, tokenPrefix(tok), clientIP(r, h.trusted))
	writeJSON(w, 200, map[string]any{"id": id, "name": name, "scope": c.Scope, "token": tok})
}

func (h *handlers) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.fail(w, ErrNotFound)
		return
	}
	what := fmt.Sprintf("id %d", id) // the log names it as its creation line did
	if list, err := h.s.Tokens(); err == nil {
		for _, t := range list {
			if t.ID == id {
				what = fmt.Sprintf("id %d %q (%s, %s…)", id, t.Name, t.Scope, t.Prefix)
			}
		}
	}
	if err := h.s.RevokeToken(id); err != nil {
		h.fail(w, err)
		return
	}
	h.s.logf("[Auth] token %s revoked from %s", what, clientIP(r, h.trusted))
	writeJSON(w, 200, map[string]bool{"ok": true})
}
