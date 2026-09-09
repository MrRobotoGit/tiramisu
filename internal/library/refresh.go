package library

import (
	"context"
	"time"
)

const defaultRefreshDelay = 15 * time.Second

// MediaServer is the Plex/Jellyfin library refresh. mediaserver.Client implements it.
type MediaServer interface {
	RefreshLibrary(ctx context.Context, sectionID int) error
}

// scheduleRefresh asks the media server to rescan a section, coalescing the requests of
// a burst of adds into one: a client filing a whole filmography must not make Plex
// rescan once per title. A stub is invisible to the media server until it rescans, so
// this is part of the add, not an extra.
func (m *Manager) scheduleRefresh(section int) {
	if m.cfg.MediaServer == nil {
		return
	}
	delay := m.cfg.RefreshDelay
	if delay <= 0 {
		delay = defaultRefreshDelay
	}

	m.mu.Lock()
	if m.refreshPending == nil {
		m.refreshPending = map[int]bool{}
	}
	if m.refreshPending[section] {
		m.mu.Unlock()
		return
	}
	m.refreshPending[section] = true
	m.mu.Unlock()

	go func() {
		time.Sleep(delay)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := m.cfg.MediaServer.RefreshLibrary(ctx, section)

		// The flag is cleared only now: adds arriving while a slow scan runs join the
		// next window instead of queueing a scan each.
		m.mu.Lock()
		delete(m.refreshPending, section)
		m.mu.Unlock()

		if err != nil {
			m.cfg.Logger.Printf("[LibraryAPI] WARNING: library refresh failed: %v", err)
		}
	}()
}
