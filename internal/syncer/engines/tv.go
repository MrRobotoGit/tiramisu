package engines

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"tiramisu/internal/metadb"
	"tiramisu/internal/prowlarr"
)

// TVSyncer runs the TV sync in pure Go (Fase 3).
type TVSyncer struct {
	engine *TVGoEngine
}

// TVSyncerConfig holds config for the Go TV engine.
type TVSyncerConfig struct {
	GoStormURL   string
	TMDBAPIKey   string
	TorrentioURL string
	PlexURL      string
	PlexToken    string
	PlexTVLib    int
	TVDir        string
	StateDir     string
	LogsDir      string
	ProwlarrCfg  prowlarr.ConfigProwlarr
	DB           *metadb.DB // V1.7.1: Optional SQLite backend
	// InvalidatePath, when set, is called after removing a stub file/dir so the FUSE
	// layer drops its cached state for it (see main.invalidateSyncRemovedPath).
	InvalidatePath func(string)
}

// NewTVSyncer creates a new Go-based TV syncer.
func NewTVSyncer(cfg TVSyncerConfig) *TVSyncer {
	exe, _ := os.Executable()
	binDir := filepath.Dir(exe)

	tvDir := cfg.TVDir
	if tvDir == "" {
		tvDir = "/mnt/torrserver/tv"
	}
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(binDir, "STATE")
	}
	logsDir := cfg.LogsDir
	if logsDir == "" {
		logsDir = filepath.Join(binDir, "logs")
	}

	engineCfg := TVEngineConfig{
		GoStormURL:     cfg.GoStormURL,
		TMDBAPIKey:     cfg.TMDBAPIKey,
		TorrentioURL:   cfg.TorrentioURL,
		PlexURL:        cfg.PlexURL,
		PlexToken:      cfg.PlexToken,
		PlexTVLib:      cfg.PlexTVLib,
		TVDir:          tvDir,
		StateDir:       stateDir,
		LogsDir:        logsDir,
		ProwlarrCfg:    cfg.ProwlarrCfg,
		InvalidatePath: cfg.InvalidatePath,
	}

	return &TVSyncer{
		engine: NewTVGoEngine(engineCfg, cfg.DB),
	}
}

func (s *TVSyncer) Name() string { return "tv" }

func (s *TVSyncer) Run(ctx context.Context) error {
	if err := s.engine.Run(ctx); err != nil {
		return fmt.Errorf("tv sync: %w", err)
	}
	return nil
}
