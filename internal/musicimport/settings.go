package musicimport

import (
	"sort"
	"time"

	"tiramisu/internal/config"
)

// maxDebutYears caps the debut filters: past it the duration would overflow.
const maxDebutYears = 100

const day = 24 * time.Hour

// ApplyDiscoveryConfig fills the runner's options from the music_discovery config.
// The engine and the preview tool share it, so they cannot drift. Values are
// sanitized on the way in: recency tiers are sorted and positive, debut years are
// capped, and a debut of 0 means no debut filter in every pass. The new-release
// section is the caller's: only it knows the media server.
func ApplyDiscoveryConfig(o *DiscoverOptions, d config.MusicDiscoveryConfig) {
	o.SeedOpts = SeedOptions{Count: d.SeedsCount, MinPlays: d.SeedsMinPlays, Windows: windowDays(d.SeedsWindowsDays), Recency: recencyTiers(d.SeedsRecencyDays)}
	o.Radio = RadioOptions{
		Mode: d.Mode, MaxSimilarArtists: d.MaxSimilarArtists,
		MaxRecordingsPerArtist: d.MaxRecordingsPerArtist, PopBegin: d.PopBegin, PopEnd: d.PopEnd,
	}
	o.MinListenCount = d.MinListenCount
	o.NewReleases.Enabled = d.NewReleases.Enabled && d.NewReleases.WindowDays > 0
	o.NewReleases.Window = time.Duration(d.NewReleases.WindowDays) * day
	o.NewArtists = NewArtistOptions{
		Enabled: d.NewArtists.Enabled && d.NewArtists.WindowDays > 0,
		Window:  time.Duration(d.NewArtists.WindowDays) * day,
		Debut:   debutYears(d.NewArtists.DebutYears),
	}
	o.Genres = GenreOptions{
		Enabled: d.Genres.Enabled && d.Genres.Count > 0,
		Count:   d.Genres.Count,
		Debut:   debutYears(d.Genres.DebutYears),
	}
	o.Similar = SimilarOptions{MinFans: d.Similar.MinFans, MaxFans: d.Similar.MaxFans, Debut: debutYears(d.Similar.DebutYears)}
	o.AlbumTypes = d.AlbumTypes
	o.MaxAlbums = d.MaxAlbumsPerRun
	o.MaxPerArtist = d.MaxAlbumsPerArtist
	o.MaxAttempts = d.MaxAttempts
	o.MinSeeders = d.MinSeeders
	o.MaxSizeBytes = int64(d.MaxSizeGB * float64(1<<30))
	o.Pace = time.Duration(d.PaceSeconds) * time.Second
}

// debutYears is 0 (no filter) for 0 or less, and capped where a Duration still holds.
func debutYears(years int) time.Duration {
	if years <= 0 {
		return 0
	}
	return time.Duration(min(years, maxDebutYears)) * 365 * day
}

// recencyTiers keeps the positive tiers in ascending order: the weights halve along
// the list, so an unsorted config would weigh recent plays less than old ones.
func recencyTiers(days []int) []time.Duration {
	var tiers []time.Duration
	for _, d := range days {
		if d > 0 {
			tiers = append(tiers, time.Duration(min(d, 36500))*day)
		}
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i] < tiers[j] })
	return tiers
}

func windowDays(days []int) []time.Duration {
	out := make([]time.Duration, 0, len(days))
	for _, d := range days {
		out = append(out, time.Duration(max(0, min(d, 36500)))*day)
	}
	return out
}
