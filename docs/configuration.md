# Configuration Reference

Where the configuration lives, every field, quality scoring, languages and environment variables.

[← Back to the README](../README.md) · [Documentation index](README.md)

---

## Overview

The configuration lives in the state database, `STATE/tiramisu.db`, next to the
inode map and the sync caches: one volume holds everything, in Docker too. Every
save is a new version; the last 50 are kept, with who made each change, and any
of them can be restored (Control Panel → **History**, or the
[history API](api.md#configuration-history)). Keys and tokens are stored there as
well; the database is created owner-only (`0600`).

You rarely edit a file. The Control Panel saves the configuration, with FUSE and
path settings applied at the next restart and engine settings applied live; the
fields below are the keys of that document, as `GET /api/config` returns it and
`POST /api/config` takes it.

### config.json: import only

A `config.json` is read only to import a configuration:

- **First start.** On an install that has never started this version, the
  `config.json` next to the binary (or at `MKV_PROXY_CONFIG_PATH`, or at
  `<path>/config.json`) is imported and renamed `config.json.imported-<date>`.
  Upgrading needs nothing else. A `config.json` that is not valid JSON stops the
  first start with the reason; a field of the wrong type keeps its default and is
  named in the log. Nothing is lost: the renamed file stays where it was.
- **Drop-in.** Later, a `config.json` put back in one of those places is imported
  on the next start, as a new version, and renamed again. A file that cannot be
  renamed (a Docker single-file mount) is imported once and then ignored until it
  changes.
- **Without one**, a fresh install starts from the defaults
  (`config.json.example` shows them).

To change a key the Control Panel does not show, export, edit and import it with
the service stopped, or send it to `POST /api/config`:

```bash
sudo systemctl stop tiramisu
./tiramisu --export-config config.json   # the saved configuration, 0600
nano config.json
./tiramisu --import-config config.json   # a new version, used from the next start
sudo systemctl start tiramisu
```

Run both as the service user. `--import-config` refuses a file that is not valid
JSON or has a field of the wrong type. Each save also writes a copy,
`STATE/config.last.json`: if the database cannot be opened, Tiramisu refuses to
start (rather than run on default paths and ports) and says how to recover from
that copy.

`enable_state_db` is gone: the state database is always on. `state_db_path` is
read only at import; a database moved elsewhere is set with `TIRAMISU_STATE_DB`.

### Full Field Reference

| Field | Default | Description |
|-------|---------|-------------|
| `physical_source_path` | *(none)* | Directory where virtual `.mkv` stubs are created |
| `fuse_mount_path` | *(none)* | FUSE mount point, seekable virtual files served here |
| `read_ahead_budget_mb` | `256` | In-memory read-ahead budget |
| `disk_warmup_quota_gb` | `15` | SSD warmup cache quota; the reference Raspberry Pi deployment raises it to 32 GB |
| `warmup_head_size_mb` | `64` | Per-file SSD warmup size |
| `master_concurrency_limit` | `25` | Max concurrent data slots |
| `metadata_cache_size_mb` | `50` | Metadata LRU cache budget |
| `fuse_max_inflight_mb` | `0` | Cap on the RAM go-fuse checks out for in-flight requests; `0` leaves go-fuse's own default |
| `gostorm_url` | `http://127.0.0.1:8090` | GoStorm internal API URL |
| `gostorm_listen_ip` | *(empty)* | Where the GoStorm API (`:8090`) listens: empty = `127.0.0.1` only; `"all"` = every interface (the behaviour before v1.11.15), or a specific IP. Stays on every interface if `gostorm_url` is not a loopback address |
| `auth_trusted_proxies` | *(empty)* | Reverse proxies (IPs or CIDRs) whose `X-Forwarded-For` the sign-in rate limit trusts; with HTTPS on, plain HTTP from them is also served. List proxy IPs, not whole subnets |
| `https.enabled` | `false` | Serve `:9080` over HTTPS too ([details](api.md#https)). Read at start-up; set it from **Control Panel → Access → HTTPS** |
| `https.cert_file` / `https.key_file` | *(empty)* | Your own certificate and key; empty = the local CA in `STATE/tls/`. Renewed files are reloaded within a minute |
| `https.extra_hostnames` | `[]` | Names or IPs added to the local certificate, e.g. the Docker host's IP |
| `proxy_listen_port` | `8080` | Legacy field kept for config compatibility: no listener uses it today |
| `metrics_port` | `9080` | Metrics, Control Panel, Webhook port |
| `media_server_type` | `plex` | `plex` or `jellyfin`; selects how the post-sync library refresh is issued and which API the music sync reads |
| `torrentio_url` | `https://torrentio.strem.fun` | Torrentio base URL, the fallback when Prowlarr is disabled or finds nothing |
| `blocklist_enabled` | `false` | Load the peer IP blocklist (not needed behind a VPN) |
| `blocklist_url` | *(iblocklist Level 1)* | Gzipped blocklist URL, refreshed every 24 h; compression is detected from the content |
| `blocklist_filter` | `(?i)\bap2p\b\|anti-?p2p` | Keep only ranges whose description matches this regexp; empty loads the list whole |
| `peer_geoip_enabled` | `false` | Show each peer's country and network (AS and owner) on hover in the swarm view. Downloads the DB-IP Lite country and ASN databases (about 10 MB) once a month into `STATE/geoip/`; peer addresses never reach the browser |
| `plex.url` | *(none)* | Media server URL: Plex or Jellyfin |
| `plex.token` | *(none)* | Plex token, or Jellyfin API key |
| `plex.library_id` | `0` | Plex movies library section ID (0 = skip the refresh) |
| `plex.tv_library_id` | `0` | Plex TV series library section ID (0 = skip the refresh) |
| `plex.music_library_id` | `0` | Plex music library section Tiramisu files into: rescanned after an audio add or removal, and the only library the music sync follows new albums from (0 = both off). Jellyfin setups leave it at 0 |
| `orphan_collection.enabled` | `false` | After each successful movie sync, remove the torrents nothing in the library references (the orphan collection of `/api/library/orphans`, with all its checks) |
| `orphan_collection.min_age_days` | `7` | Only torrents added before this many days ago |
| `orphan_collection.max_per_run` | `50` | Torrents removed per run, at most 1000 |
| `scheduler.music_sync` | Sunday 15:00 | Weekly music sync slot (not in the Control Panel Scheduler, which has no music row: change it as in [config.json: import only](#configjson-import-only); the Dashboard can still run it) |
| `music_discovery.new_releases.enabled` | `true` | Follow the new albums of the artists in Tiramisu's music library |
| `music_discovery.new_releases.window_days` | `30` | How recent an album's first edition must be to count as new |
| `music_discovery.new_artists.enabled` | `true` | Import recent albums of new artists whose nearest artists you own |
| `music_discovery.new_artists.window_days` | `30` | How far back the fresh-releases feed is read (at most 90) |
| `music_discovery.new_artists.debut_years` | `3` | An artist counts as new when its first album or EP is at most this old (0 = any age) |
| `music_discovery.max_albums_per_run` | `20` | Cap shared by the new-artist, genre and similar-artist passes (new albums of your own artists are uncapped) |
| `music_discovery.seeds_recency_days` | `[1, 7, 30, 60]` | Age tiers of the plays that pick your seed artists: a play weighs 1 in the first tier and half as much in each next one; older plays do not count, and with no plays in the last tier the genre and similar-artist passes find nothing |
| `music_discovery.seeds_min_plays` | `3` | Plays an artist needs inside the tiers to become a seed |
| `music_discovery.genres.enabled` | `true` | Import niche artists of the genres you listen to now, close to your artists |
| `music_discovery.genres.count` | `8` | How many of your genres are explored |
| `music_discovery.genres.debut_years` | `15` | An artist of the genre pass counts as new when its first album or EP is at most this old (0 = any age) |
| `music_discovery.similar.min_fans` | `10000` | Similar artists with fewer Deezer fans are too obscure to suggest (0 = no floor) |
| `music_discovery.similar.max_fans` | `100000` | Similar artists with more Deezer fans are too known to suggest (0 = no limit) |
| `music_discovery.similar.debut_years` | `15` | A similar artist's first album or EP must be at most this old (0 = any age) |
| `tmdb_api_key` | *(none)* | TMDB API key |
| `prowlarr.enabled` | `false` | Use Prowlarr as primary indexer (falls back to Torrentio if disabled) |
| `prowlarr.api_key` | *(none)* | Prowlarr API key (Settings → General → API Key) |
| `prowlarr.url` | *(none)* | Prowlarr base URL (e.g. `http://<prowlarr-host>:9696`) |
| `natpmp.enabled` | `false` | Enable NAT-PMP |
| `natpmp.gateway` | *(none)* | VPN gateway IP |
| `natpmp.vpn_interface` | `wg0` | WireGuard interface |

### Quality Scoring

Every candidate release, film or episode, is scored before it is chosen:
resolution, HDR and Dolby Vision, the audio track, remux, your preferred
language, the seeder count and the size gates all add up to a single number, and
the highest one wins. Those weights used to be fixed in the code; they now live
in an optional `quality_scoring` block of the configuration, editable from the
**Release Selection** card in the Control Panel (the sync engines read it at the
next restart).

Omitting the block keeps the shipped defaults, so an existing install changes
nothing until you add it deliberately. `movies` and `tv` are independent: you
can set one without the other, and any field you leave out falls back to that
profile's default.

```json
{
  "quality_scoring": {
    "movies": {
      "res_4k": 1000,
      "res_1080p": 200,
      "hdr": 60,
      "dolby_vision": 100,
      "atmos": 50,
      "audio_5_1": 25,
      "stereo_penalty": -50,
      "remux": 30,
      "preferred_language": 60,
      "unknown_size_4k_penalty": -5,
      "seeder_cap": 50,
      "min_seeders": 15,
      "min_4k_gb": 10,
      "max_4k_gb": 40,
      "min_1080p_gb": 4,
      "max_1080p_gb": 20
    },
    "tv": {
      "res_4k": 1000,
      "res_1080p": 200,
      "hdr": 100,
      "dolby_vision": 150,
      "atmos": 50,
      "audio_5_1": 25,
      "preferred_language": 40,
      "fullpack": 500,
      "seeder_tier_100": 100,
      "seeder_tier_50": 50,
      "seeder_tier_20": 10,
      "min_seeders": 5,
      "min_seeders_4k": 5,
      "season_skip_score": 1000
    }
  }
}
```

The **Release Selection** card ships four presets that fill both profiles at
once and then let you fine-tune single fields:

- **Best Quality (default)**: the values above; resolution, HDR and Atmos weigh
  the most, and both syncs use this scale unless you pick another preset.
- **1080p**: for setups that should stop treating 4K as automatically better, a
  NAS that cannot transcode it for instance: `res_1080p` outweighs `res_4k`.
- **Fast Swarms**: favors releases with more seeders over marginal quality gains,
  for when cold starts matter more than a Dolby Vision bump.
- **Max Quality**: pushes resolution, HDR, Atmos and remux higher and tightens
  the seeder floor, for a library where quality always wins.

### Language

Release-name language signals, used by the scoring and by the rejection gates:

```json
"language": {
  "preferred_terms": ["ita", "multi", "dual"],
  "preferred_flags": ["IT"],
  "excluded_flags": ["ES", "FR", "DE", "RU", "CN", "JP", "KR", "TH", "PT", "BR",
                     "UA", "PL", "NL", "TR", "SA", "IN", "CZ", "HU", "RO"]
}
```

`preferred_terms` are case-insensitive, word-boundary-matched release-name terms
(e.g. `ita`, `multi`, `dual`); `preferred_flags` and `excluded_flags` are ISO
3166-1 alpha-2 codes matched against the flag emoji in indexer result lines. The
block is optional, and omitting it keeps these defaults.

### Runtime Environment Variables

`TIRAMISU_RESET_AUTH=1` clears sign-in at start-up (Docker equivalent of
`tiramisu --reset-auth`); remove it afterwards.

`TIRAMISU_STATE_DB` points at a state database outside `STATE/` (where
`state_db_path` used to). `MKV_PROXY_CONFIG_PATH` only says where a `config.json`
to import may be. Environment overrides (`MKV_PROXY_CONCURRENCY`,
`MKV_PROXY_GOSTORM_URL`, `MKV_PROXY_LOG_LEVEL`, `TIRAMISU_PLEX_URL`,
`TIRAMISU_PLEX_TOKEN`, …) apply on top of the saved configuration and are never
saved into it. See
[Sign-in and API tokens](api.md#sign-in-and-api-tokens).

```ini
Environment="GOMEMLIMIT=2200MiB"
Environment="GOGC=100"
```

`GOMEMLIMIT=2200MiB` leaves headroom for OS and Samba on a 4 GB Pi 4.
