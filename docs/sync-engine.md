# Sync Engine

How the built-in sync keeps the movie, TV and music libraries up to date, and how Prowlarr fits in.

[← Back to the README](../README.md) · [Documentation index](README.md)

---

## Overview

All sync logic runs natively inside the Go binary. No Python, no external scripts, no subprocess overhead. Configure schedules from the Control Panel at `:9080/control` → **Scheduler**.

### Movies Sync

Queries TMDB Discover (English and Italian), Now Playing (US and GB), Popular (US) and Trending, evaluates Prowlarr/Torrentio results, adds the best torrent.

Trigger from Control Panel → Scheduler → Movies → "Run now", or via API:
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
(see [Episode gaps](api.md#episode-gaps-v1971) in the Library API).

### Watchlist Sync

```bash
curl -X POST http://127.0.0.1:9080/api/scheduler/watchlist/run
```

Reads Plex cloud watchlist → IMDB ID resolution → Prowlarr/Torrentio (min 15 seeders, the movie profile) → GoStorm.

### Music Sync

```bash
curl -X POST http://127.0.0.1:9080/api/scheduler/music/run
```

Weekly, Sunday 15:00 by default (`scheduler.music_sync`). Every album is filed
through the Library API as FLAC projections, one per track. Three passes:

- **New albums of your artists.** Every artist of Tiramisu's music library with a
  MusicBrainz id is followed: albums and EPs whose first edition came out in the
  last `window_days` (30) and that the library lacks are imported, with no cap.
  Live, compilation and remix groups, reissues, undated and future releases never
  qualify. An album with no torrent yet is retried on every run while it stays in
  the window. MusicBrainz is asked in batches of 40 artists, so 800 artists cost
  about 20 requests.
- **New artists close to yours.** The sitewide ListenBrainz fresh-releases feed is
  filtered down to artists you do not have whose nearest artists on ListenBrainz
  include yours, and whose first album or EP is at most `debut_years` (3) old;
  more of your artists among the neighbours ranks higher. Only about a third of new
  artists have similarity data yet, so expect a handful a month. The first run
  looks up every artist of the window (about an hour for 30 days); the answers are
  cached in the discovery state for 30 days, so later runs look up only the week's
  new names.
- **Niche artists of your genres (Plex only).** The genres of your seed artists
  (below) pick the pools: the least played recordings of each genre on ListenBrainz,
  resolved to artists you do not have. An artist is kept when one of your artists is
  among its nearest neighbours and its first album or EP is at most `debut_years`
  (15) old; one of your seeds among the neighbours ranks it highest. Its latest
  studio album already released is imported.
- **Similar artists (Plex only).** The Deezer related artists of your seed artists
  (public endpoints, no key), pooled and ranked by how many of your seeds point at
  them. Artists any library already holds, artists outside `min_fans`-`max_fans`
  (10,000-100,000) Deezer fans and artists whose first album or EP is older than
  `debut_years` (15) are dropped; the latest studio album comes from MusicBrainz.

The seeds come from the Plex server owner's plays of the last 60 days (the users the
server is shared with do not count), weighted by age: a play of
the last day counts 1, of the last week 0.5, of the last month 0.25, of the last two
months 0.125. With no plays in 60 days the genre and similar-artist passes find
nothing.

The last three passes share `max_albums_per_run` (20), in order new artists, genres,
similar artists, one album per artist.

The new-album pass reads only Tiramisu's own music library, for the artists and
for the duplicate check: on Plex the section in `plex.music_library_id` (0 turns
the pass off), on Jellyfin the music library that holds the albums Tiramisu filed.
Jellyfin keeps no play history, so there the genre and similar-artist passes are
skipped and the other two run. It reads `/Library/VirtualFolders` and `/Items` with the `Authorization: MediaBrowser`
header, which Jellyfin 10.x and 12.x both accept, and needs MusicBrainz ids on the
albums (tagged files, or the bundled MusicBrainz metadata provider).

### Prowlarr Integration

To ensure the system remains functional even when public aggregators like Torrentio are down, Tiramisu includes a **Prowlarr Adapter**. This allows you to use your own self-hosted Prowlarr instance as the primary source for torrents.

The sync engine implements a **Strict Fallback** logic: it first queries your Prowlarr indexers using IMDB IDs for maximum precision. If no results are found locally, it automatically falls back to Torrentio.

**Configuration** is done via `config.json` (or the Control Panel → **Prowlarr Indexer** section):

```json
"prowlarr": {
  "enabled": true,
  "api_key": "your-api-key",
  "url": "http://<prowlarr-host>:9696"
}
```
