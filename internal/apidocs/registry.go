// Package apidocs is the reference of the stable HTTP API on :9080, served as a page at /api-docs.
// The registry is the contract: tests check every path and MCP tool against the handlers.
package apidocs

// Param is one request field or query parameter.
type Param struct {
	Name, Type string
	Required   bool
	Desc       string
}

// Status is one response code worth knowing.
type Status struct {
	Code int
	Desc string
}

// Endpoint is one documented route. Example and Response are shown verbatim; Example may
// use {{ORIGIN}} for the address the browser is on.
type Endpoint struct {
	Method, Path, Summary string
	Desc                  string
	ParamsIn              string // "query" or "body"
	Params                []Param
	Example               string
	Response              string
	OK                    int
	Errors                []Status
}

// Group is a section of the page.
type Group struct {
	ID, Name, Desc string
	Endpoints      []Endpoint
}

// Common are the answers any endpoint can give, shown once above the groups.
var Common = []Status{
	{401, `sign-in on and no valid token: {"error":"unauthorized"}`},
	{403, "a token of the wrong type: MCP tokens open /mcp only, API tokens everything else but /mcp and /api/auth"},
	{302, "same, for a browser asking for a page: redirect to /login"},
	{403, "a write sent by a browser from another site, sign-in on or off. Behind a plain-HTTP proxy that rewrites Host, list it in auth_trusted_proxies and have it send X-Forwarded-Host"},
	{503, "sign-in state unreadable: run tiramisu --reset-auth"},
}

// MCPTools are the tools /mcp registers, in the order the page lists them.
var MCPTools = []string{
	"resolve_title", "search_releases", "get_job", "cancel_job", "add_release",
	"resolve_album", "search_album", "add_album", "inspect_torrent", "add_audio",
	"list_library", "list_gaps", "remove_entries", "check_consistency",
}

// Groups is the stable public API. UI-only routes stay out on purpose.
var Groups = []Group{
	{ID: "library", Name: "Library", Desc: `Add, list and remove titles without touching the filesystem. Errors are {"error":"…"}; bodies up to 1 MiB.`, Endpoints: []Endpoint{
		{
			Method: "POST", Path: "/api/library/add", Summary: "Register a release and write its virtual .mkv",
			Desc: "Does what the sync does for one release: registers the torrent, waits for its file list, writes the " +
				"stub with the usual naming and asks the media server to rescan. A failed call removes the torrent it added, " +
				"and so does a client that hangs up during the wait: keep its timeout above metadata_wait. " +
				"For movie and tv, path is the stub on disk and fuse_path the same file under the FUSE mount. " +
				"For music and audiobook (list the files with inspect first), path is relative to the section, there is no " +
				"fuse_path, each file carries state created or present, and unknown fields are refused.",
			ParamsIn: "body",
			Params: []Param{
				{"type", "string", false, "movie (default) or tv; music or audiobook, lowercase exactly"},
				{"hash", "string", false, "Info hash (40 hex or 32 base32). hash or magnet is required"},
				{"magnet", "string", false, "Magnet link; for video its hash wins over hash"},
				{"title", "string", true, "Title as the media server should show it"},
				{"release_title", "string", false, "Movies: raw release name, quality tags (DV, Atmos, REMUX) are read from it. Defaults to title"},
				{"year", "int", false, "Movies: release year"},
				{"release_date", "string", false, "Movies: release date; wins over year"},
				{"imdb", "string", false, "Movies: written into the stub"},
				{"season", "int", false, "TV: required"},
				{"episode", "int", false, "TV: leave it out for a season pack, which files every file named SxxEyy (seasons taken from the names) and always answers 201"},
				{"first_air_date", "string", false, "TV: puts the year in the series folder"},
				{"quality_score", "int", false, "TV: what the next sync compares against before replacing"},
				{"is_4k", "bool", false, "Movies: overrides the resolution read from release_title"},
				{"file_index", "int", false, "1-based file inside the torrent. Absent: the largest video, or for an episode the file named SxxEyy. Not on a season pack"},
				{"torrent_file", "base64", false, "The release's .torrent: metadata at once, with its own trackers. Must be the same release as hash; above ~750 KB it no longer fits the 1 MiB body"},
				{"metadata_wait", "int", false, "Seconds to wait for the swarm (default 60, max 300)"},
				{"files", "array", false, "Music and audiobook: required, 1-512 of {source_path, path[, cue_track, tags, external_id, external_id_ns]}. source_path comes from inspect; path is relative to the section and ends in _<last 8 hex of the hash> plus the source's extension (music .flac; audiobook .m4b, .m4a, .mp3)"},
			},
			Example: "curl -X POST --max-time 120 \\\n" +
				"  -H 'Authorization: Bearer tmr_…' \\\n" +
				"  -H 'Content-Type: application/json' \\\n" +
				"  -d '{\"type\":\"movie\",\"hash\":\"<infohash>\",\"title\":\"Dummy Bunny\",\"year\":2024,\n" +
				"       \"release_title\":\"Dummy.Bunny.2024.2160p.UHD.BluRay.REMUX.DV.Atmos-GRP\"}' \\\n" +
				"  {{ORIGIN}}/api/library/add",
			Response: "{\n  \"hash\": \"0a1b…e7f8a9b0\",\n  \"title\": \"Dummy Bunny\",\n  \"type\": \"movie\",\n  \"files\": [{\n" +
				"    \"path\": \"/…/movies/Dummy_Bunny_2024_2160p_DV_Atmos_REMUX_e7f8a9b0.mkv\",\n" +
				"    \"fuse_path\": \"movies/Dummy_Bunny_2024_2160p_DV_Atmos_REMUX_e7f8a9b0.mkv\",\n    \"size\": 61203886080,\n    \"file_index\": 1\n  }],\n" +
				"  \"already_present\": false\n}",
			OK: 201,
			Errors: []Status{{200, "already present"}, {400, "bad request, body over 1 MiB, unknown field (audio)"}, {405, "not POST"},
				{409, "audio: path or source already taken"}, {422, "no video, episode or source file to use"},
				{500, "stub or registry write failed"}, {502, "engine refused the torrent"},
				{503, "state DB, media directory, audio registry or cue sheet unavailable"}, {504, "no metadata in time, or the client hung up"}},
		},
		{
			Method: "GET", Path: "/api/library/list", Summary: "What the library holds: movie, tv, gaps or audio",
			Desc: "One entry per stub. movie and tv answer an array (hash is the full lowercase hex). gaps answers the episodes " +
				"the reaper removed, oldest first, at most 500 with the total in the X-Total-Count header. music and audiobook " +
				"answer {items, next_cursor}: next_cursor is \"\" on the last page, otherwise pass it back as cursor. " +
				"Any other type, including series, show or Music, falls back to the movie list.",
			ParamsIn: "query",
			Params: []Param{
				{"type", "string", false, "movie (default), tv, gaps, music or audiobook"},
				{"prefix", "string", false, "Audio: only paths starting with this string; end it with / for one folder, e.g. Artist/"},
				{"limit", "int", false, "Audio: page size (default 200, max 1000)"},
				{"cursor", "string", false, "Audio: next_cursor of the previous page"},
				{"failures", "1", false, "Audio: add the reachability counters (only the value 1)"},
			},
			Example:  "curl -H 'Authorization: Bearer tmr_…' \\\n  '{{ORIGIN}}/api/library/list?type=tv'",
			Response: "[\n  {\n    \"path\": \"/…/tv/Dummy_Bunny (2015)/Season.01/Dummy_Bunny_S01E02_0a1b2c3d.mkv\",\n    \"fuse_path\": \"tv/Dummy_Bunny (2015)/Season.01/Dummy_Bunny_S01E02_0a1b2c3d.mkv\",\n    \"size\": 2147483648,\n    \"hash\": \"0a1b2c3d…\",\n    \"file_index\": 1,\n    \"season\": 1,\n    \"episode\": 2\n  }\n]",
			OK:       200,
			Errors:   []Status{{405, "not GET"}, {500, "a media directory or the gaps could not be read"}, {503, "audio registry unavailable"}},
		},
		{
			Method: "GET", Path: "/api/library/diagnostics", Summary: "Read-only consistency check of the library against the engine",
			Desc: "Compares the torrents Tentacle holds with what the library references (stubs under movies/ and tv/, tv_episodes, audio " +
				"projections) and with the inode map. Each check has a count and up to 20 samples: orphan_torrents (with " +
				"older_than_30_days), missing_torrents, stubs_without_hash, tv_stubs_unregistered, tv_registry_without_stub, " +
				"inode_ghosts, stale_metadata_failures, stuck_audio. It changes nothing; one run at a time. Torrents added in " +
				"the last 10 minutes are not counted as orphans; while a sync runs the counts can be off by what it is changing. " +
				"With cached=1 it returns the last result since startup (null before the first) without walking anything.",
			Params:   []Param{{"cached", "1", false, "Return the last result instead of running the check"}},
			ParamsIn: "query",
			Example:  "curl -H 'Authorization: Bearer tmr_…' {{ORIGIN}}/api/library/diagnostics",
			Response: "{\n  \"at\": 1795600000,\n  \"duration_ms\": 900,\n  \"checks\": [\n    {\"id\": \"orphan_torrents\", \"description\": \"…\", \"count\": 598, \"older_than_30_days\": 594, \"samples\": [\"0a1b… Some.Release (2026-09-23)\"]}\n  ]\n}",
			OK:       200,
			Errors:   []Status{{405, "not GET"}, {409, "a check is already running"}, {500, "the state DB or a media directory could not be read"}},
		},
		{
			Method: "POST", Path: "/api/library/orphans", Summary: "List or remove the torrents nothing references",
			Desc: "The orphan torrents of the consistency check, older than min_age_days, oldest first. dry_run defaults to " +
				"true and only lists them; with dry_run false each one is removed from the engine after a last check that no " +
				"stub, tv_episodes row, audio projection or add in flight cites it (kept otherwise). At most max per call. " +
				"Refused while a stub cannot be read, since its torrent cannot be told. One run at a time, shared with the check.",
			Params: []Param{
				{"dry_run", "bool", false, "true (default) lists only"},
				{"max", "int", false, "Torrents per call, default 50, at most 1000"},
				{"min_age_days", "int", false, "Only torrents added before this many days ago, default 7"},
				{"counters_only", "bool", false, "With dry_run false: clear the stale failure counters, remove no torrent"},
			},
			ParamsIn: "body",
			Example:  "curl -X POST -H 'Authorization: Bearer tmr_…' -d '{\"dry_run\":true}' {{ORIGIN}}/api/library/orphans",
			Response: "{\n  \"dry_run\": true,\n  \"candidates\": 598,\n  \"selected\": [{\"hash\": \"0a1b…\", \"title\": \"Some.Release\", \"added\": \"2026-04-04\"}],\n  \"released\": [], \"kept\": [], \"failed\": [],\n  \"duration_ms\": 1100\n}",
			OK:       200,
			Errors:   []Status{{400, "malformed body"}, {405, "not POST"}, {409, "a check or collection is running, or a stub cannot be read"}, {500, "the state DB or a media directory could not be read"}},
		},
		{
			Method: "POST", Path: "/api/library/release", Summary: "Remove a torrent from the engine if nothing references it",
			Desc: "For a client that hydrated a torrent (inspect) and filed nothing from it. The torrent goes only if no stub, " +
				"tv_episodes row, audio projection or add in flight cites it; released says whether it went.",
			Params:   []Param{{"hash", "string", true, "40-character info hash"}},
			ParamsIn: "body",
			Example:  "curl -X POST -H 'Authorization: Bearer tmr_…' -d '{\"hash\":\"0a1b…\"}' {{ORIGIN}}/api/library/release",
			Response: "{\"released\": true}",
			OK:       200,
			Errors:   []Status{{400, "malformed body or hash"}, {404, "the engine does not hold this torrent"}, {405, "not POST"}, {502, "the engine or a reference check failed"}},
		},
		{
			Method: "POST", Path: "/api/library/remove", Summary: "Remove a stub, optionally blacklisting the release",
			Desc: "The torrent goes only when no other stub points at it: a season pack is one torrent behind many episodes. " +
				"Without blacklist the sync may add the title back, which is what you want when replacing a release. " +
				"warnings appears only when something went half-way, e.g. the stub went but the blacklist was not written; " +
				"torrent_errors when the stub went but the engine kept the torrent (the orphan collection takes it later). " +
				"Audio is idempotent and answers other objects: by path {type, path, removed (bool), state \"absent\" when " +
				"nothing was there, torrent_referenced, torrent_dropped}; by prefix {type, prefix, removed (count), " +
				"torrent_referenced, torrent_dropped}.",
			ParamsIn: "body",
			Params: []Param{
				{"path", "string", false, "Stub path as list returns it (absolute, relative or fuse_path). One of path, hash or prefix is required"},
				{"hash", "string", false, "Video: every stub of this torrent (40 hex)"},
				{"prefix", "string", false, "Audio: a whole album by folder; not together with path"},
				{"type", "string", false, "music or audiobook, exactly; anything else is treated as video"},
				{"blacklist", "bool", false, "Video: keep the release out of future syncs (audio answers 400)"},
				{"drop_torrent", "bool", false, "Audio: also remove the torrent once nothing uses it"},
			},
			Example: "curl -X POST -H 'Authorization: Bearer tmr_…' \\\n" +
				"  -H 'Content-Type: application/json' \\\n" +
				"  -d '{\"path\":\"movies/Dummy_Bunny_2024_2160p_DV_Atmos_REMUX_e7f8a9b0.mkv\",\"blacklist\":true}' \\\n" +
				"  {{ORIGIN}}/api/library/remove",
			Response: "{\n  \"removed\": [\"/…/movies/Dummy_Bunny_2024_2160p_DV_Atmos_REMUX_e7f8a9b0.mkv\"]\n}",
			OK:       200,
			Errors: []Status{{400, "bad request"}, {404, "video: no stub at that path or with that hash"}, {405, "not POST"},
				{409, "audio: the file on disk is not the expected stub, or the prefix spans several torrents"},
				{500, "a stub could not be deleted"}, {503, "audio registry unavailable"}},
		},
		{
			Method: "POST", Path: "/api/library/inspect", Summary: "Hydrate a torrent and list its files (music, audiobooks)",
			Desc: "Step one for audio: the engine adds the torrent and returns its files, and the tracks of any cue sheet, so the " +
				"caller can name each file in add. The torrent stays in the engine afterwards: add it, or remove it, to settle it.",
			ParamsIn: "body",
			Params: []Param{
				{"hash", "string", false, "Info hash. hash or magnet is required; if both, they must agree"},
				{"magnet", "string", false, "Magnet link"},
				{"title", "string", true, "Shown in the engine while it resolves"},
				{"torrent_file", "base64", false, "The release's .torrent, as for add"},
				{"metadata_wait", "int", false, "Seconds to wait for the swarm (default 60, max 300)"},
			},
			Example: "curl -X POST --max-time 120 -H 'Authorization: Bearer tmr_…' \\\n" +
				"  -H 'Content-Type: application/json' \\\n" +
				"  -d '{\"magnet\":\"magnet:?xt=urn:btih:…\",\"title\":\"Dummy Bunny - Album\"}' \\\n  {{ORIGIN}}/api/library/inspect",
			Response: "{\n  \"hash\": \"5c6d…\",\n  \"files\": [\n    {\"source_path\": \"Album/01 - Intro.flac\", \"file_index\": 1, \"size\": 31457280}\n  ],\n  \"cue_tracks\": [\n    {\"source_path\": \"Album/Album.flac\", \"track\": 1, \"title\": \"Intro\", \"performer\": \"Dummy Bunny\"}\n  ]\n}",
			OK:       200,
			Errors: []Status{{400, "bad request, hash and magnet disagree, torrent_file of another release"}, {405, "not POST"},
				{408, "request cancelled"}, {502, "engine refused the torrent"}, {504, "no metadata in time"}},
		},
	}},
	{ID: "scheduler", Name: "Scheduler", Desc: "Run the sync jobs on demand and read their state. Error bodies are plain text.", Endpoints: []Endpoint{
		{
			Method: "GET", Path: "/api/scheduler/status", Summary: "Last and next run of every job",
			Desc: "One entry per job (movies, tv, music, watchlist). last_run is when the job last finished, in the server's time " +
				"zone; a job that never ran shows 0001-01-01T00:00:00Z. next_run follows the job's schedule and is meaningful " +
				"only with the scheduler enabled. last_status is ok, failed or stopped (absent before the first run); " +
				"last_error appears after a failure.",
			Example: "curl -H 'Authorization: Bearer tmr_…' {{ORIGIN}}/api/scheduler/status",
			Errors:  []Status{{405, "not GET"}},
			Response: "{\n  \"movies\": {\n    \"last_run\": \"2026-11-17T03:41:12.483920117+01:00\",\n    \"next_run\": \"2026-11-18T03:00:00+01:00\",\n    \"running\": false,\n    \"last_status\": \"ok\"\n  },\n" +
				"  \"music\": {\n    \"last_run\": \"2026-11-16T04:02:09.1+01:00\",\n    \"next_run\": \"0001-01-01T00:00:00Z\",\n    \"running\": false,\n    \"last_error\": \"plex reports no music library\",\n    \"last_status\": \"failed\"\n  },\n  \"tv\": { … },\n  \"watchlist\": { … }\n}",
			OK: 200,
		},
		{
			Method: "POST", Path: "/api/scheduler/{job}/run", Summary: "Start a job now: movies, tv, watchlist or music",
			Desc: "Answers at once; the job runs in the background, follow it with status. A manual run ignores the schedule " +
				"switches: a job disabled in the configuration still runs.",
			Example: "curl -X POST -H 'Authorization: Bearer tmr_…' {{ORIGIN}}/api/scheduler/movies/run",
			OK:      202,
			Errors:  []Status{{404, "not /run or /stop"}, {405, "not POST"}, {409, "unknown job, or already running"}},
		},
		{
			Method: "POST", Path: "/api/scheduler/{job}/stop", Summary: "Ask a running job to stop",
			Desc:    "Answers at once; running stays true until the job notices, then last_status becomes stopped.",
			Example: "curl -X POST -H 'Authorization: Bearer tmr_…' {{ORIGIN}}/api/scheduler/movies/stop",
			OK:      202,
			Errors:  []Status{{404, "not /run or /stop"}, {405, "not POST"}, {409, "not running (also for an unknown job)"}},
		},
	}},
	{ID: "status", Name: "Status & metrics", Desc: "Read-only: what the engine is doing.", Endpoints: []Endpoint{
		{
			Method: "GET", Path: "/api/health", Summary: "Tentacle, FUSE, VPN, media servers and system load",
			Desc: "A snapshot refreshed every 2.5 s, always 200: a down service shows ok false. Right after start every field is " +
				"empty until the first refresh. media_servers is null when none is configured; cache_hit_rate_pct is null " +
				"unless something is streaming. download_mbps and upload_mbps are megabits per second.",
			Example: "curl -H 'Authorization: Bearer tmr_…' {{ORIGIN}}/api/health",
			Response: "{\n  \"timestamp\": \"2026-11-17T20:01:05.12+01:00\",\n  \"uptime\": \"3h12m5s\",\n  \"cpu_pct\": 14.2,\n  \"ram_pct\": 48.1,\n" +
				"  \"tentacle\": {\"ok\": true, \"latency_ms\": 2},\n  \"fuse\": {\"ok\": true, \"latency_ms\": 0},\n  \"vpn\": {\"ok\": true, \"latency_ms\": 0},\n" +
				"  \"natpmp\": {\"ok\": true, \"latency_ms\": 0},\n  \"media_servers\": [{\"product\": \"Plex\", \"ok\": true, \"latency_ms\": 12}],\n" +
				"  \"total_torrents\": 3,\n  \"active_count\": 1,\n  \"download_mbps\": 38.4,\n  \"upload_mbps\": 1.2,\n  \"cache_hit_rate_pct\": null,\n  …\n}",
			OK: 200,
		},
		{
			Method: "GET", Path: "/api/torrents", Summary: "Torrents in memory with speed and peers",
			Desc: "Refreshed every 2.5 s. status is idle, downloading (above 0.01 MiB/s) or streaming (active but not " +
				"downloading); speed_mbs and up_mbs are MiB/s; each entry of peer_rates carries kbps (download) and up_kbps (upload) in KiB/s. title is the engine's name; clean_title is filled only while a media " +
				"server plays it. cache_filled and cache_capacity are the bytes the piece cache in RAM holds for the torrent and its size. An unreachable engine answers [].",
			Example: "curl -H 'Authorization: Bearer tmr_…' {{ORIGIN}}/api/torrents",
			Response: "[\n  {\n    \"hash\": \"e7f8…\",\n    \"title\": \"Dummy.Bunny.2024.2160p.DV.mkv\",\n    \"clean_title\": \"\",\n    \"speed_mbs\": 4.8,\n    \"up_mbs\": 0.15,\n    \"peers\": 31,\n    \"seeders\": 22,\n" +
				"    \"size\": 61203886080,\n    \"bytes_read\": 1073741824,\n    \"is_priority\": true,\n    \"status\": \"downloading\",\n    \"is_4k\": true,\n    \"is_dv\": true\n  }\n]",
			OK: 200,
		},
		{
			Method: "GET", Path: "/metrics", Summary: "Counters and gauges, one JSON object",
			Desc: "Stable fields: version, uptime (unrounded Go duration), latest_version (empty until the " +
				"first update check), update_available, config_source, read_ahead_*, shield_active, github_stars. The " +
				"other fields are diagnostic and may change between releases.",
			Example: "curl -H 'Authorization: Bearer tmr_…' {{ORIGIN}}/metrics | jq '{version, uptime, update_available}'",
			Response: "{\n  \"version\": \"v1.11.15\",\n  \"config_source\": \"/home/pi/Tiramisu/STATE/tiramisu.db#42\",\n  \"uptime\": \"3h12m5.183746291s\",\n" +
				"  \"latest_version\": \"v1.11.15\",\n  \"update_available\": false,\n  \"read_ahead_budget\": 268435456,\n  \"shield_active\": false,\n  \"github_stars\": 159,\n  …\n}",
			OK: 200,
		},
		{
			Method: "GET", Path: "/metrics/ttff", Summary: "Time to first frame, stalls and seek latency",
			Desc: "Cumulative since start. Each histogram is {avg, p50, p95, p99, max, count} in whole " +
				"milliseconds; percentiles are bucket bounds (1, 5, 10, 25, 50, 100, 250, 500, 1000, 3000, 10000 ms), all zero " +
				"without samples. Header, deep-read and stall figures are added when a session closes (60 s idle); seek and " +
				"read latency update live. A stall is a read over 1 s.",
			Example: "curl -H 'Authorization: Bearer tmr_…' {{ORIGIN}}/metrics/ttff",
			Response: "{\n  \"sessions_completed\": 42,\n  \"sessions_filtered\": 3,\n  \"open_to_header_ms\": {\"avg\": 412, \"p50\": 250, \"p95\": 1000, \"p99\": 3000, \"max\": 2870, \"count\": 42},\n" +
				"  \"open_to_deep_read_ms\": { … },\n  \"seek_latency_ms\": { … },\n  \"read_latency_ms\": {\"warmup_head\": { … }, \"warmup_tail\": { … }, \"racache_hit\": { … }, \"fetch_block\": { … }},\n" +
				"  \"stalls\": {\"count\": 2, \"max_ms\": 1400}\n}",
			OK: 200,
		},
	}},
	{ID: "config", Name: "Configuration", Desc: "The configuration and the Tentacle settings, stored in tiramisu.db as versions. Secrets are write-only. Error bodies are plain text.", Endpoints: []Endpoint{
		{
			Method: "GET", Path: "/api/config", Summary: "Current configuration, secrets left empty",
			Desc: "The values in effect, environment overrides included. The secrets (Plex token, TMDB and Prowlarr keys) come " +
				"back empty; secrets_configured says which are set.",
			Example: "curl -H 'Authorization: Bearer tmr_…' {{ORIGIN}}/api/config > config.json",
			OK:      200,
		},
		{
			Method: "POST", Path: "/api/config", Summary: "Change settings",
			Desc: "Send only the keys to change: the rest keeps its value, nested objects merge key by key, arrays are " +
				"replaced whole; null resets only arrays and optional objects. A scoring profile never saved starts from the " +
				"defaults. The whole object GET returns works too. An empty secret keeps the stored one. Each change is saved " +
				"as a new version; values set by environment variables are never saved, a key sent back with the env's value " +
				"keeps the saved one. A request that changes nothing adds no version. The blocklist, log_level and telemetry " +
				"apply at once; everything else is read at start-up (mount, syncers, clients) and needs POST /api/restart.",
			ParamsIn: "body",
			Example: "curl -X POST -H 'Authorization: Bearer tmr_…' \\\n" +
				"  -H 'Content-Type: application/json' \\\n  -d '{\"log_level\":\"DEBUG\"}' {{ORIGIN}}/api/config",
			Response: "(empty body)",
			OK:       200,
			Errors:   []Status{{400, "not a JSON object, a field of the wrong type, or over 1 MiB"}, {405, "not GET or POST"}, {403, "with sign-in on, an API token changed a key that only a signed-in session may change"}, {500, "the configuration could not be saved"}},
		},
		{
			Method: "GET", Path: "/api/config/history", Summary: "Configuration versions, newest first",
			Desc: "The last 50 saved versions, each with who saved it (source ui, api, internal, import, cli, rollback, " +
				"default; actor the user or the token name) and the keys it changed against the version before. The very first " +
				"version is compared to the defaults; once older versions are pruned the oldest kept one is a baseline " +
				"(baseline true, no changes). Secrets show as changed, never their values. restart says the version's changes " +
				"need POST /api/restart; it is not a restart pending. With sign-in on, only a signed-in Control Panel session: " +
				"tokens get 403.",
			ParamsIn: "query",
			Params:   []Param{{Name: "limit", Type: "int", Desc: "how many versions, 1-50 (default 50)"}},
			Example:  "curl {{ORIGIN}}/api/config/history?limit=5",
			Errors:   []Status{{403, "sign-in on and no Control Panel session"}},
			Response: "[\n  {\"id\": 42, \"created_at\": 1795530000, \"source\": \"ui\", \"actor\": \"admin\", \"note\": \"\",\n" +
				"   \"changes\": [{\"key\": \"log_level\", \"from\": \"\\\"INFO\\\"\", \"to\": \"\\\"DEBUG\\\"\"}],\n" +
				"   \"restart\": false, \"current\": true}\n]",
			OK: 200,
		},
		{
			Method: "GET", Path: "/api/config/history/{id}", Summary: "One saved version",
			Desc:    "The configuration as version id saved it, secrets left empty as in GET /api/config. Same access rule as the history.",
			Example: "curl {{ORIGIN}}/api/config/history/41",
			OK:      200,
			Errors:  []Status{{400, "id is not a number"}, {403, "sign-in on and no Control Panel session"}, {404, "no such version: the history keeps the last 50"}},
		},
		{
			Method: "GET", Path: "/api/config/history/{id}/restore", Summary: "What restoring a version would change",
			Desc: "The changes POST /api/config/rollback would make for id, without saving anything: the same answer, computed " +
				"against the configuration saved now. A key or token changed and later set back is no change. Same access rule as the history.",
			Example:  "curl {{ORIGIN}}/api/config/history/41/restore",
			Response: "{\"id\": 41, \"changed\": true, \"changes\": [{\"key\": \"metrics_port\", \"from\": \"9191\", \"to\": \"9080\"}], \"restart\": true}",
			OK:       200,
			Errors:   []Status{{400, "id is not a number"}, {403, "sign-in on and no Control Panel session"}, {404, "no such version"}},
		},
		{
			Method: "POST", Path: "/api/config/rollback", Summary: "Restore a saved version",
			Desc: "Saves version id again as the newest one (source rollback, note \"rollback to #id\"): nothing is deleted, so a " +
				"rollback can be rolled back. It keeps the install's telemetry id, and brings back the keys and tokens that version " +
				"had. It applies like POST /api/config; restart says whether POST /api/restart is needed. Unlike POST /api/config " +
				"it answers with the new version and what changed; changed is false when the version equals the one in use " +
				"(nothing is saved). Same access rule as the history.",
			ParamsIn: "body",
			Params:   []Param{{Name: "id", Type: "int", Required: true, Desc: "the version to restore"}},
			Example:  "curl -X POST -H 'Content-Type: application/json' -d '{\"id\":41}' {{ORIGIN}}/api/config/rollback",
			Response: "{\"id\": 43, \"changed\": true, \"changes\": [{\"key\": \"metrics_port\", \"from\": \"9191\", \"to\": \"9080\"}], \"restart\": true}",
			OK:       200,
			Errors:   []Status{{400, "the body is not {\"id\": N}"}, {403, "sign-in on and no Control Panel session"}, {404, "no such version"}, {500, "the configuration could not be saved"}},
		},
		{
			Method: "POST", Path: "/api/tentacle/settings", Summary: "Read, change or reset the Tentacle settings",
			Desc: "action get answers with the settings document in use (field names as keys, keys Tentacle does not interpret " +
				"included). action set saves sets: only the keys it names change, the others keep their values; unknown keys " +
				"are ignored and values out of range are clamped (ReaderReadAHead 5-100; ConnectionsLimit, CacheSize and " +
				"TorrentDisconnectTimeout at 0 or below take 25, 128 MiB and 30). action def puts the defaults back. set and def " +
				"answer 200 with an empty body and are recorded in the history unless nothing changed. TorrentDisconnectTimeout, " +
				"RetrackersMode, ResponsiveMode, AdaptiveShield and ReaderReadAHead apply live; any other change reconnects the " +
				"BitTorrent client, so playback in progress stops. UseDisk and TorrentsSavePath take effect at the next start. " +
				"While NAT-PMP is on it owns PeersListenPort: set and def keep the forwarded port. Open to API tokens.",
			ParamsIn: "body",
			Params: []Param{{Name: "action", Type: "string", Required: true, Desc: "get, set or def"},
				{Name: "sets", Type: "object", Desc: "set only: the keys to change, e.g. {\"ConnectionsLimit\": 30}"}},
			Example: "curl -X POST -H 'Authorization: Bearer tmr_…' \\\n" +
				"  -H 'Content-Type: application/json' \\\n  -d '{\"action\":\"set\",\"sets\":{\"ConnectionsLimit\":30}}' {{ORIGIN}}/api/tentacle/settings",
			Response: "(get) {\"CacheSize\": 134217728, \"ReaderReadAHead\": 75, \"ConnectionsLimit\": 25, \"ResponsiveMode\": true, …}\n(set, def) empty body",
			OK:       200,
			Errors: []Status{{400, "bad JSON, sets missing or not an object, or an unknown action"}, {405, "not POST"},
				{500, "a value of the wrong type, or the settings could not be stored or applied (a failed apply stays saved and the next save retries it)"}},
		},
		{
			Method: "GET", Path: "/api/tentacle/history", Summary: "Tentacle settings versions, newest first",
			Desc: "The last 50 manual saves of the Tentacle settings (Control Panel, POST /api/tentacle/settings set or def, " +
				"a restore), each with source and actor and the keys it changed against the version before; kind is \"engine\". " +
				"Automatic changes are not versions: the NAT-PMP peer port, defaults written at start-up. StoreSettingsInJson, and " +
				"PeersListenPort while NAT-PMP is on, are never compared. The first entry is the settings when the history started " +
				"(note \"baseline\"). current says the settings in use match the newest version. Same access rule as the configuration history.",
			ParamsIn: "query",
			Params:   []Param{{Name: "limit", Type: "int", Desc: "how many versions, 1-50 (default 50)"}},
			Example:  "curl {{ORIGIN}}/api/tentacle/history?limit=5",
			Response: "[\n  {\"id\": 3, \"created_at\": 1795560000, \"source\": \"ui\", \"actor\": \"admin\", \"note\": \"\",\n" +
				"   \"changes\": [{\"key\": \"ConnectionsLimit\", \"from\": \"26\", \"to\": \"25\"}],\n" +
				"   \"restart\": false, \"current\": true, \"kind\": \"engine\"}\n]",
			OK:     200,
			Errors: []Status{{403, "sign-in on and no Control Panel session"}},
		},
		{
			Method: "GET", Path: "/api/tentacle/history/{id}/restore", Summary: "What restoring Tentacle settings would change",
			Desc:     "The changes POST /api/tentacle/rollback would make for id, against the settings in use, without applying anything.",
			Example:  "curl {{ORIGIN}}/api/tentacle/history/2/restore",
			Response: "{\"id\": 2, \"changed\": true, \"changes\": [{\"key\": \"ConnectionsLimit\", \"from\": \"25\", \"to\": \"26\"}], \"restart\": false}",
			OK:       200,
			Errors:   []Status{{400, "id is not a number"}, {403, "sign-in on and no Control Panel session"}, {404, "no such version"}},
		},
		{
			Method: "POST", Path: "/api/tentacle/rollback", Summary: "Restore saved Tentacle settings",
			Desc: "Puts version id in use like a Tentacle save: keys read on use (timeout, retrackers, responsive mode, shield, " +
				"read-ahead) apply live; any other change reconnects the BitTorrent client, so playback in progress stops. It is " +
				"recorded as a new version (source rollback, note \"rollback to #id\") unless it equals the settings in use. " +
				"changed is true only when a key the engine interprets changed. The peer port while NAT-PMP is on keeps its current value. restart is true when UseDisk or TorrentsSavePath changed: they take effect at the next start.",
			ParamsIn: "body",
			Params:   []Param{{Name: "id", Type: "int", Required: true, Desc: "the version to restore"}},
			Example:  "curl -X POST -H 'Content-Type: application/json' -d '{\"id\":2}' {{ORIGIN}}/api/tentacle/rollback",
			Response: "{\"changed\": true, \"changes\": [{\"key\": \"ConnectionsLimit\", \"from\": \"25\", \"to\": \"26\"}], \"restart\": false}",
			OK:       200,
			Errors:   []Status{{400, "the body is not {\"id\": N}"}, {403, "sign-in on and no Control Panel session"}, {404, "no such version"}, {500, "the settings could not be applied or recorded"}},
		},
		{
			Method: "POST", Path: "/api/restart", Summary: "Restart Tiramisu",
			Desc: "Answers, then shuts down gracefully and exits with code 75: systemd restarts it on any exit, the Docker " +
				"image on this code. Started by hand, it just exits.",
			Example:  "curl -X POST -H 'Authorization: Bearer tmr_…' {{ORIGIN}}/api/restart",
			Response: "{\"ok\": true}",
			OK:       200,
			Errors:   []Status{{405, "not POST"}},
		},
	}},
}
