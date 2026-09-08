---
name: tiramisu-manual-content-add
description: "Use to add a specific movie/TV release to Tiramisu by hand."
version: 3.0.0
metadata:
  hermes:
    tags: [tiramisu, torrent, manual-add, mkv, library, plex, jellyfin, prowlarr]
    related_skills: [tiramisu-development]
---

# Tiramisu Manual Content Addition

How to add a specific movie or TV series to a Tiramisu library when the automated
indexer sync missed it, replicating the same quality-scoring and virtual-file logic
the sync engine uses. Deployment, operation and debugging live in
`tiramisu-development`; this skill only covers getting one release into the library.

This skill is a SINGLE self-contained file: the helper scripts are embedded in
section [Helper scripts](#helper-scripts) below. They are NOT stored as files.
On first execution, materialize them (section [Materialize the scripts](#0-materialize-the-scripts)),
then run. No host, IP or secret is embedded anywhere: fill the placeholders per
deployment, never hardcode.

Environment placeholders:

- `API` = Tiramisu engine API base URL, default `http://127.0.0.1:8090` on the
  Tiramisu host (also overridable via `TIRAMISU_API` env var in the scripts)
- `CTRL` = Tiramisu control/metrics base URL, default `http://127.0.0.1:9080`
  (env var `TIRAMISU_CTRL`). This is a DIFFERENT port from `API` and is where
  the running configuration is read from.
- `LIB` = library root containing the `movies/` and `tv/` directories

The engine API is the GoStorm-compatible torrents API (route `POST /torrents`,
actions `add | get | set | rem | list | active | drop | wipe`).

## Core model

Tiramisu libraries are virtual: each file Plex/Jellyfin sees is a small JSON stub
on the real filesystem, exposed by the FUSE layer with the declared full size.

1. **Read the deployment's own scoring profile** from the config API
2. **Add** the torrent to the engine API so metadata resolves
3. **Poll** until the engine returns the file list (id, path, length per file)
4. **Write one JSON stub per video file** into the library
5. The FUSE layer presents each stub as a full-size virtual file

## Engine API: add the torrent

`POST {API}/torrents` with JSON body:

```json
{"action": "add", "link": "<magnet-or-url>", "title": "<title>", "save_to_db": true}
```

- `link` is required and may be a magnet or a torrent URL / base62 torrs link
- **The persistence flag is `save_to_db`, NOT `save`** (the struct tag in current
  code is `json:"save_to_db,omitempty"`)
- The response is the torrent status at that instant, with `stat` and
  `stat_string`. `stat` is an enum: `0` added, `1` getting info, `2` preload,
  `3` working, `4` closed, `5` in db. A successful add typically returns `0`
  or `1` — **do not treat `0` as a failure**
- `save_to_db` is honoured only AFTER metadata resolves, inside a background
  goroutine. If metadata never resolves, the torrent is never persisted

Pitfall: magnets contain `&`, which breaks shell quoting. Use the helper
script `add_torrent.py`, never inline curl with the raw magnet in a shell string.

## Engine API: poll for metadata

Poll `{"action": "get", "hash": "<hash>"}` with increasing sleeps until files
appear: ~12s for 1080p, ~45s for 4K, longer on cold swarms.

**Where the file list lives.** The engine publishes it at the TOP LEVEL of the
response as `file_stats` (`json:"file_stats,omitempty"`), each entry carrying
`id`, `path`, `length`. It is omitted while the torrent is not hydrated, which
is exactly what "metadata not resolved yet" looks like.

Do NOT rely on the `data` field for this. `data` is a free-form string supplied
by whoever added the torrent (it is passed straight through from the add
request). Tiramisu's own sync engine stores a `{"GoStorm":{"Files":[...]}}`
document there, so torrents created by the sync do carry a file list in `data` —
but a torrent added by hand through this skill does not, because the add sends
no `data`. Read `file_stats` first and fall back to parsing `data` only for
sync-created entries.

Two more quirks worth knowing:

- `{"action": "list"}` returns an ARRAY of torrent objects; `get` returns a
  single object. Handle both when scripting
- `get` uses a peek that deliberately does NOT renew the torrent's expiry
  timer, so polling alone will not keep an idle torrent alive

## Library layout (tree)

```
{LIB}/
├── movies/
│   └── <Title>_<Year>_<Resolution>[_<tags>]_<HASH8>.mkv     (flat, imdb set)
└── tv/
    └── <Series_Name> (<Year>)/
        └── Season.NN/
            └── <Series>_S<NN>E<XX>_<HASH8>.mkv       (nested, imdb EMPTY)
```

- **Movies**: flat in `movies/`, filename `Title_Year_Resolution[_DV|_HDR][_Atmos|_5.1][_REMUX]_HASH8.mkv`, JSON stub carries the IMDB id (e.g. `tt0088196`)
- **TV**: nested `<LIB>/tv/<Series_Name> (<Year>)/Season.NN/...`, series name with
  underscores and year in parentheses (e.g. `Alley_Cats (2026)`), stub has EMPTY
  `imdb` field (`""`) as TV convention
- **HASH8**: last 8 hex chars of the info hash, LOWERCASE, in both the filename
  and the stream URL

Stub JSON shape (same for movies and TV):

```json
{
  "url": "{API}/stream?link=<hash_lowercase>&index=<file_id>&play",
  "size": <size_in_bytes>,
  "magnet": "magnet:?xt=urn:btih:<hash_lowercase>&dn=<name>&tr=<tracker>&tr=...",
  "imdb": "tt0000000"
}
```

The `size` MUST equal the `length` the engine reports for that exact file id;
the FUSE layer enforces reads against it. **File ids are 1-based** — the engine
treats id 0 as undefined, so never pass `index=0`.

Copy the tracker list from an existing stub in the same library rather than
inventing one: the set is a deployment detail and the engine's own default list
can change between versions.

## Search for candidates

Search by **IMDB id**, not by free text. Both indexers Tiramisu uses are keyed
on it, and a title search is what makes generic one-word show names collect
unrelated releases.

### Prowlarr, through Tiramisu

Do not call Prowlarr directly and do not go looking for its API key: Tiramisu
already holds the credentials from its own config and exposes a search endpoint
that uses them.

```bash
curl -s "{CTRL}/api/prowlarr/search?imdb_id=tt1234567&type=movie&title=Some%20Title&year=2024"
curl -s "{CTRL}/api/prowlarr/search?imdb_id=tt1234567&type=series&title=Some%20Show"
```

- `imdb_id` is required, everything else is optional
- `type` is `movie` (the default) or `series`
- `title` and `year` are a secondary query for indexers that have no real IMDB
  search. Pass `year` for movies; for a series it would be the year of season 1
  and only hurts
- the response is a JSON array of `{name, title, infoHash, behaviorHints}`
- **an empty array `[]` with status 200 means Prowlarr is not configured on this
  deployment**, not that the release does not exist. Fall through to Torrentio

`title` is a multi-line string, and the extra lines are where seeders and size
live. Scoring reads the whole thing, so keep it intact rather than splitting off
the first line:

```
Brazil 1985 DC 4K HDR DV 2160p BDRemux Ita Eng x265 NAHOM
👤 2 ⬇️ 10
💾 83.24GB
```

Seeders are the number after the 👤 emoji (the engine matches exactly that).
`name` is not the release name: it carries the indexer and a resolution tag,
for example `Torrentio\n4k`.

### Torrentio, directly

Torrentio needs no credentials. Take the base URL from the config
(`torrentio_url`, default `https://torrentio.strem.fun`) and query by IMDB id:

```bash
curl -s "{TORRENTIO}/{config}/stream/movie/tt1234567.json"
curl -s "{TORRENTIO}/{config}/stream/series/tt1234567:2:5.json"    # season 2, episode 5
```

The `{config}` segment is the filter string the sync engine uses,
`sort=qualitysize|qualityfilter=480p,720p,scr,cam`.

### How the sync engine combines them

Worth mirroring, because it is not a fallback chain: the engine queries
**Prowlarr and Torrentio both**, concatenates the results, deduplicates by info
hash, and only then filters and scores. A search counts as failed only when
**every** indexer failed; if Prowlarr is simply not configured, that is not a
failure and Torrentio alone carries the run.

Finally, prefer a full H.264 season pack over per-episode x265 releases when the
client cannot decode HEVC natively (no-transcode playback).

## Score candidates

**Never hardcode weights.** Scoring is per-deployment configuration: the engine
reads the `quality_scoring` block of its config, and any value may have been
tuned by the operator. Fetch the live profile first:

```bash
python3 get_scoring.py            # prints the movie + TV profile in use
```

When `quality_scoring` is absent from the config, no profile was configured and
the engine's own built-in defaults apply; the script says so explicitly rather
than guessing numbers.

### How the score is composed

The shape of the formula is stable even though the numbers are not. For a movie
candidate, from the release title plus its seeders and size:

1. resolution: `res_4k` if the release is 4K, otherwise `res_1080p`
2. dynamic range: `dolby_vision` **or** `hdr` — mutually exclusive, DV wins
3. audio: `atmos` **or** `audio_5_1` **or** `stereo_penalty` — first match only,
   and `stereo_penalty` is negative
4. `remux` if the release is a remux
5. `preferred_language` if the title matches the configured preferred terms
   (`language.preferred_terms` in the same config)
6. `unknown_size_4k_penalty` when the indexer reported no size and the release
   is 4K
7. seeders, capped at `seeder_cap`

Note steps 2 and 3: they are **either/or**, not additive. A DV+HDR release does
not collect both bonuses.

### Rejection gates, in the order the engine applies them

1. **garbage** release tags: camrip, hdcam, hdts, telesync, TS, telecine, TC,
   SCR, screener, webscreener
2. **excluded language** matched in the title, from `language.excluded_flags`
3. **blacklisted title**, then **blacklisted hash**, if the deployment keeps a
   blacklist
4. **seeders below `min_seeders`**
5. **resolution neither 4K nor 1080p** — anything else is rejected outright,
   there is no 720p path
6. **size out of band**, and the two resolutions differ here: a 4K release with
   an unknown size (0) is ACCEPTED and merely takes `unknown_size_4k_penalty`,
   while a 1080p release with an unknown size is REJECTED
7. **final score <= 0**

The size bands are calibrated for 2h+ features: for shorter content (<=2h docs,
live shows) they may reject legitimate encodes, so relax the band manually for
those and say so when reporting what was chosen.

### TV scores differently

TV reads `quality_scoring.tv` and the formula is NOT the movie one with other
numbers. Differences that change which release wins:

1. resolution: `res_4k` or `res_1080p` — and if the release is neither, the
   score is **0 and the candidate is dropped**, there is no partial credit
2. `dolby_vision` **or** `hdr`
3. `atmos` **or** `audio_5_1` — **no stereo penalty at all**, unlike movies
4. `preferred_language`
5. seeders are **tiered, not capped**: >=100 adds `seeder_tier_100`, >=50 adds
   `seeder_tier_50`, >=20 adds `seeder_tier_20`, below 20 adds nothing
6. there is **no remux weight and no size band** for TV

Separately from the score, season packs get a priority bonus: a full pack adds
`fullpack`, a partial range (`E01-E06`) adds **half** of it.

TV gates: score 0 drops the candidate; the seeder minimum is `min_seeders_4k`
for 4K releases and `min_seeders` otherwise — **two different thresholds**;
excluded language in the title drops it. A season whose already-present episodes
average at or above `season_skip_score` is skipped entirely.

## Worked procedure

### 0. Materialize the scripts

Write the five files from [Helper scripts](#helper-scripts) into a local
working directory (or on the Tiramisu host), e.g. `/tmp/tiramisu-add/`. Verify
with `python3 -m py_compile <file>.py`. Then run them from there.

### 1. Read the deployment's scoring profile

```bash
python3 get_scoring.py
```

### 2. Add the torrent

```bash
python3 add_torrent.py "magnet:?xt=urn:btih:<HASH>&dn=<name>&tr=udp://tracker.opentrackr.org:1337" "Title Year"
```

### 3. Poll until files resolve

```bash
python3 list_torrent_files.py "<HASH>"
```

Repeat with short sleeps until it prints the video files. For TV, print the
FULL paths: file ids are NOT guaranteed to follow episode order, map id ->
SxxEyy from the filenames (never assume id 1 = E01), ignore .nfo/.txt noise.

### 4. Check it is not already there

Two different questions, two different sources. They disagree in normal
operation, so check both.

**Does the library already show it?** This is the one that matters, because the
stub files are what Plex and Jellyfin actually see. Read the filesystem:

```bash
ls "$LIB/movies" | grep -i '<title fragment>'          # same title, any release
find "$LIB/tv" -ipath '*<Series>*Season.02*' | head    # season already filled?
grep -rl '<hash8>' "$LIB" | head                       # this exact release
```

**Does the engine already know the hash?** One call, no filesystem walk:

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"list"}' "$API/torrents" | \
  python3 -c 'import sys,json; print([t["title"] for t in json.load(sys.stdin) if t["hash"].lower().endswith("<hash8lower>")])'
```

Note `endswith`: HASH8 is the **last** 8 characters of the info hash, which is
also why it is what appears in the filenames.

Do not treat the two as interchangeable. The engine DB and the library drift
apart in normal use: a stub can outlive the torrent entry, and a registered
torrent can have no stub at all. On a mature deployment the engine listed 4077
torrents against 5216 stub files. Only the filesystem answers "will this look
like a duplicate in Plex".

`action=list` returns every torrent in one response, which on such a deployment
is several megabytes. Filter it in the pipe, never print it whole.

A hit on the exact release means there is nothing to do. A hit on the title with
a different release is a decision for the user, not for the skill: two stubs for
the same title show up as duplicates in the media server.

**Do not use `{CTRL}/api/torrents` for this.** It is the dashboard view and
returns only what is streaming right now, so it will report nothing for a
library of thousands.

### 5. Pick the target file(s)

Video extensions: .mkv, .mp4, .avi, .mov, .m4v. Pick the largest within the
size band from the profile; skip featurettes/extras.

### 6. Write one stub per video file

Movie (flat + imdb):

```bash
python3 create_mkv.py "$LIB" "movies/Title_2024_1080p_<HASH8>.mkv" "<HASH>" 3 4688122884 tt1234567
```

TV, one episode at a time (nested + empty imdb):

```bash
python3 create_mkv.py "$LIB" "tv/Series_Name (2024)/Season.01/Series_S01E01_<HASH8>.mkv" "<HASH>" 1 922746880 ""
```

TV, a whole season pack in one pass — reads the file list, takes SxxEyy from
each filename and writes every episode. Dry run first, then `--write`:

```bash
python3 create_tv_season.py "$LIB" "Series Name" 2024 "<HASH>" "<trackers>"
python3 create_tv_season.py "$LIB" "Series Name" 2024 "<HASH>" "<trackers>" --write
```

Files with no recognisable SxxEyy are skipped and listed at the end rather than
guessed at, so specials and extras never get filed as episodes.

Copy scripts to the host and run there rather than through ssh heredocs:
heredocs containing single quotes corrupt the script silently (runtime
NameError on a legitimate `r.get('key')`).

### 7. Verify on both layers

The stub is a small JSON file on disk, and the same path seen through the FUSE
mount must report the declared size. Compare the two:

```bash
stat -c '%s  %n' "$LIB/movies/Title_2024_1080p_a1b2c3d4.mkv"          # ~700 B
stat -c '%s  %n' "$FUSE/movies/Title_2024_1080p_a1b2c3d4.mkv"          # full size
```

`$FUSE` is the mount point, a different path from `$LIB`. If the FUSE side
reports ~700 B too, the file is being read as a plain file and the mount is not
covering that path. If it is missing entirely, the engine has not registered
the stub yet.

Then trigger a library scan on Plex/Jellyfin for the movies or TV folder.

### 8. Undo, if the stub was wrong

Deleting the stub is not enough: the torrent stays registered in the engine.

```bash
rm "$LIB/<path to the stub>.mkv"
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"rem","hash":"<hash>"}' "$API/torrents"
```

Then rescan the library so the media server drops the entry. Only remove a hash
this skill added: `rem` on a hash the automated pipeline manages will make the
next sync re-add it, or leave an orphaned stub behind.

## Fast-track (skip the engine API)

If adding the magnet hangs or the API is unresponsive, create the stub directly
from the release page: take the info hash (40-char hex), the target file index
and size from the file list, and write the JSON stub with the standard stream
URL pattern. Metadata resolves lazily: the first client that streams the file
triggers the engine to fetch it.

## Troubleshooting

- **Add connects but never responds**: engine stuck resolving metadata; restart
  the Tiramisu service, wait for startup, retry the add
- **`file_stats` never appears**: metadata has not resolved. Check the torrent
  has peers; a dead swarm never produces a file list
- **Torrent in engine but invisible to the media server**: it never resolved
  metadata (idle), or the media server needs a library scan
- **Wrong size / missing file**: the stub declared size must equal the file
  `length` returned by the engine for that exact file id
- **Stream URL case**: the engine accepts hash in either case, but existing
  stubs use lowercase in `link=`. Stay lowercase for consistency
- **list returns an array**: `action=list` responds with a top-level JSON array;
  `action=get` with a single object. Handle both when scripting

## Do NOT

- Hardcode scoring weights, size bands or seeder minimums: read them from the
  deployment's configuration
- Print or paste the full config response: it contains API keys and tokens in
  cleartext. Extract only the scoring block
- Create stubs for content already present in the library
- Modify files that the automated pipeline manages
- Guess the naming convention: read an existing stub first and copy it
- Run the whole flow when only a verification was asked (check, don't add)

## Helper scripts

The five scripts below are embedded here as the single source of truth.
Materialize them into a working directory when first needed (see step 0), do
not keep permanent copies elsewhere. All read the API base from env vars or a
positional arg; no IP or secret is hardcoded.

### get_scoring.py

```python
#!/usr/bin/env python3
"""Print the scoring profile the running Tiramisu actually uses.

Usage:
    python3 get_scoring.py [ctrl_base]

ctrl_base defaults to http://127.0.0.1:9080 (the control/metrics port, NOT the
engine API port), overridable with TIRAMISU_CTRL.

Weights are per-deployment configuration and may have been tuned by the
operator, so they must be read, never assumed. When the config carries no
quality_scoring block the engine falls back to its own built-in profile and
this script says so instead of inventing numbers.

SECURITY: the config response also contains API keys and tokens in cleartext.
This script extracts only the scoring block and the preferred-language terms;
do not dump the whole response anywhere.
"""
import json
import os
import sys
import urllib.request


def main():
    ctrl = os.environ.get("TIRAMISU_CTRL") or (
        sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:9080"
    )
    with urllib.request.urlopen(ctrl + "/api/config", timeout=15) as r:
        cfg = json.loads(r.read())

    q = cfg.get("quality_scoring") or {}
    lang = (cfg.get("language") or {}).get("preferred_terms")

    if not q:
        print("no quality_scoring block configured -> engine built-in profile applies")
        print("read the defaults from the running code, do not guess them")
        return 1

    for profile in ("movies", "tv"):
        w = q.get(profile)
        print(f"--- {profile} ---")
        if not w:
            print("  (not configured, built-in profile applies)")
            continue
        for k in sorted(w):
            print(f"  {k:26} {w[k]}")
    if lang:
        print(f"--- preferred language terms ---\n  {lang}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
```

### add_torrent.py

```python
#!/usr/bin/env python3
"""Add a magnet (or .torrent) to the Tiramisu engine (GoStorm API).

Usage:
    python3 add_torrent.py "<magnet-or-url>" "<optional title>" [api_base]

api_base defaults to http://127.0.0.1:8090 (run on the Tiramisu host, or
override with the third positional / TIRAMISU_API env var).

Fields: action, link, title, save_to_db.
NOTE: the JSON field is save_to_db, NOT save.
NOTE: save_to_db is applied only after metadata resolves, in a background
goroutine. A torrent whose metadata never resolves is never persisted.
"""
import json
import os
import sys
import urllib.error
import urllib.request

# 0 added, 1 getting info, 2 preload, 3 working, 5 in db. 4 is closed.
OK_STATS = (0, 1, 2, 3, 5)


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)
    link = sys.argv[1]
    title = sys.argv[2] if len(sys.argv) > 2 else ""
    api = os.environ.get("TIRAMISU_API") or (
        sys.argv[3] if len(sys.argv) > 3 else "http://127.0.0.1:8090"
    )

    payload = json.dumps(
        {"action": "add", "link": link, "title": title, "save_to_db": True}
    ).encode()
    req = urllib.request.Request(
        api + "/torrents",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    # add can take a while on cold swarms: keep a generous timeout
    try:
        resp = json.loads(urllib.request.urlopen(req, timeout=120).read())
    except urllib.error.HTTPError as e:
        print(f"engine refused the add: HTTP {e.code} {e.reason}")
        return 2
    if isinstance(resp, list):
        resp = resp[0] if resp else {}
    stat = resp.get("stat")
    print("stat:", stat, resp.get("stat_string"))
    print("hash:", resp.get("hash") or resp.get("torrs_hash"))
    return 0 if stat in OK_STATS else 1


if __name__ == "__main__":
    sys.exit(main())
```

### list_torrent_files.py

```python
#!/usr/bin/env python3
"""List files of one torrent once metadata resolves (GoStorm API).

Usage:
    python3 list_torrent_files.py "<hash>" [api_base]

Prints id / length / full path per video file.

The engine publishes its file list at the TOP LEVEL of the response, in
"file_stats" (omitted while the torrent is not hydrated). The "data" field is
a free-form string echoed back from whoever added the torrent: Tiramisu's own
sync writes {"GoStorm":{"Files":[...]}} there, but a hand-added torrent has it
empty. So: file_stats first, data only as a fallback for sync-created entries.

Polling hint: after add, repeat this call with a short sleep until files
appear (typically <=12s for 1080p, <=45s for 4K, longer on cold swarms).
Note that "get" peeks without renewing the torrent's expiry timer, so polling
alone will not keep an idle torrent alive.
"""
import json
import os
import sys
import urllib.error
import urllib.request

VIDEO_EXT = (".mkv", ".mp4", ".avi", ".mov", ".m4v")


def extract_files(d):
    """Engine-provided list first, sync-stored blob as fallback."""
    fs = d.get("file_stats")
    if fs:
        return fs, "file_stats"
    data = d.get("data") or ""
    if isinstance(data, str) and data.strip():
        try:
            parsed = json.loads(data)
        except ValueError:
            return [], "none"
        files = (parsed.get("GoStorm") or {}).get("Files") or []
        if files:
            return files, "data"
    return [], "none"


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)
    h = sys.argv[1]
    api = os.environ.get("TIRAMISU_API") or (
        sys.argv[2] if len(sys.argv) > 2 else "http://127.0.0.1:8090"
    )
    payload = json.dumps({"action": "get", "hash": h}).encode()
    req = urllib.request.Request(
        api + "/torrents",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        d = json.loads(urllib.request.urlopen(req, timeout=60).read())
    except urllib.error.HTTPError as e:
        # 404 = the engine does not know this hash (add failed, or wiped)
        print(f"engine returned HTTP {e.code} for {h}")
        return 2
    if isinstance(d, list):
        d = d[0] if d else {}

    files, source = extract_files(d)
    if not files:
        print("no files yet (metadata not resolved)")
        return 1

    print(f"{d.get('title','')}  stat={d.get('stat')} {d.get('stat_string')}  [{source}]")
    for f in files:
        # file_stats uses Id/Path/Length via json tags id/path/length; the
        # sync-stored blob uses the same lowercase keys.
        p = f.get("path", "")
        if p.lower().endswith(VIDEO_EXT):
            print(f"id={f.get('id')}\t{f.get('length')}\t{p}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
```

### create_mkv.py

```python
#!/usr/bin/env python3
"""Create a virtual .mkv stub (JSON) in the Tiramisu library.

Usage:
    python3 create_mkv.py <library_root> <relative_target_path> <hash> <file_id> <size_bytes> [imdb] [trackers_from_stub]

Examples:
    # movie -> <lib>/movies/Title_2024_1080p_a1b2c3d4.mkv  (imdb required)
    python3 create_mkv.py /mnt/library movies/Title_2024_1080p_a1b2c3d4.mkv \
        a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0 3 4688122884 tt1234567

    # TV episode -> <lib>/tv/Series_Name (2024)/Season.01/Series_S01E01_a1b2c3d4.mkv (imdb EMPTY)
    python3 create_mkv.py /mnt/library \
        "tv/Series_Name (2024)/Season.01/Series_S01E01_a1b2c3d4.mkv" \
        a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0 1 922746880 ""

The stub is a tiny JSON file; the FUSE layer presents it as a full-size file.
Layout conventions: movies flat with imdb id; TV nested per Season.NN with
empty imdb. HASH8 (last 8 hash chars, lowercase) goes in filename and in the
stream link. file_id is 1-BASED: the engine treats 0 as undefined.

Trackers: pass the last argument as a comma-separated list copied from an
existing stub in the SAME library, so the magnet matches that deployment. With
no argument the magnet carries no tracker and relies on DHT alone, which is
slower to bootstrap.
"""
import json
import os
import sys
import urllib.parse


def main():
    if len(sys.argv) < 6:
        print(__doc__)
        sys.exit(2)
    lib, target, h, fid, size = sys.argv[1:6]
    imdb = sys.argv[6] if len(sys.argv) > 6 else ""
    trackers_arg = sys.argv[7] if len(sys.argv) > 7 else ""
    api = os.environ.get("TIRAMISU_API", "http://127.0.0.1:8090")

    if int(fid) < 1:
        print("file_id must be >= 1 (the engine treats 0 as undefined)")
        sys.exit(2)

    h_lower = h.lower()
    name = os.path.splitext(os.path.basename(target))[0]
    parts = [f"magnet:?xt=urn:btih:{h_lower}", "dn=" + urllib.parse.quote(name, safe="")]
    for t in (x.strip() for x in trackers_arg.split(",") if x.strip()):
        parts.append("tr=" + urllib.parse.quote(t, safe=""))
    magnet = "&".join(parts)

    data = {
        "url": f"{api}/stream?link={h_lower}&index={fid}&play",
        "size": int(size),
        "magnet": magnet,
        "imdb": imdb,
    }
    path = os.path.join(lib, target)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump(data, f)
    print("created", path, data["size"], "bytes")


if __name__ == "__main__":
    sys.exit(main())
```

### create_tv_season.py

```python
#!/usr/bin/env python3
"""Create one stub per episode for a season pack, in a single pass.

Usage:
    python3 create_tv_season.py <library_root> "<Series Name>" <year> <hash> [trackers] [api_base]

Reads the file list from the engine, maps every video file to SxxEyy taken from
its OWN path, and writes <lib>/tv/<Series_Name> (<year>)/Season.NN/<Series>_SNNEXX_<hash8>.mkv
for each. imdb is left empty, which is the TV convention.

Why a dedicated script: file ids follow the engine's own sort, NOT episode
order, so pairing "id 1" with "E01" is wrong on most packs. The episode number
must come from the filename. Files with no recognisable SxxEyy are skipped and
listed at the end, so nothing is silently mis-filed.

Dry run by default: pass --write to actually create the stubs.
"""
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

VIDEO_EXT = (".mkv", ".mp4", ".avi", ".mov", ".m4v")
# S01E02 / s1e2 / 1x02 — the first two are by far the most common
RE_SXXEYY = re.compile(r"[Ss](\d{1,2})[\s._-]?[Ee](\d{1,3})")
RE_NXNN = re.compile(r"\b(\d{1,2})x(\d{2,3})\b")


def episode_of(path):
    name = os.path.basename(path)
    m = RE_SXXEYY.search(name) or RE_NXNN.search(name)
    if not m:
        return None
    return int(m.group(1)), int(m.group(2))


def fetch_files(api, h):
    payload = json.dumps({"action": "get", "hash": h}).encode()
    req = urllib.request.Request(
        api + "/torrents",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        d = json.loads(urllib.request.urlopen(req, timeout=60).read())
    except urllib.error.HTTPError as e:
        print(f"engine returned HTTP {e.code} for {h}")
        sys.exit(2)
    if isinstance(d, list):
        d = d[0] if d else {}
    fs = d.get("file_stats")
    if fs:
        return fs
    data = d.get("data") or ""
    if isinstance(data, str) and data.strip():
        try:
            return (json.loads(data).get("GoStorm") or {}).get("Files") or []
        except ValueError:
            return []
    return []


def main():
    write = "--write" in sys.argv
    # strip flags before reading positionals, or --write lands in a positional slot
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    if len(args) < 4:
        print(__doc__)
        sys.exit(2)
    lib, series, year, h = args[:4]
    trackers_arg = args[4] if len(args) > 4 else ""
    api = os.environ.get("TIRAMISU_API") or (
        args[5] if len(args) > 5 else "http://127.0.0.1:8090"
    )

    h_lower = h.lower()
    h8 = h_lower[-8:]
    slug = series.replace(" ", "_")
    tr = [x.strip() for x in trackers_arg.split(",") if x.strip()]

    files = fetch_files(api, h_lower)
    if not files:
        print("no files yet (metadata not resolved)")
        return 1

    planned, skipped = [], []
    for f in files:
        path = f.get("path", "")
        if not path.lower().endswith(VIDEO_EXT):
            continue
        ep = episode_of(path)
        if ep is None:
            skipped.append(path)
            continue
        season, num = ep
        target = os.path.join(
            "tv",
            f"{slug} ({year})",
            f"Season.{season:02d}",
            f"{slug}_S{season:02d}E{num:02d}_{h8}.mkv",
        )
        planned.append((target, f.get("id"), f.get("length")))

    planned.sort(key=lambda x: x[0])
    for target, fid, length in planned:
        print(f"{'WRITE' if write else 'plan '}  id={fid}\t{length}\t{target}")
        if not write:
            continue
        name = os.path.splitext(os.path.basename(target))[0]
        parts = [f"magnet:?xt=urn:btih:{h_lower}", "dn=" + urllib.parse.quote(name, safe="")]
        parts += ["tr=" + urllib.parse.quote(t, safe="") for t in tr]
        stub = {
            "url": f"{api}/stream?link={h_lower}&index={fid}&play",
            "size": int(length),
            "magnet": "&".join(parts),
            "imdb": "",
        }
        full = os.path.join(lib, target)
        os.makedirs(os.path.dirname(full), exist_ok=True)
        with open(full, "w") as fh:
            json.dump(stub, fh)

    if skipped:
        print("\nno SxxEyy in these files, handle them by hand:")
        for p in skipped:
            print("  " + p)
    if not write:
        print("\ndry run: re-run with --write to create the stubs")
    return 0


if __name__ == "__main__":
    sys.exit(main())
```
