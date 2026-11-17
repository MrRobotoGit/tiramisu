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

type ctxKey struct{}

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
		setSecurityHeaders(w, r)
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
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(path, "/.well-known/") {
			http.NotFound(w, r)
			return
		}
		if tok := bearer(r); tok != "" {
			if s.IsInternal(tok) {
				next.ServeHTTP(w, r)
				return
			}
			if scope, ok := s.AuthenticateToken(tok, clientIP(r, trusted)); ok {
				if msg := scopeRefusal(scope, path); msg != "" {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": msg})
					return
				}
				next.ServeHTTP(w, r)
				return
			}
		} else if c, err := r.Cookie(cookieName); err == nil {
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
	case scope == ScopeMCP && path != "/mcp":
		return "this is an MCP token: it opens /mcp only. Create an API token in Access for the HTTP API"
	case scope == ScopeAPI && path == "/mcp":
		return "this is an API token: /mcp needs an MCP token, created in Access"
	case scope != ScopeMCP && scope != ScopeAPI:
		return "token of unknown type: create a new one in Access"
	}
	return ""
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}
