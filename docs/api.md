# API and MCP Server

The Library API, the MCP server for AI agents, and a quick reference of the HTTP endpoints.

[← Back to the README](../README.md) · [Documentation index](README.md)

---

## Library API (`:9080`)

The HTTP way to add or remove a title, for clients that cannot touch the
filesystem: an AI agent, a script on another machine, or simply `curl`. One
request does what the sync engine does for a single release: it registers the
torrent, waits for the file list, picks the video file, writes the virtual
`.mkv` with the same naming convention, registers TV episodes in the state DB,
and asks the media server to rescan.

```bash
# Add a movie. release_title is the raw release name: the quality tags in the
# filename (_DV, _Atmos, _REMUX) are read from it.
curl -s -X POST -H 'Content-Type: application/json' --max-time 120 \
  -d '{"type":"movie","hash":"<infohash>","title":"Dummy Bunny","year":2024,
       "release_title":"Dummy.Bunny.2024.2160p.UHD.BluRay.REMUX.DV.Atmos-GRP",
       "imdb":"tt1234567"}' \
  http://127.0.0.1:9080/api/library/add

# Add one episode, or a whole season pack by leaving "episode" out
curl -s -X POST -H 'Content-Type: application/json' --max-time 120 \
  -d '{"type":"tv","hash":"<infohash>","title":"Dummy Bunny","first_air_date":"2015-06-24",
       "season":1,"episode":2,"release_title":"Dummy.Bunny.S01E02.1080p"}' \
  http://127.0.0.1:9080/api/library/add
```

`add` answers `201` with the path, `fuse_path` and declared size of every stub
it wrote, or `200` with `"already_present": true` when the release was already
filed. A season pack is never short-circuited that way, because which episodes
it contains is only known once the file list arrives; re-adding one rewrites the
episodes it names and replaces the releases they came from. Failures follow the
same rule as the sync: every error removes the torrent it added, so a failed
call leaves nothing behind.

For TV, `first_air_date` puts the year in the series folder, and `quality_score`
is what the next sync compares against before replacing the episode. `imdb` is
written into movie stubs only; episode stubs carry no id, the same convention
the sync follows. `magnet` can replace `hash`; `is_4k`, `file_index` and
`quality_score` override what would otherwise be inferred, and `metadata_wait`
(default 60 seconds, capped at 300) is how long the call waits for the swarm to
answer, so keep the client timeout above it.

Errors say which half broke: `400` the request, `422` the torrent holds no video
file, `502` the engine refused it, `503` the state DB is unavailable, `504` no
metadata in time. Every failure removes the torrent it added, so a failed call
leaves nothing behind.

```bash
# What the library already holds, one entry per stub. type is a filter: movie
# lists the movie library, tv the series one, gaps the open holes. Any other
# value silently falls back to the movie list, so a typo reads as a wrong answer.
curl -s 'http://127.0.0.1:9080/api/library/list?type=movie' | jq '.[] | {fuse_path, size, hash, imdb}'
curl -s 'http://127.0.0.1:9080/api/library/list?type=tv'    | jq '.[] | {fuse_path, season, episode}'

# Remove. blacklist:true records the release the way the FUSE unlink handler
# does, so the sync will not add the title back.
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"path":"movies/Dummy_Bunny_2024_2160p_DV_Atmos_REMUX_e7f8a9b0.mkv","blacklist":true}' \
  http://127.0.0.1:9080/api/library/remove
```

Removing a stub drops its torrent only when no other stub still points at it:
one season pack is a single torrent behind many episodes.

### Music and audiobooks (v1.10.0)

`music/` and `audiobooks/` use the same endpoints with their own rules. The caller
supplies the final path and Tiramisu validates what the engine owns: section
containment, extension agreement, the mandatory `_<hash8>` suffix (the last eight
hex digits of the infohash, before the extension) and collision rules. Music admits
`.flac` only; audiobooks admit `.m4b`, `.m4a` and `.mp3`.

```bash
# 1. Inspect hydrates the magnet and returns the file list. The engine adds the
# torrent itself, so a magnet is enough; metadata_wait works as in add.
curl -s -X POST -H 'Content-Type: application/json' --max-time 300 \
  -d '{"magnet":"magnet:?xt=urn:btih:...","title":"Dummy Album"}' \
  http://127.0.0.1:9080/api/library/inspect

# 2. Add an album, one projection per file. external_id is the caller's identity
# (a MusicBrainz release group, an ASIN) and external_id_ns its namespace: it is
# stored verbatim, written into the stub, and returned by list.
curl -s -X POST -H 'Content-Type: application/json' --max-time 300 \
  -d '{"type":"music","hash":"<infohash>","title":"Dummy Album",
       "files":[{"source_path":"Release/01 - One.flac",
                 "path":"Artist/Album/01 - One_e7f8a9b0.flac",
                 "external_id":"<release-group-mbid>","external_id_ns":"musicbrainz"}]}' \
  http://127.0.0.1:9080/api/library/add

# 3. List pages instead of returning an array, and shows the identity per row.
# prefix filters the section; failures=1 adds the reachability counters.
curl -s 'http://127.0.0.1:9080/api/library/list?type=music&limit=200' | \
  jq '{next_cursor, items: [.items[] | {path, external_id, external_id_ns}]}'
curl -s 'http://127.0.0.1:9080/api/library/list?type=music&prefix=Artist/Album&failures=1'

# 4. Remove one projection by exact path, or a whole album by prefix.
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"music","path":"Artist/Album/01 - One_e7f8a9b0.flac"}' \
  http://127.0.0.1:9080/api/library/remove
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"music","prefix":"Artist/Album"}' \
  http://127.0.0.1:9080/api/library/remove
```

`add` answers `201` for the projections it created, or `200` with
`"already_present": true` when they are already filed. On a replay the stored
identity wins and a disagreement is logged, so changing an identity means removing
and adding again. `remove` is idempotent: an absent path answers `removed: false`
with `state: "absent"`. Prefix removal takes a whole album in one call and answers
`409` when the rows under it do not share exactly one torrent, which is the engine
saying that directory is not an album. `blacklist` is not audio lifecycle state and
is rejected. Hash-wide, directory and batch removal remain out of Phase 1. A
torrent is never dropped while another projection still references it.

Projections are read-only to their clients: files answer `0444`, directories
`0555`, and the mutations a scanner never needs (write open, truncate, rename,
mkdir, chmod) are refused with `EROFS`, while a direct unlink gets `EPERM`. Adding,
re-filing and removing happen only through the API.

Two things worth knowing before scripting against it. The `type` must be exactly
`music` or `audiobook`: `list` keeps the legacy fallback for unknown values, so the
plural `audiobooks` answers `200` with the movie library instead of an error. And a
music add or removal asks the media server to rescan the section in
`plex.music_library_id` (Jellyfin refreshes every library, audiobooks included).
On Plex, audiobooks have no library id yet, so the `audiobooks/` section is scanned
manually, once, when the batch is done.

### Episode gaps (v1.9.71)

When the TV reaper removes an episode whose release died, it does not just
delete the stub: it records the hole, so a show the discovery feed no longer
returns can still be repaired later. `type=gaps` lists those holes, oldest
first.

```bash
curl -s -D- 'http://127.0.0.1:9080/api/library/list?type=gaps' | \
  jq '.[] | {episode_key, show, season, path, dead_hash, removed_at, last_attempt}'
```

`last_attempt` is when the repair pass last re-searched the hole, and it stays
`0` when it never has, which is why it carries no `omitempty`: `0` means "never
tried", not "tried at epoch". The listing is capped at 500 entries and the true
count travels in the `X-Total-Count` header, so a client reading a full page can
tell it is not the whole backlog.

The TV sync repairs gaps on its own: one search per affected season, at most 5
shows per run, and no sooner than 6 hours after the last attempt on the same
hole. Filing the episode yourself with `/api/library/add` closes its gap too: a
gap is cleared whenever the episode has a live release again, whoever put it
there.

> [!WARNING]
> Nothing on `:9080` is authenticated. `/api/config` returns the whole
> configuration, keys and tokens included, and accepts a POST that rewrites it;
> `/api/library/add` and `/api/library/remove` write and delete files in the
> library. Keep the port on a trusted network, or behind a reverse proxy that
> requires authentication.

---

## MCP Server

Tiramisu serves its library tools over MCP at `http://<host>:9080/mcp`
(Streamable HTTP). An agent searches releases with the deployment's own scoring
profile, the same pipeline the sync uses, files the one you pick, lists and
removes entries:

- Claude Code: `claude mcp add --transport http tiramisu http://<host>:9080/mcp`
- Any MCP client that speaks Streamable HTTP: point it at the same URL.

| Tool | What it does |
|------|--------------|
| `resolve_title` | Title → IMDb id (and TMDB id, first-air date for shows) |
| `search_releases` | Prowlarr + Torrentio search, gated and scored like the sync; returns a job |
| `get_job` | Polls a search, add or inspect job |
| `add_release` | Files the candidate you chose; TV releases are checked against the show's names |
| `resolve_album` | Artist → discography from MusicBrainz, or one album; flags what is already in the library |
| `search_album`, `add_album` | The music sync's own search (lossless only) and filing: MusicBrainz track ids, cue images split per track |
| `inspect_torrent`, `add_audio` | A torrent you name, and audiobooks: list its files, file its tracks |
| `list_library`, `list_gaps` | What the library holds; TV episodes the reaper removed |
| `remove_entries` | Preview first; removes only with `confirm`, for good or to replace |

Searches and adds run as jobs: indexer searches can take minutes. The agent
always asks before adding or removing. It changes the scoring profile only for
one search and only when you ask, and never writes it to `config.json`.

```
add the latest Dummy Bunny movie in 4K
add Dummy Bunny's discography
season 2 of Dummy Bunny is missing episodes 4 to 8, fill them in
is Dummy Bunny (2024) already in the library, and in what quality?
```

The MCP server needs v1.11.5 or later.

> **Trust model:** like the rest of `:9080`, `/mcp` has no authentication.
> Keep the port on your LAN.

---

## API Quick Reference

Two JSON APIs, one per layer: the GoStorm engine at `:8090` manages torrents and
settings, the Tiramisu side at `:9080` exposes metrics (and the Library API, in
its own section above). Handy for scripts, and for checking what is happening
without opening the web UI.

### GoStorm API (`:8090`)

```bash
# List all torrents (count)
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"list"}' http://127.0.0.1:8090/torrents | jq length

# Add a torrent
curl -X POST -H 'Content-Type: application/json' \
  -d '{"action":"add","link":"magnet:?xt=urn:btih:...","title":"Film Title (Year)"}' \
  http://127.0.0.1:8090/torrents

# Active torrents (in RAM, not DB)
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"active"}' http://127.0.0.1:8090/torrents | \
  jq '.[] | {title: .title[:50], speed_mbps: ((.download_speed//0)/1048576|round), peers: (.active_peers//0)}'

# Read settings
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"get"}' http://127.0.0.1:8090/settings | jq

# Remove a torrent
curl -X POST -H 'Content-Type: application/json' \
  -d '{"action":"rem","hash":"<infohash>"}' http://127.0.0.1:8090/torrents
```

The engine also accepts `drop` (unload from RAM, keep it in the DB) and `wipe`
(remove every torrent, rarely what you want).

Removing a title means removing its stub, not just its torrent. Two ways do it
properly, and deleting the stub from the source directory is neither: that
leaves the torrent registered and records nothing, so the next sync brings the
title back.

```bash
# through the Library API, with the same effect as the unlink below
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"path":"movies/<file>.mkv","blacklist":true}' \
  http://127.0.0.1:9080/api/library/remove

# or through the FUSE mount: the unlink handler closes open handles, removes the
# torrent from the engine and blacklists it
rm /mnt/tiramisu-mkv-virtual/movies/<file>.mkv
```

### Tiramisu API (`:9080`)

```bash
# Key metrics fields
curl -s http://127.0.0.1:9080/metrics | \
  jq '{version, uptime, read_ahead_active_bytes, config_source}'

# Playback quality: time to first byte, stalls, seek latency
curl -s http://127.0.0.1:9080/metrics/ttff | jq

# Blocklist status
curl -s http://127.0.0.1:9080/metrics/blocklist | jq

# Piece map of a stream a player is reading (404 otherwise): read position, bytes
# ready before the first gap, and up to 34 cells of `bucket` pieces each.
# Cell states: r ready, d downloading with a deadline, p partly here, m not
# requested, b played and still in memory, e played and evicted.
curl -s 'http://127.0.0.1:9080/api/pieces?hash=<infohash>' | \
  jq '{play, ahead_bytes, first_gap, bucket, cells: [.cells[].s] | join("")}'

# Health check and scheduler state
curl -s http://127.0.0.1:9080/api/health | jq
curl -s http://127.0.0.1:9080/api/scheduler/status | jq

# Search indexers by IMDB id, using the Prowlarr credentials from the config.
# Queries Prowlarr only: the sync engine also queries Torrentio and merges.
# An empty array means Prowlarr is not configured, not that nothing was found.
curl -s "http://127.0.0.1:9080/api/prowlarr/search?imdb_id=tt0088196&type=movie&year=1985" | jq

# Running configuration, including the quality_scoring profile in use
curl -s http://127.0.0.1:9080/api/config | jq '.quality_scoring'
```
