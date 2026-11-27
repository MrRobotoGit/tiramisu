package engines

import (
	"context"
	"errors"
	"path/filepath"
	"sort"

	"tiramisu/internal/catalog/tmdb"
	"tiramisu/internal/metadb"
)

// libraryShow is one show folder of the library with the episodes registered in it.
type libraryShow struct {
	folder   string
	entries  []metadb.EpisodeEntry
	showIMDB string
}

// libraryShowGroups groups the registered episodes by show folder, in folder order.
// Episodes outside the <show>/Season.NN layout are left out: no name to resolve.
func libraryShowGroups(entries []metadb.EpisodeEntry) []libraryShow {
	byFolder := make(map[string]*libraryShow)
	for _, ep := range entries {
		if ep.FilePath == "" {
			continue
		}
		if name, _ := showQueryFromPath(ep.FilePath); name == "" {
			continue
		}
		folder := filepath.Dir(filepath.Dir(ep.FilePath))
		g, ok := byFolder[folder]
		if !ok {
			g = &libraryShow{folder: folder}
			byFolder[folder] = g
		}
		g.entries = append(g.entries, ep)
		if g.showIMDB == "" && ep.ShowIMDB != "" {
			g.showIMDB = ep.ShowIMDB
		}
	}
	out := make([]libraryShow, 0, len(byFolder))
	for _, g := range byFolder {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].folder < out[j].folder })
	return out
}

// followLibraryShows searches the library shows discovery did not return, so a show
// still airing keeps getting its new episodes and its holes after it leaves the TMDB
// feeds. processShow's recency gate drops the ones off the air.
func (e *TVGoEngine) followLibraryShows(ctx context.Context, handled map[int]bool) {
	if e.db == nil {
		return
	}
	entries, err := e.db.AllEpisodes()
	if err != nil {
		e.logger.Printf("[TVSync] WARNING: could not read the library shows: %v", err)
		return
	}
	groups := libraryShowGroups(entries)
	followed := 0
	for _, g := range groups {
		select {
		case <-ctx.Done():
			return
		default:
		}
		show, ok := e.libraryShowIdentity(ctx, g)
		if !ok || handled[show.ID] {
			continue
		}
		handled[show.ID] = true
		followed++
		e.logger.Printf("[library] %s", show.Name)
		e.processShow(ctx, show)
	}
	e.logger.Printf("[TVSync] Library: %d show(s) outside discovery checked, %d folder(s)", followed, len(groups))
}

// libraryShowIdentity finds the TMDB show of a folder: by its stored IMDB id when it
// has one, by name otherwise (resolveShow stores the id for the next run).
func (e *TVGoEngine) libraryShowIdentity(ctx context.Context, g libraryShow) (tmdb.TVShow, bool) {
	if g.showIMDB != "" {
		if show, err := e.tmdb.FindTVByIMDB(ctx, g.showIMDB); err == nil && show.ID != 0 {
			return show, true
		}
	}
	show, _, err := e.resolveShow(ctx, g.entries)
	if err != nil {
		if !errors.Is(err, ErrShowIdentityUnclear) {
			e.logger.Printf("[TVSync] Library: cannot resolve %s (%v)", filepath.Base(g.folder), err)
		}
		return tmdb.TVShow{}, false
	}
	return show, show.ID != 0
}
