package tuner

import (
	"context"
	"log"
	"time"

	"tiramisu/internal/gostorm/torr"
)

const (
	sampleInterval   = 5 * time.Second
	announceCooldown = 120 * time.Second
	weakSwarmRatio   = 0.15
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

	hash := active.Hash().String()
	if hash != lastAnnounceHash {
		lastAnnounceHash = hash
		lastAnnounceAt = time.Time{}
	}

	size := active.Size
	if size == 0 {
		size = active.Torrent.Length()
	}
	fileSizeGB := float64(size) / (1024 * 1024 * 1024)
	speedMBs := st.DownloadSpeed / (1024 * 1024)

	now := time.Now()
	if !swarmWeak(st.ConnectedSeeders, speedMBs, fileSizeGB) || !announceDue(now, lastAnnounceAt) {
		return
	}
	lastAnnounceAt = now
	active.Torrent.Announce()
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
	return connectedSeeders < 2 && speedMBs < fileSizeGB*weakSwarmRatio
}

func announceDue(now, last time.Time) bool {
	return last.IsZero() || now.Sub(last) >= announceCooldown
}
