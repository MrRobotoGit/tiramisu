package engines

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"tiramisu/internal/prowlarr"
)

// WatchlistSyncer runs the watchlist sync in pure Go (Fase 2).
type WatchlistSyncer struct {
	engine *WatchlistGoEngine
}

// WatchlistSyncerConfig holds config for the Go watchlist engine.
type WatchlistSyncerConfig struct {
	GoStormURL      string
	TMDBAPIKey      string
	TorrentioURL    string
	PlexURL         string
	PlexToken       string
	PlexSection     int
	MoviesDir       string
	MediaServerType string
	LogsDir         string
	ProwlarrCfg     prowlarr.ConfigProwlarr
}

// NewWatchlistSyncer creates a new Go-based watchlist syncer.
func NewWatchlistSyncer(cfg WatchlistSyncerConfig) *WatchlistSyncer {
	exe, _ := os.Executable()
	binDir := filepath.Dir(exe)

	moviesDir := cfg.MoviesDir
	if moviesDir == "" {
		moviesDir = "/mnt/torrserver/movies"
	}
	logsDir := cfg.LogsDir
	if logsDir == "" {
		logsDir = filepath.Join(binDir, "logs")
	}

	engineCfg := WatchlistConfig{
		GoStormURL:      cfg.GoStormURL,
		TMDBAPIKey:      cfg.TMDBAPIKey,
		TorrentioURL:    cfg.TorrentioURL,
		PlexURL:         cfg.PlexURL,
		PlexToken:       cfg.PlexToken,
		PlexSection:     cfg.PlexSection,
		MoviesDir:       moviesDir,
		MediaServerType: cfg.MediaServerType,
		LogsDir:         logsDir,
		ProwlarrCfg:     cfg.ProwlarrCfg,
	}

	return &WatchlistSyncer{
		engine: NewWatchlistGoEngine(engineCfg),
	}
}

func (s *WatchlistSyncer) Name() string { return "watchlist" }

func (s *WatchlistSyncer) Run(ctx context.Context) error {
	if err := s.engine.Run(ctx); err != nil {
		return fmt.Errorf("watchlist sync: %w", err)
	}
	return nil
}
