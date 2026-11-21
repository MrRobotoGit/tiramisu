package engines

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"tiramisu/internal/config"
	"tiramisu/internal/library"
	"tiramisu/internal/metadb"
	"tiramisu/internal/prowlarr"
)

// MoviesSyncer runs the movie sync in pure Go (Fase 4).
type MoviesSyncer struct {
	engine   *MovieGoEngine
	afterRun func(ctx context.Context)
}

// MoviesSyncerConfig holds config for the Go movie engine.
type MoviesSyncerConfig struct {
	GoStormURL      string
	Torrents        Backend // the engine (stub URLs still use GoStormURL)
	TMDBAPIKey      string
	TorrentioURL    string
	PlexURL         string
	PlexToken       string
	MediaServerType string
	PlexLib         int
	MoviesDir       string
	StateDir        string
	LogsDir         string
	ProwlarrCfg     prowlarr.ConfigProwlarr
	Language        config.LanguageConfig
	QualityScoring  config.QualityScoringConfig
	// InvalidatePath, when set, is called after removing a stub file so the FUSE
	// layer drops its cached state for it (see main.invalidateSyncRemovedPath).
	InvalidatePath func(string)
	// DB is optional: without it the engine simply never reaps dead releases.
	DB            *metadb.DB
	AudioRegistry library.AudioRegistry
	// Release, when set, is the library's single release point (ReleaseIfUnused).
	Release ReleaseFunc
	// AfterRun, when set, runs after a successful sync (the orphan collection).
	AfterRun func(ctx context.Context)
}

// NewMoviesSyncer creates a new Go-based movie syncer.
func NewMoviesSyncer(cfg MoviesSyncerConfig) *MoviesSyncer {
	exe, _ := os.Executable()
	binDir := filepath.Dir(exe)

	moviesDir := cfg.MoviesDir
	if moviesDir == "" {
		moviesDir = "/mnt/torrserver/movies"
	}
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(binDir, "STATE")
	}
	logsDir := cfg.LogsDir
	if logsDir == "" {
		logsDir = filepath.Join(binDir, "logs")
	}

	engineCfg := MovieEngineConfig{
		GoStormURL:      cfg.GoStormURL,
		Torrents:        cfg.Torrents,
		TMDBAPIKey:      cfg.TMDBAPIKey,
		TorrentioURL:    cfg.TorrentioURL,
		PlexURL:         cfg.PlexURL,
		PlexToken:       cfg.PlexToken,
		MediaServerType: cfg.MediaServerType,
		PlexLib:         cfg.PlexLib,
		MoviesDir:       moviesDir,
		StateDir:        stateDir,
		LogsDir:         logsDir,
		ProwlarrCfg:     cfg.ProwlarrCfg,
		Language:        cfg.Language,
		Weights:         cfg.QualityScoring.MovieWeights(),
		InvalidatePath:  cfg.InvalidatePath,
		DB:              cfg.DB,
		AudioRegistry:   cfg.AudioRegistry,
		Release:         cfg.Release,
	}

	return &MoviesSyncer{
		engine:   NewMovieGoEngine(engineCfg),
		afterRun: cfg.AfterRun,
	}
}

func (s *MoviesSyncer) Name() string { return "movies" }

func (s *MoviesSyncer) Run(ctx context.Context) error {
	if err := s.engine.Run(ctx); err != nil {
		return fmt.Errorf("movie sync: %w", err)
	}
	if s.afterRun != nil && ctx.Err() == nil {
		s.afterRun(ctx)
	}
	return nil
}
