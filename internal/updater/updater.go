package updater

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	repo          = "MrRobotoGit/tiramisu"
	checkInitial  = 60 * time.Second
	checkInterval = 12 * time.Hour
)

var (
	latestVersion   atomic.Value
	updateAvailable atomic.Bool
	logger          = log.New(log.Writer(), "[Updater] ", log.LstdFlags)
)

func init() {
	latestVersion.Store("")
}

// LatestVersion returns the latest version found on GitHub, or empty string.
func LatestVersion() string {
	v, _ := latestVersion.Load().(string)
	return v
}

// UpdateAvailable returns true if a newer version is available.
func UpdateAvailable() bool {
	return updateAvailable.Load()
}

// Start begins the periodic update check loop.
// Call in a goroutine; respects stop channel for shutdown.
func Start(currentVersion string, stop <-chan struct{}) {
	time.Sleep(checkInitial)

	checkOnce(currentVersion)

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			checkOnce(currentVersion)
		case <-stop:
			return
		}
	}
}

func checkOnce(currentVersion string) {
	latest, err := CheckLatest(repo)
	if err != nil {
		logger.Printf("Check failed: %v", err)
		return
	}

	latestVersion.Store(latest)

	if latest == "" {
		updateAvailable.Store(false)
		return
	}

	// Dev builds always show the latest stable version
	if currentVersion == "dev" || currentVersion == "" {
		updateAvailable.Store(true)
		logger.Printf("Dev build — stable release: %s", latest)
		return
	}

	available, err := isNewer(currentVersion, latest)
	if err != nil {
		logger.Printf("Version compare error (%q vs %q): %v", currentVersion, latest, err)
		updateAvailable.Store(false)
		return
	}

	updateAvailable.Store(available)
	if available {
		logger.Printf("Update available: %s -> %s", currentVersion, latest)
	}
}

func isNewer(current, latest string) (bool, error) {
	cv, err := parseVersion(current)
	if err != nil {
		return false, err
	}
	lv, err := parseVersion(latest)
	if err != nil {
		return false, err
	}

	if lv[0] != cv[0] {
		return lv[0] > cv[0], nil
	}
	if lv[1] != cv[1] {
		return lv[1] > cv[1], nil
	}
	return lv[2] > cv[2], nil
}

func parseVersion(v string) ([3]int, error) {
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")

	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return [3]int{}, fmt.Errorf("invalid version: %s", v)
	}

	var nums [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		// Strip pre-release suffix: "0-rc1" → "0", "2-beta" → "2"
		num := parts[i]
		if idx := strings.IndexAny(num, "-+"); idx >= 0 {
			num = num[:idx]
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			return [3]int{}, fmt.Errorf("invalid version component %q in %s", parts[i], v)
		}
		nums[i] = n
	}
	return nums, nil
}
