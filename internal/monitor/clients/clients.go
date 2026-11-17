// Package clients answers "who is watching this torrent": the playing device as the media
// server names it (Plex /status/sessions, Jellyfin /Sessions), or, for players that read the
// Samba share directly (Infuse), the SMB client address from smbstatus.
package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Client is one stream's reader. Source is "plex", "jellyfin" or "smb".
type Client struct {
	Hash    string `json:"hash"`
	Source  string `json:"source"`
	Device  string `json:"device"`
	Product string `json:"product,omitempty"`
	State   string `json:"state,omitempty"`
	Address string `json:"address,omitempty"`
	// PositionS is the playback position in seconds, as the media server reports it.
	PositionS int64 `json:"position_s,omitempty"`
}

// Config is read on every refresh so a saved config applies without a restart.
type Config struct {
	ServerType string // "plex" (default) or "jellyfin"
	URL        string
	Token      string
	// Resolve maps a virtual .mkv basename to its torrent hash; "" when it is not playing.
	Resolve func(basename string) string
	// Active reports whether any file is being played; without it smbstatus is not run.
	Active func() bool
}

const (
	cacheTTL      = 5 * time.Second
	smbTTL        = 15 * time.Second // each smbstatus call is a sudo session: keep them rare
	smbRetryAfter = 5 * time.Minute
	dnsTTL        = 10 * time.Minute
)

// Service caches the joined view; Handler serves it at /api/clients.
type Service struct {
	cfg  func() Config
	http *http.Client
	// smbCmd runs smbstatus with the given flag; swapped in tests.
	smbCmd func(ctx context.Context, flag string) ([]byte, error)
	goos   string
	lookup func(ctx context.Context, ip string) ([]string, error)

	mu         sync.Mutex
	cached     []Client
	cachedAt   time.Time
	refreshing bool      // one refresh at a time; concurrent callers get the cached copy
	smbOff     time.Time // smbstatus failed (no sudo rule, no Samba): retry after this
	smbCache   []Client
	smbAt      time.Time
	dns        map[string]dnsEntry
}

type dnsEntry struct {
	name string
	at   time.Time
}

// New builds the service.
func New(cfg func() Config) *Service {
	return &Service{
		cfg:    cfg,
		http:   &http.Client{Timeout: 3 * time.Second},
		smbCmd: runSmbstatus,
		goos:   runtime.GOOS,
		lookup: (&net.Resolver{}).LookupAddr,
		dns:    map[string]dnsEntry{},
	}
}

// Handler serves the current clients as JSON.
func (s *Service) Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	out := s.Clients(r.Context())
	if out == nil {
		out = []Client{}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// Clients returns one entry per playing torrent whose reader is known.
func (s *Service) Clients(ctx context.Context) []Client {
	s.mu.Lock()
	if time.Since(s.cachedAt) < cacheTTL || s.refreshing {
		out := s.cached
		s.mu.Unlock()
		return out
	}
	s.refreshing = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.refreshing = false
		s.mu.Unlock()
	}()

	cfg := s.cfg()
	if cfg.Resolve == nil {
		return nil
	}
	// Not tied to the caller: a closed browser tab must not abort (and back off) smbstatus.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	byHash := map[string]Client{}
	// Media server sessions name the device; they win over the SMB address.
	var sessions []Client
	switch cfg.ServerType {
	case "jellyfin":
		sessions = s.jellyfinSessions(ctx, cfg)
	default:
		sessions = s.plexSessions(ctx, cfg)
	}
	for _, c := range sessions {
		if c.Hash != "" {
			if _, dup := byHash[c.Hash]; !dup {
				byHash[c.Hash] = c
			}
		}
	}
	serverAddrs := addrsOf(ctx, hostOf(cfg.URL))
	for _, c := range s.smbClients(cfg, serverAddrs) {
		if _, named := byHash[c.Hash]; named || c.Hash == "" {
			continue
		}
		byHash[c.Hash] = c
	}

	out := make([]Client, 0, len(byHash))
	for _, c := range byHash {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	s.mu.Lock()
	s.cached, s.cachedAt = out, time.Now()
	s.mu.Unlock()
	return out
}

// ---------- Plex ----------

type plexSessions struct {
	MediaContainer struct {
		Metadata []struct {
			ViewOffset int64 `json:"viewOffset"` // ms
			Media      []struct {
				Part []struct {
					File string `json:"file"`
				} `json:"Part"`
			} `json:"Media"`
			Player struct {
				Title    string `json:"title"`
				Device   string `json:"device"`
				Product  string `json:"product"`
				Platform string `json:"platform"`
				State    string `json:"state"`
				Address  string `json:"address"`
			} `json:"Player"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

func (s *Service) plexSessions(ctx context.Context, cfg Config) []Client {
	if cfg.URL == "" || cfg.Token == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.URL, "/")+"/status/sessions", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", cfg.Token)
	var body plexSessions
	if !s.getJSON(req, &body) {
		return nil
	}
	var out []Client
	for _, m := range body.MediaContainer.Metadata {
		device := firstNonEmpty(m.Player.Title, m.Player.Device, m.Player.Platform, "Plex player")
		for _, media := range m.Media {
			for _, part := range media.Part {
				out = append(out, Client{
					Hash:      cfg.Resolve(path.Base(filepathSlash(part.File))),
					Source:    "plex",
					Device:    device,
					Product:   m.Player.Product,
					State:     m.Player.State,
					Address:   m.Player.Address,
					PositionS: m.ViewOffset / 1000,
				})
			}
		}
	}
	return out
}

// ---------- Jellyfin ----------

type jellyfinSession struct {
	DeviceName     string `json:"DeviceName"`
	Client         string `json:"Client"`
	RemoteEndPoint string `json:"RemoteEndPoint"`
	NowPlayingItem *struct {
		Path string `json:"Path"`
	} `json:"NowPlayingItem"`
	PlayState struct {
		IsPaused      bool  `json:"IsPaused"`
		PositionTicks int64 `json:"PositionTicks"` // 100 ns
	} `json:"PlayState"`
}

func (s *Service) jellyfinSessions(ctx context.Context, cfg Config) []Client {
	if cfg.URL == "" || cfg.Token == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.URL, "/")+"/Sessions?activeWithinSeconds=120", nil)
	if err != nil {
		return nil
	}
	// Jellyfin 12.0 stopped parsing X-Emby-Token; the MediaBrowser scheme works on 10.x too.
	req.Header.Set("Authorization", fmt.Sprintf("MediaBrowser Token=%q", cfg.Token))
	var body []jellyfinSession
	if !s.getJSON(req, &body) {
		return nil
	}
	var out []Client
	for _, js := range body {
		if js.NowPlayingItem == nil || js.NowPlayingItem.Path == "" {
			continue
		}
		state := "playing"
		if js.PlayState.IsPaused {
			state = "paused"
		}
		out = append(out, Client{
			Hash:      cfg.Resolve(path.Base(filepathSlash(js.NowPlayingItem.Path))),
			Source:    "jellyfin",
			Device:    firstNonEmpty(js.DeviceName, js.Client, "Jellyfin player"),
			Product:   js.Client,
			State:     state,
			Address:   js.RemoteEndPoint,
			PositionS: js.PlayState.PositionTicks / 10_000_000,
		})
	}
	return out
}

func (s *Service) getJSON(req *http.Request, v interface{}) bool {
	resp, err := s.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(resp.Body).Decode(v) == nil
}

// ---------- Samba ----------

type smbLocks struct {
	OpenFiles map[string]struct {
		Filename string `json:"filename"`
		Opens    map[string]struct {
			ServerID struct {
				PID string `json:"pid"`
			} `json:"server_id"`
		} `json:"opens"`
	} `json:"open_files"`
}

type smbProcs struct {
	Sessions map[string]struct {
		ServerID struct {
			PID string `json:"pid"`
		} `json:"server_id"`
		RemoteMachine string `json:"remote_machine"`
	} `json:"sessions"`
}

// smbClients joins open files (smbstatus -L) to sessions (-p) by smbd pid. smbstatus only
// works as root, so it runs through `sudo -n`; without a passwordless rule it is skipped.
// The media server host's own opens are skipped: it reads on behalf of a player (whose name
// comes from the session) or scans, and is never the client itself.
func (s *Service) smbClients(cfg Config, serverAddrs map[string]bool) []Client {
	s.mu.Lock()
	off := time.Now().Before(s.smbOff)
	fresh := time.Since(s.smbAt) < smbTTL
	cached := s.smbCache
	s.mu.Unlock()
	if off || s.goos != "linux" || (cfg.Active != nil && !cfg.Active()) {
		return nil
	}
	if fresh {
		return cached
	}
	out, err := s.querySmb(cfg, serverAddrs)
	s.mu.Lock()
	if err != nil {
		s.smbOff = time.Now().Add(smbRetryAfter)
		out = nil
	}
	s.smbCache, s.smbAt = out, time.Now()
	s.mu.Unlock()
	return out
}

func (s *Service) querySmb(cfg Config, serverAddrs map[string]bool) ([]Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lraw, err := s.smbCmd(ctx, "-L")
	var locks smbLocks
	if err == nil {
		err = json.Unmarshal(lraw, &locks)
	}
	if err != nil {
		return nil, err
	}
	if len(locks.OpenFiles) == 0 {
		return nil, nil
	}
	praw, err := s.smbCmd(ctx, "-p")
	var procs smbProcs
	if err == nil {
		err = json.Unmarshal(praw, &procs)
	}
	if err != nil {
		return nil, err
	}
	ipByPID := map[string]string{}
	for _, sess := range procs.Sessions {
		ipByPID[sess.ServerID.PID] = sess.RemoteMachine
	}
	var out []Client
	for _, f := range locks.OpenFiles {
		hash := cfg.Resolve(path.Base(f.Filename))
		if hash == "" {
			continue
		}
		// Map order is random: walk the opens in a fixed order so the reported reader is stable.
		keys := make([]string, 0, len(f.Opens))
		for k := range f.Opens {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			o := f.Opens[key]
			ip := ipByPID[o.ServerID.PID]
			if ip == "" || serverAddrs[ip] {
				continue
			}
			out = append(out, Client{Hash: hash, Source: "smb", Device: s.name(ip), Address: ip})
			break
		}
	}
	return out, nil
}

func runSmbstatus(ctx context.Context, flag string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sudo", "-n", "smbstatus", flag, "--json")
	// SIGKILL on sudo does not reach the root smbstatus child, which would keep the pipe open
	// and hang Output(): SIGTERM is relayed by sudo, and WaitDelay bounds the wait regardless.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

// name turns an address into the short host name the LAN DNS gives it, or keeps the address.
func (s *Service) name(ip string) string {
	s.mu.Lock()
	e, ok := s.dns[ip]
	s.mu.Unlock()
	if ok && time.Since(e.at) < dnsTTL {
		return e.name
	}
	n := ip
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if names, err := s.lookup(ctx, ip); err == nil && len(names) > 0 {
		if h := strings.TrimSuffix(names[0], "."); h != "" {
			n = strings.SplitN(h, ".", 2)[0]
		}
	}
	s.mu.Lock()
	s.dns[ip] = dnsEntry{name: n, at: time.Now()}
	s.mu.Unlock()
	return n
}

// ---------- helpers ----------

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// addrsOf resolves the media server host to its addresses (an IP resolves to itself).
func addrsOf(ctx context.Context, host string) map[string]bool {
	out := map[string]bool{}
	if host == "" {
		return out
	}
	out[host] = true
	if net.ParseIP(host) == nil {
		ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if ips, err := (&net.Resolver{}).LookupHost(ctx, host); err == nil {
			for _, ip := range ips {
				out[ip] = true
			}
		}
	}
	return out
}

// filepathSlash normalises Windows paths a media server may report, so path.Base works.
func filepathSlash(p string) string { return strings.ReplaceAll(p, "\\", "/") }

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
