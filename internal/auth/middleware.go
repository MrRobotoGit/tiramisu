package auth

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	pathpkg "path"
	"strings"
)

const cookieName = "tiramisu_session"

// secureCookieName is the session cookie over HTTPS. A separate name: browsers refuse to
// let plain HTTP replace a Secure cookie of the same name, so turning HTTPS off would
// otherwise lock the user out of http:// until the old cookie expires.
const secureCookieName = "__Host-tiramisu_session"

// sessionCookie returns the session cookie the request carries, the HTTPS one first.
func sessionCookie(r *http.Request) (*http.Cookie, error) {
	if c, err := r.Cookie(secureCookieName); err == nil {
		return c, nil
	}
	return r.Cookie(cookieName)
}

type ctxKey struct{}

// tokenKey carries the name of the token a request came with ("" for the internal one).
type tokenKey struct{}

// TokenNameFrom is the name of the API token r came with, and whether it came with one.
func TokenNameFrom(r *http.Request) (string, bool) {
	v, ok := r.Context().Value(tokenKey{}).(string)
	return v, ok
}

// Actor names who made a change, for the configuration history: the signed-in user, a
// token by name, the engine itself, or the Control Panel with sign-in off.
func (s *Service) Actor(r *http.Request) (source, actor string) {
	if name, ok := TokenNameFrom(r); ok {
		if name == "" {
			return "internal", "engine"
		}
		return "api", name
	}
	if SessionFrom(r) != "" {
		if st := s.State(); st.Username != "" {
			return "ui", st.Username
		}
		return "ui", "signed in"
	}
	return "ui", ""
}

// SessionFrom returns the session cookie value of an authenticated request, if any.
func SessionFrom(r *http.Request) string {
	v, _ := r.Context().Value(ctxKey{}).(string)
	return v
}

func exempt(path string) bool {
	switch path {
	case "/plex/webhook", "/webhook", "/login", "/api/auth/login", "/api/auth/status":
		return true
	}
	return strings.HasPrefix(path, "/ui/")
}

// CleanPath is the canonical path auth decides on; other guards in front of Wrap use it too.
func CleanPath(p string) string { return cleanPath(p) }

func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	c := pathpkg.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Wrap guards next with the auth rules; it is applied once, around the whole mux.
func Wrap(s *Service, trusted []netip.Prefix, next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, (r.TLS != nil && s.HSTS()) || (r.TLS == nil && secureRequest(r)))
		// Decide on the canonical path, so /ui/../api cannot ride the /ui/ exemption.
		path := cleanPath(r.URL.Path)
		// Always, sign-in or not: otherwise any page the user opens could rewrite the config or
		// restart the engine. Clients that are not browsers send no Sec-Fetch-Site/Origin and pass.
		if !sameOrigin(cop, r, trusted) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}
		if s.FailClosed() {
			if path == "/plex/webhook" || path == "/webhook" {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "authentication state unavailable: stop the service, run tiramisu --reset-auth, start it again", http.StatusServiceUnavailable)
			return
		}
		if !s.Enabled() || exempt(path) {
			// Sign-in off: nothing is checked, but a token still names who made a change.
			if tok := bearer(r); tok != "" && !s.Enabled() {
				if s.IsInternal(tok) {
					r = r.WithContext(context.WithValue(r.Context(), tokenKey{}, ""))
				} else if _, name, ok := s.AuthenticateTokenNamed(tok, clientIP(r, trusted)); ok {
					r = r.WithContext(context.WithValue(r.Context(), tokenKey{}, name))
				}
			}
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(path, "/.well-known/") {
			http.NotFound(w, r)
			return
		}
		if tok := bearer(r); tok != "" {
			if s.IsInternal(tok) {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, "")))
				return
			}
			if scope, name, ok := s.AuthenticateTokenNamed(tok, clientIP(r, trusted)); ok {
				if msg := scopeRefusal(scope, path); msg != "" {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": msg})
					return
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, name)))
				return
			}
		} else if c, err := sessionCookie(r); err == nil {
			if ok, renewed := s.AuthenticateSession(c.Value); ok {
				if renewed {
					setSessionCookie(w, r, c.Value)
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c.Value)))
				return
			}
		}
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="tiramisu"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	})
}

// sameOrigin is CrossOriginProtection, plus one case it cannot see: over plain HTTP the
// browser sends no Sec-Fetch-Site, and a proxy that rewrites Host makes Origin differ from
// it. A configured proxy that names the original host in X-Forwarded-Host is believed.
func sameOrigin(cop *http.CrossOriginProtection, r *http.Request, trusted []netip.Prefix) bool {
	if cop.Check(r) == nil {
		return true
	}
	fwd := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
	if fwd == "" || r.Header.Get("Sec-Fetch-Site") != "" || !fromTrustedProxy(r, trusted) {
		return false
	}
	o, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && o.Host != "" && strings.EqualFold(o.Host, fwd)
}

// scopeRefusal says why a valid token may not open path, or "" when it may. Tokens never
// reach the sign-in and token routes: an agent holding one must not mint, revoke or lock out.
func scopeRefusal(scope, path string) string {
	switch {
	case strings.HasPrefix(path, "/api/auth/"):
		return "tokens and sign-in are managed from the Control Panel, signed in: no token opens /api/auth"
	case strings.HasPrefix(path, "/api/config/history") || path == "/api/config/rollback":
		return "the configuration history and rollback are for the Control Panel, signed in: a rollback can turn HTTPS off or change the trusted proxies"
	case scope == ScopeMCP && path != "/mcp":
		return "this is an MCP token: it opens /mcp only. Create an API token in Access for the HTTP API"
	case scope == ScopeAPI && path == "/mcp":
		return "this is an API token: /mcp needs an MCP token, created in Access"
	case scope != ScopeMCP && scope != ScopeAPI:
		return "token of unknown type: create a new one in Access"
	}
	return ""
}

// secureRequest: TLS here, or a proxy that says it terminated it. The header is believed
// from anyone: forging it only marks the forger's own cookie Secure.
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, value string) {
	name, secure := cookieName, secureRequest(r)
	if secure {
		name = secureCookieName
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()), Secure: secure})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: secureCookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1, Secure: true})
}
