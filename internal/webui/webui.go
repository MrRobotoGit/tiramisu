// Package webui serves the assets shared by the Control Panel and the dashboard: the
// Aluminium/Anthracite theme, its script and the bundled fonts (OFL, no CDN, works offline).
package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
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
		// Embedded files carry no modtime: a content ETag lets the browser revalidate, so a page
		// and the scripts it calls never come from two different builds after a deploy.
		if strings.HasPrefix(r.URL.Path, "fonts/") {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
			if tag := etag(r.URL.Path); tag != "" {
				w.Header().Set("ETag", tag)
			}
		}
		files.ServeHTTP(w, r)
	})
}

var etags sync.Map // path → quoted content hash

func etag(path string) string {
	if v, ok := etags.Load(path); ok {
		return v.(string)
	}
	b, err := assets.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	tag := `"` + hex.EncodeToString(sum[:8]) + `"`
	etags.Store(path, tag)
	return tag
}
