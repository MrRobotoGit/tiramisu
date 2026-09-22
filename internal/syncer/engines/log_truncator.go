package engines

import (
	"os"
	"path/filepath"
	"time"
)

// LogTruncator starts a goroutine that truncates log files at midnight.
func StartLogTruncator(logsDir string, stop <-chan struct{}) {
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
			delay := next.Sub(now)

			select {
			case <-stop:
				return
			case <-time.After(delay):
				// Truncate all sync log files
				for _, name := range []string{"movies-sync.log", "tv-sync.log", "watchlist-sync.log", "music-sync.log"} {
					path := filepath.Join(logsDir, name)
					os.Truncate(path, 0)
				}
			}
		}
	}()
}
