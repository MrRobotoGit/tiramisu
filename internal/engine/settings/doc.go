// Package settings keeps the engine settings document (gostorm_kv Settings/BitTorr in
// tiramisu.db): snapshots, saves with today's clamps, history and rollback, legacy imports.
// It imports neither the engine nor the BitTorrent library; internal/engine/applier does.
package settings
