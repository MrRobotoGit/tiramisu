package musicimport

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// discoveryStatus is what happened to one album candidate.
type discoveryStatus string

const (
	discoImported  discoveryStatus = "imported"
	discoPresent   discoveryStatus = "present"
	discoNoTorrent discoveryStatus = "no-torrent"
	discoFailed    discoveryStatus = "failed"
	discoParked    discoveryStatus = "parked"
)

// discoveryAlbum is the discovery's memory of one release group.
type discoveryAlbum struct {
	Artist      string          `json:"artist,omitempty"`
	Title       string          `json:"title,omitempty"`
	Status      discoveryStatus `json:"status"`
	Attempts    int             `json:"attempts,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	TorrentHash string          `json:"torrent_hash,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// DiscoveryState is the run's durable memory: the seeds of the last run, the
// MusicBrainz answers the run paid for, and what happened to every album. The
// recording cache is not persisted on purpose: it only avoids paying twice inside
// one run, and keeping it out of the file keeps the weekly file bounded.
type DiscoveryState struct {
	Version    int                           `json:"version"`
	Seeds      []Seed                        `json:"seeds,omitempty"`
	Window     string                        `json:"seed_window,omitempty"`
	Releases   map[string]string             `json:"releases,omitempty"`
	Recordings map[string][]RecordingRelease `json:"-"`
	Albums     map[string]discoveryAlbum     `json:"albums"`

	path string
}

// loadedStateVersion is the schema this binary writes and understands.
const loadedStateVersion = 1

// LoadDiscoveryState loads the state or creates an empty one. A missing file is a
// first run, not an error; a corrupt file is quarantined (renamed .bad-*) and the
// run starts fresh, because an unattended weekly job must not stay stuck on a
// damaged cache forever. A relative path is resolved against the working directory
// once, so a cron and a manual run cannot end up with two different states.
func LoadDiscoveryState(path string) (*DiscoveryState, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("state path: %w", err)
	}
	path = abs
	state := &DiscoveryState{
		Version:    loadedStateVersion,
		Releases:   map[string]string{},
		Recordings: map[string][]RecordingRelease{},
		Albums:     map[string]discoveryAlbum{},
		path:       path,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, state); err != nil {
		quarantine := fmt.Sprintf("%s.bad-%d", path, time.Now().Unix())
		if renameErr := os.Rename(path, quarantine); renameErr != nil {
			return nil, fmt.Errorf("state %s is corrupt and cannot be quarantined: %w", path, err)
		}
		fresh := &DiscoveryState{
			Version:    loadedStateVersion,
			Releases:   map[string]string{},
			Recordings: map[string][]RecordingRelease{},
			Albums:     map[string]discoveryAlbum{},
			path:       path,
		}
		return fresh, nil
	}
	if state.Version > loadedStateVersion {
		return nil, fmt.Errorf("state %s was written by a newer version (%d > %d)", path, state.Version, loadedStateVersion)
	}
	if state.Version == 0 {
		state.Version = loadedStateVersion
	}
	if state.Releases == nil {
		state.Releases = map[string]string{}
	}
	if state.Recordings == nil {
		state.Recordings = map[string][]RecordingRelease{}
	}
	if state.Albums == nil {
		state.Albums = map[string]discoveryAlbum{}
	}
	state.path = path
	return state, nil
}

// lockDiscoveryState takes an exclusive advisory lock for the duration of the run:
// the Sunday job and a manual run must not load the same state and then overwrite
// each other's outcomes (last writer wins). The lock is released by the process
// anyway, so a crash leaves no stale lock behind.
func lockDiscoveryState(path string) (func(), error) {
	if path == "" {
		return func() {}, nil
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another discovery run is in progress (%s)", path)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, nil
}

// setAlbumStatus records an outcome. A failure parks the album once the attempts
// reach parkedAt, so the run stops retrying it.
func (s *DiscoveryState) setAlbumStatus(rgID, artist, title string, status discoveryStatus, attempts, parkedAt int, reason string) {
	entry := s.Albums[rgID]
	entry.Artist, entry.Title = artist, title
	entry.Attempts = attempts
	if status == discoFailed || status == discoNoTorrent {
		if parkedAt <= 0 {
			parkedAt = 3
		}
		if entry.Attempts >= parkedAt {
			status = discoParked
		}
	}
	entry.Status, entry.Reason, entry.UpdatedAt = status, reason, time.Now()
	s.Albums[rgID] = entry
}

// MergeImportState warms the release cache with the answers the import tool already
// paid for: the same Plex album was resolved to its release group once, and a weekly
// discovery run must not ask MusicBrainz three thousand times again.
func (s *DiscoveryState) MergeImportState(imports *State) {
	if imports == nil {
		return
	}
	for _, entry := range imports.Albums {
		if entry.ReleaseID != "" && entry.ReleaseGroupID != "" {
			if _, ok := s.Releases[entry.ReleaseID]; !ok {
				s.Releases[entry.ReleaseID] = entry.ReleaseGroupID
			}
		}
	}
}

// recordingCache answers a recording resolution already paid for.
func (s *DiscoveryState) recordingCache(recordingMBID string) ([]RecordingRelease, bool) {
	releases, ok := s.Recordings[recordingMBID]
	return releases, ok
}

func (s *DiscoveryState) setRecordingCache(recordingMBID string, releases []RecordingRelease) {
	s.Recordings[recordingMBID] = releases
}

// Save writes the state atomically: a crash mid-run must not truncate the cache.
func (s *DiscoveryState) Save() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temp, s.path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// recommendation is one surviving LB recording with the releases it maps to.
type recommendation struct {
	ArtistName string
	ArtistMBID string
	Recordings []string
	Releases   []RecordingRelease
	Listens    int
}

// candidate is one album the discovery decided to try, with the evidence behind it.
type candidate struct {
	Artist     string
	ArtistMBID string
	Title      string
	RGID       string
	ReleaseID  string
	Recordings []string
	Listens    int
}

// buildCandidates groups the recommended recordings by release group, drops every
// release that is not an allowed studio type (no live, no compilation), and orders
// the albums by the strength of the recommendation: more recommended tracks first,
// then summed global popularity. The release chosen for the tracklist is the oldest
// official edition, the stable original.
func buildCandidates(recs []recommendation, allowed map[string]bool) []candidate {
	type bucket struct {
		cand       candidate
		recordings map[string]bool
		listens    int
		best       *RecordingRelease
	}
	buckets := map[string]*bucket{}
	for _, rec := range recs {
		for _, release := range rec.Releases {
			if release.ReleaseID == "" || release.ReleaseGroupID == "" {
				continue
			}
			if !allowed[release.PrimaryType] || len(release.SecondaryTypes) > 0 {
				continue
			}
			b := buckets[release.ReleaseGroupID]
			if b == nil {
				b = &bucket{
					cand: candidate{
						Artist:     rec.ArtistName,
						ArtistMBID: rec.ArtistMBID,
						Title:      release.ReleaseGroupTitle,
						RGID:       release.ReleaseGroupID,
						ReleaseID:  release.ReleaseID,
					},
					recordings: map[string]bool{},
				}
				buckets[release.ReleaseGroupID] = b
			}
			for _, recording := range rec.Recordings {
				if b.recordings[recording] {
					continue
				}
				b.recordings[recording] = true
				b.cand.Recordings = append(b.cand.Recordings, recording)
				b.listens += rec.Listens
			}
			if b.best == nil || betterRelease(release, *b.best) {
				r := release
				b.best = &r
				b.cand.ReleaseID = release.ReleaseID
			}
		}
	}
	out := make([]candidate, 0, len(buckets))
	for _, b := range buckets {
		b.cand.Listens = b.listens
		out = append(out, b.cand)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Recordings) != len(out[j].Recordings) {
			return len(out[i].Recordings) > len(out[j].Recordings)
		}
		if out[i].Listens != out[j].Listens {
			return out[i].Listens > out[j].Listens
		}
		return out[i].RGID < out[j].RGID
	})
	return out
}

// betterRelease orders the editions of one group for the tracklist: official first,
// then dated before undated, then the oldest.
func betterRelease(a, b RecordingRelease) bool {
	aOfficial, bOfficial := strings.EqualFold(a.Status, "official"), strings.EqualFold(b.Status, "official")
	if aOfficial != bOfficial {
		return aOfficial
	}
	if (a.Date != "") != (b.Date != "") {
		return a.Date != ""
	}
	return a.Date < b.Date
}

// allowedTypes is the set of primary types the discovery accepts.
func allowedTypes(types []string) map[string]bool {
	allowed := make(map[string]bool, len(types))
	for _, t := range types {
		allowed[t] = true
	}
	return allowed
}

// discoverPlex is the slice of Plex the runner needs.
type discoverPlex interface {
	History(ctx context.Context, after time.Time) ([]Play, error)
	Artists(ctx context.Context, section string) ([]Artist, error)
	Albums(ctx context.Context, section string) ([]Album, error)
}

// discoverBrainz is the slice of MusicBrainz the runner needs.
type discoverBrainz interface {
	artistSearcher
	RecordingReleases(ctx context.Context, recordingMBID string) ([]RecordingRelease, error)
	ReleaseGroupOfRelease(ctx context.Context, releaseID string) (string, bool, error)
	ReleaseDetails(ctx context.Context, releaseID string) (ReleaseGroup, []ReleaseTrack, bool, error)
}

// discoverListen is the slice of ListenBrainz the runner needs.
type discoverListen interface {
	RadioArtist(ctx context.Context, seedMBID string, opts RadioOptions) ([]LBTrack, error)
}

// discoverLibrary is the slice of the Library API the runner needs.
type discoverLibrary interface {
	libraryWriter
	Committed(ctx context.Context) (CommittedSet, error)
}

// DiscoverOptions is one discovery run: the knobs, from config or from the command line.
type DiscoverOptions struct {
	Section        string
	Sections       []Section
	SeedOpts       SeedOptions
	Radio          RadioOptions
	MinListenCount int
	AlbumTypes     []string
	MaxAlbums      int
	MaxPerArtist   int
	MaxAttempts    int
	MinSeeders     int
	MaxSizeBytes   int64
	IndexerIDs     []int
	IDStyle        string
	Pace           time.Duration
	DryRun         bool
	Logf           func(string, ...any)
	Now            func() time.Time
	Sleep          func(ctx context.Context, d time.Duration) error
}

// DiscoverRunner walks one discovery run.
type DiscoverRunner struct {
	Plex    discoverPlex
	Brainz  discoverBrainz
	Listen  discoverListen
	Indexer torrentSearcher
	Library discoverLibrary
	State   *DiscoveryState
	Options DiscoverOptions

	logf func(string, ...any)
}

// DiscoverSummary is what a run did, for the log and the job status.
type DiscoverSummary struct {
	Seeds      int
	Window     string
	Candidates int
	Present    int
	Imported   int
	Planned    int
	NoTorrent  int
	Failed     int
	Parked     int
	Notes      []string
}

// Run executes one discovery pass: seeds, similar artists, album candidates, dedup,
// then paced imports. A failed candidate never aborts the run; only an unreachable
// source does.
func (r *DiscoverRunner) Run(ctx context.Context) (summary DiscoverSummary, err error) {
	unlock, err := lockDiscoveryState(r.State.path)
	if err != nil {
		return summary, err
	}
	defer unlock()
	// The state is written on every exit, errors and stops included: the outcomes
	// and attempts of this run must not be lost.
	defer func() {
		if saveErr := r.State.Save(); saveErr != nil && err == nil {
			err = saveErr
		}
	}()
	logf := r.Options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	r.logf = logf
	sleep := r.Options.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	now := time.Now()
	if r.Options.Now != nil {
		now = r.Options.Now()
	}
	if r.Options.MaxAttempts <= 0 {
		r.Options.MaxAttempts = 3
	}

	// 1. Seeds.
	seeds, window, err := collectSeeds(ctx, r.Plex, r.Brainz, seedSections(r.Options.Section, r.Options.Sections), r.Options.SeedOpts, now, logf)
	if err != nil {
		return summary, err
	}
	summary.Seeds, summary.Window = len(seeds), windowLabel(window)
	r.State.Seeds, r.State.Window = seeds, windowLabel(window)
	logf("seeds: %d artists from the %s window", len(seeds), windowLabel(window))
	if len(seeds) == 0 {
		return summary, nil
	}

	// 2. Similar artists and their releases.
	// A seed whose radio fails is skipped; only a ListenBrainz that fails every seed
	// ends the run.
	var recs []recommendation
	var lastErr error
	answered := 0
	for _, seed := range seeds {
		tracks, err := r.Listen.RadioArtist(ctx, seed.MBID, r.Options.Radio)
		if err != nil {
			if ctx.Err() != nil {
				return summary, ctx.Err()
			}
			lastErr = err
			logf("listenbrainz seed %s: %v, skipped", seed.Name, err)
			continue
		}
		answered++
		kept := 0
		for _, track := range tracks {
			if track.ListenCount < r.Options.MinListenCount {
				continue
			}
			releases, err := r.recordingReleases(ctx, track.RecordingMBID)
			if err != nil {
				logf("recording %s: %v", track.RecordingMBID, err)
				continue
			}
			if len(releases) == 0 {
				continue
			}
			recs = append(recs, recommendation{
				ArtistName: track.ArtistName,
				ArtistMBID: track.ArtistMBID,
				Recordings: []string{track.RecordingMBID},
				Releases:   releases,
				Listens:    track.ListenCount,
			})
			kept++
		}
		logf("seed %s: %d recordings kept above %d listens", seed.Name, kept, r.Options.MinListenCount)
	}

	if answered == 0 {
		return summary, fmt.Errorf("listenbrainz: every seed failed: %w", lastErr)
	}

	// 3. Album candidates.
	cands := buildCandidates(recs, allowedTypes(r.Options.AlbumTypes))
	summary.Candidates = len(cands)
	logf("candidates: %d albums from %d recordings", len(cands), len(recs))

	// 4. Dedup against Plex and against what Tiramisu already filed.
	committed, err := r.Library.Committed(ctx)
	if err != nil {
		return summary, fmt.Errorf("library: %w", err)
	}
	index, err := BuildLibraryIndex(ctx, r.Plex, r.Options.Sections, r.resolveReleaseGroup, committed, logf)
	if err != nil {
		return summary, err
	}

	// 5. Paced imports. The pause follows a real import only: a failed attempt
	// downloaded nothing. Attempts are capped so a week of dead swarms does not search
	// Prowlarr for every candidate.
	perArtist := map[string]int{}
	attempted, pauseDue := 0, false
	for _, cand := range cands {
		if summary.Imported+summary.Planned >= r.Options.MaxAlbums {
			break
		}
		if attempted >= r.Options.MaxAlbums*triesPerAlbum {
			logf("attempt cap reached (%d)", attempted)
			break
		}
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		// The artist cap counts imports and goes first: it is free, the dedup may cost
		// MusicBrainz calls.
		if perArtist[cand.ArtistMBID] >= r.Options.MaxPerArtist {
			continue
		}
		if r.alreadySeen(ctx, cand, index, now) {
			continue
		}
		if pauseDue {
			if err := sleep(ctx, r.Options.Pace); err != nil {
				return summary, err
			}
		}
		attempted++
		imported, planned := summary.Imported, summary.Planned
		r.importCandidate(ctx, cand, &summary, logf)
		pauseDue = summary.Imported > imported
		if summary.Imported > imported || summary.Planned > planned {
			perArtist[cand.ArtistMBID]++
		}
	}
	return summary, nil
}

// triesPerAlbum bounds the attempts of a run to this many per album of the cap.
const triesPerAlbum = 3

// seedSections lists the sections the seed index reads: the configured one first,
// so it wins a name two sections disagree on, then every other artist section.
func seedSections(configured string, sections []Section) []string {
	keys := make([]string, 0, len(sections)+1)
	if configured != "" {
		keys = append(keys, configured)
	}
	for _, s := range sections {
		if s.Key != configured {
			keys = append(keys, s.Key)
		}
	}
	return keys
}

// RunDryRun is Run with no pacing and no Library API write: the plan (including the
// torrent each album would take) is printed, nothing is imported.
func (r *DiscoverRunner) RunDryRun(ctx context.Context, dry bool) (DiscoverSummary, error) {
	r.Options.DryRun = dry
	if dry {
		r.Options.Pace = 0
	}
	return r.Run(ctx)
}

// recordingReleases resolves a recording through the state cache: the answer never
// changes and MusicBrainz is the slowest step of the run.
func (r *DiscoverRunner) recordingReleases(ctx context.Context, recordingMBID string) ([]RecordingRelease, error) {
	if releases, ok := r.State.recordingCache(recordingMBID); ok {
		return releases, nil
	}
	releases, err := r.Brainz.RecordingReleases(ctx, recordingMBID)
	if err != nil {
		return nil, err
	}
	r.State.setRecordingCache(recordingMBID, releases)
	return releases, nil
}

// indexCheckpoint is how many release-group resolutions pass between two state
// flushes, so a stopped run keeps what it paid MusicBrainz for.
const indexCheckpoint = 100

// resolveReleaseGroup answers from the cache first; an empty answer is cached too, so
// a release that does not resolve is never asked twice.
func (r *DiscoverRunner) resolveReleaseGroup(ctx context.Context, releaseID string) (string, bool, error) {
	if rg, ok := r.State.Releases[releaseID]; ok {
		return rg, rg != "", nil
	}
	rg, found, err := r.Brainz.ReleaseGroupOfRelease(ctx, releaseID)
	if err != nil {
		return "", false, err
	}
	r.State.Releases[releaseID] = rg
	if len(r.State.Releases)%indexCheckpoint == 0 {
		r.indexLogf("index progress: %d release groups resolved", len(r.State.Releases))
		if err := r.State.Save(); err != nil {
			r.indexLogf("state save: %v", err)
		}
	}
	return rg, found, nil
}

// indexLogf logs through the run's logger, or stays silent when the runner is used
// without Run (tests).
func (r *DiscoverRunner) indexLogf(format string, args ...any) {
	if r.logf != nil {
		r.logf(format, args...)
	}
}

// parkedRetryAfter is how long a parked album stays parked: torrents appear and
// listening comes back, so a failure is a verdict on the swarm of that month, not a
// life sentence.
const parkedRetryAfter = 90 * 24 * time.Hour

// alreadySeen decides whether a candidate can be attempted: present in the libraries
// or in Tiramisu, parked after too many failures (until the retry window passes), or
// already handled by this run.
func (r *DiscoverRunner) alreadySeen(ctx context.Context, cand candidate, index *LibraryIndex, now time.Time) bool {
	if entry, ok := r.State.Albums[cand.RGID]; ok {
		switch entry.Status {
		case discoImported, discoPresent:
			return true
		case discoParked:
			if now.Sub(entry.UpdatedAt) < parkedRetryAfter {
				return true
			}
			delete(r.State.Albums, cand.RGID)
		}
	}
	index.ResolveArtist(ctx, cand.ArtistMBID, cand.Artist)
	if index.AlbumPresent(cand.RGID, cand.Artist, cand.Title) || index.CommittedAlbum(cand.Artist, cand.Title, cand.RGID) {
		r.mark(cand, discoPresent, "already in the library")
		return true
	}
	return false
}

// importCandidate runs the mouth of the pipeline for one album: search, tracklist,
// inspect, file. Failures are recorded, never fatal. In dry-run the search still runs
// (the plan must name the torrent) but nothing is written.
func (r *DiscoverRunner) importCandidate(ctx context.Context, cand candidate, summary *DiscoverSummary, logf func(string, ...any)) {
	torrent, ok := selectAlbumTorrent(ctx, r.Indexer, r.Options.IndexerIDs, cand.Artist, cand.Title, r.Options.MinSeeders, r.Options.MaxSizeBytes, logf)
	if !ok {
		summary.NoTorrent++
		if r.markAttempt(cand, discoNoTorrent, "no lossless torrent above the seeder floor") {
			summary.Parked++
		}
		logf("no torrent for %s / %s", cand.Artist, cand.Title)
		return
	}
	if r.Options.DryRun {
		summary.Planned++
		summary.Notes = append(summary.Notes, fmt.Sprintf("would import %s / %s: %s (%d seeders)", cand.Artist, cand.Title, torrent.Title, torrent.Seeders))
		logf("would import %s / %s: %s", cand.Artist, cand.Title, torrent.Title)
		return
	}
	group := ReleaseGroup{ID: cand.RGID, Artist: cand.Artist, Title: cand.Title}
	_, tracks, ok, err := r.Brainz.ReleaseDetails(ctx, cand.ReleaseID)
	if err != nil {
		logf("tracklist %s: %v", cand.ReleaseID, err)
	}
	if !ok {
		tracks = nil
	}
	result, err := applyFiles(ctx, r.Library, cand.Artist, cand.Title, group, tracks, torrent, r.Options.IDStyle)
	if err != nil {
		summary.Failed++
		if r.markAttempt(cand, discoFailed, err.Error()) {
			summary.Parked++
		}
		logf("import %s / %s: %v", cand.Artist, cand.Title, err)
		return
	}
	if result.AlreadyPresent {
		summary.Present++
		r.mark(cand, discoPresent, "already in the library")
		return
	}
	summary.Imported++
	r.mark(cand, discoImported, "imported "+torrent.Title)
	logf("imported %s / %s: %s", cand.Artist, cand.Title, torrent.Title)
}

// mark records a non-failure outcome (no attempt counted).
func (r *DiscoverRunner) mark(cand candidate, status discoveryStatus, reason string) {
	r.State.setAlbumStatus(cand.RGID, cand.Artist, cand.Title, status, r.State.Albums[cand.RGID].Attempts, r.Options.MaxAttempts, reason)
}

// markAttempt counts one attempt and lets the state park the album at the cap; it
// reports whether the album was parked.
func (r *DiscoverRunner) markAttempt(cand candidate, status discoveryStatus, reason string) bool {
	attempts := r.State.Albums[cand.RGID].Attempts + 1
	r.State.setAlbumStatus(cand.RGID, cand.Artist, cand.Title, status, attempts, r.Options.MaxAttempts, reason)
	return r.State.Albums[cand.RGID].Status == discoParked
}

// sleepCtx waits unless the job is stopped.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
