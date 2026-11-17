package apidocs

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
	"strings"
)

//go:embed page.html
var pageHTML string

var page = template.Must(template.New("api").Funcs(template.FuncMap{
	"lower": strings.ToLower,
	"inc":   func(i int) int { return i + 1 },
}).Parse(pageHTML))

type view struct {
	Origin      string
	AuthEnabled bool
	Groups      []Group
	MCPTools    []string
	Common      []Status
}

// Expand puts the server-side origin into an example; the page script then swaps it for
// location.origin.
func (v view) Expand(s string) string { return strings.ReplaceAll(s, "{{ORIGIN}}", v.Origin) }

// origin is the address the client used; the page swaps in location.origin, which also
// knows about a TLS proxy in front.
func origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// Register mounts the page at GET /api-docs; authEnabled picks the token note in the MCP box.
func Register(mux *http.ServeMux, authEnabled func() bool) {
	mux.HandleFunc("GET /api-docs", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		v := view{Origin: origin(r), AuthEnabled: authEnabled != nil && authEnabled(), Groups: Groups, MCPTools: MCPTools, Common: Common}
		if err := page.Execute(&buf, v); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(buf.Bytes())
	})
}
