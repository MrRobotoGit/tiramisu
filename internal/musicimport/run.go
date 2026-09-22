package musicimport

import (
	"context"
	"fmt"
	"strings"

	"tiramisu/internal/prowlarr"
)

// Options controls one run.
type Options struct {
	Section    string
	IndexerIDs []int
	// IDStyle picks which MusicBrainz id a projection carries: "track" (default) is
	// the release's track id, the one Plex sends in webhooks; "recording" is the
	// recording id, the one Jellyfin exposes as MusicBrainzTrack.
	IDStyle      string
	MinSeeders   int
	MaxSizeBytes int64
	Limit        int
	Apply        bool
	Logf         func(format string, args ...interface{})
}

// Summary is the run's tally and the per-album notes worth reading.
type Summary struct {
	Albums     int
	Present    int
	NoIdentity int
	NoMatch    int
	Selected   int
	Applied    int
	Errors     int
	Notes      []string
}

// Runner wires the four sides of the import: Plex, MusicBrainz, Prowlarr and the
// Library API.
type Runner struct {
	Plex    *PlexClient
	Brainz  *MusicBrainz
	Indexer *prowlarr.Client
	Library *Tiramisu
	State   *State
	Options Options
}

func (r *Runner) logf(format string, args ...interface{}) {
	if r.Options.Logf != nil {
		r.Options.Logf(format, args...)
	}
}

// Run walks the Plex albums once. It never mutates the library unless Apply is set.
func (r *Runner) Run(ctx context.Context) (Summary, error) {
	var summary Summary
	committed, err := r.Library.Committed(ctx)
	if err != nil {
		return summary, fmt.Errorf("read the library: %w", err)
	}
	albums, err := r.Plex.Albums(ctx, r.Options.Section)
	if err != nil {
		return summary, fmt.Errorf("read Plex: %w", err)
	}
	r.logf("albums in Plex: %d, album prefixes already in the library: %d", len(albums), len(committed.AlbumPrefixes))

	for _, album := range albums {
		if ctx.Err() != nil {
			break
		}
		if r.Options.Limit > 0 && summary.Albums >= r.Options.Limit {
			break
		}
		summary.Albums++
		// Progress is persisted every few albums: a long import must survive a kill
		// without losing hours of MusicBrainz and Prowlarr answers.
		if summary.Albums%5 == 0 {
			if err := r.State.Save(); err != nil {
				r.logf("state save: %v", err)
			}
			r.logf("progress: %d processed, %d applied, %d present, %d no-match, %d errors",
				summary.Albums, summary.Applied+summary.Present, summary.Present, summary.NoMatch, summary.Errors)
		}

		entry, cached := r.State.Get(album.RatingKey)
		if !cached || entry.Artist == "" {
			entry = Entry{Artist: album.Artist, Title: album.Title, ReleaseID: album.ReleaseID, Status: StatusPending}
		}
		switch entry.Status {
		case StatusApplied, StatusAlreadyPresent:
			summary.Present++
			continue
		}

		group, tracks, ok, err := r.resolveAlbum(ctx, album, &entry)
		if err != nil {
			r.fail(&summary, album, &entry, err)
			continue
		}
		if !ok {
			r.finish(&summary, album, &entry, StatusNoIdentity, "no MusicBrainz release group")
			continue
		}
		entry.ReleaseGroupID = group.ID

		if committed.HasAlbum(album.Artist, album.Title, group.ID) {
			summary.Present++
			r.finish(&summary, album, &entry, StatusAlreadyPresent, "already in the library")
			continue
		}

		candidate, ok := r.selectTorrent(ctx, album, group, &entry)
		if !ok {
			r.finish(&summary, album, &entry, StatusNoMatch, "no lossless torrent above the seeder floor")
			continue
		}

		if !r.Options.Apply {
			entry.Status = StatusSelected
			entry.TorrentHash = candidate.Hash
			entry.TorrentTitle = candidate.Title
			entry.Seeders = candidate.Seeders
			r.State.Set(album.RatingKey, entry)
			summary.Selected++
			summary.Notes = append(summary.Notes, fmt.Sprintf("would add %s / %s: %s (%d seeders, %.0f MB)",
				album.Artist, album.Title, candidate.Title, candidate.Seeders, float64(candidate.Size)/(1<<20)))
			continue
		}

		if err := r.apply(ctx, album, group, tracks, candidate, &entry); err != nil {
			r.fail(&summary, album, &entry, err)
			continue
		}
		entry.Status = StatusApplied
		entry.TorrentHash = candidate.Hash
		entry.TorrentTitle = candidate.Title
		entry.Seeders = candidate.Seeders
		r.State.Set(album.RatingKey, entry)
		summary.Applied++
		summary.Notes = append(summary.Notes, fmt.Sprintf("added %s / %s: %s", album.Artist, album.Title, candidate.Title))
	}
	if err := r.State.Save(); err != nil {
		return summary, fmt.Errorf("save state: %w", err)
	}
	return summary, nil
}

// resolveAlbum answers from the state first: the MusicBrainz lookup is the slowest
// step in the pipeline and its answer never changes. It returns the release's
// tracklist when it has one, which is where the per-file track ids live.
func (r *Runner) resolveAlbum(ctx context.Context, album Album, entry *Entry) (ReleaseGroup, []ReleaseTrack, bool, error) {
	if entry.ReleaseGroupID != "" {
		return ReleaseGroup{ID: entry.ReleaseGroupID, Artist: entry.Artist, Title: entry.Title}, nil, true, nil
	}
	if album.ReleaseID != "" {
		group, tracks, ok, err := r.Brainz.ReleaseDetails(ctx, album.ReleaseID)
		if err != nil {
			return ReleaseGroup{}, nil, false, err
		}
		if ok {
			return group, tracks, true, nil
		}
	}
	group, ok, err := r.Brainz.SearchReleaseGroup(ctx, album.Artist, album.Title)
	if err != nil {
		return ReleaseGroup{}, nil, false, err
	}
	return group, nil, ok, nil
}

// trackIdentity picks the id a projection carries for one track. The default is the
// release's track id because that is what Plex sends in a music webhook; Jellyfin
// speaks recording ids, exposed as the MusicBrainzTrack provider.
func (r *Runner) trackIdentity(track ReleaseTrack, fallback string) string {
	if strings.EqualFold(r.Options.IDStyle, "recording") && track.Recording != "" {
		return track.Recording
	}
	if track.ID != "" {
		return track.ID
	}
	if track.Recording != "" {
		return track.Recording
	}
	return fallback
}

// selectTorrent tries the explicit lossless query first, then a plain one: some
// releases never spell FLAC in the title but are still lossless inside.
func (r *Runner) selectTorrent(ctx context.Context, album Album, group ReleaseGroup, entry *Entry) (Candidate, bool) {
	artist, title := album.Artist, album.Title
	if artist == "" {
		artist = group.Artist
	}
	if title == "" {
		title = group.Title
	}
	for _, query := range []string{
		strings.TrimSpace(artist + " " + title + " FLAC"),
		strings.TrimSpace(artist + " " + title),
	} {
		results, err := r.Indexer.SearchWithOptions(ctx, query, prowlarr.SearchOptions{IndexerIDs: r.Options.IndexerIDs})
		if err != nil {
			r.logf("prowlarr %q: %v", query, err)
			continue
		}
		for _, candidate := range SelectCandidates(results, artist, title, r.Options.MinSeeders, r.Options.MaxSizeBytes, 3) {
			if candidate.Hash == "" {
				hash := r.Indexer.ResolveHash(candidate.DownloadURL)
				if hash == "" {
					r.logf("cannot resolve a hash for %q", candidate.Title)
					continue
				}
				candidate.Hash = strings.ToLower(hash)
			}
			return candidate, true
		}
	}
	return Candidate{}, false
}

// apply inspects the torrent and files its lossless files as projections. Each file
// carries its own track id when the release tracklist could be read: that is the id
// Plex sends in a webhook, so the projection and the player finally speak the same
// id. Files the tracklist cannot be matched by keep the album's release group.
func (r *Runner) apply(ctx context.Context, album Album, group ReleaseGroup, tracks []ReleaseTrack, candidate Candidate, entry *Entry) error {
	files, err := r.Library.Inspect(ctx, candidate.Hash, album.Artist+" - "+album.Title)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", candidate.Hash, err)
	}
	adds := make([]AddFile, 0, len(files))
	for _, file := range files {
		if !strings.HasSuffix(strings.ToLower(file.SourcePath), ".flac") {
			continue
		}
		externalID := group.ID
		if track, ok := matchTrack(file.SourcePath, tracks); ok {
			externalID = r.trackIdentity(track, group.ID)
		}
		adds = append(adds, AddFile{
			SourcePath:          file.SourcePath,
			Path:                VirtualPath(album.Artist, album.Title, candidate.Hash, file.SourcePath),
			ExternalID:          externalID,
			ExternalIDNamespace: "musicbrainz",
		})
	}
	if len(adds) == 0 {
		return fmt.Errorf("no FLAC file in %s", candidate.Title)
	}
	result, err := r.Library.Add(ctx, candidate.Hash, album.Artist+" - "+album.Title, adds)
	if err != nil {
		return fmt.Errorf("add %s: %w", candidate.Title, err)
	}
	if result.AlreadyPresent {
		entry.Status = StatusAlreadyPresent
	}
	return nil
}

func (r *Runner) finish(summary *Summary, album Album, entry *Entry, status Status, reason string) {
	switch status {
	case StatusNoIdentity:
		summary.NoIdentity++
	case StatusAlreadyPresent:
		// counted by the caller
	case StatusNoMatch:
		summary.NoMatch++
	}
	entry.Status = status
	entry.Reason = reason
	r.State.Set(album.RatingKey, *entry)
}

func (r *Runner) fail(summary *Summary, album Album, entry *Entry, err error) {
	summary.Errors++
	summary.Notes = append(summary.Notes, fmt.Sprintf("error on %s / %s: %v", album.Artist, album.Title, err))
	entry.Status = StatusError
	entry.Reason = err.Error()
	r.State.Set(album.RatingKey, *entry)
}

// VirtualPath builds the engine path for one source file. The release's own
// folders are kept, and the leaf gets the mandatory _<hash8> suffix.
func VirtualPath(artist, album, hash, sourcePath string) string {
	parts := strings.Split(sourcePath, "/")
	if len(parts) > 1 {
		parts = parts[1:]
	}
	relative := strings.Join(parts, "/")
	stem, extension := splitExtension(relative)
	return fmt.Sprintf("%s/%s/%s_%s%s", sanitizeComponent(artist), sanitizeComponent(album), stem, hashSuffix(hash), extension)
}

func splitExtension(path string) (string, string) {
	if index := strings.LastIndexByte(path, '.'); index > 0 {
		return path[:index], path[index:]
	}
	return path, ""
}

func hashSuffix(hash string) string {
	hash = strings.ToLower(hash)
	if len(hash) < 8 {
		return hash
	}
	return hash[len(hash)-8:]
}

func sanitizeComponent(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.TrimSpace(name)
	if name == "" {
		return "Unknown"
	}
	return name
}
