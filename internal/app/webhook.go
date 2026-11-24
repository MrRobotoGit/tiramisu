package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	_ "net/http/pprof"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"tiramisu/internal/metadb"
	"tiramisu/internal/warmup"
	"tiramisu/internal/webhook"
)

// handlePlexWebhook gestisce i messaggi in arrivo dal server Plex
func handlePlexWebhook(w http.ResponseWriter, r *http.Request) {
	logger.Printf("[PLEX] Webhook connection from %s", r.RemoteAddr)

	var payloadStr string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
		if err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		payloadStr = string(body)
	} else {
		// A form post is a form post: the plugin may send multipart or the older
		// application/x-www-form-urlencoded, and ParseMultipartForm reports the
		// latter as ErrNotMultipart after ParseForm has already filled the values.
		//
		// The cap matters for the urlencoded branch: ParseForm reads that body with
		// ReadAll, unbounded, and this process shares a 2200MB limit with the engine.
		//
		// ParseForm runs first on purpose: ParseMultipartForm swallows its error for a
		// urlencoded body and reports ErrNotMultipart instead, so an oversized post
		// would otherwise be read until the cap and then answer 200.
		//
		// The cap matches the multipart limit below rather than undercutting it: this
		// reader wraps the body before either branch, so a smaller value here would
		// reject a Plex webhook whose multipart carries a thumbnail, and a rejected
		// webhook is a playback session that never gets confirmed.
		r.Body = http.MaxBytesReader(w, r.Body, webhook.MaxBody)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		if err := r.ParseMultipartForm(webhook.MaxBody); err != nil && !errors.Is(err, http.ErrNotMultipart) {
			http.Error(w, "Bad request", 400)
			return
		}
		payloadStr = r.FormValue("payload")
	}
	if payloadStr == "" {
		// Silent 200s make a misconfigured plugin a mystery: say so once per event.
		logger.Printf("[PLEX] Webhook from %s carried no payload", r.RemoteAddr)
		return
	}

	// Logga il payload per debug (limitato ai primi 500 caratteri per non intasare)
	displayPayload := payloadStr
	if len(displayPayload) > 500 {
		displayPayload = displayPayload[:500] + "..."
	}
	logger.Printf("[DEBUG] Webhook received: %s", displayPayload)

	var payload struct {
		Event    string `json:"event"`
		Metadata struct {
			Title              string `json:"title"`
			GrandparentTitle   string `json:"grandparentTitle"` // for TV series
			Year               int    `json:"year"`
			LibrarySectionType string `json:"librarySectionType"`
			Media              []struct {
				Part []struct {
					File string `json:"file"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	}

	// Sanitize empty numeric fields produced by Jellyfin templates (e.g. "year":, → "year":0,)
	sanitized := webhook.SanitizeEmptyNumbers(payloadStr)
	if err := json.Unmarshal([]byte(sanitized), &payload); err != nil {
		return
	}

	// Normalize Jellyfin event names to Plex-style
	switch payload.Event {
	case "PlaybackStart":
		payload.Event = "media.play"
	case "PlaybackStop":
		payload.Event = "media.stop"
	case "PlaybackProgress":
		payload.Event = "media.resume"
	}

	// Normalize Jellyfin ItemType values to Plex-style ("Movie"→"movie", "Episode"→"show")
	// and Plex music section types ("artist", "album") to one audio marker. Audio is
	// accepted so the identity pass can run, but its fuzzy name fallbacks stay behind:
	// an album id is shared by every track, and a name-similarity guess would be worse
	// than no match.
	audioWebhook := false
	switch payload.Metadata.LibrarySectionType {
	case "Movie":
		payload.Metadata.LibrarySectionType = "movie"
	case "Episode":
		payload.Metadata.LibrarySectionType = "show"
	case "artist", "album", "music", "Audio":
		payload.Metadata.LibrarySectionType = "music"
		audioWebhook = true
	case "audiobook":
		audioWebhook = true
	}

	switch payload.Metadata.LibrarySectionType {
	case "movie", "show", "music", "audiobook":
	default:
		return
	}

	if payload.Event == "media.play" || payload.Event == "media.resume" {
		targetTitle := strings.ToLower(payload.Metadata.Title)
		seriesTitle := strings.ToLower(payload.Metadata.GrandparentTitle)
		targetYear := payload.Metadata.Year

		logger.Printf("[DEBUG] Webhook for '%s' / '%s' (%d). Current registry:", targetTitle, seriesTitle, targetYear)
		playbackRegistry.Range(func(key, value interface{}) bool {
			logger.Printf("  - Registered: %s", key.(string))
			return true
		})

		// Two-pass matching: exact first (filename, IMDB), fuzzy only as fallback.
		// The hash suffix is not a key: season-pack episodes share it (HashPrefix =
		// hash[:8]), so a suffix-only match can bind the wrong episode.

		// Extract IMDB ID via regex on raw payload (struct unmarshal would cause UnmarshalTypeError
		// due to Plex sending both lowercase "guid" and capital "Guid" fields).
		webhookImdbID := webhook.ImdbID(payloadStr)
		identities := webhook.IdentityKeysFromPayload(sanitized)

		// Pass 1: Exact matches only (filename, IMDB ID).
		var basenames []string
		for _, m := range payload.Metadata.Media {
			for _, p := range m.Part {
				if b := filepath.Base(p.File); b != "" && b != "." {
					basenames = append(basenames, b)
				}
			}
		}
		exactMatch, exactState := findExactMatch(
			exactMatchKeys{imdbID: webhookImdbID, identities: identities, basenames: basenames},
			snapshotPlaybackEntries(),
		)

		// Pass 1c: IMDB bootstrap — if webhookImdbID is available but no state has it yet,
		// find the unique registered path of the matching library type with empty ImdbID.
		// One-time bootstrap: saves webhookImdbID into state so future sessions match via 0a.
		if exactMatch == "" && webhookImdbID != "" {
			sectionDir := ""
			switch payload.Metadata.LibrarySectionType {
			case "show":
				sectionDir = "/tv/"
			case "movie":
				sectionDir = "/movies/"
			}
			if sectionDir != "" {
				var bootPath string
				var bootState *PlaybackState
				bootCount := 0
				playbackRegistry.Range(func(key, value interface{}) bool {
					path := key.(string)
					state := value.(*PlaybackState)
					if strings.Contains(path, sectionDir) && state.GetImdbID() == "" {
						bootPath = path
						bootState = state
						bootCount++
					}
					return true
				})
				if bootCount == 1 {
					exactMatch = bootPath
					exactState = bootState
				}
			}
		}

		// Pass 2: Fuzzy matches only if no exact match found. Never for audio: the
		// identity pass above is the precise match, and a name guess on a music payload
		// could confirm a different track of the same album.
		if exactMatch == "" && !audioWebhook {
			var bestMatch string
			var bestState *PlaybackState
			bestLevel := 0 // higher = better match

			playbackRegistry.Range(func(key, value interface{}) bool {
				path := key.(string)
				filename := strings.ToLower(filepath.Base(path))
				level := 0

				// Tentativo 2: Match per Titolo completo (dot and underscore separators)
				if targetTitle != "" && (strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", ".")) ||
					strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", "_"))) {
					level = 3
				}

				// Tentativo 3: Match per Titolo Serie (TV Shows)
				if level == 0 && seriesTitle != "" {
					if strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", ".")) ||
						strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", "_")) {
						level = 2
					} else {
						words := strings.Fields(seriesTitle)
						if len(words) > 0 && len(words[0]) > 4 {
							cleanWord := strings.TrimRight(words[0], ":.,;!?")
							if len(cleanWord) > 4 && strings.Contains(filename, cleanWord) {
								level = 1
							}
						}
					}
				}

				// Tentativo 4: Match per prima parola + anno (first word > 3 chars to avoid "the", "a")
				if level == 0 && len(strings.Fields(targetTitle)) > 0 {
					firstWord := strings.Fields(targetTitle)[0]
					if len(firstWord) > 3 && strings.Contains(filename, firstWord) {
						if targetYear > 0 && strings.Contains(filename, strconv.Itoa(targetYear)) {
							level = 1
						}
					}
				}

				if level > bestLevel {
					bestLevel = level
					bestMatch = path
					bestState = value.(*PlaybackState)
				}
				return true
			})
			exactMatch = bestMatch
			exactState = bestState
		}

		if exactMatch != "" && exactState != nil {
			exactState.SetHealthy(true)
			eng.FriendsPlayback(exactState.Hash, exactMatch, "webhook", true)
			exactState.mu.Lock()
			exactState.IsStopped = false
			// Cache webhookImdbID into state for fast IMDB matching in future sessions.
			// A pass-1 match is precise (filename or IMDB ID) and cannot cross-pollinate
			// episodes of a season pack; the pass-1c/fuzzy fallbacks are less precise.
			if exactState.ImdbID == "" && webhookImdbID != "" {
				exactState.ImdbID = webhookImdbID
				logger.Printf("[PLEX] IMDB ID cached for future matching: %s → %s", filepath.Base(exactMatch), webhookImdbID)
			}
			exactState.mu.Unlock()
			logger.Printf("[PLEX] Playback confirmed by webhook for: %s", filepath.Base(exactMatch))

			if exactState.Hash != "" {
				if t := eng.Get(exactState.Hash); t != nil {
					t.IsPriority.Store(true)
					nativeBridge.SetAggressive(exactState.Hash, true, GetEffectiveConcurrencyLimit())
					logger.Printf("[PLEX] High Priority + Aggressive Mode for: %s", exactState.Hash)
				}
			}
		}
	} else if payload.Event == "media.stop" {
		targetTitle := strings.ToLower(payload.Metadata.Title)
		seriesTitle := strings.ToLower(payload.Metadata.GrandparentTitle)
		targetYear := payload.Metadata.Year

		stopImdbID := webhook.ImdbID(payloadStr)
		stopIdentities := webhook.IdentityKeysFromPayload(sanitized)

		var basenames []string
		for _, m := range payload.Metadata.Media {
			for _, p := range m.Part {
				if b := filepath.Base(p.File); b != "" && b != "." {
					basenames = append(basenames, b)
				}
			}
		}

		// Pass 1: Exact matches (filename, IMDB ID) — never the shared hash suffix.
		stopMatch, stopState := findExactMatch(
			exactMatchKeys{imdbID: stopImdbID, identities: stopIdentities, basenames: basenames},
			snapshotPlaybackEntries(),
		)

		stopMatchFromFuzzy := false

		// Pass 2: Fuzzy matches only if no exact match, and never for audio (see the
		// play path): a name guess must not stop the wrong track.
		if stopMatch == "" && !audioWebhook {
			bestLevel := 0
			playbackRegistry.Range(func(key, value interface{}) bool {
				path := key.(string)
				filename := strings.ToLower(filepath.Base(path))
				level := 0

				if targetTitle != "" && (strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", ".")) ||
					strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", "_"))) {
					level = 3
				}

				if level == 0 && seriesTitle != "" {
					if strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", ".")) ||
						strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", "_")) {
						level = 2
					} else {
						words := strings.Fields(seriesTitle)
						if len(words) > 0 && len(words[0]) > 4 {
							cleanWord := strings.TrimRight(words[0], ":.,;!?")
							if len(cleanWord) > 4 && strings.Contains(filename, cleanWord) {
								level = 1
							}
						}
					}
				}

				if level == 0 && len(strings.Fields(targetTitle)) > 0 {
					firstWord := strings.Fields(targetTitle)[0]
					if len(firstWord) > 3 && strings.Contains(filename, firstWord) {
						if targetYear > 0 && strings.Contains(filename, strconv.Itoa(targetYear)) {
							level = 1
						}
					}
				}

				if level > bestLevel {
					bestLevel = level
					stopMatch = path
					stopState = value.(*PlaybackState)
					stopMatchFromFuzzy = true
				}
				return true
			})
		}

		// Only a basename match (pass 1) is the file by construction. An IMDB-only match
		// is precise on this deployment (episode-level IDs) but only when the registry
		// holds exactly one entry with that ID; otherwise it is a guess like fuzzy.
		matchByBasename := false
		if stopMatch != "" {
			base := filepath.Base(stopMatch)
			for _, b := range basenames {
				if b == base {
					matchByBasename = true
					break
				}
			}
		}
		imdbUnique := false
		if stopMatch != "" && !matchByBasename && !stopMatchFromFuzzy && stopImdbID != "" {
			n := 0
			playbackRegistry.Range(func(_, v interface{}) bool {
				ps, ok := v.(*PlaybackState)
				if !ok || ps == nil {
					return true
				}
				if ps.GetImdbID() == stopImdbID {
					n++
				}
				return n <= 1
			})
			imdbUnique = n == 1
		}
		// An audio identity match is exact: the matched state's registered id is one
		// of the ids the webhook carried, at the best rank available. It is trusted
		// like a basename even though sibling tracks can share a coarser id, because
		// the identity pass preferred the session with the most specific match.
		matchByIdentity := false
		if stopMatch != "" && stopState != nil && !stopIdentities.Empty() {
			id, ns := stopState.GetExternalIdentity()
			if id != "" && (ns == "" || strings.EqualFold(ns, "musicbrainz")) {
				_, matchByIdentity = stopIdentities.Rank(id)
			}
		}

		// A fuzzy (pass 2) match is a name-similarity guess; an IMDB-only match is only
		// trusted when unique. If such a match still has a live handle it is likely a
		// sibling episode being read, so a late or spurious stop must not kill it.
		// Basename and unique-IMDB matches are honored even with the handle open:
		// ignoring them would leave the pump alive until the 2h idle timeout (V262).
		if stopMatch != "" && stopState != nil && !matchByBasename && !matchByIdentity && !imdbUnique && anyLiveHandleFor(stopMatch) {
			logger.Printf("[PLEX] STOP ignored for %s: fuzzy or ambiguous match with handle still open", filepath.Base(stopMatch))
			stopState.noteIgnoredStop(time.Now())
		} else if stopMatch != "" && stopState != nil {
			applyPlexStop(stopMatch, stopState, payload.Event)
		}
	}
	w.WriteHeader(200)
}

// applyPlexStop ends playback for path: state stopped, pump cancelled, shield reset, priority dropped.
func applyPlexStop(stopMatch string, stopState *PlaybackState, event string) {
	stopState.mu.Lock()
	stopState.IsStopped = true
	stopState.mu.Unlock()
	stopState.SetHealthy(false) // persists with IsStopped=true
	eng.FriendsPlayback(stopState.Hash, stopMatch, "", false)
	logger.Printf("[PLEX] STOP applied for: %s (Event: %s)", filepath.Base(stopMatch), event)

	if val, ok := activePumps.Load(stopMatch); ok {
		ps := val.(*NativePumpState)
		if ps.cancel != nil {
			ps.cancel()
		}
		activePumps.Delete(stopMatch)
		logger.Printf("[PLEX] STOP: force-terminated pump for %s", filepath.Base(stopMatch))
	}

	eng.ResetShield(stopState.Hash)
	logger.Printf("[AdaptiveShield] Shield reset on media.stop")

	// Pull the rest of the tail window now that the pump is gone: piece requests at
	// the file end would otherwise compete with the sequential download front. Ready
	// for the next open, which is when the MKV Cues probe needs it.
	if warmup.DiskWarmup != nil {
		if v, ok := tailFillTargets.Load(stopMatch); ok {
			t := v.(tailFillTarget)
			warmup.DiskWarmup.EnsureTail(t.hash, t.fileID, t.size)
		}
	}

	// Deactivate Core Priority unless a sibling episode of the same torrent is
	// still live (shared hash): priority/aggressive off and the 30s expiry would
	// hit the stream that is actually playing.
	if shouldDropTorrentPriority(stopState.Hash, stopMatch) {
		if t := eng.Get(stopState.Hash); t != nil {
			t.IsPriority.Store(false)
			nativeBridge.SetAggressive(stopState.Hash, false, 0) // Back to normal download priority
			t.AddExpiredTime(30 * time.Second)
			logger.Printf("[PLEX] STOP detected. Grace period 30s for: %s", stopState.Hash)
		}
	}
}

// V750: Persist a PlaybackState to SQLite (best-effort)
func savePlaybackStateToDB(ps *PlaybackState) {
	if stateDB == nil {
		return
	}
	ps.mu.RLock()
	rec := &metadb.PlaybackRecord{
		Path:         ps.Path,
		Hash:         ps.Hash,
		ImdbID:       ps.ImdbID,
		ExternalID:   ps.ExternalID,
		ExternalIDNS: ps.ExternalIDNamespace,
		OpenedAt:     ps.OpenedAt,
		ConfirmedAt:  ps.ConfirmedAt,
		IsHealthy:    ps.IsHealthy,
		IsStopped:    ps.IsStopped,
		LastReadAt:   ps.LastReadAt,
		ReadCount:    ps.ReadCount,
		LastSeekOff:  ps.LastSeekOff,
	}
	ps.mu.RUnlock()
	if err := stateDB.SavePlaybackState(rec); err != nil {
		logger.Printf("[V750] Failed to persist playback state for %s: %v", filepath.Base(rec.Path), err)
	}
}

// V750: Restore PlaybackState from SQLite at boot
func restorePlaybackStates(db *metadb.DB) {
	records, err := db.LoadPlaybackStates(4 * time.Hour)
	if err != nil {
		logger.Printf("[V750] Failed to load playback states: %v", err)
		return
	}
	restored := 0
	priorityApplied := 0
	for _, rec := range records {
		if rec.IsHealthy && !rec.ConfirmedAt.IsZero() {
			ps := &PlaybackState{
				Path:                rec.Path,
				Hash:                rec.Hash,
				ImdbID:              rec.ImdbID,
				ExternalID:          rec.ExternalID,
				ExternalIDNamespace: rec.ExternalIDNS,
				OpenedAt:            rec.OpenedAt,
				ConfirmedAt:         rec.ConfirmedAt,
				IsHealthy:           true,
				ReadCount:           rec.ReadCount,
				LastSeekOff:         rec.LastSeekOff,
				LastReadAt:          rec.LastReadAt,
			}
			playbackRegistry.Store(rec.Path, ps)
			// Restore Tentacle priority
			if rec.Hash != "" {
				if t := eng.Get(rec.Hash); t != nil {
					t.IsPriority.Store(true)
					nativeBridge.SetAggressive(rec.Hash, true, GetEffectiveConcurrencyLimit())
					priorityApplied++
					logger.Printf("[V750] Priority RESTORED from DB: %s", filepath.Base(rec.Path))
				}
			}
			restored++
		}
	}
	logger.Printf("[V750] Restored %d/%d PlaybackStates, %d priorities reapplied", restored, len(records), priorityApplied)
}
