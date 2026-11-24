package dashboard

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"tiramisu/internal/monitor/collector"
	"tiramisu/internal/monitor/logtail"
)

//go:embed dashboard.html
var dashboardHTML []byte

// Handler serves the dashboard and API endpoints.
type Handler struct {
	collector *collector.Collector
	logsDir   string
}

// New creates a dashboard handler.
func New(c *collector.Collector, logsDir string) *Handler {
	return &Handler{collector: c, logsDir: logsDir}
}

// Dashboard serves the HTML page.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(dashboardHTML)
}

// Health serves the /api/health JSON endpoint.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.collector.Status())
}

// Torrents serves the /api/torrents JSON endpoint.
func (h *Handler) Torrents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	t := h.collector.Torrents()
	if t == nil {
		t = []collector.TorrentInfo{}
	}
	json.NewEncoder(w).Encode(t)
}

// SpeedHistory serves the /api/speed-history JSON endpoint.
func (h *Handler) SpeedHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s := h.collector.SpeedHistory()
	if s == nil {
		s = []collector.SpeedPoint{}
	}
	json.NewEncoder(w).Encode(s)
}

// ShieldEvents serves the /api/shield-events JSON endpoint.
func (h *Handler) ShieldEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	evts := h.collector.ShieldEvents()
	if evts == nil {
		evts = []collector.ShieldEvent{}
	}
	json.NewEncoder(w).Encode(evts)
}

// Logs serves the /api/logs endpoint (tail of log files).
func (h *Handler) Logs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	allowed := map[string]string{
		"tiramisu":       "tiramisu.log",
		"movies-sync":    "movies-sync.log",
		"tv-sync":        "tv-sync.log",
		"watchlist-sync": "watchlist-sync.log",
		"music-sync":     "music-sync.log",
	}

	file := r.URL.Query().Get("file")
	if file == "" {
		file = "tiramisu"
	}
	logName, ok := allowed[file]
	if !ok {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "invalid log file"})
		return
	}

	lines := 80
	if v := r.URL.Query().Get("lines"); v != "" {
		if n := atoi(v); n > 0 && n <= 200 {
			lines = n
		}
	}

	logPath := filepath.Join(h.logsDir, logName)
	result := tailFile(logPath, lines)
	json.NewEncoder(w).Encode(map[string]interface{}{"lines": result})
}

func tailFile(path string, n int) []string {
	// Only the tail is needed; reading the whole file kept a 218MB log resident.
	data := logtail.Read(path, 128*1024)
	if data == nil {
		return []string{}
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// KillStream drops the given torrent from memory.
func (h *Handler) KillStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte(`{"status":"error","message":"method not allowed"}`))
		return
	}
	hash := strings.TrimPrefix(r.URL.Path, "/api/kill-stream/")
	if hash == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"status":"error","message":"missing hash"}`))
		return
	}
	if err := h.collector.DropTorrent(hash); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"status":"error","message":%q}`, err.Error())
		return
	}
	w.Write([]byte(`{"status":"ok"}`))
}

// PlexThumb proxies a Plex thumbnail through tiramisu.
// This avoids exposing the Plex token in client-side HTML/JS and decouples
// the browser from the configured plex_url (which may not be directly reachable
// from the client network).
func (h *Handler) PlexThumb(w http.ResponseWriter, r *http.Request) {
	// Only the dashboard's own <img> loads it; browsers that say so are held to that.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	target, ok := plexThumbURL(h.collector.PlexURL(), r.URL.Query().Get("p"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	req.Header.Set("X-Plex-Token", h.collector.PlexToken())
	resp, err := thumbClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		http.NotFound(w, r)
		return
	}
	defer resp.Body.Close()
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !thumbTypes[ct] {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	io.Copy(w, io.LimitReader(resp.Body, maxThumbBytes)) //nolint:errcheck
}

const maxThumbBytes = 16 << 20

// Raster images only: an SVG served from this origin could run script.
var thumbTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true}

// thumbPath is the shape of a poster path in Plex sessions: no other Plex endpoint is reachable.
var thumbPath = regexp.MustCompile(`^/library/metadata/[0-9]+/(thumb|art|banner|poster)(/[0-9]+)?$`)

// thumbClient never follows a redirect: the token header must not leave the media server.
var thumbClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// plexThumbURL is the media server's URL for a poster path as Plex reports it, or false for
// anything else: the host is always the configured one.
func plexThumbURL(base, p string) (string, bool) {
	if base == "" || !thumbPath.MatchString(p) {
		return "", false
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	host, user := u.Host, u.User.String()
	u = u.JoinPath(p)
	if u.Host != host || u.User.String() != user {
		return "", false
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String(), true
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			break
		}
	}
	return n
}
