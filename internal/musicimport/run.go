package musicimport

import (
	"context"
	"fmt"
	"strings"

	"tiramisu/internal/library"
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
	// RetryFailed walks again the albums a previous run left without a torrent, an
	// identity or with an error. Off, a restart resumes where the last run stopped.
	RetryFailed bool
	Logf        func(format string, args ...interface{})
}

// Summary is the run's tally and the per-album notes worth reading.
type Summary struct {
	Albums     int
	Skipped    int // settled by an earlier run and not retried
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
		case StatusNoMatch, StatusNoIdentity, StatusError:
			if !r.Options.RetryFailed {
				summary.Skipped++
				continue
			}
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

// trackIDForStyle picks the id a projection carries for one track: the release track
// id for Plex webhooks, the recording id for Jellyfin.
func trackIDForStyle(track ReleaseTrack, fallback, style string) string {
	if strings.EqualFold(style, "recording") && track.Recording != "" {
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

func (r *Runner) trackIdentity(track ReleaseTrack, fallback string) string {
	return trackIDForStyle(track, fallback, r.Options.IDStyle)
}

// torrentSearcher is the slice of the Prowlarr client the search needs. An interface
// so the discovery runner is testable without a server.
type torrentSearcher interface {
	SearchWithOptions(ctx context.Context, query string, opts prowlarr.SearchOptions) ([]prowlarr.ProwlarrResult, error)
	ResolveHash(downloadURL string) string
}

// selectAlbumTorrent is the search the importer and the discovery share: the lossless
// query first, the plain one second, first candidate that clears the floor and whose
// hash resolves.
// releaseFetcher is the Prowlarr side that fetches a release's .torrent or magnet.
type releaseFetcher interface {
	FetchTorrent(ctx context.Context, downloadURL string) (library.TorrentSource, error)
}

// withRelease attaches the release's .torrent and trackers to a candidate, when the
// indexer can fetch them and they are the release the candidate names.
func withRelease(ctx context.Context, indexer torrentSearcher, c Candidate, logf func(string, ...any)) Candidate {
	f, ok := indexer.(releaseFetcher)
	if !ok || c.DownloadURL == "" {
		return c
	}
	src, err := f.FetchTorrent(ctx, c.DownloadURL)
	if err != nil {
		logf("release file for %q: %v", c.Title, err)
		return c
	}
	if !strings.EqualFold(src.Hash, c.Hash) {
		return c
	}
	c.TorrentFile, c.Trackers = src.File, src.Trackers
	return c
}

// releaseRef is how a candidate is named to the Library API: its hash, or a magnet
// carrying the indexer's trackers when those came without a .torrent.
func releaseRef(c Candidate) string {
	if len(c.TorrentFile) == 0 && len(c.Trackers) > 0 {
		return library.BuildMagnet(c.Hash, c.Title, library.MergeTrackers(library.DefaultTrackers(), c.Trackers))
	}
	return c.Hash
}

func selectAlbumTorrent(ctx context.Context, indexer torrentSearcher, indexerIDs []int, artist, title string, minSeeders int, maxSizeBytes int64, logf func(string, ...any)) (Candidate, bool) {
	artist, title = asciiPunctuation(artist), asciiPunctuation(title)
	for _, query := range []string{
		strings.TrimSpace(artist + " " + title + " FLAC"),
		strings.TrimSpace(artist + " " + title),
	} {
		results, err := indexer.SearchWithOptions(ctx, query, prowlarr.SearchOptions{IndexerIDs: indexerIDs})
		if err != nil {
			logf("prowlarr %q: %v", query, err)
			continue
		}
		for _, candidate := range SelectCandidates(results, artist, title, minSeeders, maxSizeBytes, 3) {
			if candidate.Hash == "" {
				hash := indexer.ResolveHash(candidate.DownloadURL)
				if hash == "" {
					logf("cannot resolve a hash for %q", candidate.Title)
					continue
				}
				candidate.Hash = strings.ToLower(hash)
			}
			return withRelease(ctx, indexer, candidate, logf), true
		}
	}
	return Candidate{}, false
}

// typographicPunctuation maps the dashes and quotes MusicBrainz spells names with to
// the ASCII the indexers match; letters, accents included, are left alone.
var typographicPunctuation = strings.NewReplacer(
	"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-", "\u2014", "-", "\u2015", "-",
	"\u2018", "'", "\u2019", "'", "\u02bc", "'", "\u2032", "'",
	"\u201c", `"`, "\u201d", `"`, "\u2026", "...",
)

func asciiPunctuation(s string) string { return typographicPunctuation.Replace(s) }

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
	return selectAlbumTorrent(ctx, r.Indexer, r.Options.IndexerIDs, artist, title, r.Options.MinSeeders, r.Options.MaxSizeBytes, r.logf)
}

// libraryWriter is what applyFiles needs from the Library API client.
type libraryWriter interface {
	Inspect(ctx context.Context, hash, title string, torrentFile []byte) ([]SourceFile, error)
	Add(ctx context.Context, hash, title string, torrentFile []byte, files []AddFile) (AddResult, error)
}

// applyFiles inspects the torrent and files its lossless files as projections, each
// with its own track id when the tracklist matches and the group id otherwise.
// Shared by the importer and the discovery.
func applyFiles(ctx context.Context, library libraryWriter, artist, title string, group ReleaseGroup, tracks []ReleaseTrack, candidate Candidate, idStyle string) (AddResult, error) {
	files, err := library.Inspect(ctx, releaseRef(candidate), artist+" - "+title, candidate.TorrentFile)
	if err != nil {
		return AddResult{}, fmt.Errorf("inspect %s: %w", candidate.Hash, err)
	}
	adds := make([]AddFile, 0, len(files))
	for _, file := range files {
		if !strings.HasSuffix(strings.ToLower(file.SourcePath), ".flac") {
			continue
		}
		// An album image is filed track by track, cut by its cue sheet.
		if len(file.CueTracks) > 1 {
			adds = append(adds, cueTrackAdds(artist, title, candidate.Hash, file, group, tracks, idStyle)...)
			continue
		}
		externalID := group.ID
		if track, ok := matchTrack(file.SourcePath, tracks); ok {
			externalID = trackIDForStyle(track, group.ID, idStyle)
		}
		adds = append(adds, AddFile{
			SourcePath:          file.SourcePath,
			Path:                VirtualPath(artist, title, candidate.Hash, file.SourcePath),
			ExternalID:          externalID,
			ExternalIDNamespace: "musicbrainz",
		})
	}
	if len(adds) == 0 {
		return AddResult{}, fmt.Errorf("no FLAC file in %s", candidate.Title)
	}
	result, err := library.Add(ctx, releaseRef(candidate), artist+" - "+title, candidate.TorrentFile, adds)
	if err != nil {
		// The release title stays in the error: the caller logs it verbatim and an
		// anonymous "add failed" costs a manual investigation.
		return AddResult{}, fmt.Errorf("add %s: %w", candidate.Title, err)
	}
	return result, nil
}

// cueTrackAdds files each cue track of an image as its own projection. The track is
// matched to the release tracklist as a file named "NN - Title" in the image's
// directory would be, so the disc a directory names still pins the medium.
func cueTrackAdds(artist, album, hash string, file SourceFile, group ReleaseGroup, tracks []ReleaseTrack, idStyle string) []AddFile {
	dir := ""
	if cut := strings.LastIndexByte(file.SourcePath, '/'); cut >= 0 {
		dir = file.SourcePath[:cut+1]
	}
	disc, _ := trackNumbers(dir + "x.flac")
	matched := matchCueTracks(file.CueTracks, tracks, disc)
	adds := make([]AddFile, 0, len(file.CueTracks))
	for _, cue := range file.CueTracks {
		name := cue.Title
		if name == "" {
			name = fmt.Sprintf("Track %02d", cue.Track)
		}
		externalID := group.ID
		tags := map[string]string{"ARTIST": artist, "ALBUMARTIST": artist, "ALBUM": album, "TRACKNUMBER": fmt.Sprint(cue.Track)}
		if group.ID != "" {
			tags["MUSICBRAINZ_RELEASEGROUPID"] = group.ID
		}
		if track, ok := matched[cue.Track]; ok {
			externalID = trackIDForStyle(track, group.ID, idStyle)
			if track.Title != "" {
				name = track.Title
			}
			if track.Medium > 0 {
				tags["DISCNUMBER"] = fmt.Sprint(track.Medium)
			}
			if track.ID != "" {
				tags["MUSICBRAINZ_RELEASETRACKID"] = track.ID
			}
			if track.Recording != "" {
				tags["MUSICBRAINZ_TRACKID"] = track.Recording
			}
		}
		tags["TITLE"] = name
		virtual := dir + library.SafeComponent(fmt.Sprintf("%02d - %s", cue.Track, name)) + ".flac"
		adds = append(adds, AddFile{
			SourcePath:          file.SourcePath,
			Path:                VirtualPath(artist, album, hash, virtual),
			ExternalID:          externalID,
			ExternalIDNamespace: "musicbrainz",
			CueTrack:            cue.Track,
			Tags:                tags,
		})
	}
	return adds
}

// matchCueTracks pairs cue tracks with release tracks by title first: an image of
// another edition (a bonus track, a different running order) shifts positions, so a
// number alone would hand one track another's id. Position decides only for a cue
// track without a title. A release track is never given to two cue tracks; an
// unmatched cue track gets none, which is better than a wrong one.
func matchCueTracks(cues []CueTrack, tracks []ReleaseTrack, disc int) map[int]ReleaseTrack {
	out := map[int]ReleaseTrack{}
	claimed := map[int]bool{}
	onDisc := func(t ReleaseTrack) bool { return disc == 0 || t.Medium == disc }
	for _, cue := range cues {
		want := strings.TrimSpace(normalizeTitle(cue.Title))
		if want == "" {
			continue
		}
		best := -1
		for i, t := range tracks {
			if claimed[i] || !onDisc(t) || strings.TrimSpace(normalizeTitle(t.Title)) != want {
				continue
			}
			// Two media can repeat a title: the one at the cue's position wins, then
			// the first medium.
			if best < 0 || positionIs(t, cue.Track) && !positionIs(tracks[best], cue.Track) {
				best = i
			}
		}
		if best >= 0 {
			claimed[best] = true
			out[cue.Track] = tracks[best]
		}
	}
	for _, cue := range cues {
		if _, done := out[cue.Track]; done || strings.TrimSpace(normalizeTitle(cue.Title)) != "" {
			continue
		}
		for i, t := range tracks {
			if !claimed[i] && onDisc(t) && positionIs(t, cue.Track) && (disc > 0 || t.Medium <= 1) {
				claimed[i] = true
				out[cue.Track] = t
				break
			}
		}
	}
	return out
}

func positionIs(t ReleaseTrack, n int) bool {
	p, ok := numericPosition(t.Position)
	return ok && p == n
}

// apply inspects the torrent and files its lossless files as projections. Each file
// carries its own track id when the release tracklist could be read: that is the id
// Plex sends in a webhook, so the projection and the player finally speak the same
// id. Files the tracklist cannot be matched by keep the album's release group.
func (r *Runner) apply(ctx context.Context, album Album, group ReleaseGroup, tracks []ReleaseTrack, candidate Candidate, entry *Entry) error {
	result, err := applyFiles(ctx, r.Library, album.Artist, album.Title, group, tracks, candidate, r.Options.IDStyle)
	if err != nil {
		return err
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
