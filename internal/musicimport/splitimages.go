package musicimport

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
)

// splitLibrary is the slice of the Library API the image conversion needs.
type splitLibrary interface {
	libraryWriter
	AudioRows(ctx context.Context) ([]AudioRow, error)
	RemovePath(ctx context.Context, path string) error
}

// releaseReader is the slice of MusicBrainz the conversion needs for tracklists.
type releaseReader interface {
	ReleaseDetails(ctx context.Context, releaseID string) (ReleaseGroup, []ReleaseTrack, bool, error)
}

// SplitSummary is what a conversion did.
type SplitSummary struct {
	Albums    int // albums filed as one file per image
	NoCue     int // images whose torrent has no usable cue sheet
	Converted int
	Planned   int
	Failed    int
	Tracks    int
	Resynced  int // albums already split whose tracks were refiled
	Fixed     int // tracks refiled by the resync
	Notes     []string
}

// SplitImages refiles the albums filed as whole single-file images track by track,
// cut by the cue sheet their torrent carries. The tracks are added before the image
// projection is removed, so an album is never missing from the library; a failed add
// leaves the image as it was.
//
// With resync it also refiles albums already split whose matching changed. That path
// removes a track before re-adding it, so an add that fails loses the track: it is
// opt-in for that reason.
func SplitImages(ctx context.Context, library splitLibrary, brainz releaseReader, imports *State, idStyle string, apply, resyncSplit bool, limit int, logf func(string, ...any)) (SplitSummary, error) {
	var summary SplitSummary
	rows, err := library.AudioRows(ctx)
	if err != nil {
		return summary, fmt.Errorf("library: %w", err)
	}
	albums := imageAlbums(rows)
	summary.Albums = len(albums)
	if resyncSplit {
		albums = append(albums, splitAlbums(rows)...)
	}
	releases := map[string]string{} // torrent hash -> release id, from the import state
	groups := map[string]string{}   // torrent hash -> release group id
	if imports != nil {
		for _, e := range imports.Albums {
			if e.TorrentHash != "" && e.ReleaseID != "" {
				releases[strings.ToLower(e.TorrentHash)] = e.ReleaseID
			}
			if e.TorrentHash != "" && e.ReleaseGroupID != "" {
				groups[strings.ToLower(e.TorrentHash)] = e.ReleaseGroupID
			}
		}
	}
	for _, album := range albums {
		if limit > 0 && summary.Converted+summary.Planned >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		title := album.artist + " - " + album.album
		files, err := library.Inspect(ctx, album.hash, title, nil)
		if err != nil {
			summary.Failed++
			logf("inspect %s: %v", title, err)
			continue
		}
		group := ReleaseGroup{ID: album.groupID, Artist: album.artist, Title: album.album}
		if id := groups[album.hash]; id != "" {
			group.ID = id
		}
		var tracks []ReleaseTrack
		if id := releases[album.hash]; id != "" {
			if g, t, ok, err := brainz.ReleaseDetails(ctx, id); err != nil {
				logf("tracklist %s: %v", title, err)
			} else if ok {
				tracks = t
				if g.ID != "" {
					group.ID = g.ID
				}
			}
		}
		var adds []AddFile
		for _, f := range files {
			if album.sources[f.SourcePath] && len(f.CueTracks) > 1 {
				adds = append(adds, cueTrackAdds(album.artist, album.album, album.hash, f, group, tracks, idStyle)...)
			}
		}
		if album.split != nil {
			resync(ctx, library, album, adds, apply, &summary, logf)
			continue
		}
		if len(adds) == 0 {
			summary.NoCue++
			logf("no cue sheet cuts %s", title)
			continue
		}
		if !apply {
			summary.Planned++
			summary.Tracks += len(adds)
			summary.Notes = append(summary.Notes, fmt.Sprintf("would split %s into %d tracks", title, len(adds)))
			continue
		}
		if _, err := library.Add(ctx, album.hash, title, nil, adds); err != nil {
			summary.Failed++
			logf("add tracks of %s: %v", title, err)
			continue
		}
		for _, p := range album.paths {
			if err := library.RemovePath(ctx, p); err != nil {
				logf("remove image %s: %v", p, err)
			}
		}
		summary.Converted++
		summary.Tracks += len(adds)
		logf("split %s into %d tracks", title, len(adds))
	}
	return summary, nil
}

// resync refiles the tracks of an album already split whose name or id the current
// matching would change. A changed track is removed before its replacement is added:
// the two share the cue track, which the registry holds once.
func resync(ctx context.Context, library splitLibrary, album imageAlbum, adds []AddFile, apply bool, summary *SplitSummary, logf func(string, ...any)) {
	title := album.artist + " - " + album.album
	var changed []AddFile
	var stale []string
	for _, add := range adds {
		old, ok := album.split[cueKey(add.SourcePath, add.CueTrack)]
		if ok && old.Path == add.Path && old.ExternalID == add.ExternalID {
			continue
		}
		changed = append(changed, add)
		if ok {
			stale = append(stale, old.Path)
		}
	}
	if len(changed) == 0 {
		return
	}
	if !apply {
		summary.Notes = append(summary.Notes, fmt.Sprintf("would refile %d tracks of %s", len(changed), title))
		summary.Fixed += len(changed)
		return
	}
	for _, p := range stale {
		if err := library.RemovePath(ctx, p); err != nil {
			logf("remove %s: %v", p, err)
		}
	}
	if _, err := library.Add(ctx, album.hash, title, nil, changed); err != nil {
		summary.Failed++
		logf("refile %d tracks of %s: %v", len(changed), title, err)
		return
	}
	summary.Resynced++
	summary.Fixed += len(changed)
	logf("refiled %d tracks of %s", len(changed), title)
}

func cueKey(source string, track int) string { return fmt.Sprintf("%s\x00%d", source, track) }

// splitAlbums returns the torrents already filed track by track from their images:
// every projection of the hash is a cue track.
func splitAlbums(rows []AudioRow) []imageAlbum {
	byHash := map[string][]AudioRow{}
	for _, r := range rows {
		byHash[strings.ToLower(r.Hash)] = append(byHash[strings.ToLower(r.Hash)], r)
	}
	var out []imageAlbum
	for hash, rs := range byHash {
		ok := true
		for _, r := range rs {
			if r.CueTrack == 0 {
				ok = false
				break
			}
		}
		parts := strings.Split(rs[0].Path, "/")
		if !ok || len(parts) < 3 {
			continue
		}
		// The group id is the fallback every unmatched track carries: the most frequent id.
		counts := map[string]int{}
		for _, r := range rs {
			counts[r.ExternalID]++
		}
		groupID := ""
		for id, n := range counts {
			if n > counts[groupID] || n == counts[groupID] && id < groupID {
				groupID = id
			}
		}
		a := imageAlbum{hash: hash, artist: parts[0], album: parts[1], groupID: groupID, sources: map[string]bool{}, split: map[string]AudioRow{}}
		for _, r := range rs {
			a.sources[r.SourcePath] = true
			a.split[cueKey(r.SourcePath, r.CueTrack)] = r
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].artist+"/"+out[i].album < out[j].artist+"/"+out[j].album })
	return out
}

// imageAlbum is one torrent filed as whole images: every projection of the hash is a
// FLAC alone in its directory (one image per disc).
type imageAlbum struct {
	hash, artist, album, groupID string
	paths                        []string
	sources                      map[string]bool
	split                        map[string]AudioRow // set for an album already split, by cue key
}

func imageAlbums(rows []AudioRow) []imageAlbum {
	perDir := map[string]int{}
	for _, r := range rows {
		perDir[path.Dir(r.Path)]++
	}
	byHash := map[string][]AudioRow{}
	for _, r := range rows {
		byHash[strings.ToLower(r.Hash)] = append(byHash[strings.ToLower(r.Hash)], r)
	}
	var out []imageAlbum
	for hash, rs := range byHash {
		ok := true
		for _, r := range rs {
			if r.CueTrack != 0 || perDir[path.Dir(r.Path)] != 1 || !strings.HasSuffix(strings.ToLower(r.SourcePath), ".flac") {
				ok = false
				break
			}
		}
		parts := strings.Split(rs[0].Path, "/")
		if !ok || len(parts) < 3 {
			continue
		}
		a := imageAlbum{hash: hash, artist: parts[0], album: parts[1], groupID: rs[0].ExternalID, sources: map[string]bool{}}
		for _, r := range rs {
			a.paths = append(a.paths, r.Path)
			a.sources[r.SourcePath] = true
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].artist+"/"+out[i].album < out[j].artist+"/"+out[j].album })
	return out
}
