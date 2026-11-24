package app

import (
	_ "net/http/pprof"
	"tiramisu/internal/config"
	"tiramisu/internal/prowlarr"
)

// applyConfig puts a saved configuration in use and applies what can change live
// (V1.4.0 Live Update). Callers hold configMu.
func applyConfig(cfg config.Config) {
	// Reload in memory (V1.4.0 Live Update)
	oldEnabled := gc().BlockListEnabled
	oldURL := gc().BlockListURL
	oldFilter := gc().BlockListFilter
	globalConfig.Store(&cfg)
	prowlarrClient = prowlarr.NewClient(gc().Prowlarr)

	newEnabled := gc().BlockListEnabled
	newURL := gc().BlockListURL
	if eng == nil {
		return
	}
	eng.SetBlockListEnabled(newEnabled)
	newFilter := gc().BlockListFilter
	if err := eng.SetBlockListFilter(newFilter); err != nil {
		logger.Printf("[BlockList] %v — loading the list unfiltered", err)
	}
	// The filter changes which ranges the same file yields, so re-read it rather
	// than waiting for the next download.
	if newEnabled && newFilter != oldFilter {
		if list, err := eng.ReadBlockedIP(); err == nil {
			eng.SetIPBlocklist(list)
			logger.Printf("[BlockList] Filter changed, reloaded from the existing file")
		} else {
			logger.Printf("[BlockList] WARNING: filter changed but reload failed: %v", err)
		}
	}
	switch {
	case newEnabled && (!oldEnabled || newURL != oldURL):
		// was off -> on, or URL changed while staying on: (re)start with fresh URL
		blockLists.Start(newURL)
	case !newEnabled && oldEnabled:
		// was on -> off: stop future refreshes and clear the ranges already loaded
		// into the engine, otherwise the file keeps banning peers until a restart.
		blockLists.Stop()
		eng.SetIPBlocklist(nil)
		logger.Printf("[BlockList] Disabled: cleared from running engine")
	}
}
