package engines

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"tiramisu/internal/config"
	"tiramisu/internal/musicimport"
	"tiramisu/internal/prowlarr"
)

// MusicSyncConfig holds what the discovery engine needs: the Plex server, the local
// Library API, Prowlarr, and the discovery knobs.
type MusicSyncConfig struct {
	PlexURL      string
	PlexToken    string
	PlexMusicLib string // artist section new albums land in
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

// Run is one discovery pass. The scheduler's context cancels the pacing sleeps.
func (e *MusicSyncEngine) Run(ctx context.Context) error {
	plex := musicimport.NewPlexClient(e.cfg.PlexURL, e.cfg.PlexToken)
	library := musicimport.NewTiramisu(e.cfg.LibraryURL)
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
	sections, err := plex.ArtistSections(ctx)
	if err != nil {
		return fmt.Errorf("plex sections: %w", err)
	}
	if len(sections) == 0 {
		return fmt.Errorf("plex reports no artist section")
	}
	section := musicSection(e.cfg.PlexMusicLib, sections)

	style := musicimport.IDStyleForPlayer("plex")
	if serverType, err := library.MediaServerType(ctx); err == nil {
		style = musicimport.IDStyleForPlayer(serverType)
	}

	d := e.cfg.Discovery
	windows := make([]time.Duration, 0, len(d.SeedsWindowsDays))
	for _, days := range d.SeedsWindowsDays {
		windows = append(windows, time.Duration(days)*24*time.Hour)
	}

	runner := &musicimport.DiscoverRunner{
		Plex:    plex,
		Brainz:  musicimport.NewMusicBrainz(),
		Listen:  musicimport.NewListenBrainz(),
		Indexer: indexer,
		Library: library,
		State:   state,
		Options: musicimport.DiscoverOptions{
			Section:  section,
			Sections: sections,
			SeedOpts: musicimport.SeedOptions{Count: d.SeedsCount, MinPlays: d.SeedsMinPlays, Windows: windows},
			Radio: musicimport.RadioOptions{
				Mode: d.Mode, MaxSimilarArtists: d.MaxSimilarArtists,
				MaxRecordingsPerArtist: d.MaxRecordingsPerArtist, PopBegin: d.PopBegin, PopEnd: d.PopEnd,
			},
			MinListenCount: d.MinListenCount,
			AlbumTypes:     d.AlbumTypes,
			MaxAlbums:      d.MaxAlbumsPerRun,
			MaxPerArtist:   d.MaxAlbumsPerArtist,
			MaxAttempts:    d.MaxAttempts,
			MinSeeders:     d.MinSeeders,
			MaxSizeBytes:   int64(d.MaxSizeGB * float64(1<<30)),
			IDStyle:        style,
			Pace:           time.Duration(d.PaceSeconds) * time.Second,
			Logf:           e.logger.Printf,
		},
	}
	summary, err := runner.Run(ctx)
	if err != nil {
		return err
	}
	e.logger.Printf("run done: seeds %d (%s), candidates %d, present %d, imported %d, no-torrent %d, failed %d, parked %d",
		summary.Seeds, summary.Window, summary.Candidates, summary.Present, summary.Imported, summary.NoTorrent, summary.Failed, summary.Parked)
	return nil
}
