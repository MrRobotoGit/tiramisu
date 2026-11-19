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

### Consistency check and orphan torrents (v1.12.1)

`diagnostics` compares what the engine holds with what the library references
(stubs, `tv_episodes`, audio projections, the inode map) and changes nothing.
`cached=1` returns the last result instead of walking the stubs again; the
Dashboard reads it that way.

```bash
curl -s 'http://127.0.0.1:9080/api/library/diagnostics' | jq '.checks[] | {id, count, removable}'
curl -s 'http://127.0.0.1:9080/api/library/diagnostics?cached=1'
```

`orphans` lists the torrents nothing references, older than `min_age_days`
(default 7), oldest first. `dry_run` defaults to `true`; with `false` each one is
checked again just before it is removed. At most `max` per call (default 50, up
to 1000). It refuses (409) when `movies/` or `tv/` is missing or has no stub,
when a stub cannot be read, or when more than 25% of the torrents look
unreferenced. `counters_only` clears the failure counters of torrents no longer
in the engine and removes no torrent.

```bash
curl -s -X POST -d '{}' http://127.0.0.1:9080/api/library/orphans | jq '{candidates, selected: .selected[:5]}'
curl -s -X POST -d '{"dry_run":false,"max":50}' http://127.0.0.1:9080/api/library/orphans | jq '{released: (.released|length), kept, failed}'
```

`release` removes one torrent only if nothing references it, for a client that
hydrated one with `inspect` and filed nothing from it (404 when the engine does
not hold it). A `remove` whose torrent the engine kept reports it in
`torrent_errors`; the orphan collection takes it later.

```bash
curl -s -X POST -d '{"hash":"<40-char info hash>"}' http://127.0.0.1:9080/api/library/release
```

> [!WARNING]
> `/api/library/add`, `/api/library/remove`, `/api/library/orphans` and
> `/api/library/release` write and delete files or torrents in the
> library. Turn on [sign-in](#sign-in-and-api-tokens) before exposing `:9080`
> beyond a trusted network.

---

## Sign-in and API tokens

Sign-in is optional and off by default. Turn it on in **Control Panel → Access**:
it protects everything on `:9080` (Control Panel, Dashboard, `/api/*`,
`/metrics`, `/debug/pprof`, `/mcp`). The Plex and Jellyfin webhooks stay open, so
playback tracking keeps working without changes on the media server.

- **One user**, with a password (bcrypt). The browser session lasts 30 days from
  the last visit; **Sign out everywhere else** ends the others.
- **Tokens** for everything that is not a browser: create one per client in
  **Access → New token** and pick its type. An **MCP token** opens only `/mcp`
  (agents such as Hermes and Claude Code); an **API token** opens the HTTP API
  but not `/mcp` (custom clients and apps built on Tiramisu, the menu bar app, scripts). A token of the wrong type gets
  `403`. No token can reach `/api/auth/*`: creating and revoking tokens, the
  password and turning sign-in off take the Control Panel, signed in, and so does
  changing `auth_trusted_proxies`. Tokens do not expire; the value (`tmr_…`) is
  shown once and stored only as a SHA-256 hash. Revoke it from the same table.
- **Upgrading from v1.11.15:** tokens created before token types become MCP
  tokens. Re-create the ones your apps, menu bar app and scripts use as API tokens.
- Tokens can be created while sign-in is still off: configure your clients first,
  then turn sign-in on.

Clients send the token as a bearer header (an API token here):

```bash
export TIRAMISU_TOKEN=tmr_...
curl -H "Authorization: Bearer $TIRAMISU_TOKEN" http://127.0.0.1:9080/api/health
```

Without credentials, browsers are redirected to `/login` and API clients get
`401` with `WWW-Authenticate: Bearer realm="tiramisu"`. The login is limited to
5 attempts per minute per IP. Behind a reverse proxy, list it in
`auth_trusted_proxies` so the limit applies per client, and forward `Host` and
`X-Forwarded-Proto` (the session cookie is marked `Secure` on HTTPS). A plain-HTTP
proxy that rewrites `Host` must also send `X-Forwarded-Host`, or browser saves are
refused as cross-origin. `/mcp` through a reverse proxy on the same machine needs
sign-in on.

**Forgotten password:** stop the service, run `tiramisu --reset-auth` on the
host, start it again. It clears the user, the sessions and the tokens and turns
sign-in off. With Docker, start the container once with `TIRAMISU_RESET_AUTH=1`
and remove the variable afterwards: while it is set, sign-in cannot be turned on.

The user, sessions and tokens live in `STATE/tiramisu.db`, apart from the
configuration and never in its history. If sign-in is on and that database cannot be read, `:9080` answers
`503` (webhooks excepted) instead of opening up.

---

## HTTPS

`:9080` can serve HTTPS itself: **Control Panel → Access → HTTPS**, switch it on,
**Apply & restart**. It is off by default, and a reverse proxy in front keeps
working either way. The card then walks through the rest: download the CA,
trust it on each device, move your clients to `https://`.

**Certificate.** Tiramisu creates a local CA once (`STATE/tls/ca.pem`, ECDSA
P-256, valid 10 years) and uses it to sign the server certificate
(`STATE/tls/cert.pem`, 365 days). Devices trust the CA once; the server
certificate renews on its own (expiry, a new IPv4 address, new extra hostnames)
without anyone noticing. It covers the hostname, `<hostname>.local`, `localhost`,
`127.0.0.1`, `::1`, the host's IPv4 addresses (IPv6 addresses rotate and are left
out, and so are VPN tunnels, link-local addresses and Docker bridges) and
`https.extra_hostnames`. The CA is
downloaded from the card as `tiramisu-ca.crt` (also `GET /api/auth/tls/cert`):
compare its SHA-256 with the one on the card before trusting it. Your own
certificate (Let's Encrypt or any public CA) can replace it: set
`https.cert_file` and `https.key_file`; renewed files are picked up within a
minute, and if they cannot be read Tiramisu falls back to the local CA and logs
`[TLS] ERROR`.

**One port, both schemes.** TLS and plain HTTP share `:9080`. With HTTPS on, plain
HTTP is answered like this:

| Plain HTTP request | Sign-in off at start-up | Sign-in on at start-up |
|---|---|---|
| From the Tiramisu host itself (loopback or its own addresses) | served | served |
| From a proxy listed in `auth_trusted_proxies` | served | served |
| `/plex/webhook`, `/webhook` | served | served |
| A page in a browser | `308` to `https://` | `308` to `https://` |
| Anything else from the LAN | served | `403 {"error":"use https"}` |

So the `http://127.0.0.1:9080` examples in this guide keep working on the host,
and Plex and Jellyfin need no change. From another machine use `https://` and
point the client at the CA:

```bash
curl --cacert tiramisu-ca.crt -H "Authorization: Bearer $TIRAMISU_TOKEN" \
  https://<host>:9080/api/health
```

| Client | How it trusts the CA |
|---|---|
| macOS (Safari, Chrome, the menu bar app) | Open `tiramisu-ca.crt` → Keychain Access → Trust → *Always Trust* |
| iOS / iPadOS | Install the profile, then enable full trust in *Settings → General → About → Certificate Trust Settings* |
| Node clients (Claude Code and other MCP clients) | `export NODE_EXTRA_CA_CERTS=/path/to/tiramisu-ca.crt` |
| Python (`requests`, `httpx`) | `export REQUESTS_CA_BUNDLE=/path/to/tiramisu-ca.crt` and `SSL_CERT_FILE=…`; `httpx` ignores the system store and uses `certifi` unless `SSL_CERT_FILE` is set |
| curl | `--cacert tiramisu-ca.crt` |

The macOS menu bar app picks `http://` or `https://` by itself: it moves to
`https://` when the plain address answers `use https`, and back when HTTPS is
turned off.

- **Restart.** The setting is read at start-up, and so is the sign-in state the
  plain-HTTP rule uses: turning sign-in on while HTTPS runs leaves plain HTTP open
  until the next restart, and the card says so.
- **Tokens seen in clear.** With HTTPS on, a valid token that still arrives over
  plain HTTP from another machine is listed in the card (step 4) with its IP:
  switch that client and consider revoking the token.
- **HSTS** is never sent with the local CA (it would lock browsers out before the
  CA is trusted). With your own certificate it is sent for 180 days: turning
  HTTPS off afterwards keeps browsers on `https://` until it expires.
- **Session cookie.** It is `__Host-tiramisu_session` over HTTPS and
  `tiramisu_session` over plain HTTP, so turning HTTPS off does not lock you out.
- **Docker.** The container's own IP and hostname change on every re-creation and
  are not what clients use: add the host's IP or name to `https.extra_hostnames`.
- **Reverse proxy.** With HTTPS on, a proxy on another machine must be listed in
  `auth_trusted_proxies` (its IP, not a whole subnet: a subnet exempts the whole
  LAN from the rule above).

`GET /api/auth/tls` returns whether HTTPS runs, the certificate in use and
whether a restart is pending; with sign-in on it needs a browser session, like the
rest of `/api/auth/*` (tokens are refused).

**Secrets are write-only.** `GET /api/config` never returns `plex.token`,
`tmdb_api_key` or `prowlarr.api_key`: it reports whether each is set in
`secrets_configured`. A `POST` that leaves one empty keeps the stored value.

---

## Configuration history

Every save of the configuration is a version in `STATE/tiramisu.db`; the last 50
are kept. `GET /api/config/history` lists them newest first, each with its source
(`ui`, `api`, `internal`, `import`, `cli`, `rollback`, `default`), the user or token
name that saved it, the keys it changed against the version before, and whether
those changes need a restart. Keys and tokens show as changed, never their values.

```bash
curl -s http://192.168.1.2:9080/api/config/history?limit=5 | jq '.[] | {id, source, actor, changes}'
curl -s -X POST -H 'Content-Type: application/json' -d '{"id": 41}' \
  http://192.168.1.2:9080/api/config/rollback
```

`GET /api/config/history/41/restore` answers what a rollback to 41 would change,
without saving: the Control Panel shows it before you apply. A rollback saves
version 41 again as the newest: nothing is deleted, so it can be
rolled back too. It brings back the keys and tokens that version had, keeps the
install's telemetry id, and answers with the new version, what changed and whether
`POST /api/restart` is needed. With sign-in on, the history and the rollback answer
only a signed-in Control Panel session: a token gets `403`, because a rollback can
turn HTTPS off or change the trusted proxies.

### Engine settings history

The GoStorm Engine settings (cache size, connections, peer port…) have their own
history, shown in the same Control Panel page: `GET /api/gostorm/history`,
`GET /api/gostorm/history/{id}/restore` and `POST /api/gostorm/rollback` work like the
configuration routes above, with the same access rule. Only manual saves become
versions (Control Panel, `POST /api/gostorm/settings` with `set` or `def`, a restore):
the peer port NAT-PMP sets and the defaults written at start-up do not.
`StoreSettingsInJson`, the SSL fields, and `PeersListenPort` while NAT-PMP is on, are
never compared, and a save or a restore keeps their current value: with NAT-PMP the
peer port is the one the VPN forwards, not the one in the form. A restore applies at once, with no restart,
but GoStorm reconnects its BitTorrent client: playback in progress stops.

---

## MCP Server

Tiramisu serves its library tools over MCP at `http://<host>:9080/mcp`, or
`https://<host>:9080/mcp` with [HTTPS](#https) on (Streamable HTTP). An agent searches releases with the deployment's own scoring
profile, the same pipeline the sync uses, files the one you pick, lists and
removes entries:

- Claude Code: `claude mcp add --transport http tiramisu http://<host>:9080/mcp`
  (with sign-in on, add `--header "Authorization: Bearer tmr_..."`; with HTTPS on,
  use `https://` and set `NODE_EXTRA_CA_CERTS` to the CA file)
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
one search and only when you ask, and never saves it in the configuration.

```
add the latest Dummy Bunny movie in 4K
add Dummy Bunny's discography
season 2 of Dummy Bunny is missing episodes 4 to 8, fill them in
is Dummy Bunny (2024) already in the library, and in what quality?
```

The MCP server needs v1.11.5 or later.

> **Authentication:** with [sign-in](#sign-in-and-api-tokens) on, `/mcp` needs an
> API token in the `Authorization: Bearer` header. **Access → New token** shows a
> ready-to-paste config for Hermes, Claude Code and curl.

---

## API Quick Reference

Two JSON APIs, one per layer: the GoStorm engine at `:8090` manages torrents and
settings, the Tiramisu side at `:9080` exposes metrics (and the Library API, in
its own section above). Handy for scripts, and for checking what is happening
without opening the web UI.

### GoStorm API (`:8090`)

The GoStorm API listens on `127.0.0.1` only: run these commands on the Tiramisu
host (or inside the container). Only the engine itself calls it. To reach it from
the network, as before v1.11.15, set `"gostorm_listen_ip": "all"` in the
configuration (see [config.json: import only](configuration.md#configjson-import-only),
or `POST /api/config`); it has no authentication, so do that only on a trusted network.

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

The examples run on the Tiramisu host. From another machine with [HTTPS](#https)
on, use `https://<host>:9080` with `--cacert tiramisu-ca.crt`.

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

# Running configuration, including the quality_scoring profile in use (secrets are never returned)
curl -s http://127.0.0.1:9080/api/config | jq '.quality_scoring'
```
