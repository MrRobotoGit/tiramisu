// Package watchdog watches for smbd processes stuck on FUSE I/O.
package watchdog

import (
	"bytes"
	"os/exec"
	"time"
)

// Smbd checks every minute for smbd stuck in D-state (FUSE I/O): after 3 hits it calls
// unblock once, after 10 it calls restart and returns.
func Smbd(logf func(string, ...any), unblock, restart func()) {
	const checkInterval = 60 * time.Second
	const unblockThreshold = 3  // 180s - Emergency unblock (interrupt all pumps)
	const restartThreshold = 10 // 600s - Full restart (persistent stall)
	consecutiveHits := 0

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for range ticker.C {
		if countDStateSmbd() > 0 {
			consecutiveHits++
			logf("[Watchdog] D-state smbd detected (%d/%d)", consecutiveHits, restartThreshold)

			// Level 1 (3 consecutive hits): soft-interrupt all pumps to unblock hung FUSE reads.
			// Only closes pipe readers — pumps stay alive and will retry on next FUSE read.
			// Preserves pump survival during temporary peer shortages (swarm may recover).
			if consecutiveHits == unblockThreshold {
				logf("[Watchdog] D-state persistent for %ds — triggering soft interrupt (pumps stay alive)",
					consecutiveHits*int(checkInterval/time.Second))
				unblock()
			}

			// Level 2 (10 consecutive hits): graceful restart.
			if consecutiveHits >= restartThreshold {
				logf("[Watchdog] D-state STILL persistent for %ds — triggering graceful restart",
					consecutiveHits*int(checkInterval/time.Second))
				restart()
				return
			}
		} else {
			if consecutiveHits > 0 {
				logf("[Watchdog] D-state cleared after %d hit(s) (Max: %d)", consecutiveHits, restartThreshold)
			}
			consecutiveHits = 0
		}
	}
}

// countDStateSmbd returns the number of smbd processes in D-state (uninterruptible sleep).
func countDStateSmbd() int {
	// ps -eo stat,comm: STAT column starts with D for uninterruptible sleep
	out, err := exec.Command("ps", "-eo", "stat,comm").Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range bytes.Split(out, []byte("\n")) {
		// Match lines where STAT starts with D and command is smbd
		fields := bytes.Fields(line)
		if len(fields) >= 2 && fields[0][0] == 'D' && string(fields[1]) == "smbd" {
			count++
		}
	}
	return count
}
