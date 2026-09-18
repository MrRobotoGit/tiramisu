package tuner

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"tiramisu/internal/gostorm/torr"
)

const (
	sampleInterval   = 5 * time.Second
	announceCooldown = 120 * time.Second
	weakSwarmRatio   = 0.15
	// weakSwarmMaxSeeders is the seeder headroom below which the swarm counts as
	// depleted. The 2 inherited from the AI era left slow streams with 3-4
	// seeders uncovered; the announce diagnostic shows whether 5 finds peers or
	// just adds churn.
	weakSwarmMaxSeeders = 5
	maxFailureShown     = 5
	maxFailureChars     = 120
)

var (
	lastAnnounceAt   time.Time
	lastAnnounceHash string
)

func Start(ctx context.Context) {
	log.Printf("[Tuner] Discovery boost starting... (Stats: 5s)")
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			runCycle()
		case <-ctx.Done():
			return
		}
	}
}

func runCycle() {
	active := pickActive(torr.ListActiveTorrent())
	if active == nil || active.Torrent == nil {
		return
	}

	st := active.StatHighFreq()
	if st == nil {
		return
	}

	size := active.Size
	if size == 0 {
		size = active.Torrent.Length()
	}
	fileSizeGB := float64(size) / (1024 * 1024 * 1024)
	speedMBs := st.DownloadSpeed / (1024 * 1024)

	private := false
	if info := active.Torrent.Info(); info != nil && info.Private != nil {
		private = *info.Private
	}

	if !shouldBoost(st.ConnectedSeeders, speedMBs, st.LoadedSize, size, private, active.IsPriority.Load()) {
		return
	}

	hash := active.Hash().String()
	if hash != lastAnnounceHash {
		lastAnnounceHash = hash
		lastAnnounceAt = time.Time{}
	}

	now := time.Now()
	if !announceDue(now, lastAnnounceAt) {
		return
	}
	lastAnnounceAt = now
	active.Torrent.AnnounceTracked(func(peers, errs, total int, failures []string) {
		log.Printf("[Tuner] DiscoveryBoost: announce completed peers=%d errs=%d total=%d failures=%s",
			peers, errs, total, summarizeFailures(failures))
	})
	log.Printf("[Tuner] DiscoveryBoost: weak swarm (seeds=%d speed=%.1fMB/s threshold=%.1fMB/s) -> tracker re-announce triggered",
		st.ConnectedSeeders, speedMBs, fileSizeGB*weakSwarmRatio)
}

func pickActive(active []*torr.Torrent) *torr.Torrent {
	if len(active) == 0 {
		return nil
	}
	if len(active) > 1 {
		var priority []*torr.Torrent
		for _, t := range active {
			if t.IsPriority.Load() {
				priority = append(priority, t)
			}
		}
		if len(priority) != 1 {
			return nil
		}
		active = priority
	}
	for _, t := range active {
		if t.Torrent != nil {
			return t
		}
	}
	return nil
}

func swarmWeak(connectedSeeders int, speedMBs, fileSizeGB float64) bool {
	if fileSizeGB <= 0 {
		return false
	}
	return connectedSeeders < weakSwarmMaxSeeders && speedMBs < fileSizeGB*weakSwarmRatio
}

// shouldBoost holds every reason not to re-announce: only a confirmed playback is worth
// boosting (sync probes and scans never set the priority flag), completed torrents have
// nothing to discover (speed is zero by definition), and private trackers do not tolerate
// unscheduled announces.
func shouldBoost(connectedSeeders int, speedMBs float64, loaded, size int64, private, priority bool) bool {
	if private || !priority || (size > 0 && loaded >= size) {
		return false
	}
	return swarmWeak(connectedSeeders, speedMBs, float64(size)/(1024*1024*1024))
}

func announceDue(now, last time.Time) bool {
	return last.IsZero() || now.Sub(last) >= announceCooldown
}

// summarizeFailures renders announce errors for one log line: a bounded sample, each entry
// truncated, so a 35-tracker failure does not turn the line into a wall of text.
func summarizeFailures(failures []string) string {
	if len(failures) == 0 {
		return ""
	}
	shown := failures
	suffix := ""
	if len(shown) > maxFailureShown {
		suffix = fmt.Sprintf("; (+%d more)", len(shown)-maxFailureShown)
		shown = shown[:maxFailureShown]
	}
	parts := make([]string, 0, len(shown))
	for _, f := range shown {
		if len(f) > maxFailureChars {
			f = f[:maxFailureChars-3] + "..."
		}
		parts = append(parts, f)
	}
	return strings.Join(parts, "; ") + suffix
}
