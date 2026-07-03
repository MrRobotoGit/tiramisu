package engines

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"tiramisu/internal/prowlarr"
)

// MoviesSyncer runs the movie sync in pure Go (Fase 4).
type MoviesSyncer struct {
	engine *MovieGoEngine
}

// MoviesSyncerConfig holds config for the Go movie engine.
type MoviesSyncerConfig struct {
	GoStormURL   string
	TMDBAPIKey   string
	TorrentioURL string
	PlexURL      string
	PlexToken    string
	PlexLib      int
	MoviesDir    string
	StateDir     string
	LogsDir      string
	ProwlarrCfg  prowlarr.ConfigProwlarr
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
		GoStormURL:   cfg.GoStormURL,
		TMDBAPIKey:   cfg.TMDBAPIKey,
		TorrentioURL: cfg.TorrentioURL,
		PlexURL:      cfg.PlexURL,
		PlexToken:    cfg.PlexToken,
		PlexLib:      cfg.PlexLib,
		MoviesDir:    moviesDir,
		StateDir:     stateDir,
		LogsDir:      logsDir,
		ProwlarrCfg:  cfg.ProwlarrCfg,
	}

	return &MoviesSyncer{
		engine: NewMovieGoEngine(engineCfg),
	}
}

func (s *MoviesSyncer) Name() string { return "movies" }

func (s *MoviesSyncer) Run(ctx context.Context) error {
	if err := s.engine.Run(ctx); err != nil {
		return fmt.Errorf("movie sync: %w", err)
	}
	return nil
}
