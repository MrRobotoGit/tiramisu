package engines

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"tiramisu/internal/config"
	"tiramisu/internal/musicimport"
	"tiramisu/internal/prowlarr"
)

// MusicSyncConfig holds what the discovery engine needs: the media server (Plex or
// Jellyfin, same URL and token fields), the local Library API, Prowlarr, and the
// discovery knobs.
type MusicSyncConfig struct {
	PlexURL      string
	PlexToken    string
	PlexMusicLib string // Plex artist section the seed index reads first
	LibraryURL   string // e.g. http://127.0.0.1:9080
	StateDir     string
	LogsDir      string
	ProwlarrCfg  prowlarr.ConfigProwlarr
	Discovery    config.MusicDiscoveryConfig
}

// MusicSyncEngine is the weekly discovery syncer. It works silently: no dashboard
// card, but the scheduler status and the trigger/stop API see it like the others.
type MusicSyncEngine struct {
	cfg    MusicSyncConfig
	logger *log.Logger
}

// NewMusicSyncEngine creates the discovery engine. The log file matches the other
// engines' and is truncated at midnight by the shared log truncator.
func NewMusicSyncEngine(cfg MusicSyncConfig) *MusicSyncEngine {
	logPath := filepath.Join(cfg.LogsDir, "music-sync.log")
	logFile, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	logger := log.New(io.MultiWriter(os.Stdout, logFile), "[MusicSync] ", log.LstdFlags)
	return &MusicSyncEngine{cfg: cfg, logger: logger}
}

func (e *MusicSyncEngine) Name() string { return "music" }

func (e *MusicSyncEngine) statePath() string {
	return filepath.Join(e.cfg.StateDir, "music-discovery-state.json")
}

// musicSection picks the Plex section the seed index reads: the configured one, or
// the first artist section when the config never set it (an unset int reaches the
// engine as strconv.Itoa(0), hence the "0" case).
func musicSection(configured string, sections []musicimport.Section) string {
	if configured != "" && configured != "0" {
		return configured
	}
	return sections[0].Key
}

// musicServer is the slice of the media server the engine reads.
type musicServer interface {
	ArtistSections(ctx context.Context) ([]musicimport.Section, error)
	Artists(ctx context.Context, section string) ([]musicimport.Artist, error)
	Albums(ctx context.Context, section string) ([]musicimport.Album, error)
}

// Run is one discovery pass. The scheduler's context cancels the pacing sleeps.
func (e *MusicSyncEngine) Run(ctx context.Context) error {
	library := musicimport.NewTiramisu(e.cfg.LibraryURL)
	serverType := "plex"
	if configured, err := library.MediaServerType(ctx); err == nil && configured != "" {
		serverType = configured
	}
	var server musicServer = musicimport.NewPlexClient(e.cfg.PlexURL, e.cfg.PlexToken)
	if serverType == "jellyfin" {
		server = musicimport.NewJellyfinClient(e.cfg.PlexURL, e.cfg.PlexToken)
	}
	indexer := prowlarr.NewClient(e.cfg.ProwlarrCfg)

	state, err := musicimport.LoadDiscoveryState(e.statePath())
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	// The import tool's state (when co-located) carries the release group of every
	// album it filed: a free warm cache, not a second source of truth.
	if imports, err := musicimport.LoadState(filepath.Join(e.cfg.StateDir, "musicimport-state.json")); err == nil {
		state.MergeImportState(imports)
	}
	sections, err := server.ArtistSections(ctx)
	if err != nil {
		return fmt.Errorf("%s sections: %w", serverType, err)
	}
	if len(sections) == 0 {
		return fmt.Errorf("%s reports no music library", serverType)
	}
	// The configured section is a Plex id; Jellyfin libraries are all read alike.
	section := ""
	if serverType != "jellyfin" {
		section = musicSection(e.cfg.PlexMusicLib, sections)
	}
	style := musicimport.IDStyleForPlayer(serverType)

	opts := musicimport.DiscoverOptions{
		Section:  section,
		Sections: sections,
		IDStyle:  style,
		Logf:     e.logger.Printf,
	}
	musicimport.ApplyDiscoveryConfig(&opts, e.cfg.Discovery)
	if opts.NewReleases.Enabled {
		own, ok, err := e.ownSection(ctx, serverType, server, library, sections)
		if err != nil {
			return err
		}
		if !ok && serverType == "jellyfin" {
			e.logger.Printf("new releases off: no Jellyfin music library holds Tiramisu's albums yet")
		} else if !ok {
			e.logger.Printf("new releases off: plex.music_library_id %q is not a Plex music section", e.cfg.PlexMusicLib)
		}
		opts.NewReleases.Enabled, opts.NewReleases.Section = ok, own
	}

	listenBrainz := musicimport.NewListenBrainz()
	runner := &musicimport.DiscoverRunner{
		Media:   server,
		Brainz:  musicimport.NewMusicBrainz(),
		Listen:  listenBrainz,
		Tags:    listenBrainz,
		Similar: musicimport.NewDeezer(),
		Indexer: indexer,
		Library: library,
		State:   state,
		Options: opts,
	}
	summary, err := runner.Run(ctx)
	// The summary is logged even on error: a failed pass does not undo the imports
	// the others made.
	outcome := "run done"
	if err != nil {
		outcome = fmt.Sprintf("run ended with an error (%v)", err)
	}
	e.logger.Printf("%s: seeds %d (%s), similar candidates %d, genre candidates %d, new artists %d, new releases %d, present %d, imported %d, no-torrent %d, failed %d, parked %d",
		outcome, summary.Seeds, summary.Window, summary.Candidates, summary.Genres, summary.NewArtists, summary.NewReleases, summary.Present, summary.Imported, summary.NoTorrent, summary.Failed, summary.Parked)
	return err
}

// ownSection is the library Tiramisu files music into, the only one the new-release
// follow reads: the configured section on Plex, the one holding Tiramisu's albums on
// Jellyfin (its ids are not numbers the panel can hold).
func (e *MusicSyncEngine) ownSection(ctx context.Context, serverType string, server musicServer, library *musicimport.Tiramisu, sections []musicimport.Section) (musicimport.Section, bool, error) {
	if serverType != "jellyfin" {
		for _, s := range sections {
			if s.Key == e.cfg.PlexMusicLib {
				return s, true, nil
			}
		}
		return musicimport.Section{}, false, nil
	}
	committed, err := library.Committed(ctx)
	if err != nil {
		return musicimport.Section{}, false, fmt.Errorf("library: %w", err)
	}
	return musicimport.OwnMusicSection(ctx, server, sections, committed)
}
