package clients

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// A library scan is the media server (or another program) opening many files through FUSE
// with nobody watching them. Opens are noted here with the reading process; a file that a
// known client plays, or played in the last minute, is not a scan read.

const (
	scanWindow = time.Minute      // rate window, and how long the scan outlives its last read
	scanGrace  = 10 * time.Second // a playback's session can take this long to be reported
	scanKeep   = 4096             // opens remembered, at most: a faster scan is undercounted
	nameTTL    = time.Minute      // pids get reused: names are re-read after this
)

type scanOpen struct {
	at     time.Time
	hash   string
	file   string
	pid    uint32 // process (thread group) id, 0 when unknown
	reader string // process name, "" when unknown
}

type procEntry struct {
	tgid uint32
	name string
	at   time.Time
}

type scanLog struct {
	mu       sync.Mutex
	opens    []scanOpen
	start    time.Time // first read of the current scan episode
	lastRead time.Time // newest scan read already counted
	files    map[string]bool
	played   map[string]time.Time // hash → last seen playing
	names    map[uint32]procEntry // FUSE pid (a thread id) → process
}

var scans = newScanLog()

func newScanLog() *scanLog {
	return &scanLog{files: map[string]bool{}, played: map[string]time.Time{}, names: map[uint32]procEntry{}}
}

// NoteFUSEOpen records a FUSE open, with the calling process the kernel reports.
func NoteFUSEOpen(ctx context.Context, hash, file string) {
	var pid uint32
	if c, ok := fuse.FromContext(ctx); ok {
		pid = c.Pid
	}
	NoteOpen(hash, file, pid)
}

// NoteOpen records that a FUSE file was opened by pid (0 when the kernel does not say,
// as across containers). The /proc lookup runs outside the lock, once per pid a minute.
func NoteOpen(hash, file string, pid uint32) {
	if hash == "" {
		return
	}
	scans.note(hash, file, pid, time.Now())
}

func (l *scanLog) note(hash, file string, pid uint32, now time.Time) {
	var p procEntry
	if pid != 0 {
		l.mu.Lock()
		e, ok := l.names[pid]
		l.mu.Unlock()
		if ok && now.Sub(e.at) < nameTTL {
			p = e
		} else {
			p = lookupProc(pid, now)
			l.mu.Lock()
			if len(l.names) > 256 {
				l.names = map[uint32]procEntry{}
			}
			l.names[pid] = p
			l.mu.Unlock()
		}
	}
	l.mu.Lock()
	l.opens = append(l.opens, scanOpen{at: now, hash: hash, file: file, pid: p.tgid, reader: p.name})
	if len(l.opens) > scanKeep {
		l.opens = append([]scanOpen(nil), l.opens[len(l.opens)-scanKeep/2:]...)
	}
	l.mu.Unlock()
}

// lookupProc turns the pid FUSE reports, which is the calling thread's id, into its
// process: the thread group leader and its name (a worker thread has its own name).
func lookupProc(tid uint32, now time.Time) procEntry {
	e := procEntry{tgid: tid, at: now}
	base := "/proc/" + strconv.FormatUint(uint64(tid), 10)
	if f, err := os.Open(base + "/status"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "Tgid:"); ok {
				if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32); err == nil && n != 0 {
					e.tgid = uint32(n)
				}
				break
			}
		}
		f.Close()
	}
	if b, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(e.tgid), 10) + "/comm"); err == nil {
		e.name = strings.TrimSpace(string(b))
	}
	return e
}

// Scan is what the swarm view draws as the scanner's client node.
type Scan struct {
	Active      bool             `json:"active"`
	Label       string           `json:"label,omitempty"`  // "Plex scan", "Library scan · Apple TV", "Library scan"
	Reader      string           `json:"reader,omitempty"` // process name, when known
	FilesPerMin int              `json:"files_per_min"`
	FilesRead   int              `json:"files_read"` // since the scan began
	SinceS      int64            `json:"since_s,omitempty"`
	Torrents    map[string]int64 `json:"torrents,omitempty"` // hash → seconds since its last scan read
}

// sambaPeers tells, for an smbd process, which host it serves: the media server
// (serverAddrs) or another device. ok is false when Samba was not queried.
type sambaPeers struct {
	ipByPID map[string]string
	server  map[string]bool
	name    func(ip string) string
}

// scanLabel names the scan after the program that reads. smbd serves one remote host per
// process: the media server's host makes it the media server's scan, another host a scan by
// that device (Infuse building its library, Finder), an unknown host a plain library scan.
func scanLabel(reader string, pid uint32, serverType string, smb *sambaPeers) string {
	r := strings.ToLower(reader)
	server := "Plex scan"
	if serverType == "jellyfin" {
		server = "Jellyfin scan"
	}
	switch {
	case strings.Contains(r, "plex"):
		return "Plex scan"
	case strings.Contains(r, "jellyfin"):
		return "Jellyfin scan"
	case strings.Contains(r, "emby"):
		return "Emby scan"
	case r == "smbd" && smb != nil && pid != 0:
		ip := smb.ipByPID[strconv.FormatUint(uint64(pid), 10)]
		switch {
		case ip == "":
		case smb.server[ip]:
			return server
		default:
			return "Library scan · " + smb.name(ip)
		}
	}
	return "Library scan"
}

// snapshot reports the scan running at now; playing holds the hashes a known client reads.
func (l *scanLog) snapshot(now time.Time, playing map[string]bool, serverType string, smb *sambaPeers) Scan {
	l.mu.Lock()
	cut := now.Add(-scanWindow)
	keep := l.opens[:0]
	for _, o := range l.opens {
		if o.at.After(cut) {
			keep = append(keep, o)
		}
	}
	l.opens = keep
	opens := append([]scanOpen(nil), l.opens...)
	// A playback that just stopped, or whose session is not reported yet, must not turn into a scan.
	for h := range playing {
		l.played[h] = now
	}
	played := make(map[string]time.Time, len(l.played))
	for h, at := range l.played {
		if now.Sub(at) > scanWindow {
			delete(l.played, h)
		} else {
			played[h] = at
		}
	}
	l.mu.Unlock()

	type key struct {
		reader string
		pid    uint32
	}
	recent := map[string]bool{}
	readers := map[key]int{}
	last := map[string]time.Time{}
	var first, newest time.Time
	for _, o := range opens {
		if _, p := played[o.hash]; p || now.Sub(o.at) < scanGrace {
			continue
		}
		recent[o.file] = true
		readers[key{o.reader, o.pid}]++
		if o.at.After(last[o.hash]) {
			last[o.hash] = o.at
		}
		if first.IsZero() || o.at.Before(first) {
			first = o.at
		}
		if o.at.After(newest) {
			newest = o.at
		}
	}
	var out Scan
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(recent) == 0 {
		return out
	}
	// A new episode when the previous one ended more than a minute before this read, whether
	// or not anybody was polling in between.
	if l.start.IsZero() || first.Sub(l.lastRead) > scanWindow {
		l.start, l.files = first, map[string]bool{}
	}
	if newest.After(l.lastRead) {
		l.lastRead = newest
	}
	for f := range recent {
		l.files[f] = true
	}
	// The busiest reader names the scan; ties go to a named one, then alphabetically.
	ks := make([]key, 0, len(readers))
	for k := range readers {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if readers[ks[i]] != readers[ks[j]] {
			return readers[ks[i]] > readers[ks[j]]
		}
		if (ks[i].reader == "") != (ks[j].reader == "") {
			return ks[j].reader == ""
		}
		return ks[i].reader < ks[j].reader
	})
	out.Active = true
	out.Reader = ks[0].reader
	out.Label = scanLabel(ks[0].reader, ks[0].pid, serverType, smb)
	out.FilesPerMin = len(recent)
	out.FilesRead = len(l.files)
	out.SinceS = int64(now.Sub(l.start).Seconds())
	out.Torrents = make(map[string]int64, len(last))
	for h, at := range last {
		out.Torrents[h] = int64(now.Sub(at).Seconds())
	}
	return out
}

// ScanHandler answers GET /api/scan.
func (s *Service) ScanHandler(w http.ResponseWriter, r *http.Request) {
	playing := map[string]bool{}
	for _, c := range s.Clients(r.Context()) {
		playing[c.Hash] = true
	}
	s.mu.Lock()
	var smb *sambaPeers
	if s.smbPIDs != nil {
		smb = &sambaPeers{ipByPID: s.smbPIDs, server: s.smbServer, name: s.name}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(scans.snapshot(time.Now(), playing, s.cfg().ServerType, smb))
}
