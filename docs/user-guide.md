# User Guide

First-run steps, the Control Panel and the Dashboard.

[← Back to the README](../README.md) · [Documentation index](README.md)

---

## Getting Started

> [!CAUTION]
> **Attention:** After the first run of the Movie and TV sync engine, hundreds of `.mkv` files will be created. Plex/Jellyfin will then begin scanning these files to populate its UI. During this initial scan, playback will be extremely slow or may time out. This is because the media server opens and closes files hundreds of times during analysis, which congests the BitTorrent engine. Please wait for Plex/Jellyfin to complete its scan before starting any streams.


<details>
<summary><b>Step 1: Configure the Webhook (Plex or Jellyfin)</b></summary>

Required for Priority Mode (bitrate boost during playback), fast-drop on stop, and IMDB-ID-based file matching.

**Plex**: Settings → Webhooks → Add Webhook:
```
http://<tiramisu-host>:9080/plex/webhook
```

**Jellyfin**: Install the Webhook plugin, add one webhook (events: `PlaybackStart`, `PlaybackStop`, `PlaybackProgress`):
- URL: `http://<tiramisu-host>:9080/plex/webhook` (keep `http://` even with [HTTPS](api.md#https) on: webhooks stay on plain HTTP)
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
curl -X POST http://<tiramisu-host>:9080/plex/webhook \
  -H 'Content-Type: application/json' \
  -d '{"event":"media.play"}'
```

</details>

<details>
<summary><b>Step 2: Configure the Plex/Jellyfin Library</b></summary>

Add a Movies library in Plex/Jellyfin pointing to the Samba share:
```
smb://<tiramisu-host>/tiramisu-mkv-virtual/movies
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
Trigger from the Control Panel (`:9080/control`) → Scheduler → Movies → "Run now", or via API:
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
7. Playback begins (0.1–0.5 s with warmup, 2–4 s cold)
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

Trigger manually from the Control Panel (`:9080/control`) → Scheduler → Watchlist → "Run now", or via API:
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
open http://<tiramisu-host>:9080/control

# Dashboard: live swarm, streams, services, scheduler, logs
open http://<tiramisu-host>:9080/dashboard

# Raw metrics (JSON)
curl -s http://<tiramisu-host>:9080/metrics | python3 -m json.tool

# Scheduler status
curl -s http://<tiramisu-host>:9080/api/scheduler/status | jq

# Live log key events only
ssh pi@<tiramisu-host> "tail -f /home/pi/Tiramisu/logs/tiramisu.log | grep -E '(OPEN|NATIVE|Interrupt|Jump|DiskWarmup|Emergency)'"

# Active torrents in RAM with speed (the GoStorm API answers on the host only)
ssh pi@<tiramisu-host> "curl -s -X POST -H 'Content-Type: application/json' \
  -d '{\"action\":\"active\"}' http://127.0.0.1:8090/torrents" | \
  jq '.[] | {title: .title[:60], speed_mbps: ((.download_speed//0)/1048576|round), peers: (.active_peers//0)}'
```

</details>

<details>
<summary><b>Step 8: Configure the MKV Creation Scheduler</b></summary>

Tiramisu includes a built-in scheduler that replaces system cron. Configure it from the Control Panel at `:9080/control` → **Scheduler** section.

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

## Control Panel

The Control Panel is where you tune the engine, point it at your paths and
credentials, run a sync by hand and restart the service, all without editing a
file or opening an SSH session. It is a web UI **embedded in the Tiramisu
binary**: no additional server, no React build step, no external dependencies.
Served at `:9080/control`.

```
http://<your-pi-ip>:9080/control
```

With [HTTPS](api.md#https) on, the same address moves to `https://<your-pi-ip>:9080/control`.

### Layout

Eight sections, one per page, listed in the left rail: **Streaming & FUSE**, **Network & VPN**,
**Sources & Media**, **Engine**, **Release Selection**, **Scheduler**, **Access** and **History**. Each section says when
its settings take effect: on restart (Tiramisu settings, saved in the state database) or live
(GoStorm engine settings, pushed through its API). Find any setting with <kbd>⌘K</kbd>.

- **Simple / Advanced**: Simple shows the settings you change most often; Advanced shows everything, with expert cards marked as such.
- **Change bar**: every edit is collected at the bottom of the page with its old and new value. **Apply** saves them all (live ones take effect at once) and becomes **Apply & restart** when one of them needs a restart; **Discard** reverts. Leaving the page with pending changes asks first.

| Section | Settings |
|---------|----------|
| **Streaming & FUSE** | FUSE Cache Budget, Master Concurrency, Streaming Threshold; Physical Source Path, FUSE Mount Path; FUSE Block, Attr/Entry Timeout; Metadata Cache, Max Cache Entries, Cleanup Interval |
| **Network & VPN** | NAT-PMP (enable, Gateway IP, VPN Interface, Local Port, Refresh/Lifetime); GoStorm URL, Rescue Grace/Cooldown, Metrics Port, Proxy Port, Log Level; IP BlockList (enable, URL, filter) |
| **Sources & Media** | Prowlarr URL and API key, Torrentio URL; TMDB API key; media server type (Plex or Jellyfin), URL, token, Movies / TV Series / Music library IDs |
| **Engine** *(live)* | Cache Size, Readahead Cache; Warmup Cache, path and SSD quota; Connections Limit, DL/UP Rate, Disconnect Timeout; Listen Port, Retrackers Mode, IPv6/DHT/PEX/TCP/uTP/Upload/Force Encrypt; Smart Responsive Mode, Adaptive Shield, Debug Log |
| **Release Selection** | Preferred and excluded language flags, preferred terms; quality scoring preset and bonuses for Movies and TV Series, with a preview of what wins right now |
| **Scheduler** | Movies and TV sync days and start time; Watchlist sync interval (Music keeps its own schedule, `scheduler.music_sync`) |
| **Access** *(live)* | Optional sign-in for `:9080` (one user, password), API tokens for agents and scripts, browser sessions, and HTTPS *(restart)*: a guided card to turn it on, download and trust the CA, and move clients to `https://`. See [Sign-in and API tokens](api.md#sign-in-and-api-tokens) and [HTTPS](api.md#https) |
| **History** *(restart)* | The last 50 saved versions of the configuration and, marked *engine*, the last 50 manual saves of the Engine settings: who saved each one and what it changed (keys and tokens only as changed); filter by restart, secrets or Engine, and restore any version through the change bar: the newer ones stay in the list. An Engine restore applies at once and restarts the BitTorrent client, so playback in progress stops |

Keys and tokens (media server token, TMDB and Prowlarr API keys) are write-only: the
Control Panel shows whether one is set, never its value. Leave the field empty to keep it.

---

## Dashboard

Embedded Go service at port **`:9080`**. Real-time operational view of the entire
stack: one page that answers "is it healthy right now, and what is it doing",
without reading a log.

```
http://<your-pi-ip>:9080/dashboard
```

With [HTTPS](api.md#https) on: `https://<your-pi-ip>:9080/dashboard`.

### Live Swarm

The default view of the speed panel (switch to **Timeline** for the classic chart).
One graph from left to right: **peers → torrents → piece cache → FUSE buffer → clients**.

- **Peers**: seeders, leechers, uTP and half-open connections; newcomers pulse in, departures fade out; banned peers are marked with a cross.
- **Torrents**: pipe width follows the download speed; a torrent that is downloading pulses, an idle one stays still.
- **FUSE buffer**: the pipe between cache and FUSE works as a level meter of the read-ahead buffer (active / stale).
- **Clients**: the device that is watching, from Plex or Jellyfin sessions (`Apple TV · Plex`), or the SMB client address when the player reads the share directly.
- **Adaptive Shield**: a hash failure travels down the affected pipe; in STRICT the live pipes turn orange with a clean-streak ring on GoStorm, and the colour drains toward the clients when FAST mode returns.

### Status Grid

Six tiles:

| Card | What it shows |
|------|--------------| 
| **GOSTORM** | API ping latency (green = responding). |
| **FUSE** | Number of virtual `.mkv` files currently exposed. |
| **VPN** | WireGuard interface status: VPN IP and gateway. |
| **NAT-PMP** | Active external port assigned by VPN gateway. |
| **MEDIA SERVER** | Plex and/or Jellyfin reachability and latency. |
| **SYSTEM** | CPU %, RAM %, free disk space, live via gopsutil. |

### Download Speed Graph

**Rolling chart** (last 5 minutes) of GoStorm download speed in Mbps, sampled every 5 seconds, with the Adaptive Shield timeline underneath.

### Active Streams

Every torrent in use, with poster, title and year, hash and size, quality chips (`4K`/`1080p`,
`DV`/`HDR`, audio codec and channels, `PRIORITY`), current speed and peer/seed count, and a ✕
to stop the stream.

### Sync Controls

The **Sync scheduler** card runs or stops each sync by hand (Movies, TV, Watchlist, Music) and shows its last result and next run; the **Logs** card next to it tails `tiramisu`, `movies`, `tv`, `watchlist` and `music` logs.

### MKV Creation Scheduler

Built into the Go binary. Configured from the **Scheduler** section of the Control Panel:
- **Movies Library Sync**: select weekdays + start time
- **TV Series Libraries Sync**: select weekdays + start time
- **Plex Watchlist Sync**: interval-based (1 h, 2 h … 24 h)

Last/next run times and live status are shown in the card. State persists across restarts in `STATE/tiramisu.db`.
