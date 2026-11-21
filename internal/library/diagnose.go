package library

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tiramisu/internal/metadb"
)

// DiagnosticsDB is what the consistency check reads; *metadb.DB satisfies it.
type DiagnosticsDB interface {
	ListGoStormTorrentRefs() ([]metadb.TorrentRef, error)
	AudioRefs() ([]metadb.AudioRef, error)
	InodeFiles() ([]metadb.InodeFile, error)
	MetadataFailureHashes() ([]string, error)
	EpisodeRefs() ([]metadb.EpisodeEntry, error)
}

// Check is one consistency rule: how many items break it and a few of them.
type Check struct {
	ID    string `json:"id"`
	Desc  string `json:"description"`
	Count int    `json:"count"`
	Older int    `json:"older_than_30_days,omitempty"`
	// Removable is how many the orphan collection would take (past its default age).
	Removable int      `json:"removable,omitempty"`
	Samples   []string `json:"samples"`
}

// Diagnosis is the report of one consistency check. It changes nothing.
type Diagnosis struct {
	At         int64   `json:"at"`
	DurationMS int64   `json:"duration_ms"`
	Checks     []Check `json:"checks"`
}

// ErrDiagnosisRunning is returned while another check is going.
var ErrDiagnosisRunning = errors.New("a consistency check is already running")

const diagSamples = 20

func (c *Check) add(sample string) {
	c.Count++
	if len(c.Samples) < diagSamples {
		c.Samples = append(c.Samples, sample)
	}
}

// diagHash is the torrent a reference names: lower case, without a CUE track suffix.
func diagHash(h string) string {
	if i := strings.IndexByte(h, '#'); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(strings.TrimSpace(h))
}

// Diagnose compares what the engine holds with what the library references. Read-only:
// stubs on disk are the source of truth for video (the inode map only has what FUSE listed).
func (m *Manager) Diagnose(ctx context.Context) (*Diagnosis, error) {
	db := m.cfg.Diagnostics
	if db == nil {
		return nil, errors.New("the consistency check needs the state DB")
	}
	if !m.diagMu.TryLock() {
		return nil, ErrDiagnosisRunning
	}
	defer m.diagMu.Unlock()
	start := time.Now()
	snap, err := m.snapshotRefs(ctx, db)
	if err != nil {
		return nil, err
	}
	inodes, err := db.InodeFiles()
	if err != nil {
		return nil, fmt.Errorf("inode map: %w", err)
	}
	failures, err := db.MetadataFailureHashes()
	if err != nil {
		return nil, fmt.Errorf("metadata failures: %w", err)
	}
	stubs, audio, episodes, engine := snap.stubs, snap.audio, snap.episodes, snap.engine
	files := map[string]bool{} // every path the library has: stubs and audio files
	tvStubs := map[string]bool{}
	for _, s := range stubs {
		files[s.Path] = true
		if strings.HasPrefix(s.Path, m.cfg.TVDir+string(filepath.Separator)) {
			tvStubs[s.Path] = true
		}
	}
	for _, a := range audio {
		if m.cfg.AudioRoot != "" {
			files[filepath.Join(m.cfg.AudioRoot, a.Section, a.VirtualPath)] = true
		}
	}

	orphans := Check{ID: "orphan_torrents", Desc: "torrents in Tentacle that no stub (movies/, tv/), episode or audio projection references; added in the last 10 minutes are left out"}
	month := time.Now().Add(-30 * 24 * time.Hour).Unix()
	removableBefore := start.Add(-orphanDefaultMinDays * 24 * time.Hour).Unix()
	for _, t := range snap.orphans(start.Add(-10 * time.Minute)) { // an add in flight has its torrent before its stub
		title, _, _ := strings.Cut(t.Title, "\n")
		orphans.add(fmt.Sprintf("%s %s (%s)", diagHash(t.Hash), title, time.Unix(t.Timestamp, 0).Format("2006-01-02")))
		if t.Timestamp < month {
			orphans.Older++
		}
		if t.Timestamp <= removableBefore {
			orphans.Removable++
		}
	}

	missing := Check{ID: "missing_torrents", Desc: "stubs, episodes and committed audio whose torrent is not in Tentacle"}
	seen := map[string]bool{}
	miss := func(hash, path string) {
		if hash = diagHash(hash); hash == "" || engine[hash] || seen[path] {
			return
		}
		seen[path] = true
		missing.add(hash + " " + path)
	}
	for _, s := range stubs {
		miss(s.Hash, s.Path)
	}
	for _, e := range episodes {
		miss(e.Hash, e.FilePath)
	}
	for _, a := range audio {
		if a.State == "committed" {
			miss(a.Hash, a.Section+"/"+a.VirtualPath)
		}
	}

	noHash := Check{ID: "stubs_without_hash", Desc: "stubs that cannot be read or carry no hash"}
	for _, s := range stubs {
		if s.Hash == "" {
			noHash.add(s.Path)
		}
	}

	registered := map[string]bool{}
	tvOrphanRows := Check{ID: "tv_registry_without_stub", Desc: "tv_episodes rows whose file is gone"}
	for _, e := range episodes {
		registered[e.FilePath] = true
		if !tvStubs[e.FilePath] {
			tvOrphanRows.add(e.EpisodeKey + " " + e.FilePath)
		}
	}
	tvUnregistered := Check{ID: "tv_stubs_unregistered", Desc: "TV stubs with no tv_episodes row"}
	tvPaths := make([]string, 0, len(tvStubs))
	for p := range tvStubs {
		tvPaths = append(tvPaths, p)
	}
	sort.Strings(tvPaths)
	for _, p := range tvPaths {
		if !registered[p] {
			tvUnregistered.add(p)
		}
	}

	ghosts := Check{ID: "inode_ghosts", Desc: "inode map files with no stub or audio file behind them"}
	for _, f := range inodes {
		if !files[f.Path] {
			ghosts.add(f.Path)
		}
	}

	staleFail := Check{ID: "stale_metadata_failures", Desc: "metadata failure rows for torrents no longer in Tentacle"}
	for _, h := range failures {
		if !engine[diagHash(h)] {
			staleFail.add(h)
		}
	}

	stuck := Check{ID: "stuck_audio", Desc: "audio projections staged or being removed for over an hour"}
	hourAgo := time.Now().Add(-time.Hour).UnixNano()
	for _, a := range audio {
		if (a.State == "staged" || a.State == "removing") && a.UpdatedAtNS < hourAgo {
			stuck.add(a.State + " " + a.Section + "/" + a.VirtualPath)
		}
	}

	checks := []Check{orphans, missing, noHash, tvUnregistered, tvOrphanRows, ghosts, staleFail, stuck}
	for i := range checks {
		if checks[i].Samples == nil {
			checks[i].Samples = []string{}
		}
	}
	d := &Diagnosis{At: start.Unix(), DurationMS: time.Since(start).Milliseconds(), Checks: checks}
	m.lastDiag.Store(d)
	return d, nil
}

// refSnapshot is the engine and the library read around one stub walk.
type refSnapshot struct {
	torrents   []metadb.TorrentRef // in both engine reads
	engine     map[string]bool     // present in either read
	referenced map[string]bool     // cited by a stub, an episode row or an audio row
	stubs      []Item
	audio      []metadb.AudioRef
	episodes   []metadb.EpisodeEntry
}

// snapshotRefs reads the torrents before and after the walk: a torrent counts as orphan
// only if unreferenced in both views, and as present if seen in either, so an add or a
// removal racing the walk does not show up as drift. Stubs are read by content: the full
// hash inside counts, whatever the file name says.
func (m *Manager) snapshotRefs(ctx context.Context, db DiagnosticsDB) (*refSnapshot, error) {
	before, err := db.ListGoStormTorrentRefs()
	if err != nil {
		return nil, fmt.Errorf("torrents: %w", err)
	}
	snap := &refSnapshot{engine: map[string]bool{}, referenced: map[string]bool{}}
	if snap.audio, err = db.AudioRefs(); err != nil {
		return nil, fmt.Errorf("audio projections: %w", err)
	}
	if snap.episodes, err = db.EpisodeRefs(); err != nil {
		return nil, fmt.Errorf("episodes: %w", err)
	}
	for _, kind := range []string{"movie", "tv"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		items, err := m.list(ctx, kind)
		if err != nil {
			return nil, fmt.Errorf("%s stubs: %w", kind, err)
		}
		snap.stubs = append(snap.stubs, items...)
	}
	after, err := db.ListGoStormTorrentRefs()
	if err != nil {
		return nil, fmt.Errorf("torrents: %w", err)
	}
	inAfter := map[string]bool{}
	for _, t := range after {
		snap.engine[diagHash(t.Hash)] = true
		inAfter[diagHash(t.Hash)] = true
	}
	for _, t := range before {
		h := diagHash(t.Hash)
		snap.engine[h] = true
		if inAfter[h] {
			snap.torrents = append(snap.torrents, t)
		}
	}
	for _, s := range snap.stubs {
		if s.Hash != "" {
			snap.referenced[diagHash(s.Hash)] = true
		}
	}
	for _, e := range snap.episodes {
		snap.referenced[diagHash(e.Hash)] = true
	}
	for _, a := range snap.audio {
		snap.referenced[diagHash(a.Hash)] = true // any state: a removal may still have a reader
	}
	return snap, nil
}

// orphans are the unreferenced torrents not added nor used since cutoff, oldest first.
func (s *refSnapshot) orphans(cutoff time.Time) []metadb.TorrentRef {
	var out []metadb.TorrentRef
	for _, t := range s.torrents {
		if !s.referenced[diagHash(t.Hash)] && t.LastActivity() <= cutoff.Unix() {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].LastActivity(), out[j].LastActivity(); a != b {
			return a < b
		}
		return out[i].Hash < out[j].Hash
	})
	return out
}

// unreadableStubs counts stubs whose torrent cannot be told.
func (s *refSnapshot) unreadableStubs() int {
	n := 0
	for _, st := range s.stubs {
		if st.Hash == "" {
			n++
		}
	}
	return n
}

// LastDiagnosis is the result of the last check since startup, nil before the first.
func (m *Manager) LastDiagnosis() *Diagnosis { return m.lastDiag.Load() }
