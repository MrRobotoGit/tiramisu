package auth

import "net/http"

// csp allows only this origin. 'unsafe-inline' stays because the pages use inline
// scripts and onclick handlers; frame-ancestors is what stops clickjacking.
const csp = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; " +
	"form-action 'self'; frame-ancestors 'self'"

// setSecurityHeaders runs on every :9080 response, signed in or not.
func setSecurityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "SAMEORIGIN") // for browsers without frame-ancestors
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", csp)
	// HSTS only over HTTPS (directly or behind a proxy that says so): on plain HTTP it is ignored anyway.
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		h.Set("Strict-Transport-Security", "max-age=15552000")
	}
}
