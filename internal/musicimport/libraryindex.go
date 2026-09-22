package musicimport

import (
	"context"
	"fmt"
)

// LibraryIndex answers what Plex and Tiramisu already hold, at artist and album
// level, so the discovery never imports a duplicate. MusicBrainz ids are the keys
// when present; normalized names are the fallback for what was never matched.
type LibraryIndex struct {
	artistsMBID map[string]bool
	artistNames map[string]bool
	albumsRG    map[string]bool
	albumNames  map[string]bool
	committed   CommittedSet

	// Release groups are resolved lazily, per candidate artist: Plex stores the
	// release, and resolving the whole library costs an hour of MusicBrainz calls.
	resolveRG     func(context.Context, string) (string, bool, error)
	releasesByKey map[string][]string // album artist name key -> Plex release ids
	artistKeys    map[string]string   // artist mbid -> name key, links LB and Plex spellings
	resolvedKeys  map[string]bool
	logf          func(string, ...any)
}

// AddArtist records one artist.
func (ix *LibraryIndex) AddArtist(mbid, name string) {
	if mbid != "" {
		ix.artistsMBID[mbid] = true
	}
	if key := nameKey(name); key != "" {
		ix.artistNames[key] = true
	}
}

// AddAlbum records one album. rgID may be empty when the release group could not be
// resolved; the normalized name still guards against the obvious duplicate.
func (ix *LibraryIndex) AddAlbum(rgID, artist, title string) {
	if rgID != "" {
		ix.albumsRG[rgID] = true
	}
	if key := albumKey(artist, title); key != "" {
		ix.albumNames[key] = true
	}
}

// ArtistPresent reports whether the library has the artist, by id or by name.
func (ix *LibraryIndex) ArtistPresent(mbid, name string) bool {
	if mbid != "" && ix.artistsMBID[mbid] {
		return true
	}
	return name != "" && ix.artistNames[nameKey(name)]
}

// AlbumPresent reports whether the library has the album, by release group or by
// normalized artist+title.
func (ix *LibraryIndex) AlbumPresent(rgID, artist, title string) bool {
	if rgID != "" && ix.albumsRG[rgID] {
		return true
	}
	key := albumKey(artist, title)
	return key != "" && ix.albumNames[key]
}

// ResolveArtist resolves the release groups of the Plex albums of one artist, found
// by name or by the MBID Plex holds for the artist, once per run. A failed lookup
// leaves that album to the name fallback.
func (ix *LibraryIndex) ResolveArtist(ctx context.Context, mbid, name string) {
	if ix.resolveRG == nil {
		return
	}
	if ix.resolvedKeys == nil {
		ix.resolvedKeys = map[string]bool{}
	}
	keys := []string{nameKey(name)}
	if key := ix.artistKeys[mbid]; mbid != "" && key != "" {
		keys = append(keys, key)
	}
	for _, key := range keys {
		if key == "" || ix.resolvedKeys[key] {
			continue
		}
		ix.resolvedKeys[key] = true
		for _, releaseID := range ix.releasesByKey[key] {
			if ctx.Err() != nil {
				return
			}
			rgID, ok, err := ix.resolveRG(ctx, releaseID)
			if err != nil {
				if ix.logf != nil {
					ix.logf("release group %s: %v", releaseID, err)
				}
				continue
			}
			if ok {
				ix.albumsRG[rgID] = true
			}
		}
	}
}

// CommittedAlbum covers what Tiramisu has filed but Plex has not scanned yet.
func (ix *LibraryIndex) CommittedAlbum(artist, title, identity string) bool {
	return ix.committed.HasAlbum(artist, title, identity)
}

func albumKey(artist, title string) string {
	a, t := nameKey(artist), nameKey(title)
	if a == "" || t == "" {
		return ""
	}
	return a + "|" + t
}

// albumSource is the slice of Plex the index builder walks.
type albumSource interface {
	Artists(ctx context.Context, section string) ([]Artist, error)
	Albums(ctx context.Context, section string) ([]Album, error)
}

// BuildLibraryIndex walks every artist section of Plex and indexes albums by name.
// The release ids (Plex stores the release, not the group) are kept for ResolveArtist,
// which resolves them through the given caching resolver only for candidate artists.
func BuildLibraryIndex(ctx context.Context, plex albumSource, sections []Section, resolveRG func(context.Context, string) (string, bool, error), committed CommittedSet, logf func(string, ...any)) (*LibraryIndex, error) {
	ix := &LibraryIndex{
		artistsMBID: map[string]bool{},
		artistNames: map[string]bool{},
		albumsRG:    map[string]bool{},
		albumNames:  map[string]bool{},
		committed:   committed,

		resolveRG:     resolveRG,
		releasesByKey: map[string][]string{},
		artistKeys:    map[string]string{},
		resolvedKeys:  map[string]bool{},
		logf:          logf,
	}
	for _, section := range sections {
		artists, err := plex.Artists(ctx, section.Key)
		if err != nil {
			return nil, fmt.Errorf("plex artists %s: %w", section.Title, err)
		}
		for _, a := range artists {
			ix.AddArtist(a.MBID, a.Name)
			if key := nameKey(a.Name); a.MBID != "" && key != "" {
				ix.artistKeys[a.MBID] = key
			}
		}

		albums, err := plex.Albums(ctx, section.Key)
		if err != nil {
			return nil, fmt.Errorf("plex albums %s: %w", section.Title, err)
		}
		for _, album := range albums {
			ix.AddAlbum("", album.Artist, album.Title)
			if key := nameKey(album.Artist); album.ReleaseID != "" && key != "" {
				ix.releasesByKey[key] = append(ix.releasesByKey[key], album.ReleaseID)
			}
		}
	}
	return ix, nil
}
