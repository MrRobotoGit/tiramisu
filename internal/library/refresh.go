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
		m.refreshDirty = map[int]bool{}
	}
	if m.refreshPending[section] {
		// A scan for this section is already coming. If it is the one being waited
		// for, it will cover this request too; if it is already running, it cannot,
		// so mark the section dirty and let it schedule another when it finishes.
		// Dropping the request is how a remove and the add that replaces it end up
		// sharing one scan that saw only half the change.
		m.refreshDirty[section] = true
		m.mu.Unlock()
		return
	}
	m.refreshPending[section] = true
	m.mu.Unlock()

	go func() {
		time.Sleep(delay)

		// Requests that arrived during the wait are covered by the scan below.
		m.mu.Lock()
		delete(m.refreshDirty, section)
		m.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := m.cfg.MediaServer.RefreshLibrary(ctx, section)

		m.mu.Lock()
		delete(m.refreshPending, section)
		again := m.refreshDirty[section]
		delete(m.refreshDirty, section)
		m.mu.Unlock()

		if err != nil {
			m.cfg.Logger.Printf("[LibraryAPI] WARNING: library refresh failed: %v", err)
		}
		if again {
			m.scheduleRefresh(section)
		}
	}()
}
