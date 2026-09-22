```
████████╗ ██╗ ██████╗   █████╗  ███╗   ███╗ ██╗ ███████╗ ██╗   ██╗
╚══██╔══╝ ██║ ██╔══██╗ ██╔══██╗ ████╗ ████║ ██║ ██╔════╝ ██║   ██║
   ██║    ██║ ██████╔╝ ███████║ ██╔████╔██║ ██║ ███████╗ ██║   ██║
   ██║    ██║ ██╔══██╗ ██╔══██║ ██║╚██╔╝██║ ██║ ╚════██║ ██║   ██║
   ██║    ██║ ██║  ██║ ██║  ██║ ██║ ╚═╝ ██║ ██║ ███████║ ╚██████╔╝
   ╚═╝    ╚═╝ ╚═╝  ╚═╝ ╚═╝  ╚═╝ ╚═╝     ╚═╝ ╚═╝ ╚══════╝  ╚═════╝
```

<h1><sub><sub><strong>Tiramisu</strong> is the most advanced BitTorrent engine and FUSE virtual filesystem for live streaming to your private Plex or Jellyfin library. Hermes and OpenClaw ready. Forget Real-Debrid.</sub></sub></h1>

[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/MrRobotoGit/tiramisu)
[![Mentioned in Awesome Selfhosted](https://awesome.re/mentioned-badge.svg)](https://github.com/awesome-selfhosted/awesome-selfhosted)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE.md)
[![Docker Pulls](https://img.shields.io/docker/pulls/mrrobotogit/tiramisu?logo=docker&label=Docker+Pulls)](https://hub.docker.com/r/mrrobotogit/tiramisu)
[![Docker Image Size](https://img.shields.io/docker/image-size/mrrobotogit/tiramisu/latest?logo=docker&label=Image+Size)](https://hub.docker.com/r/mrrobotogit/tiramisu)

> [!NOTE]
> This project used to be called GoStream. It is now Tiramisu, same project, same codebase, just a new name. Nothing about how it works has changed, and everything in this README still applies. If you are upgrading from an older install, note that the binary name, config paths, and systemd service name have all changed too, so a fresh run of the install script is the easiest way to pick up the new layout.

---

![Tiramisu: click play on a torrent, streaming starts in about a second](docs/screenshots/tiramisu-demo.gif)

Tiramisu exposes a **custom FUSE virtual filesystem** where every `.mkv` file is a perfect illusion: it looks like a real file on disk, but every byte is served live from a BitTorrent swarm on demand. No downloading. No temp files. No storage quota.

The BitTorrent engine runs **inside the same OS process** as the FUSE layer, connected by an in-memory `io.Pipe()`. When Plex/Jellyfin reads a byte range, there is no HTTP round-trip, no serialization, no proxy overhead, just bytes, flowing directly from peers through RAM to the media server at full speed.

The result: **4K HDR Dolby Vision**, fully seekable, starting in 0.1 seconds, even on a **Raspberry Pi 4**.

This is not a torrent client with a media server bolted on. The FUSE filesystem *is* the product, custom-built from scratch around the constraints of torrent streaming: non-sequential byte-range requests, multi-gigabyte files that must be seekable at any position, and a Plex/Jellyfin scanner that probes every file in a library of hundreds of titles on startup.

### What's included

**The virtual filesystem is the product.** Every `.mkv` in the library is a live
torrent that the media server sees as an ordinary file: the correct size,
seekable, with no temporary copies and nothing downloaded to disk. Below it sits
**GoStorm**, a deeply patched fork of
[TorrServer Matrix](https://github.com/YouROK/TorrServer) and
[anacrolix/torrent](https://github.com/anacrolix/torrent), running in the
same OS process as the FUSE layer, so there is no HTTP proxy and no serialization
between the swarm and the player. Both upstreams carry streaming patches that do
not exist in the originals.

**The library keeps itself up to date.** A built-in sync engine, pure Go with no
Python and no external scripts, asks TMDB what is new, trending and popular on a
schedule, searches Prowlarr with Torrentio as fallback, and files the best
release it finds. When a better version appears later, it replaces the old one
automatically: a 1080p entry becomes 4K HDR without anyone asking. TV series
follow the same path, fullpack-first, in a Plex-compatible folder layout. Add a
title to your Plex cloud watchlist and it shows up within the hour.

**Your music and audiobooks ride the same filesystem.** `music/` and
`audiobooks/` sit next to `movies/` and `tv/` under the same mount, served by the
same FUSE layer and filed through the same Library API. An album is added by
exact path with a caller-supplied MusicBrainz or ASIN identity
(`external_id` + `external_id_ns`), which is stored in the SQLite projection
registry and written into the virtual file, so a release keeps a real identity
instead of a filename guess. Audio projections are immutable and read-only to
their clients: files answer `0444`, directories `0555`, and the mutations a
scanner never needs (write open, truncate, rename, mkdir, chmod) are refused
with `EROFS`, while a direct unlink gets `EPERM`. Playback is served live from
the swarm exactly like video, so Plex and Plexamp index and play a lossless
album that occupies no disk. FLAC for music; M4B, M4A and MP3 for audiobooks.

**Playback quality is the reason for the rest.** These pieces exist to turn an
unpredictable swarm into something a media player can trust:

- **Adaptive Shield** trades a little speed for data integrity only when a peer
  is actually sending bad data, then relaxes once the swarm proves clean. A peer
  that keeps corrupting pieces is banned outright, and the ban is remembered for
  30 days, across restarts.
- **TailHedge** removes the stutter caused by one slow peer holding up the exact
  byte the player is waiting for, by asking a second peer for the same data and
  using whichever arrives first.
- **PEXChurn** drops peers that turn out to be useless for the file you are
  actually watching, freeing connection slots for peers that can help.
- **Adaptive Chunk Size** scales the buffer to what the file needs instead of
  pinning one fixed size for every bitrate.

**Everything else is built in, not bolted on.** The **Control Panel** at
`:9080/control` adjusts every FUSE and engine setting live, and the **Health
Monitor Dashboard** at `:9080/dashboard` shows a real-time speed graph, the
active stream with poster and quality badges, sync controls and system stats.
**Plex and Jellyfin webhooks** are handled natively: `media.play` switches the
stream to Priority Mode with aggressive piece prioritization, and the IMDB id is
extracted from the raw payload, so localized titles still match. **NAT-PMP**
lets a WireGuard setup request an inbound port mapping from the VPN gateway and
install the `iptables REDIRECT` rules, all without a restart. The **peer
blocklist** is optional and off by default; when enabled it is downloaded at
startup, refreshed every 24 hours, and injected into the engine before any
connection is made. A **Library API** (`/api/library/add`, `/remove`, `/list`)
lets any HTTP client file or remove a title, movies, series, music and
audiobooks alike, without touching the filesystem, and
`hermes/SKILL.md` is the ready-made skill that teaches an AI agent to use it.
All of it, engine included, ships as a single `tiramisu` binary.

---
## Control Panel

![Tiramisu Control Panel overview](docs/screenshots/control_1.png)

---

## Table of Contents

- [The Setup: Tiramisu + Plex/Jellyfin/Infuse on Apple TV](#the-setup-tiramisu--plexjellyfininfuse-on-apple-tv)
- [How the Magic Works](#how-the-magic-works)
- [Architecture](#architecture)
- [Core Engineering](#core-engineering)
- [Performance](#performance)
- [Requirements](#requirements)
- [Quick Install](#quick-install)
- [How-To Guide](#how-to-guide)
- [Control Panel](#tiramisu-control-panel)
- [Health Monitor](#health-monitor-dashboard)
- [Configuration Reference](#configuration-reference)
- [Sync Engine](#sync-engine-go-native)
- [Library API](#library-api-9080)
- [AI Agent Skill](#ai-agent-skill)
- [Prowlarr Integration](#prowlarr-integration-resilience)
- [Plex/Jellyfin & Samba Setup](#plexjellyfin-and-samba-setup)
- [Build from Source](#build-from-source)
- [Docker](#docker)
- [API Reference](#api-quick-reference)
- [FAQ](#faq)
- [Troubleshooting](#troubleshooting)
- [Donate](#support)
- [License](#license)

---

## The Setup: Tiramisu + Plex/Jellyfin/Infuse on Apple TV

> Not a developer? This section explains what you actually get and why it works so well.

![Plex library populated by sync scripts](docs/screenshots/library.png)


**The end result**: you open Infuse on your Apple TV, your entire movie library appears with posters and metadata, you press Play on a 4K Dolby Vision film and it starts in under a second. No buffering. No "downloading...". No subscription to Real-Debrid or any external service. Everything runs on a single board computer or any always-on Linux box in your home.

### How the three pieces fit together

**Tiramisu** runs on your Linux device, a Raspberry Pi, a NAS, a VPS, or any always-on machine. It creates a virtual hard drive that looks completely real to the rest of your network: it contains thousands of `.mkv` files, each the correct size, each seekable. In reality, none of those files exist on disk. When anything reads a byte, Tiramisu silently fetches it in real-time from the BitTorrent network and passes it through.

**Plex** (or **Jellyfin**, or any media server) sees this virtual hard drive as a normal media library. It scans the files, downloads posters and descriptions from the internet, tracks what you've watched, and makes everything available on your home network, just like it would with a real NAS.

**Infuse** on Apple TV connects to your Plex/Jellyfin library and plays the files using Direct Play: it reads the video stream directly from the file, with no conversion or re-encoding. This is why it handles 4K HDR Dolby Vision effortlessly, even though it is coming from a torrent in real time.

Because Tiramisu exposes standard `.mkv` files on a standard filesystem, any player or media server that can read a network share works: Plex, Jellyfin, Emby, Kodi, VLC, mpv, or anything else. No plugins, no special configuration.

### How your library gets populated automatically

Tiramisu includes a built-in sync engine that runs on a schedule and keeps your library up to date without any manual intervention.

Every day, the engine queries **TMDB** (The Movie Database) for the latest releases, trending titles, and popular movies. For each title it finds, it searches **Prowlarr** (with Torrentio fallback) for the best available torrent (preferring 4K Dolby Vision, falling back to 1080p). If a good torrent is found, it registers it in GoStorm and creates the corresponding virtual `.mkv` file in the library.

The next time the media server scans, it finds a new file, downloads the poster and description, and the film appears in your library ready to play.

If a better version of a film becomes available later (for example a 4K HDR release of a title you already have in 1080p), the engine replaces it automatically.

TV series work the same way: the sync engine finds new seasons and episodes, organises them in the Plex/Jellyfin-compatible folder structure (`Show Name (Year)/Season.01/`), and they appear in your library within the week.

You can also add a title to your **Plex Watchlist** from any device and it will appear in your library within the hour.
### Prowlarr Integration (Resilience)

To ensure the system remains functional even when public aggregators like Torrentio are down, Tiramisu includes a **Prowlarr Adapter**. This allows you to use your own self-hosted Prowlarr instance as the primary source for torrents.

The sync engine implements a **Strict Fallback** logic: it first queries your Prowlarr indexers using IMDB IDs for maximum precision. If no results are found locally, it automatically falls back to Torrentio.

**Configuration** is done via `config.json` (or the Control Panel → **Prowlarr Indexer** section):

```json
"prowlarr": {
  "enabled": true,
  "api_key": "your-api-key",
  "url": "http://192.168.1.x:9696"
}
```

See the [Prowlarr Adapter Documentation](docs/prowlarr-adapter.md) for technical details.


### 100% local, no subscriptions

Tiramisu has no external dependency at playback time. No third-party service, no monthly fee, no data leaving your home. Your library is always available, even without an internet connection, and it never disappears because a remote service went down.

### Why Infuse starts in under a second

When you press Play, Infuse immediately reads the beginning and end of the file to load the video index and seek tables. On a real hard drive this is instant. Tiramisu replicates this with an **SSD warmup cache**: the first 64 MB and last few MB of every file are pre-cached on the Pi's SSD during the initial Plex/Jellyfin library scan. By the time you press Play, those bytes are already on disk and Infuse gets them in milliseconds.


### Why your library survives a reboot

Every file on a real filesystem has a permanent ID called an **inode**. Plex/Jellyfin and Infuse use these IDs to recognize files across restarts, so they know "this is the same film I scanned last week" and do not re-download metadata or reset your watch history.

On a standard virtual filesystem, these IDs are random and change every time the system restarts. Tiramisu solves this by persisting a permanent inode map to a SQLite database (`STATE/tiramisu.db`). After a reboot, every virtual `.mkv` gets back the exact same ID it had before. To Plex/Jellyfin and Infuse, it is indistinguishable from a file that never moved.

### When you press Play: the full chain

You tap Play on Infuse, and Infuse asks Plex (or Jellyfin) for the file. Plex
reads it from Tiramisu's virtual filesystem, which is where the illusion ends:
the media server also fires a webhook at Tiramisu saying "user started playing
this film". Tiramisu matches the event to a torrent and switches that stream to
**Priority Mode**, so every spare byte of bandwidth goes to the film you are
watching and background activity pauses. From there the bytes travel the only
path they ever take: from BitTorrent peers into Tiramisu's memory, through the
FUSE layer to Plex, from Plex to Infuse, and onto the TV. If you seek, the same
machinery jumps straight to the new position in the torrent instead of
re-buffering from the beginning.

---

## How the Magic Works

Plex/Jellyfin reads `/mnt/tiramisu-mkv-virtual/movies/Interstellar.mkv`. From Plex/Jellyfin's perspective, it's a normal 55 GB file on a local disk. In reality, the file does not exist. The FUSE kernel module intercepts the read, calls into Tiramisu, and Tiramisu serves the exact bytes from a three-layer cache, backed by a live BitTorrent swarm.

| Layer | What | Size | Purpose |
|-------|------|------|---------|
| **L1** | In-memory Read-Ahead | 256 MB | 32-shard concurrent buffer with per-shard LRU |
| **L2** *(optional)* | SSD Warmup Head | 64 MB/file | Instant TTFF on repeat playback, served at 150–200 MB/s from SSD |
| **L3** *(optional)* | SSD Warmup Tail | 16 MB/file | MKV Cues (seek index), Plex/Jellyfin probes the end of every file before confirming playback |

What makes this non-trivial: a FUSE filesystem that backs a real directory of static files is straightforward. A FUSE filesystem that must handle non-sequential byte-range requests across hundreds of files, each backed by an independent torrent with variable peer availability, while a Plex/Jellyfin scanner hammers every inode in parallel: that required building every subsystem from scratch.

---

## Architecture

```
BitTorrent Peers ←→ GoStorm Engine (:8090)
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
| `:8090` | GoStorm API, JSON torrent management |
| `:9080` | Control Panel, Metrics, Dashboard, Webhook, Scheduler, Library API |

---

## Core Engineering

> **Eleven purpose-built subsystems**, each one the answer to a problem that
> surfaced while running this on Raspberry Pi 4 hardware.

A media server gives a streaming engine no second chances: it opens many files
at once, reads the end of a file before the beginning, seeks wherever the user
drags the bar, and expects the bytes to be there. On a Pi the margin is thin, so
every subsystem below exists to remove one specific way the illusion can break.

### 1. Zero-Network Native Bridge

Tiramisu is one process, not two. The GoStorm engine and the FUSE layer are
compiled into the same binary, so when the media server asks for a byte range
the FUSE side calls straight into the engine through an in-memory `io.Pipe()`.
There is no TCP round trip, no HTTP headers to parse, no serialization and no
proxy in the middle; metadata lookups are plain Go function calls. That is what
removes the network latency that makes HTTP-based streaming proxies stutter on
small hardware.

### 2. Two-Layer SSD Warmup Cache *(optional)*

An optional SSD cache that makes repeat playback almost instant. It is enabled
and configured from the Control Panel → GoStorm settings, and it is independent
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
30 days, across restarts. The mode switch itself is an atomic boolean, so the
hot read path pays no lock.

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
| **Movies** | Scheduler / manual | TMDB Discover + Popular → Prowlarr/Torrentio → GoStorm → virtual `.mkv` |
| **TV Series** | Scheduler / manual | TV series with fullpack-first approach, episode registry |
| **Watchlist** | Scheduler / manual | Plex cloud watchlist → IMDB → Prowlarr/Torrentio → GoStorm |

**Quality ladder**: `4K DV > 4K HDR10+ > 4K HDR > 4K > 1080p REMUX > 1080p`\
**Minimum seeders**: 15, for the main sync and the watchlist alike (the watchlist uses the movie profile)

Everything the engine needs to remember, the episode registry, the negative
caches and the scheduler state, is persisted in `STATE/tiramisu.db` (SQLite).
The built-in scheduler replaces system cron, and it is configured from the
Control Panel.

### 8. NAT-PMP Native VPN Port Forwarding

Behind a WireGuard tunnel the home router's port forwarding no longer applies,
because the traffic leaves through the VPN gateway instead. Tiramisu runs a
NAT-PMP sidecar that periodically asks the gateway for a TCP+UDP port mapping,
installs the `iptables PREROUTING REDIRECT` rules that point the mapping at the
local listener, and updates GoStorm's listen port. All of it at runtime: no
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

### 11. GoStorm Engine: a deep fork of TorrServer Matrix + anacrolix/torrent

GoStorm is a fork of **[TorrServer Matrix](https://github.com/YouROK/TorrServer)**
(BitTorrent management layer) and
**[anacrolix/torrent](https://github.com/anacrolix/torrent)** (peer
protocol engine). Both upstreams have been patched extensively for streaming
correctness and performance, in ways that are not present in the originals:

<details>
<summary><b>Click to expand: 15+ targeted optimizations</b></summary>

| Optimization | Problem → Solution |
|---|---|
| **O(1) `AddTorrent` DB write** | Original rewrote all 520 torrents on every add (O(N) fsync). Fixed to single `tdb.Set()`. |
| **O(1) `GetTorrentDB`** | Original called `ListTorrent()` + 520 unmarshals to find one torrent. Fixed to direct key lookup. |
| **InfoBytes + PeerAddrs caching** | `TorrentSpec.InfoBytes` was never persisted, re-activation required full metadata fetch. Now saved on `Wake()`. |
| **Request rebuild debounce** | 300 rebuilds/s reduced to 60 → **5x CPU reduction**. |
| **O(1) `clearPriority`** | Original iterated all ~512 cached pieces with global lock. Replaced with `localPriority` map tracking ~25 active pieces. |
| **4 MB MemPiece buffer zeroing** | Channel pools reused buffers without zeroing → stale data from different files caused forward-jump corruption. Fixed with `clear(p.buffer)`. |
| **raCache defensive copies** | `Get()` returned sub-slices of pooled buffers. On eviction, Plex/Jellyfin received overwritten data. Fixed with copies on `Put()` and `Get()`. |
| **`cleanTrigger` panic fix** | `Cache.Close()` closed the channel while goroutines could still send → panics during peer upload. Fixed with separate `cleanStop` channel. |
| **`PeekTorrent` discipline** | Monitoring endpoints using `GetTorrent()` caused silent torrent activation loops. All monitoring paths now use `PeekTorrent()`. |
| **InodeMap GC fix** | Inode cleanup pruned virtual MKV stubs every 5 min (529 files). Fixed to use `WalkDir(physicalSourcePath)`. |
| **8 additional race condition fixes** | Concurrent map writes, torn reads, nil pointer dereferences across `requesting.go`, `piece.go`, `cache.go`, `apihelper.go`. |

</details>

---

## Performance

> Measured on a **Raspberry Pi 4** (4 GB RAM, Cortex-A72, arm64, no hardware
> crypto), the minimum supported baseline. On amd64 or a more powerful arm64 box
> the numbers are better.

The experience in two lines: a first play from a cold swarm takes 2–4 seconds, a
repeat play from the SSD warmup cache opens in a fraction of a second, and 4K
HDR playback costs about a fifth of one core. The table is the detail behind
those two.

| Metric | Value |
|--------|-------|
| Cold start (first play, no warmup) | 2–4 s |
| Warm start (SSD warmup HIT) | **0.1–0.5 s** |
| Seek latency (cached position) | **< 0.01 s** |
| CPU at 4K HDR streaming | **20–23%** of one core |
| CPU reduction vs. baseline | **−87%** (161% → 20%) |
| Binary size | **33 MB** (60% smaller than legacy builds) |
| Memory footprint (read-ahead) | Deterministic 256 MB |
| GOMEMLIMIT | 2200 MiB |
| Peak throughput (fast seeder) | **400+ Mbps** |
| Plex/Jellyfin scan peer count | ~6 total (was ~15,000 before fix) |
| Inode shard count | 32 (collision-protected) |
| Warmup cache capacity | ~400 films at 80 MB each (32 GB) |

---

## Requirements

| Component | Details |
|-----------|---------|
| **Hardware** | Any `linux/amd64` or `linux/arm64` device, **Raspberry Pi 4 is the minimum tested baseline** (4 GB RAM recommended). Runs on NAS, VPS, mini-PC, or any always-on Linux box. **On amd64, AVX2 is strongly recommended** (any x86_64 CPU from ~2013 onward, Intel Haswell or AMD Excavator and later). Without it, Go's SHA1 piece verification falls back to its slowest scalar path and may not sustain real playback bitrates; Tiramisu still starts, logging a warning at boot so high CPU and stuttering have a visible cause. arm64 has no equivalent requirement: NEON is mandatory on every ARMv8-A chip, so any real arm64 device, the Pi 4 included, already clears the floor. |
| **Go** | 1.26+ (`linux/amd64` or `linux/arm64`), do **not** use the 32-bit `linux/arm` toolchain. Build with `CGO_ENABLED=1`: the SQLite piece-completion layer is cgo |
| **FUSE 3** | `sudo apt install fuse3 libfuse3-dev` |
| **systemd** | For service management |
| **Samba** | `sudo apt install samba` |
| **Plex/Jellyfin** | Media Server (on Synology or any network host) |
| **TMDB API key** | Free at [themoviedb.org/settings/api](https://www.themoviedb.org/settings/api) |
| **Plex token** | Settings → Account → XML API |

---

## Quick Install

```bash
curl -fsSL https://raw.githubusercontent.com/MrRobotoGit/tiramisu/main/install.sh -o install.sh
chmod +x install.sh
./install.sh
```

![Tiramisu interactive installer](docs/screenshots/install.png?v=2)

The interactive installer handles everything end-to-end:
1. Installs system dependencies (`fuse3`, `libfuse3-dev`, `gcc`, `samba`, `git`)
2. Prompts for all required paths, Plex credentials, TMDB key, and NAT-PMP settings
3. Generates `config.json` from `config.json.example`
4. Creates `Tiramisu/STATE/`, `logs/`, and FUSE mount point directories
5. **Compiles the Tiramisu binary** (downloads Go if needed, detects architecture automatically)
6. Writes and enables the systemd service for `tiramisu`

Once complete:

```bash
sudo systemctl start tiramisu
```

---

## How-To Guide

> [!CAUTION]
> **Attention:** After the first run of the Movie and TV sync engine, hundreds of `.mkv` files will be created. Plex/Jellyfin will then begin scanning these files to populate its UI. During this initial scan, playback will be extremely slow or may time out. This is because the media server opens and closes files hundreds of times during analysis, which congests the BitTorrent engine. Please wait for Plex/Jellyfin to complete its scan before starting any streams. Enjoy!


<details>
<summary><b>Step 1: Configure the Webhook (Plex or Jellyfin)</b></summary>

Required for Priority Mode (bitrate boost during playback), fast-drop on stop, and IMDB-ID-based file matching.

**Plex**: Settings → Webhooks → Add Webhook:
```
http://192.168.1.2:9080/plex/webhook
```

**Jellyfin**: Install the Webhook plugin, add one webhook (events: `PlaybackStart`, `PlaybackStop`, `PlaybackProgress`):
- URL: `http://192.168.1.2:9080/plex/webhook`
- Header: Key `Content-Type` / Value `application/json`
- Template:
```
{"event":"{{NotificationType}}","Metadata":{"title":"{{{Name}}}","grandparentTitle":"{{{SeriesName}}}","librarySectionType":"{{ItemType}}","guid":"imdb://{{Provider_imdb}}","Guid":[{"id":"imdb://{{Provider_imdb}}"}]}}
```

Music needs a second Generic destination with its own template, same URL and events:

```
{"event":"{{NotificationType}}","Metadata":{"title":"{{{Name}}}","grandparentTitle":"{{{Artist}}}","librarySectionType":"{{ItemType}}","ProviderIds":{"MusicBrainzTrack":"{{Provider_musicbrainztrack}}","MusicBrainzReleaseGroup":"{{Provider_musicbrainzreleasegroup}}","MusicBrainzAlbum":"{{Provider_musicbrainzalbum}}","MusicBrainzArtist":"{{Provider_musicbrainzartist}}"}}}
```

Test connectivity:
```bash
curl -X POST http://192.168.1.2:9080/plex/webhook \
  -H 'Content-Type: application/json' \
  -d '{"event":"media.play"}'
```

</details>

<details>
<summary><b>Step 2: Configure the Plex/Jellyfin Library</b></summary>

Add a Movies library in Plex/Jellyfin pointing to the Samba share:
```
smb://192.168.1.2/tiramisu-mkv-virtual/movies
```
Or, if using Synology, point Plex/Jellyfin to the CIFS mount: `/volume1/Tiramisu/movies`.

Run a library scan after adding the library. Plex/Jellyfin reads the first megabyte of every `.mkv` file during the scan, this automatically populates the SSD warmup head cache for every title. Subsequent plays will start in under 0.5 seconds.

</details>

<details>
<summary><b>Step 3: Add Your First Movie</b></summary>

**Manually via API:**
```bash
curl -X POST http://127.0.0.1:8090/torrents \
  -H "Content-Type: application/json" \
  -d '{"action":"add","link":"magnet:?xt=urn:btih:...","title":"Interstellar (2014)"}'
```

**Via sync engine (recommended):**
Trigger from the Control Panel (`:9080/control`) → Sync Scheduler → Movies → "Run now", or via API:
```bash
curl -X POST http://127.0.0.1:9080/api/scheduler/movies/run
```
The engine fetches popular films from TMDB, finds the best available torrent for each via Prowlarr/Torrentio, adds them to GoStorm, and writes virtual `.mkv` stub files.

</details>

<details>
<summary><b>Step 4: Watch a Film (What Happens Internally)</b></summary>

```
1. Plex/Jellyfin requests /mnt/tiramisu-mkv-virtual/movies/Interstellar.mkv
2. FUSE Open() triggers Wake(), GoStorm activates the torrent
3. Plex/Jellyfin metadata probes → served from SSD warmup cache if enabled, otherwise from the torrent pump
4. Plex/Jellyfin probes MKV Cues at end → served from SSD tail cache if enabled
5. Plex or Jellyfin sends media.play webhook → Tiramisu activates Priority Mode
6. Streaming reads → served from Read-Ahead Cache or Native Bridge pump
7. Playback begins (0.1–0.5 s with warmup, 2–4 s cold) ✨
```

</details>

<details>
<summary><b>Step 5: Seek in 4K</b></summary>

When Plex/Jellyfin seeks to a new timestamp:

1. `Read()` is called at the new offset, `lastOff` is updated immediately
2. If the jump exceeds 256 MB: `Interrupt()` closes the pipe, pump goroutine unblocks atomically
3. Pump detects `lastOff` is > 256 MB ahead, snaps to aligned chunk position
4. Pump restarts via `startStream(newOff)`, GoStorm repositions torrent reader
5. Data arrives from peers or SSD cache within seconds

The pump goroutine survives `ErrInterrupted`, it sleeps 200 ms and continues the read loop, so no goroutine restart overhead.

</details>

<details>
<summary><b>Step 6: Add from Your Plex Watchlist</b></summary>

Add any movie to your Plex cloud watchlist (desktop or mobile app). Within the interval configured in the Scheduler (default: 1 hour), the Go sync engine will resolve it to an IMDB ID, find the best torrent, and add it automatically.

Trigger manually from the Control Panel (`:9080/control`) → Sync Scheduler → Watchlist → "Run now", or via API:
```bash
curl -X POST http://127.0.0.1:9080/api/scheduler/watchlist/run
```

The engine:
1. Queries `discover.provider.plex.tv` for your watchlist
2. Resolves each entry to an IMDB ID (falls back to TMDB)
3. Queries Prowlarr/Torrentio for the best stream (minimum 10 seeders)
4. Adds to GoStorm and writes a virtual `.mkv` stub

</details>

<details>
<summary><b>Step 7: Monitor in Real Time</b></summary>

```bash
# Control Panel, Tiramisu + GoStorm settings, paths, scheduler, restart button
open http://192.168.1.2:9080/control

# Health Dashboard, speed graph, torrent stats, active stream, system stats
open http://192.168.1.2:9080/dashboard

# Raw metrics (JSON)
curl -s http://192.168.1.2:9080/metrics | python3 -m json.tool

# Scheduler status
curl -s http://192.168.1.2:9080/api/scheduler/status | jq

# Live log key events only
ssh pi@192.168.1.2 "tail -f /home/pi/Tiramisu/logs/tiramisu.log | grep -E '(OPEN|NATIVE|Interrupt|Jump|DiskWarmup|Emergency)'"

# Active torrents in RAM with speed
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"active"}' http://192.168.1.2:8090/torrents | \
  jq '.[] | {title: .title[:60], speed_mbps: ((.download_speed//0)/1048576|round), peers: (.active_peers//0)}'
```

</details>

<details>
<summary><b>Step 8: Configure the MKV Creation Scheduler</b></summary>

Tiramisu includes a built-in scheduler that replaces system cron. Configure it from the Control Panel at `:9080/control` → **Sync Scheduler** card.

**Three jobs are available:**

| Job | Default schedule | Log file |
|-----|-----------------|----------|
| Movies Library Sync | Mon + Thu at 03:00 | `logs/movies-sync.log` |
| TV Series Libraries Sync | Wed + Fri at 04:00 | `logs/tv-sync.log` |
| Plex Watchlist Sync | Every 1 hour | `logs/watchlist-sync.log` |

Enable the scheduler with the **Enable Scheduler** toggle, pick weekdays and start time for each job, then click **Save schedule**. Changes require a service restart to take effect.

> **Run now**: trigger any job immediately from the Control Panel or via `POST /api/scheduler/{type}/run` on port 9080.

</details>

<details>
<summary><b>Step 9: Tune GoStorm Settings</b></summary>

Via the Control Panel at `:9080/control`, or via API:

```bash
curl -X POST http://127.0.0.1:8090/settings \
  -H "Content-Type: application/json" \
  -d '{
    "action": "set",
    "sets": {
      "CacheSize": 100663296,
      "ReaderReadAHead": 95,
      "PreloadCache": 0,
      "ConnectionsLimit": 25,
      "TorrentDisconnectTimeout": 10,
      "UseDisk": true,
      "ResponsiveMode": true
    }
  }'
```

| Setting | Value | Rationale |
|---------|-------|-----------| 
| `CacheSize` | 96 MB | Lean engine strategy, feed FUSE 256 MB buffer; smaller heap = lower GC. 64 MB caused buffering in testing. |
| `ConnectionsLimit` | 25 | Matches FUSE master semaphore; prevents Samba thread exhaustion |
| `ResponsiveMode` | `true` | Serve unverified data; Adaptive Shield corrects corruption automatically |
| `UseDisk` | `true` | Enable SSD warmup cache |
| `TorrentDisconnectTimeout` | 10 s | Fast peer cleanup for RAM footprint |

</details>

<details>
<summary><b>Step 10: Regenerate the PGO Profile</b></summary>

Capture a CPU profile during real streaming workload:

```bash
# Single profile (120 seconds while streaming a 4K film)
curl -o ~/Tiramisu/default.pgo \
  "http://127.0.0.1:9080/debug/pprof/profile?seconds=120"

# Or merge multiple workloads for better coverage
curl -o /tmp/pgo-stream.pprof "http://127.0.0.1:9080/debug/pprof/profile?seconds=120"
curl -o /tmp/pgo-sync.pprof   "http://127.0.0.1:9080/debug/pprof/profile?seconds=120"
go tool pprof -proto /tmp/pgo-stream.pprof /tmp/pgo-sync.pprof > ~/Tiramisu/default.pgo

# Rebuild Go detects the changed profile and re-optimizes automatically
cd ~/Tiramisu
GOARCH=arm64 CGO_ENABLED=1 /usr/local/go/bin/go build -pgo=auto -o tiramisu .
```

Regenerate after significant code changes. On Pi 4 Cortex-A72, `sha1.blockGeneric` in the profile is expected: the A72 has no hardware SHA1 extensions.

</details>

---

## Tiramisu Control Panel

The Control Panel is where you tune the engine, point it at your paths and
credentials, run a sync by hand and restart the service, all without opening
`config.json` or an SSH session. It is a web UI **embedded in the Tiramisu
binary**: no additional server, no React build step, no external dependencies.
Served at `:9080/control`.

```
http://<your-pi-ip>:9080/control
```

### Simple / Advanced Mode

A toggle in the top-right corner switches between two views:

- **Simple**: Most frequently changed settings: read-ahead budget, concurrency, cache size, paths, and NAT-PMP toggle
- **Advanced**: All tunable parameters, split into labelled groups across two panels

### Tiramisu FUSE Panel (left)

Settings are written to `config.json` and require a **service restart**. The **Restart** button in the header triggers an immediate restart.

| Group | Settings |
|-------|----------|
| **Core & Streaming** | ReadAhead Budget (MB), Master Concurrency, Max Streaming Slots, Streaming Threshold (KB) |
| **Paths** | Physical Source Path (Samba root), FUSE Mount Path |
| **Prowlarr Indexer** | Enable Prowlarr, API Key, URL |
| **Media Manager (Plex)** | TMDB API Key, Plex URL, Plex Token, Movies Library ID, Series TV Library ID |
| **Connectivity & Rescue** | GoStorm URL, Rescue Grace/Cooldown, Metrics Port, Log Level, Tiramisu Port, Enable BlockList, BlockList URL |

### GoStorm Engine Panel (right)

Settings are pushed **live via API**: no restart needed. **Apply All Core Settings** sends values immediately.

| Group | Settings |
|-------|----------|
| **Cache & Data** | Cache Size (MB), Readahead Cache (%), Preload Cache (%) |
| **Warmup & SSD** | Use Warmup Cache, Warmup path, SSD Quota (GB), Head Warmup (MB) |
| **Swarm Limits** | Connections Limit, DL/UP Rate (KB/s), Disconnect Timeout (s) |
| **Network & Protocol** | Listen Port, Retrackers Mode, IPv6/DHT/PEX/TCP/uTP/Upload/Force Encrypt |
| **NAT-PMP (WireGuard)** | Enable, Gateway IP, VPN Interface, Refresh/Lifetime (s), Local Port |
| **Behaviors** | Smart Responsive Mode, Debug Log |

---

## Health Monitor Dashboard

<p align="center"><img src="docs/screenshots/health_monitor_1.png" alt="Health Monitor, Status grid and sync controls" width="500"></p>

Embedded Go service at port **`:9080`**. Real-time operational view of the entire
stack: one page that answers "is it healthy right now, and what is it doing",
without reading a log.

```
http://<your-pi-ip>:9080/dashboard
```

### Status Grid

Six cards in a 2×3 grid:

| Card | What it shows |
|------|--------------| 
| **GOSTORM** | API ping latency (green = responding). Restart button. |
| **FUSE MOUNT** | Number of virtual `.mkv` files currently exposed. |
| **VPN (WG0)** | WireGuard interface status: VPN IP and gateway. |
| **NAT-PMP** | Active external port assigned by VPN gateway. |
| **PLEX** | Server version and reachability. |
| **SYSTEM** | CPU %, RAM %, free disk space, live via gopsutil. |

### Download Speed Graph

**15-minute rolling chart** of GoStorm download speed in Mbps. Samples every 5 seconds with auto-scroll.

### Active Stream Panel

Appears automatically during playback:
- 🎬 **Movie poster** (fetched from TMDB)
- 🏷️ **Quality badges**: `PRIORITY`, `4K`, `DV`, `ATMOS`, `HDR10+`
- 📡 **LIVE indicator** + 5-minute average speed
- 👥 **Peer/seeder count** for the active torrent

### Sync Controls

Two panels for manual sync execution without SSH:
- **MOVIES SYNC**: Triggers the Go movie sync engine with live log streaming
- **TV SYNC**: Triggers the Go TV sync engine with Start/Idle status

### MKV Creation Scheduler

Built into the Go binary. Configured from the **Sync Scheduler** card in the Control Panel:
- **Movies Library Sync**: select weekdays + start time
- **TV Series Libraries Sync**: select weekdays + start time
- **Plex Watchlist Sync**: interval-based (1 h, 2 h … 24 h)

Last/next run times and live status are shown in the card. State persists across restarts in `STATE/tiramisu.db`.

---

## Configuration Reference

`config.json` is resolved relative to the binary's path (`os.Executable()`), so
no path argument is needed, and it is not tracked by git because it holds
credentials. In practice you rarely edit it by hand: the Control Panel writes
the same file, with FUSE and path settings applied at the next restart and
engine settings applied live.

```bash
cp config.json.example /home/pi/Tiramisu/config.json
nano /home/pi/Tiramisu/config.json
```

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
| `proxy_listen_port` | `8080` | Legacy field kept for config compatibility: no listener uses it today |
| `metrics_port` | `9080` | Metrics, Control Panel, Webhook port |
| `media_server_type` | `plex` | `plex` or `jellyfin`; selects how the post-sync library refresh is issued |
| `torrentio_url` | `https://torrentio.strem.fun` | Torrentio base URL, the fallback when Prowlarr is disabled or finds nothing |
| `blocklist_enabled` | `false` | Load the peer IP blocklist (not needed behind a VPN) |
| `blocklist_url` | *(iblocklist Level 1)* | Gzipped blocklist URL, refreshed every 24 h; compression is detected from the content |
| `blocklist_filter` | `(?i)\bap2p\b\|anti-?p2p` | Keep only ranges whose description matches this regexp; empty loads the list whole |
| `plex.url` | *(none)* | Media server URL: Plex or Jellyfin |
| `plex.token` | *(none)* | Plex token, or Jellyfin API key |
| `plex.library_id` | `0` | Plex movies library section ID (0 = skip the refresh) |
| `plex.tv_library_id` | `0` | Plex TV series library section ID (0 = skip the refresh) |
| `tmdb_api_key` | *(none)* | TMDB API key |
| `prowlarr.enabled` | `false` | Use Prowlarr as primary indexer (falls back to Torrentio if disabled) |
| `prowlarr.api_key` | *(none)* | Prowlarr API key (Settings → General → API Key) |
| `prowlarr.url` | *(none)* | Prowlarr base URL (e.g. `http://192.168.1.x:9696`) |
| `natpmp.enabled` | `false` | Enable NAT-PMP |
| `natpmp.gateway` | *(none)* | VPN gateway IP |
| `natpmp.vpn_interface` | `wg0` | WireGuard interface |

### Quality Scoring

Every candidate release, film or episode, is scored before it is chosen:
resolution, HDR and Dolby Vision, the audio track, remux, your preferred
language, the seeder count and the size gates all add up to a single number, and
the highest one wins. Those weights used to be fixed in the code; they now live
in an optional `quality_scoring` block in `config.json`, editable live from the
**Release Selection** card in the Control Panel.

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

```ini
Environment="GOMEMLIMIT=2200MiB"
Environment="GOGC=100"
```

`GOMEMLIMIT=2200MiB` leaves headroom for OS and Samba on a 4 GB Pi 4.

---

## Sync Engine (Go Native)

All sync logic runs natively inside the Go binary. No Python, no external scripts, no subprocess overhead. Configure schedules from the Control Panel at `:9080/control` → **Sync Scheduler**.

### Movies Sync

Queries TMDB Discover + Popular (Italian + English, region IT+US), evaluates Prowlarr/Torrentio results, adds the best torrent.

Trigger from Control Panel → Sync Scheduler → Movies → "Run now", or via API:
```bash
curl -X POST http://127.0.0.1:9080/api/scheduler/movies/run
```

- Quality: `4K DV > 4K HDR10+ > 4K HDR > 4K > 1080p REMUX > 1080p`
- Min seeders: 15 · Min size: 10 GB (4K), 4 GB (1080p)
- Skips existing films (by TMDB ID) · Upgrades lower-quality entries

### TV Sync

```bash
curl -X POST http://127.0.0.1:9080/api/scheduler/tv/run
```

Fullpack-first approach, prefers complete season packs. Plex/Jellyfin-compatible directory structure:
```
Show Name (Year)/
  Season.01/
    Show_Name_S01E01_<hash8>.mkv
    Show_Name_S01E02_<hash8>.mkv
```

### Dead-Release Reaper

A torrent can die without anyone noticing: the release stays in the library, but
its swarm stops answering. The engine counts the metadata timeouts of each
release, and when a title crosses the threshold it is re-searched in later syncs
instead of being left as a file that can never play.

On movies the dead release is skipped, the title is looked up again even outside
the usual discovery window, and either a live release replaces it or the stub is
removed. On TV the decision is per pack, because one season pack stands behind
up to twenty episodes: the pack is removed only when every season it covers was
searched end to end, and an episode that simply could not be checked is kept
rather than guessed away. Every episode removed this way is also recorded as a
hole, so a show the discovery feed no longer returns can still be repaired later
(see [Episode gaps](#episode-gaps-v1971) in the Library API).

### Watchlist Sync

```bash
curl -X POST http://127.0.0.1:9080/api/scheduler/watchlist/run
```

Reads Plex cloud watchlist → IMDB ID resolution → Prowlarr/Torrentio (min 15 seeders, the movie profile) → GoStorm.

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

## AI Agent Skill

`hermes/SKILL.md` is a portable skill file for an AI agent, written for
[Hermes](https://github.com/NousResearch/hermes-agent) and following the same
`SKILL.md` convention as [OpenClaw](https://github.com/openclaw/openclaw) and
Claude Code: YAML frontmatter with a name, a description, a version, an author
and a license, then markdown instructions. Drop it in a skills directory to use
it with any of them.

It teaches the agent how to add one specific release by hand for the times the
automated sync misses something, and how to reason about the holes the reaper
leaves behind. The flow reads the deployment's own scoring profile from
`/api/config`, searches the indexers, scores the candidates the way the sync
engine would, files the winner through the [Library API](#library-api-9080), and
undoes it if the pick was wrong. Everything goes over HTTP against the control
port, so the agent needs no access to the host and nothing to install. No hosts,
ports or weights are hardcoded: they are read from the running configuration, so
the same file works unchanged on any deployment.

The add flow needs v1.9.64 or later, the gap workflow v1.9.71 or later.

Once the skill is loaded, plain language is enough:

```
Hermes, add the latest Dummy Bunny movie in 4K
Hermes, season 2 of Dummy Bunny is missing episodes 4 to 8, fill them in
Hermes, is Dummy Bunny (2024) already in the library, and in what quality?
```

The agent picks the release, applies the same scoring the sync engine uses, and
reports what it chose and why before writing anything.

---

## Plex/Jellyfin and Samba Setup

### Connecting a Media Server

Point the **Media Server** card in the Control Panel at your server, and Tiramisu
identifies which product answers, showing it as verified with its latency. It
also broadcasts on the LAN, Plex GDM on UDP 32414 and Jellyfin on UDP 7359, and
lists whatever replies, so a server on the same network appears without
configuration. A broadcast does not cross a Docker bridge network, and Jellyfin
lets you switch its discovery off, so setting the URL is the reliable way.

| Setting | Plex | Jellyfin |
|---------|------|----------|
| `media_server_type` | `plex` | `jellyfin` |
| `plex.url` | `http://host:32400` | `http://host:8096` |
| `plex.token` | [X-Plex-Token](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/) | API key: Dashboard → Advanced → API Keys → **+** |
| `plex.library_id` / `plex.tv_library_id` | Section IDs to refresh after a sync | Leave at `0` |

The token drives the library refresh Tiramisu issues when the movies, TV and
watchlist syncs finish, and the field names keep the `plex.` prefix for backward
compatibility: they hold whichever server `media_server_type` names. Plex
refreshes the section named by `library_id` and does nothing without one;
Jellyfin refreshes every library, which is why its section IDs stay at `0`.
Without a token the media server finds new files on its own scan schedule
instead. The playback sessions and posters on the dashboard are Plex-only.

### Samba Configuration

Critical parameters in `/etc/samba/smb.conf` to prevent FUSE deadlocks during Plex/Jellyfin library scans:

```ini
[tiramisu-mkv-virtual]
   path = /mnt/tiramisu-mkv-virtual
   browseable = yes
   read only = yes
   oplocks = no           # CRITICAL: prevents kernel exclusive locks on FUSE files
   aio read size = 1      # CRITICAL: forces async I/O, prevents smbd D-state
   deadtime = 15          # cleans inactive SMB connections every 15 minutes
   vfs objects = fileid   # CRITICAL: transmits 64-bit inodes to Synology/Plex/Jellyfin
```

> [!WARNING]
> **`oplocks = no` is non-negotiable.** With oplocks enabled, the kernel requests exclusive locks on FUSE-backed files, causing `smbd` threads to enter D-state indefinitely during concurrent Plex/Jellyfin scans.

> [!IMPORTANT]
> **`vfs objects = fileid`** ensures 64-bit inode transmission. Without it, Synology receives truncated 32-bit inodes, causing Plex to misidentify files.

### Synology CIFS Mount

```
Source:  //pi-ip/tiramisu-mkv-virtual
Target:  /volume1/Tiramisu
Options: serverino,vers=3.0,uid=1024,gid=100,file_mode=0777,dir_mode=0777
```

`serverino` must remain active. Synology may silently drop it after network timeouts. Schedule a Task Scheduler job (every 5 min) to verify and remount if needed.

---

## Build from Source

> [!IMPORTANT]
> Compile natively on the target machine. Do not cross-compile: the PGO profile must match the target CPU architecture.

```bash
ssh pi@192.168.1.2
cd ~/Tiramisu

/usr/local/go/bin/go clean -cache
/usr/local/go/bin/go mod tidy
GOARCH=arm64 CGO_ENABLED=1 /usr/local/go/bin/go build -pgo=auto -o tiramisu .

# Deploy: the install directory is ~/Tiramisu by default; if you built
# elsewhere, copy the binary there first
sudo systemctl stop tiramisu
sudo systemctl start tiramisu
```

**Verify the toolchain is 64-bit:**
```bash
/usr/local/go/bin/go version
# Required: go version go1.26.x linux/arm64
# Wrong:    go version go1.26.x linux/arm   <-- 32-bit
```

**Install Go 1.26 if needed:**
```bash
wget https://go.dev/dl/go1.26.8.linux-arm64.tar.gz
sudo tar -C /usr/local -xzf go1.26.8.linux-arm64.tar.gz
```

---

## Docker

> [!IMPORTANT]
> Tiramisu mounts a FUSE filesystem at startup. Docker blocks this syscall by default. The container requires elevated privileges to run.

Pre-built images for `linux/amd64` and `linux/arm64` are published automatically on every release to Docker Hub and GitHub Container Registry:

| Registry | Image |
|----------|-------|
| **Docker Hub** | `mrrobotogit/tiramisu:latest` |
| **GHCR** | `ghcr.io/mrrobotogit/tiramisu:latest` |

**Pull:**

```bash
docker pull mrrobotogit/tiramisu:latest
# or
docker pull ghcr.io/mrrobotogit/tiramisu:latest
```

**Run:**

```bash
docker run -d \
  --name tiramisu \
  --restart unless-stopped \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --cap-add NET_ADMIN \
  -v /path/to/config.json:/config.json \
  -v /path/to/state:/state \
  -v /mnt/tiramisu-mkv-real:/mnt/tiramisu-mkv-real \
  -v /mnt/tiramisu-mkv-virtual:/mnt/tiramisu-mkv-virtual:rshared \
  -p 8090:8090 \
  -p 9080:9080 \
  mrrobotogit/tiramisu:latest
```

Or use `--privileged` as a simpler alternative to the individual capabilities (e.g. on a Raspberry Pi where the container is fully trusted). In practice `--cap-add SYS_ADMIN`/`NET_ADMIN` alone have been reported insufficient on some Ubuntu hosts — if the container starts cleanly but the virtual directory stays empty, use `--privileged`.

`config.json` must be volume-mounted at `/config.json` (the default `MKV_PROXY_CONFIG_PATH`). Use `config.json.example` as the starting point. Do not mount it `:ro`: the Control Panel writes settings back to this file, and a read-only mount makes every save fail.

**Mount a volume on `/state`.** Everything that must outlive the container lives there:

| Path | Holds |
|------|-------|
| `/state/config.db` | Torrent list, and GoStorm settings when `StoreSettingsInJson` is off |
| `/state/settings.json` | GoStorm settings: peer port, cache size, connection limit |
| `/state/STATE/` | Inode map and sync caches |
| `/state/logs/` | Log files the Control Panel tails |
| `/state/blocklist` | The downloaded IP blocklist |

Mount a host directory there and everything survives `docker rm`. Without that volume the peer port, the torrent list and every other setting are discarded when the container is removed, and the next `docker run` starts from the defaults; the container prints a warning on startup when it detects this. `/state` is the default, so `TIRAMISU_ROOT_PATH` only needs setting to move the whole directory elsewhere, and `TIRAMISU_STATE_DIR` / `TIRAMISU_LOG_DIR` default to `STATE/` and `logs/` underneath it.

Do not mount a named or anonymous Docker volume expecting the warning to catch a mistake: an anonymous volume looks mounted, but a fresh `docker run` attaches a new empty one. Use a host path.

**Updating the container.** Unmount the virtual directory between removing the old container and starting the new one: Docker refuses to bind-mount over the FUSE mount the previous container left behind.

```bash
docker pull mrrobotogit/tiramisu:latest
docker rm -f tiramisu
sudo fusermount3 -uz /mnt/tiramisu-mkv-virtual
docker run -d ...   # same flags as above
```

> [!TIP]
> **Troubleshooting: real directory fills up, virtual stays empty.** If `docker logs` shows `FUSE mounted at ... all systems active` and the InodeMap saving files, but `/mnt/tiramisu-mkv-virtual` is empty on the host, the FUSE mount succeeded *inside* the container but never propagated out — Docker bind mounts default to private propagation, so a mount created inside the container isn't visible outside it. Fix: add `:rshared` to the virtual volume's `-v` flag (as above). If Docker then refuses to start with an error like *"must be shared or slave"*, the host mountpoint itself isn't shared yet - run `sudo mount --make-rshared /mnt` (or whichever parent directory holds it) once, then restart the container.

**Build from source** (from the repository root):

```bash
docker build -f docker/Dockerfile -t tiramisu .
```

### Windows Docker Installer

For Windows users, a dedicated installer generates a ready-to-use [Dockge](https://github.com/louislam/dockge) stack with Tiramisu + Plex **or** Jellyfin in a single container:

👉 **[docker-windows/](https://github.com/MrRobotoGit/tiramisu/tree/main/docker-windows)**

- Run `docker-windows\install-rebuild.bat` for interactive setup (flavor, paths, ports)
- Auto port conflict resolution
- Idempotent rebuild: never deletes existing media or config data

> **Requires:** Docker Desktop with WSL2 (Linux containers) and FUSE support (`/dev/fuse` available inside containers).

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

---

## FAQ

**What is Tiramisu?**
Tiramisu is a self-hosted BitTorrent engine with a built-in FUSE virtual filesystem. It presents torrents as normal, seekable `.mkv` files to Plex or Jellyfin, and streams the bytes live from the swarm as they are read, with no download step and no temp files.

**How is this different from Real-Debrid or other debrid services?**
Debrid services are third-party paid subscriptions that cache torrents on someone else's servers and hand you an HTTP link. Tiramisu runs entirely on hardware you own, no subscription, no third-party server, no monthly fee. You get the same "instant playback" experience, but the BitTorrent swarm itself is the source, not a rented cache.

**Does Tiramisu store or download the files?**
No persistent copy of the media is kept. Byte ranges pass through RAM (and an optional bounded SSD cache limited to the first/last few MB of each file, purely to speed up the first seconds of playback) and are never written to disk as complete files. There is no storage quota to plan around because there is nothing to store.

**Do I need to seed or keep uploading?**
Tiramisu participates in the swarm like any BitTorrent client while a file is open, but retention drops automatically once playback stops, and no long-term seeding is required or expected.

**What hardware do I need?**
A Raspberry Pi 4 (4 GB RAM) is the tested minimum baseline, and it comfortably handles 4K HDR streaming. Any `linux/amd64` or `linux/arm64` machine works: NAS, VPS, mini-PC, or any always-on Linux box.

**Does it work with Sonarr, Radarr, or other *Arr tools?**
Not directly. Tiramisu has its own built-in sync engine that talks to TMDB, Prowlarr, and Torrentio to discover and add titles automatically, so it does not need the *Arr suite to function. There is currently no compatibility layer for those tools specifically.

**Is this legal?**
Tiramisu is a general-purpose BitTorrent engine and streaming filesystem. It does not host, index, or distribute any copyrighted content itself; what you do with it is your responsibility, same as with any BitTorrent client.

**Is my data private?**
Everything runs on your own hardware. No account, no third-party API key is required for the core engine to work (TMDB, Prowlarr, and Plex/Jellyfin integrations are optional and use your own credentials).

---

## Troubleshooting

<details>
<summary><b>Plex shows buffering or "Playback Error"</b></summary>

Check warmup cache:
```bash
curl -s http://127.0.0.1:9080/metrics | jq '.read_ahead_active_bytes'
```

If empty, force a Plex/Jellyfin library scan. The scan reads the first MB of each file and populates the SSD head warmup.

Check active torrent status:
```bash
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"active"}' http://127.0.0.1:8090/torrents | \
  jq '.[] | {title: .title[:50], speed_mbps: ((.download_speed//0)/1048576|round), peers: (.active_peers//0)}'
```

If peer count < 3 and you are routing through WireGuard, check NAT-PMP configuration.

</details>

<details>
<summary><b>smbd D-state or Samba hangs during Plex/Jellyfin scan</b></summary>

Almost always one of three causes:

1. **`oplocks = no` missing** from `smb.conf`: kernel acquires exclusive locks on FUSE files, smbd blocks indefinitely
2. **`vfs objects = fileid` missing**: Synology receives truncated 32-bit inodes
3. **`serverino` dropped** on Synology CIFS mount: check with `mount | grep tiramisu`

Check for D-state processes:
```bash
ps aux | grep -E 'smbd|tiramisu|fuse' | awk '$8 == "D"'
```

</details>

<details>
<summary><b>Few seeders or slow downloads</b></summary>

If you are routing BitTorrent traffic through WireGuard, enable NAT-PMP to restore inbound port reachability through the VPN gateway:
```json
"natpmp": {
  "enabled": true,
  "gateway": "10.2.0.1",
  "vpn_interface": "wg0"
}
```

Without an open inbound port, peers cannot initiate connections: the engine
relies solely on outbound ones.

**If you are not using a VPN, forward the peer port on your router.** Tiramisu
asks the router for the mapping over UPnP on its own, but many routers ship with
UPnP disabled (and some ISP routers do not offer it at all). When that request
fails nothing is logged as an error: the port simply stays closed and peer
counts stay low.

1. Pin the port. In the Control Panel, set **GoStorm → `PeersListenPort`** to a
   fixed high port (e.g. `64407`). Left at `0` the engine picks a new random port
   on every start, which no static forward can follow.
2. Forward that same port to the machine running Tiramisu, **both TCP and UDP**.
3. Verify from outside your network that the port is open: a closed port and a
   working UPnP mapping look identical from the inside.

A router that drops the unforwarded traffic *and* logs every drop can also wear
out its own flash storage over time; if your router has a firewall logging
option, check it is not writing a line per dropped packet.

</details>

<details>
<summary><b>Service fails to start</b></summary>

```bash
sudo systemctl status tiramisu
tail -30 /home/pi/logs/tiramisu.log
```

If the FUSE mount is stale:
```bash
fusermount3 -uz /mnt/tiramisu-mkv-virtual
sudo systemctl start tiramisu
```

Ensure mount point exists:
```bash
sudo mkdir -p /mnt/tiramisu-mkv-virtual
sudo chown pi:pi /mnt/tiramisu-mkv-virtual
```

</details>

<details>
<summary><b>Webhook not triggering Priority Mode (Plex or Jellyfin)</b></summary>

Verify connectivity:
```bash
curl -v http://127.0.0.1:9080/plex/webhook \
  -X POST -H 'Content-Type: application/json' \
  -d '{"event":"media.play","Metadata":{"librarySectionType":"movie","guid":"imdb://tt1234567","Guid":[{"id":"imdb://tt1234567"}]}}'
```

Check logs:
```bash
grep -i webhook /home/pi/logs/tiramisu.log | tail -20
```

If the connection appears in logs but IMDB matching fails, verify the raw payload contains `imdb://tt\d+`. Tiramisu uses regex on the raw JSON string, so it works even with localized titles.

**Jellyfin-specific**: if you see no log entry at all, confirm the `Content-Type` header is set correctly (Key: `Content-Type`, Value: `application/json`). Jellyfin does not send the request when the Content-Type override is malformed.

</details>

<details>
<summary><b>High CPU usage</b></summary>

Profile the live binary:
```bash
go tool pprof -top "http://127.0.0.1:9080/debug/pprof/profile?seconds=30"
```

Expected hot paths: `sha1.blockGeneric` (no crypto extensions on Pi 4 A72), `io.ReadFull`, `sync.(*Mutex).Lock`. Regenerating the PGO profile typically reduces CPU 5–7%.

</details>

---

## Key File Locations

Paths below use the defaults set by `install.sh`. All are configurable during installation.

**Runtime (install directory, default `~/Tiramisu/`)**

| Path | Purpose |
|------|---------|
| `~/Tiramisu/tiramisu` | Production binary |
| `~/Tiramisu/config.json` | Live configuration (edit → `sudo systemctl restart tiramisu`) |
| `~/Tiramisu/STATE/tiramisu.db` | SQLite state database: inode map, sync caches, episode registry, scheduler state |
| `~/Tiramisu/logs/` | All service logs (`tiramisu.log`, `movies-sync.log`, `tv-sync.log`, `watchlist-sync.log`) |
| `/mnt/tiramisu-mkv-virtual/` | FUSE mount point (served to Plex/Jellyfin -> Samba) |
| `/etc/systemd/system/tiramisu.service` | systemd service definition |

**Build (cloned repository)**

| Path | Purpose |
|------|---------|
| `~/Tiramisu/` | Install directory: clone, build and runtime |
| `~/Tiramisu/default.pgo` | PGO profile, regenerate after major code changes |

---

## Support

Tiramisu is free and open source. If it's saving you a monthly chip, consider fueling the engine!

[![](https://img.shields.io/static/v1?label=DONATE!&message=%E2%9D%A4&logo=GitHub&color=%23fe8ebb)](https://github.com/sponsors/MrRobotoGit)

---

## License

GNU General Public License v3.0
