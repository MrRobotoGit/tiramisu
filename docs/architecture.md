# Architecture

How the FUSE layer, the cache and the embedded BitTorrent engine work together, and how fast it is.

[← Back to the README](../README.md) · [Documentation index](README.md)

---

## How It Works

Plex/Jellyfin reads `/mnt/tiramisu-mkv-virtual/movies/Interstellar.mkv`. From Plex/Jellyfin's perspective, it's a normal 55 GB file on a local disk. In reality, the file does not exist. The FUSE kernel module intercepts the read, calls into Tiramisu, and Tiramisu serves the exact bytes from a three-layer cache, backed by a live BitTorrent swarm.

| Layer | What | Size | Purpose |
|-------|------|------|---------|
| **L1** | In-memory Read-Ahead | 256 MB | 32-shard concurrent buffer with per-shard LRU |
| **L2** *(optional)* | SSD Warmup Head | 64 MB/file | Instant TTFF on repeat playback, served at 150–200 MB/s from SSD |
| **L3** *(optional)* | SSD Warmup Tail | 16 MB/file | MKV Cues (seek index), Plex/Jellyfin probes the end of every file before confirming playback |

What makes this non-trivial: a FUSE filesystem that backs a real directory of static files is straightforward. A FUSE filesystem that must handle non-sequential byte-range requests across hundreds of files, each backed by an independent torrent with variable peer availability, while a Plex/Jellyfin scanner reads every inode in parallel: that required building every subsystem from scratch.

### Why Infuse starts in under a second

When you press Play, Infuse immediately reads the beginning and end of the file to load the video index and seek tables. On a real hard drive this is instant. Tiramisu replicates this with an **SSD warmup cache**: the first 64 MB and last few MB of every file are pre-cached on the Pi's SSD during the initial Plex/Jellyfin library scan. By the time you press Play, those bytes are already on disk and Infuse gets them in milliseconds.


### Why your library survives a reboot

Every file on a real filesystem has a permanent ID called an **inode**. Plex/Jellyfin and Infuse use these IDs to recognize files across restarts, so they know "this is the same film I scanned last week" and do not re-download metadata or reset your watch history.

On a standard virtual filesystem, these IDs are random and change every time the system restarts. Tiramisu solves this by persisting a permanent inode map to a SQLite database (`STATE/tiramisu.db`). After a reboot, every virtual `.mkv` gets back the exact same ID it had before. To Plex/Jellyfin and Infuse, it is indistinguishable from a file that never moved.

---

## Components

```
BitTorrent Peers ←→ anacrolix/torrent fork (peer protocol)
                         │
         Tentacle Engine (in-process, no port)
         piece store · streamer · torrent registry
                         │
              Native Bridge (In-Memory Pipe)
              Zero-Network, Zero-Copy Hot Path
                         │
         ┌───────────────────────────────────┐
         │       Tiramisu FUSE Layer         │
         │  L1: Read-Ahead Cache (256 MB)    │
         │  L2: SSD Warmup Head (64 MB/file) │
         │  L3: SSD Warmup Tail (16 MB/file) │
         └───────────────────────────────────┘
                         │
         /mnt/tiramisu-mkv-virtual/*.mkv  (FUSE mount)
                         │
         Samba share (smbd, oplocks=no, vfs objects=fileid)
                         │
         Synology CIFS mount (serverino, vers=3.0)
                         │
         Plex/Jellyfin Media Server libraries
```

### Port Map

| Port | Purpose |
|------|---------|
| `:9080` | Control Panel, Metrics, Dashboard, Webhook, Scheduler, Library API, MCP. Plain HTTP, and HTTPS on the same port when turned on ([HTTPS](api.md#https)) |

The engine has no HTTP port of its own. Up to v1.12 the GoStorm engine also served
a JSON API on `:8090`; since v1.13.0 nothing listens there.

---

## Core Engineering

> **Eleven purpose-built subsystems**, each one the answer to a problem that
> surfaced while running this on Raspberry Pi 4 hardware.

A media server gives a streaming engine no second chances: it opens many files
at once, reads the end of a file before the beginning, seeks wherever the user
drags the bar, and expects the bytes to be there. On a Pi the margin is thin, so
every subsystem below exists to remove one specific way this can fail.

### 1. Zero-Network Native Bridge

Tiramisu is one process, not two. The Tentacle engine and the FUSE layer are
compiled into the same binary, so when the media server asks for a byte range
the FUSE side calls straight into the engine through an in-memory `io.Pipe()`.
Behind the pipe, an ordered stream reads the engine's piece store from the
player's position and advances only as the pipe is drained, so the player's
consumption sets the pace of priorities and deadlines.
There is no TCP round trip, no HTTP headers to parse, no serialization and no
proxy in the middle; metadata lookups are plain Go function calls. That is what
removes the network latency that makes HTTP-based streaming proxies stutter on
small hardware.

### 2. Two-Layer SSD Warmup Cache *(optional)*

An optional SSD cache that makes repeat playback almost instant. It is enabled
and configured from the Control Panel → Tentacle settings, and it is independent
of the core streaming path: without it Tiramisu still works, it just pays the
cold-start cost again.

When enabled, the **head cache** keeps the first 64 MB of each file on SSD from
the first play onward, so a repeat playback opens in **under 0.01 s** (an SSD
reads at 150–200 MB/s, against 2–4 s for a cold torrent activation). The **tail
cache** keeps the last 16 MB separately: MKV files store their Cues, the seek
index, near the end, and the media server probes that region before confirming
playback, which is why without it the seek bar can render as unavailable on the
first open. The quota is configurable by LRU write time; the reference
Raspberry Pi deployment uses 32 GB, enough for around 400 films. No manual
warming is needed: a library scan reads the first 1 MB of every file, which is
exactly what the head cache needs.

### 3. Webhook Integration & Smart Streaming

Tiramisu listens for the media server's "user pressed Play" event at
`:9080/plex/webhook` (the shorter `/webhook` works too), for **Plex** and
**Jellyfin** alike. When `media.play` arrives, four things happen at once: the
IMDB id is pulled out of the raw payload, the pieces covering the byte range
being played are given priority, the MKV Cues at the end of the file are frozen
so a seek does not have to fetch them again, and when playback stops the torrent is kept for 30 s, or for 5 s when it was only a scanner probe no webhook ever confirmed, so ordinary library traffic releases its peers almost at once.

The IMDB id is extracted with a regex *before* the payload is decoded, and that
is deliberate: Plex sends its identifiers in a non-standard `Guid` array that
makes a normal `json.Unmarshal` fail silently. The id is also what makes the
match language-independent, because media servers send titles in the user's
display language (`"den stygge stesøsteren"` instead of "The Ugly Stepsister")
and fuzzy matching fails on exactly those cases.

**Plex**: Settings → Webhooks → Add Webhook:

```
http://<your-pi-ip>:9080/plex/webhook
```

**Jellyfin**: install the
[Webhook plugin](https://github.com/jellyfin/jellyfin-plugin-webhook), then add
one webhook with the events `PlaybackStart`, `PlaybackStop` and
`PlaybackProgress`:

| Field | Value |
|-------|-------|
| URL | `http://<your-pi-ip>:9080/plex/webhook` |
| Header Key | `Content-Type` |
| Header Value | `application/json` |

Template:
```
{"event":"{{NotificationType}}","Metadata":{"title":"{{{Name}}}","grandparentTitle":"{{{SeriesName}}}","librarySectionType":"{{ItemType}}","guid":"imdb://{{Provider_imdb}}","Guid":[{"id":"imdb://{{Provider_imdb}}"}]}}
```

Jellyfin's own event names and item types are mapped onto Tiramisu's internally
(`PlaybackStart`→`media.play`, `PlaybackStop`→`media.stop`, `Movie`→`movie`,
`Episode`→`show`), so neither side needs a code change or a plugin hack.

**Music** uses the same endpoint with a different identity: a track has no IMDb id,
so Tiramisu matches on the MusicBrainz id the projection was filed with. **Plex and
Plexamp** need no extra configuration: play an album and the log shows
`[PLEX] Playback confirmed by webhook for: <file>.flac`. **Jellyfin** needs a second
Generic destination (Jellyfin 10.11 requires plugin 18 or later, 10.10 requires 16
or later, current is 22) with the same URL and events and this body:

```
{"event":"{{NotificationType}}","Metadata":{"title":"{{{Name}}}","grandparentTitle":"{{{Artist}}}","librarySectionType":"{{ItemType}}","ProviderIds":{"MusicBrainzTrack":"{{Provider_musicbrainztrack}}","MusicBrainzReleaseGroup":"{{Provider_musicbrainzreleasegroup}}","MusicBrainzAlbum":"{{Provider_musicbrainzalbum}}","MusicBrainzArtist":"{{Provider_musicbrainzartist}}"}}}
```

Plex sends the release track id, Jellyfin the recording id (the `MUSICBRAINZ_TRACKID`
tag, exposed as `MusicBrainzTrack`). The id style follows the Plex/Jellyfin switch in
the Control Panel, so a library must be filed with the style of the player that
actually plays it, and switching player afterwards means re-filing it with an align
run in the new style. Jellyfin exposes MusicBrainz providers only when the files
carry MusicBrainz tags: tagless files still play, but their webhook has no id to
match and the session falls back to inferred playback.

### 4. Adaptive Shield

Two read modes, managed automatically:

| Mode | Behavior | When |
|------|----------|------|
| **Responsive** *(default)* | Data served before SHA1 verification, instant start | Normal operation |
| **Strict** | Only SHA1-verified pieces served | Automatically activated on corruption detection |

Responsive is the default because it is what makes playback start instantly:
bytes go out before their SHA1 is checked. When a corrupt piece is detected
(`MarkNotComplete()`), the shield switches to Strict and serves only verified
data, then waits for a clean streak before relaxing again. The streak starts at
30 seconds and grows by another 30 each time Strict re-triggers, up to a cap of
30 minutes; the first corruption in a session only logs and bans the peer,
leaving Fast mode on. The counter clears on a genuine clean streak, not on
seeks or a `media.stop`, so a swarm that keeps sending bad pieces gets
progressively more scrutiny instead of restarting the same short timer.
A peer that keeps corrupting is banned outright, and the ban is remembered for
30 days, across restarts. A piece that fails its hash check is downloaded again
from peers that did not touch the failed attempt, a failure counts toward a
peer's ban only when that peer was the piece's sole contributor, and peers cached
from earlier sessions get no special trust. The shield is kept per torrent: one
swarm sending bad data does not put the other streams in Strict. The piece store
never serves a byte that was not written, in either mode.

### 5. Seek-Master Architecture

Seeking is the hardest thing a player can ask for: the target can be gigabytes
away from where the pump is reading, and the player will not wait. Five
coordinated fixes make it feel local:

| Fix | What it does |
|-----|-------------|
| **Eager offset update** | Updates `lastOff` before the cache check, so the pump sees the target on the same `Read()` call |
| **Atomic pipe interrupt** | `Interrupt()` closes the pipe reader atomically when the player jumps > 256 MB, unblocking an `io.ReadFull` that is already waiting |
| **Reactive jump** | If the player is > 256 MB ahead of the pump, snap to `(playerOff / chunkSize) * chunkSize` |
| **Pump survival** | The pump survives `ErrInterrupted` with a 200 ms sleep-and-continue, with no goroutine restart |
| **Tail probe detection** | The media server's end-of-file MKV Cues probe is served from the SSD tail cache without steering the pump |

None of them works alone: the offset update tells the pump where the reader is
going, the interrupt unblocks a read in flight, the reactive jump covers the
huge gaps, the pump survives the interruption instead of being torn down, and
the Cues probe is recognised as a probe rather than allowed to drag the pump to
the end of the file.

### 6. 32-Shard Read-Ahead Cache

The 256 MB read-ahead budget is not one lock-protected structure. It is split
across **32 independent shards**, keyed by a hash of file path and offset, each
with its own LRU and mutex, so concurrent playback sessions and the media
server's scanner threads never stand in line behind one another. Both `Put()`
and `Get()` work on **defensive copies**: returning a sub-slice of a pooled
buffer means the next user of that buffer can overwrite data somebody is still
reading, which is exactly the kind of corruption that reaches the player as a
stutter.

### 7. Native Sync Engine (Go)

All sync logic runs inside the Go binary: no Python, no external scripts, no
subprocess to babysit.

| Engine | Trigger | What it does |
|--------|---------|-------------|
| **Movies** | Scheduler / manual | TMDB Discover + Popular → Prowlarr/Torrentio → Tentacle → virtual `.mkv` |
| **TV Series** | Scheduler / manual | TV series with fullpack-first approach, episode registry |
| **Watchlist** | Scheduler / manual | Plex cloud watchlist → IMDB → Prowlarr/Torrentio → Tentacle |
| **Music** | Scheduler / manual | New albums of your artists + new artists close to yours + niche artists of your genres + similar artists (Deezer) → MusicBrainz → Prowlarr → FLAC projections |

**Quality ladder**: `4K DV > 4K HDR10+ > 4K HDR > 4K > 1080p REMUX > 1080p`\
**Minimum seeders**: 15, for the main sync and the watchlist alike (the watchlist uses the movie profile)

Everything the engine needs to remember, the torrent list, the episode
registry, the negative caches and the scheduler state, is persisted in
`STATE/tiramisu.db` (SQLite).
The built-in scheduler replaces system cron, and it is configured from the
Control Panel.

### 8. NAT-PMP Native VPN Port Forwarding

Behind a WireGuard tunnel the home router's port forwarding no longer applies,
because the traffic leaves through the VPN gateway instead. Tiramisu runs a
NAT-PMP sidecar that periodically asks the gateway for a TCP+UDP port mapping,
installs the `iptables PREROUTING REDIRECT` rules that point the mapping at the
local listener, and updates the engine's peer listen port. All of it at runtime: no
restart, no manual rule editing.

### 9. IP Blocklist (optional)

Off by default. When `blocklist_enabled` is set, Tiramisu downloads a gzipped
blocklist at startup, refreshes it every 24 hours, and injects the ranges
straight into anacrolix/torrent's IP filter, so a known-bad actor is refused
before any connection attempt. Compression is detected from the content, so a
URL that serves gzip without a `.gz` extension still works, and a download that
yields no usable ranges is discarded rather than replacing a working list.

Published lists need care, which is why the filter exists. iblocklist Level 1
carries around 236,000 ranges covering **17% of IPv4**, and most of it comes
from whois records dating to the 1990s: entire /16s still attributed to
companies that sold the address space years ago. Loaded whole, it rejects
ordinary peers, not monitoring outfits. `blocklist_filter` is a regexp over the
entry's description that keeps only the part still maintained:

```json
"blocklist_url": "https://list.iblocklist.com/?list=ydxerpxkpcfqjaybcssw&fileformat=p2p&archiveformat=gz",
"blocklist_filter": "(?i)\\bap2p\\b|anti-?p2p"
```

That turns 236,172 ranges into 5,134, from 17% of IPv4 down to **0.03%**.
Leave the filter empty to load a list whole; aggregate lists that mix anti-P2P,
ads, malware and whole-country blocks can reject almost every peer and stop
playback from starting at all. `GET /metrics/blocklist` reports how many
addresses were rejected and which ranges did the rejecting, which is the only
honest way to tell a list that earns its keep from one quietly eating good
peers.

### 10. Profile-Guided Optimization (PGO)

The binary is built with `-pgo=auto`, so the Go toolchain reads `default.pgo`
and uses real production profiling data to inline hot paths and lay out
branches. On a Pi 4 Cortex-A72, which has no hardware AES or SHA1, PGO alone is
worth roughly **5–7% of CPU**.

### 11. Tentacle: a torrent engine built around FUSE

Tentacle is Tiramisu's own native torrent engine, and it was built around FUSE
rather than bolted onto it.

A normal torrent client downloads a file and hands it over when it is done.
Tentacle starts from the opposite end: a player opening a file that does not
exist yet. Every read that arrives through FUSE becomes the engine's plan,
which pieces to ask the swarm for, in what order and by when, so the strangers
on the other side end up racing to stay a few seconds ahead of the scene you
are watching. It keeps only as much of the film in memory as that lead needs,
lets the player's own pace set how fast data flows, and turns strict the moment
a peer sends bad data, without stopping the picture.

To Plex, Jellyfin or Infuse it is just a file on a disk. Under the hood, the
peer protocol comes from Tiramisu's patched fork of
**[anacrolix/torrent](https://github.com/anacrolix/torrent)** (MPL-2.0); the
engine replaced GoStorm, derived from
[TorrServer Matrix](https://github.com/YouROK/TorrServer), in v1.13.0. Its parts:

| Part | What it does |
|---|---|
| **Piece store** | Keeps piece data in RAM, allocated in chunks of up to 256 KiB as blocks arrive, so memory follows the data actually written. A per-block bitmap means a read never returns a byte that was not written. Eviction drops only verified pieces outside every reader's protected window and the head and tail zones, least recently used first. Runs Adaptive Shield, one per torrent. |
| **Streamer** | Turns the piece store into ordered byte streams and schedules the engine: piece priorities, playout deadlines and the read-ahead window, anchored to each stream's position. Playback streams and short on-demand reads (seek probes, tail fetches) are kept apart, so a probe does not shrink the read-ahead of the film being watched. A read on a stalled swarm fails after an inactivity timeout instead of a fixed deadline, so a healthy stream is never cut. |
| **Engine** | The torrent registry: adding, waking, expiring and removing torrents, persistence in `STATE/tiramisu.db`, statistics for `/metrics` and the Dashboard, trackers, the IP blocklist and its settings, with history and rollback. Settings that can apply live do so without reconnecting the BitTorrent client. |
| **Native bridge** | The FUSE side's door to the engine: the pump that feeds the in-memory pipe, read-ahead fetches and wake-ups. |

Data stays compatible with earlier releases: the same database tables
(`gostorm_torrents`, `gostorm_kv`, `gostorm_settings_versions`, kept under their
old names), the same library and the same stubs.

---

## Performance

> Measured on a **Raspberry Pi 4** (4 GB RAM, Cortex-A72, arm64, no hardware
> crypto), the minimum supported baseline. On amd64 or a more powerful arm64 box
> the numbers are better.

The experience in two lines: a first play from a cold swarm takes 2–4 seconds, a
repeat play from the SSD warmup cache opens in a fraction of a second, and 4K
HDR playback keeps the download well ahead of the player. The table is the
detail behind those two.

| Metric | Value |
|--------|-------|
| Cold start (first play, no warmup) | 2–4 s |
| Warm start (SSD warmup HIT) | **0.1–0.5 s** |
| Seek latency (cached position) | **< 0.01 s** |
| Binary size | **39 MB** (arm64, PGO; the GoStorm build was 54 MB) |
| Memory footprint (read-ahead) | Deterministic 256 MB |
| GOMEMLIMIT | 2200 MiB |
| Peak throughput (fast seeder) | **400+ Mbps** |
| Peers opened by a Plex/Jellyfin library scan | ~6 in total |
| Inode shard count | 32 (collision-protected) |
| Warmup cache capacity | ~400 films at 80 MB each (32 GB) |
