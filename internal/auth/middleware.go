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
		if (s.Enabled() || strings.HasPrefix(path, "/api/auth/")) && cop.Check(r) != nil {
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
			if s.IsInternal(tok) || s.AuthenticateToken(tok, clientIP(r, trusted)) {
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

func setSessionCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}
