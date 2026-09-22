// Command musicimport rebuilds a Plex music library inside Tiramisu: it reads the
// albums Plex already knows, resolves each one to a MusicBrainz release group,
// finds a lossless torrent with acceptable seeders and files it through the
// Library API.
//
// It is dry-run by default. Pass --apply to write to the library and --limit to
// keep a run small; the state file makes runs resumable.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"tiramisu/internal/config"
	"tiramisu/internal/musicimport"
	"tiramisu/internal/prowlarr"
)

func main() {
	plexURL := flag.String("plex-url", "", "Plex base URL (default: plex.url from config.json)")
	plexToken := flag.String("plex-token", "", "Plex token (default: plex.token from config.json)")
	section := flag.String("section", "", "Plex artist section key (default: the only artist section)")
	prowlarrURL := flag.String("prowlarr-url", "", "Prowlarr base URL (default: prowlarr.url from config.json)")
	prowlarrKey := flag.String("prowlarr-key", "", "Prowlarr API key (default: prowlarr.api_key from config.json)")
	libraryURL := flag.String("library-url", "http://127.0.0.1:9080", "Tiramisu Library API base URL")
	statePath := flag.String("state", "musicimport-state.json", "state file, created when missing")
	// 5 comes from the first production library: albums that played had a median of
	// 7.5 seeders, the ones that failed their metadata a median of 4. Below this the
	// release is likely to be filed and then never readable.
	minSeeders := flag.Int("min-seeders", 5, "minimum seeders for a music release")
	maxSizeGB := flag.Float64("max-size-gb", 3, "largest accepted album size in GB")
	limit := flag.Int("limit", 0, "stop after this many albums (0 = all)")
	indexers := flag.String("indexers", "", "comma-separated Prowlarr indexer ids to search (default: all enabled)")
	apply := flag.Bool("apply", false, "write to the library; without it the run is a dry run")
	retryFailed := flag.Bool("retry-failed", false, "walk again the albums an earlier run left unmatched or failed; by default a run resumes where the last stopped")
	reap := flag.Bool("reap", false, "remove albums whose swarm has been unreachable, instead of importing")
	splitImages := flag.Bool("split-images", false, "refile albums filed as one single-file image track by track, cut by their cue sheet")
	resync := flag.Bool("resync", false, "with --split-images, also refile tracks of albums already split whose matching changed (removes before re-adding)")
	discover := flag.Bool("discover", false, "weekly-style discovery run: Plex listens -> ListenBrainz -> import, instead of the album walk")
	discoverState := flag.String("discover-state", "music-discovery-state.json", "discovery state file")
	seedsMinPlays := flag.Int("seeds-min-plays", 8, "plays an artist needs to become a seed")
	maxAlbums := flag.Int("max-albums", 10, "most albums imported in one discovery run")
	pace := flag.Duration("pace", 10*time.Second, "wait between two album imports")
	idStyle := flag.String("id-style", "", "MusicBrainz id to register per file: track (Plex webhooks) or recording (Jellyfin); default follows media_server_type from the panel. It applies to new imports only: existing projections keep the id they were filed with")
	reapMinFailures := flag.Int("reap-min-failures", 3, "failures needed before an album is condemned")
	reapMinSpan := flag.Duration("reap-min-span", 24*time.Hour, "how long the failures must span")
	reapLimit := flag.Int("reap-limit", 25, "most albums removed in one run (0 = no cap)")
	flag.Parse()

	cfg := config.LoadConfig()
	if *plexURL == "" {
		*plexURL = cfg.Plex.URL
	}
	if *plexToken == "" {
		*plexToken = cfg.Plex.Token
	}
	if *prowlarrURL == "" {
		*prowlarrURL = cfg.Prowlarr.URL
	}
	if *prowlarrKey == "" {
		*prowlarrKey = cfg.Prowlarr.APIKey
	}
	if *plexURL == "" || *plexToken == "" {
		log.Fatal("musicimport: Plex URL and token are required (flags or config.json)")
	}
	if *prowlarrURL == "" || *prowlarrKey == "" {
		log.Fatal("musicimport: Prowlarr URL and API key are required (flags or config.json)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()

	plex := musicimport.NewPlexClient(*plexURL, *plexToken)
	// The dedup index walks every artist section, exactly like the engine does: a
	// dry run that skipped them would call "missing" an album Plex already holds.
	allSections, err := plex.ArtistSections(ctx)
	if err != nil {
		log.Fatalf("musicimport: %v", err)
	}
	if *section == "" {
		switch len(allSections) {
		case 0:
			log.Fatal("musicimport: Plex reports no artist section")
		case 1:
			*section = allSections[0].Key
		default:
			for _, s := range allSections {
				log.Printf("section %s: %s", s.Key, s.Title)
			}
			log.Fatal("musicimport: more than one artist section, pick one with --section")
		}
	}

	library := musicimport.NewTiramisu(*libraryURL)
	style := *idStyle
	if style == "" {
		// The panel's Plex/Jellyfin switch decides which id the player will send back.
		serverType, err := library.MediaServerType(ctx)
		if err != nil {
			log.Printf("musicimport: cannot read media_server_type, assuming Plex: %v", err)
		}
		style = musicimport.IDStyleForPlayer(serverType)
		log.Printf("musicimport: media server %q, registering %s ids", serverType, style)
	}

	state, err := musicimport.LoadState(*statePath)
	if err != nil {
		log.Fatalf("musicimport: state: %v", err)
	}
	runner := &musicimport.Runner{
		Plex:    plex,
		Brainz:  musicimport.NewMusicBrainz(),
		Indexer: prowlarr.NewClient(prowlarr.ConfigProwlarr{Enabled: true, URL: *prowlarrURL, APIKey: *prowlarrKey}),
		Library: library,
		State:   state,
		Options: musicimport.Options{
			Section:      *section,
			IndexerIDs:   parseIndexers(*indexers),
			IDStyle:      style,
			RetryFailed:  *retryFailed,
			MinSeeders:   *minSeeders,
			MaxSizeBytes: int64(*maxSizeGB * float64(1<<30)),
			Limit:        *limit,
			Apply:        *apply,
			Logf:         log.Printf,
		},
	}

	if *splitImages {
		summary, err := musicimport.SplitImages(ctx, library, musicimport.NewMusicBrainz(), state, style, *apply, *resync, *limit, log.Printf)
		if err != nil {
			log.Fatalf("musicimport: %v", err)
		}
		fmt.Printf("\n%s: image albums %d, no cue %d, planned %d, converted %d, failed %d, tracks %d, resynced albums %d, refiled tracks %d\n",
			modeLabel(*apply, "split dry run", "split"), summary.Albums, summary.NoCue, summary.Planned, summary.Converted, summary.Failed, summary.Tracks, summary.Resynced, summary.Fixed)
		for _, note := range summary.Notes {
			fmt.Println(" -", note)
		}
		return
	}

	if *reap {
		summary, err := runner.Reap(ctx, musicimport.ReapOptions{
			MinFailures: *reapMinFailures,
			MinSpan:     *reapMinSpan,
			Limit:       *reapLimit,
			Apply:       *apply,
		})
		if err != nil {
			log.Fatalf("musicimport: %v", err)
		}
		mode := "reap dry run"
		if *apply {
			mode = "reaped"
		}
		fmt.Printf("\n%s: albums %d, candidates %d, removed %d, projections %d, skipped for an active session %d\n",
			mode, summary.Albums, summary.Candidates, summary.Removed, summary.Files, summary.SkippedActive)
		for _, note := range summary.Notes {
			fmt.Println(" -", note)
		}
		return
	}

	if *discover {
		state, err := musicimport.LoadDiscoveryState(*discoverState)
		if err != nil {
			log.Fatalf("musicimport: discovery state: %v", err)
		}
		// The album walk's own state already resolved every Plex album to its release
		// group: merging it saves an hour of MusicBrainz calls on the first run.
		if imports, err := musicimport.LoadState(*statePath); err == nil {
			state.MergeImportState(imports)
		}
		discovery := &musicimport.DiscoverRunner{
			Plex:    plex,
			Brainz:  musicimport.NewMusicBrainz(),
			Listen:  musicimport.NewListenBrainz(),
			Indexer: prowlarr.NewClient(prowlarr.ConfigProwlarr{Enabled: true, URL: *prowlarrURL, APIKey: *prowlarrKey}),
			Library: library,
			State:   state,
			Options: musicimport.DiscoverOptions{
				Section:  *section,
				Sections: allSections,
				SeedOpts: musicimport.SeedOptions{Count: 5, MinPlays: *seedsMinPlays, Windows: []time.Duration{7 * 24 * time.Hour, 30 * 24 * time.Hour, 90 * 24 * time.Hour, 365 * 24 * time.Hour, 0}},
				Radio: musicimport.RadioOptions{
					Mode: "medium", MaxSimilarArtists: 5, MaxRecordingsPerArtist: 3, PopBegin: 10, PopEnd: 60,
				},
				MinListenCount: 500,
				AlbumTypes:     []string{"Album", "EP"},
				MaxAlbums:      *maxAlbums,
				MaxPerArtist:   1,
				MinSeeders:     *minSeeders,
				MaxSizeBytes:   int64(*maxSizeGB * float64(1<<30)),
				IDStyle:        style,
				Pace:           *pace,
				Logf:           log.Printf,
			},
		}
		// The dry run is safe by construction: it reads Plex, ListenBrainz and Prowlarr
		// (to show the torrent it would pick) and stops before the Library API write.
		if *apply {
			log.Print("musicimport: --discover --apply imports for real; without --apply nothing is written")
		}
		summary, err := discovery.RunDryRun(ctx, !*apply)
		if err != nil {
			log.Fatalf("musicimport: %v", err)
		}
		fmt.Printf("\n%s: seeds %d (%s), candidates %d, present %d, planned %d, imported %d, no-torrent %d, failed %d\n",
			modeLabel(*apply, "discover dry run", "discovered"),
			summary.Seeds, summary.Window, summary.Candidates, summary.Present, summary.Planned, summary.Imported, summary.NoTorrent, summary.Failed)
		for _, note := range summary.Notes {
			fmt.Println(" -", note)
		}
		return
	}

	summary, err := runner.Run(ctx)
	if err != nil {
		log.Fatalf("musicimport: %v", err)
	}
	mode := "dry run"
	if *apply {
		mode = "applied"
	}
	fmt.Printf("\n%s: albums %d, already present %d, settled earlier %d, no identity %d, no match %d, selected %d, applied %d, errors %d\n",
		mode, summary.Albums, summary.Present, summary.Skipped, summary.NoIdentity, summary.NoMatch, summary.Selected, summary.Applied, summary.Errors)
	for _, note := range summary.Notes {
		fmt.Println(" -", note)
	}
}

// modeLabel names a run for the summary line.
func modeLabel(apply bool, dry, done string) string {
	if apply {
		return done
	}
	return dry
}

// parseIndexers reads the --indexers flag: "10,2" becomes [10 2], empty becomes nil.
func parseIndexers(value string) []int {
	var ids []int
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.Atoi(part)
		if err != nil {
			log.Fatalf("musicimport: bad indexer id %q", part)
		}
		ids = append(ids, id)
	}
	return ids
}
