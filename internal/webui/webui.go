// Package webui serves the assets shared by the Control Panel and the dashboard: the
// Aluminium/Anthracite theme, its script and the bundled fonts (OFL, no CDN, works offline).
package webui

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed theme.css theme.js swarm.js fonts/*.woff2 fonts/*.txt
var assets embed.FS

// Handler serves the assets; mount it with http.StripPrefix("/ui/", ...).
func Handler() http.Handler {
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No directory listings: only the files themselves are served.
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		// Embedded files carry no modtime, so cache for a bounded time instead of revalidating.
		if strings.HasPrefix(r.URL.Path, "fonts/") {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		files.ServeHTTP(w, r)
	})
}
