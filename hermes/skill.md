---
name: tiramisu-manual-content-add
description: "Use when adding a specific movie/TV release to a Tiramisu library by hand. Picks a release with the deployment's own scoring and files it through the Library API, which needs no access to the filesystem."
version: 4.0.1
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

Everything happens over HTTP against the control port: the server writes the
files, so this runs from anywhere that can reach the deployment and needs no
access to its filesystem. One helper script is embedded in
[Helper scripts](#helper-scripts) and only reads the configuration. No host, IP
or secret is embedded anywhere: fill the placeholders per deployment, never
hardcode.

**Requires Tiramisu v1.9.64 or later.** On an older build the endpoints answer
`404`. Say so and stop. Updating is the fix: writing the stub by hand is never
the answer, wherever you happen to be running.

**The only thing you need from the operator is `CTRL`**, the control API base
(`http://<host>:9080` by default). The deployment describes itself from there:
`GET {CTRL}/api/config` returns every other path and port, so ask for them only
if that call fails.

| Name | Where it comes from | Example |
|------|--------------------|---------|
| `CTRL` | the operator, or the default `http://127.0.0.1:9080` | control API, everything goes here |

The same response also carries `tmdb_api_key`, the Prowlarr block,
`torrentio_url`, `media_server_type` and the `plex` block with its library ids.
Resolve them, do not ask for them.

If `media_server_type` is empty, infer it: a populated `plex.url` means Plex.

What you take from that response is the scoring profile and the indexer
credentials. The paths it reports are the server's own business.

## Core model

Tiramisu libraries are virtual: each file Plex/Jellyfin sees is a small JSON stub
on the real filesystem, exposed by the FUSE layer with the declared full size.

1. **Read the deployment's own scoring profile** from the config API
2. **Pick a release**, which is the part nobody can do for you
3. **Hand it to `POST {CTRL}/api/library/add`**, which registers the torrent,
   waits for its file list, writes one JSON stub per video file and asks the
   media server to rescan
4. The FUSE layer presents each stub as a full-size virtual file

Step 3 is one HTTP call. Knowing what it does server-side is still worth it: it
is what lets you tell a bad pick from a broken deployment.

## Library API: the whole add in one call

Available from v1.9.64 on, on the control port. It does what the sync engine does for
one title: registers the torrent, waits for the file list, picks the file, writes
the stub with the deployment's own naming, registers TV episodes in the state DB
and asks the media server to rescan. No filesystem access, no stub written by
hand, no scan call of your own.

```bash
curl -s -X POST -H 'Content-Type: application/json' --max-time 120 \
  -d '{"type":"movie","hash":"<40 hex>","title":"Dune Part Two","year":2024,
       "release_title":"Dune.Part.Two.2024.2160p.UHD.BluRay.REMUX.DV.Atmos-GRP",
       "imdb":"tt15239678"}' \
  "{CTRL}/api/library/add"
```

| Field | Meaning |
|-------|---------|
| `type` | `movie` (default) or `tv` |
| `hash` / `magnet` | one of the two. With `hash` alone the server builds the magnet with its own default tracker list; a magnet's own trackers are kept as they are |
| `title` | display title, and the folder name for a series |
| `release_title` | the raw release name; the quality tags in the filename (`_DV`, `_Atmos`, `_REMUX`) are read from here. Defaults to `title`, which loses them |
| `year` / `release_date` | either; the year ends up in the filename. Movies only |
| `first_air_date` | TV. The `(YYYY)` in the series folder comes from here and from nowhere else: without it the show lands in `Series` instead of `Series (2024)`, which is a second entry in the media server |
| `imdb` | written into a movie stub, and what bulk operations later filter on. **Ignored for TV**: episode stubs carry no id, because the webhook matcher pairs a Plex episode event with an open file by looking at the ones whose id is empty |
| `is_4k` | overrides the resolution read from `release_title` |
| `season`, `episode` | TV. `season` alone means "season pack": every file whose name carries SxxEyy is filed |
| `file_index` | overrides the largest-video-file pick |
| `quality_score` | stored in the TV registry, and what the next TV sync compares against. Left out it is zero, so the sync replaces the episode with the first release it scores above that. Pass the score you computed for the release you picked, the same number [Score candidates](#score-candidates) produces |
| `metadata_wait` | seconds to wait for the file list, default 60, capped at 300 |

Answers `201` with the stubs it created, or `200` with `"already_present": true`
when the release was already filed. `--max-time` has to exceed `metadata_wait`:
a cold swarm uses all of it.

```json
{"hash":"...","title":"Dune Part Two","type":"movie","already_present":false,
 "files":[{"path":"/mnt/torrserver/movies/Dune_Part_Two_2024_2160p_DV_Atmos_REMUX_deadbeef.mkv",
           "fuse_path":"movies/Dune_Part_Two_2024_2160p_DV_Atmos_REMUX_deadbeef.mkv",
           "size":68719476736,"file_index":2}]}
```

Failures say which half broke: `400` the request, `422` the torrent holds no
video file, `502` the engine refused it, `503` the state DB is unavailable (TV
only: an episode that cannot be registered would be deleted by the next sync),
`504` no metadata within `metadata_wait`. Every failure removes the torrent it
added, so a failed call leaves nothing behind and can simply be retried.

### What is already there

```bash
curl -s "{CTRL}/api/library/list?type=movie" | \
  python3 -c 'import sys,json; [print(i["fuse_path"], i["hash"][-8:], i.get("imdb","")) for i in json.load(sys.stdin)]'
```

The slice is the half that appears in a movie filename; for `type=tv` print
`i["hash"][:8]` instead. The full hash is in the JSON either way, so script
against that rather than the fragment.

One entry per stub, with `size`, `hash`, `imdb` and, for TV, `season`/`episode`.
This is the dedup check: it reads the filesystem server-side, which is the only
source that answers "will this look like a duplicate in Plex".

### Removing

```bash
# take fuse_path from add or list
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"path":"movies/Title_2024_1080p_e7f8a9b0.mkv","blacklist":true}' \
  "{CTRL}/api/library/remove"
```

`{"hash":"<40 hex>"}` works too and removes every stub of that release.

**`blacklist` is the difference between a removal that sticks and one that does
not.** With it, the release is recorded the way the FUSE unlink handler records
it, and the sync engines will not add the title back. Without it, they are free
to, which is what you want when removing only to make room for a better release.

The torrent behind a removed stub is dropped only when no other stub still points
at it: one season pack is a single torrent behind many episodes, and removing one
episode must not break the others.

## What the library looks like

You do not write any of this, the server does. Knowing the shape is what lets
you read a `list` response and tell whether a title is already there.

```
movies/
└── <Title>_<Year>_<Resolution>[_<tags>]_<HASH8>.mkv     (flat, imdb set)
tv/
└── <Series_Name> (<Year>)/
    └── Season.NN/
        └── <Series>_S<NN>E<XX>_<HASH8>.mkv              (nested, imdb empty)
```

- **Movies** are flat, and the stub carries the IMDB id you passed
- **The quality tags are exclusive pairs.** A release tagged both DV and HDR
  becomes `_DV` only, and `_Atmos` wins over `_5.1`. Do not expect `_DV_HDR`:
  raw release names in the library may carry both, but those are not names
  Tiramisu generated
- **TV** is nested per series and season, the year comes from `first_air_date`,
  and the stub's `imdb` is empty by convention. Passing one for a TV add is
  ignored on purpose: the webhook matcher pairs a Plex episode event with an
  open file by looking at the ones whose id is empty
- **HASH8** is 8 lowercase hex chars of the info hash, but not the same 8:
  **movies use the LAST 8, episodes the FIRST 8.** When you match a `list` entry
  by hash, use `endswith` for a movie and `startswith` for an episode

## Resolve the IMDB id first

Every search below is keyed on the IMDB id, and nothing in the flow will find it
for you. Resolve it through TMDB, the same way the sync engine does, using
`tmdb_api_key` from the config:

```bash
# 1. title + year -> TMDB id
curl -s "https://api.themoviedb.org/3/search/movie?api_key=$TMDB&query=Paris%2C%20Texas&year=1984"

# 2. TMDB id -> IMDB id
curl -s "https://api.themoviedb.org/3/movie/<tmdb_id>/external_ids?api_key=$TMDB"   # -> imdb_id
```

For a series the second call is `/tv/<tmdb_id>/external_ids`.

**Check the title and year that come back before using the id.** Picking the
neighbouring result is easy and silent: `tt0087884` is Paris, Texas and
`tt0087889` is The Party Animal. Everything downstream will then quietly work on
the wrong film.

## Search for candidates

Search by **IMDB id**, not by free text. Both indexers Tiramisu uses are keyed
on it, and a title search is what makes generic one-word show names collect
unrelated releases.

### Prowlarr, through Tiramisu

For searching, go through Tiramisu: it already holds the credentials and exposes
an endpoint that uses them, so there is no need to hunt for an API key.

That is the default, not an absolute. When the endpoint returns nothing and the
timing points at the deadline, calling Prowlarr directly with `prowlarr.url` and
`prowlarr.api_key` from the config is the correct move, and often the only way
to get the candidates at all. Use the key for that, never print it.

```bash
# --max-time 180: this endpoint routinely takes minutes, see below
curl -s --max-time 180 "{CTRL}/api/prowlarr/search?imdb_id=tt1234567&type=movie&title=Some%20Title&year=2024"
curl -s --max-time 180 "{CTRL}/api/prowlarr/search?imdb_id=tt1234567&type=series&title=Some%20Show"
```

- `imdb_id` is required, everything else is optional
- `type` is `movie` (the default) or `series`
- `title` and `year` are a secondary query for indexers that have no real IMDB
  search. Pass `year` for movies; for a series it would be the year of season 1
  and only hurts
- the response is a JSON array of `{name, title, infoHash, behaviorHints}`

**An empty `[]` has three different causes, and the status code separates only
one of them.** Do not read it as a single condition:

| What you get | What it means |
|---|---|
| `[]`, status 200 | Prowlarr is not configured on this deployment |
| `[]`, status 200 | every query ran and genuinely found nothing |
| `[]`, status 200 | **one query answered with nothing while the others timed out** |
| HTTP 502, body `prowlarr search failed: all N Prowlarr queries failed: ...` | every query failed, usually `context deadline exceeded` |

The third row is the trap. The client reports an error only when **all** queries
fail; if one answers, the search counts as completed even when the rest died on
the deadline, so a half-broken search is indistinguishable from a real "nothing
found". A well known title coming back empty is the symptom.

### Give it minutes, not seconds

**This endpoint is slow by construction, and a short client timeout is the most
common way to misjudge it.** Allow at least 180s before calling it broken.

The call has two phases and only the first is bounded:

1. **querying the indexers**, capped at 45s
2. **resolving the info hashes**, with no overall cap

Phase 2 exists because some indexers, 1337x among them, do not return an
`infoHash` inline: each such result needs a redirect followed through Prowlarr's
download proxy. That runs 5 at a time with a 20s budget each, so 40-odd results
needing resolution is minutes of legitimate work, not a hang.

Measured on a real deployment: `HTTP 200 in 129.9s` with 55 results, on the same
query where a direct Prowlarr search answered in 29.4s. **The direct search is
faster because it does not resolve hashes at all** — and those hashes are exactly
what you need to add anything. Faster there does not mean better.

A client timeout below the total is indistinguishable from a dead endpoint: you
get no status and no body, and conclude the service is broken while it is still
working. If you cut a call short, say so as "I did not wait long enough", never
as "the endpoint does not respond".

### Telling a timeout from an empty answer

**Time the call.** A genuine "nothing found" comes back quickly. An empty array
that arrives at almost exactly 45s is phase 1 being cut off, not an answer.

```bash
curl -s -o /dev/null -w '%{time_total}s\n' --max-time 180 "{CTRL}/api/prowlarr/search?imdb_id=..."
```

Measured on the same deployment: a title with 57 results on Prowlarr came back as
`[]` after `45.024s`. The indexers were healthy and answering; phase 1 was simply
cut off.

When the timing says deadline, **query Prowlarr directly to confirm and to
recover the candidates**. Take `prowlarr.url` and `prowlarr.api_key` from the
config for this:

```bash
curl -s -H "X-Api-Key: {prowlarr.api_key}" \
  "{prowlarr.url}/api/v1/search?query=<title>&categories=2000"
```

Results there and none through the endpoint means the deadline, full stop: keep
the direct results and say the endpoint timed out. Nothing in either place means
the indexers really have nothing.

Either way fall through to Torrentio as well, and report which of the causes it
was. Silently calling a timeout "no results" is how candidates get lost.

**This endpoint queries Prowlarr and nothing else.** It is not the same search
the sync engine performs, so its result is not the full candidate set: to see
what the engine would see, query Torrentio as well and merge the two lists
yourself, deduplicating by info hash. A 4K release missing here is very often
present on Torrentio.

Two things that mislead when reading the response:

- `name` is literally `"Torrentio\n<resolution>"` even for Prowlarr results. It
  is a Stremio format label, not the source. Torrentio has NOT been consulted
- the 45 second deadline applies to the indexer queries only, not to the whole
  call, which routinely runs far longer. The same query can return a different
  number of results minute to minute, so few results is not proof that few exist

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

**The numbers are in `title`, not in `name`.** Torrentio answers
`{"streams":[...]}`, and each stream carries the release name on the first line
of `title`, the counters on the second and, for some indexers, flags on a third.
`name` holds the indexer and the resolution tag, never the seeders or the size.
Parse the fields, do not eyeball them:

```bash
curl -s "{TORRENTIO}/{config}/stream/movie/tt1234567.json" | python3 -c '
import sys, json, re
for x in json.load(sys.stdin).get("streams", []):
    t = x.get("title", "")
    name = t.split("\n")[0]
    seeders = int(re.search(r"\U0001F464 (\d+)", t).group(1)) if re.search(r"\U0001F464 (\d+)", t) else 0
    size = float(re.search(r"([0-9.]+) GB", t).group(1)) if re.search(r"([0-9.]+) GB", t) else 0.0
    print(f"{seeders:4d} {size:6.2f}GB {x.get('infoHash','')} {name[:70]}")
'
```

The same shape comes back from `/api/prowlarr/search`, which formats its results
the Torrentio way on purpose, so one parser serves both. A stream whose size is
reported in MB rather than GB is not a video file.

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
python3 resolve_deployment.py     # media server, library ids, scoring profile
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

**A gate is not always the right answer, and this one in particular.** The gates
reproduce what the unattended sync does, where nobody is around to ask. You are
not in that position. When the only candidates carrying the preferred audio
language fall outside the size band, while the in-band ones are missing it, do
not silently drop the first group: the deployment declares it wants that
language (`preferred_language` is a positive weight), so the tradeoff is real
and it is the user's to make. Show both options with size and audio, and ask.

The same applies whenever the gates leave nothing at all: report what was
rejected and why, rather than concluding no release exists.

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

**Check the deployment can do this, before anything else:**

```bash
curl -s -o /dev/null -m 10 -w '%{http_code}\n' "{CTRL}/api/library/list?type=movie"
```

- `200`: carry on.
- `404`: Tiramisu is older than v1.9.64. Tell the operator to update, and stop.
- `000`, a timeout, or a 5xx: `CTRL` is wrong or the service is down.
- anything else, `401` and `403` included: something in front of Tiramisu is
  answering, not Tiramisu. That is an access problem for the operator to fix.

Only the first case is a green light. In every other one, say what came back and
stop: there is no second route to fall back to.

### 1. Read the configuration, then resolve the IMDB id

The TMDB lookup below needs `tmdb_api_key`, so the config call comes first:

```bash
curl -s "{CTRL}/api/config" | python3 -c 'import sys,json; c=json.load(sys.stdin); print(c["tmdb_api_key"])'
```

Read the fields you need and keep them in memory. Never print the whole response
and never write it to disk: it carries `plex.token`, `prowlarr.api_key` and
`tmdb_api_key` in cleartext.

Then resolve the id: see [Resolve the IMDB id first](#resolve-the-imdb-id-first).
Confirm the title and year that come back before going further.

### 2. Resolve the deployment

One call answers where everything is and how this deployment scores:

```bash
python3 resolve_deployment.py            # or: resolve_deployment.py http://host:9080
```

It prints the scoring profile and what the deployment has configured: media
server, library ids, whether each key is set. Only `CTRL` has to be given, and
only when it is not the default.

**The config is never cached.** The response carries `plex.token`,
`prowlarr.api_key` and `tmdb_api_key` in cleartext, so writing it to disk would
leave secrets in a file any local user can read. One call costs milliseconds and
is always current: make it every time, and keep in memory only the handful of
fields you need.

**Run it from wherever you are.** The paths in that response are the server's:
it writes the stubs itself, so they never have to be reachable from your side.
`CTRL` is the only address that has to work.

### 3. Add

First check the title is not already there, which on
this route means one call and has to happen before the add, not after:

```bash
# type is a filter, not a hint: movie lists the movie library, tv the series one,
# and neither returns the other. Ask for the one you are about to write into.
curl -s "{CTRL}/api/library/list?type=movie" | \
  python3 -c 'import sys,json; [print(i["fuse_path"]) for i in json.load(sys.stdin)]' | grep -i '<title fragment>'

# TV: entries carry season and episode, so check the episodes you are filling
curl -s "{CTRL}/api/library/list?type=tv" | \
  python3 -c 'import sys,json; [print(i["fuse_path"], i.get("season"), i.get("episode")) for i in json.load(sys.stdin)]' | grep -i '<series fragment>'
```

`add` answers `200` with `already_present` instead of filing a movie or a single
episode twice, but that guard does not cover the two cases that matter here:

- a **different release** of a title already in the library is filed, and the
  media server then shows two versions. That is a decision for the user, exactly
  as it would be for a title the sync engine had picked
- a **season pack** is never short-circuited, because which episodes it holds is
  only known once its file list arrives. Re-adding one rewrites every episode it
  names, deletes the stubs of the releases it replaces and drops their torrents,
  and answers `201`. That is an upgrade, not a duplicate, but it is not a no-op:
  do not re-send a pack to "check" whether it is there, use `list`

One call does the add, the file pick, the stub and the library scan. See
[Library API](#library-api-the-whole-add-in-one-call) for the full field list.

```bash
curl -s -X POST -H 'Content-Type: application/json' --max-time 120 \
  -d '{"type":"movie","hash":"<HASH>","title":"<Title>","year":<YEAR>,
       "release_title":"<the raw release name>","imdb":"<tt...>"}' \
  "{CTRL}/api/library/add"
```

Pass `release_title` verbatim from the indexer result: it is what puts `_DV`,
`_Atmos` and `_REMUX` in the filename, and `title` alone silently loses them.
The response already carries the path and size of every stub written: that is
the receipt, go to step 4.

A season pack, which files every episode the torrent names:

```bash
curl -s -X POST -H 'Content-Type: application/json' --max-time 180 \
  -d '{"type":"tv","hash":"<HASH>","title":"<Series>","first_air_date":"<YYYY-MM-DD>",
       "season":1,"release_title":"<the raw release name>","quality_score":<score>,
       "metadata_wait":120}' \
  "{CTRL}/api/library/add"
```

For a single episode add `"episode":<N>` and keep `"season"`: season alone is
what makes it a pack. `first_air_date` is what puts the year in the folder name, and the
score is what stops the next sync replacing your pick.

Sending `hash` alone is enough. The server builds the magnet with its own
default tracker list, so the torrent does not start DHT-only. Pass `magnet`
instead when the indexer gave you one: its trackers are kept as they are.

### 4. Verify

The response is the receipt: it lists the path, the `fuse_path` and the declared
size of every stub written. Confirm the entry is really there with one `list`
call, and stop. The scan was already triggered by the add.

```bash
curl -s "{CTRL}/api/library/list?type=movie" | \
  python3 -c 'import sys,json; [print(i["fuse_path"], i["size"]) for i in json.load(sys.stdin)]' | grep -i '<title fragment>'
```

If the operator has shell access to the host and something looks wrong, reading
the first bytes through the FUSE mount streams the real MKV header from the
swarm, which is the strongest proof the stub works end to end:

```bash
# the mount point is fuse_mount_path in the config, which only the operator can
# read for you: this script does not print it and dumping the config is not an
# option
head -c 2M "<fuse_mount_path>/movies/<file>.mkv" | ffprobe -v error -show_streams -
```

Real codec data coming back means engine, FUSE and swarm are all doing their
job. That is a diagnostic, not a step: do not ask for host access to run it.

#### The library scan is not yours to trigger

`add` and `remove` already ask for it, and they coalesce a burst: filing twenty
titles asks for one scan, not twenty. Calling the media server yourself only
adds load, and a `library_id` of `0` means the operator disabled the refresh on
purpose.

Expect a delay before the title appears, longer with the library on a remote
share: the scan starts about 15 seconds after the call and the media server
takes its own time after that. A title missing from Plex right after the add is
not evidence that anything is wrong. `list` is the authority on whether the stub
exists.

### 5. Undo, if the release was wrong

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"path":"<fuse_path from the add response>","blacklist":true}' \
  "{CTRL}/api/library/remove"
```

`blacklist: true` is what makes the removal stick. It records the release the
way the FUSE unlink handler does, and both sync engines check that record before
adding anything, so the title stays out. Leave it out only when the removal is a
step towards a better release for the same title, which is exactly when you want
the sync free to manage it again.

The torrent behind the stub is dropped only once no other stub points at it: one
season pack is a single torrent behind many episodes, so removing one episode
does not break the others.

## Bulk removal

Requests like "remove everything older than 2025" or "remove every Ridley Scott
film" are not one call. `remove` takes one title at a time, and no endpoint
filters the library by year, director or quality. `list` gives you everything
there is; the selection is yours to build from it.

### Build the selection

- **Year** is in the `fuse_path` that `list` reports
  (`Title_2025_1080p_<hash8>.mkv`), so it needs nothing but that one call and a
  pattern
- **Quality tags** are there too: `_2160p`, `_DV`, `_Atmos`, `_REMUX`
- **Anything else**, director included, is not stored anywhere in Tiramisu. The
  stub does carry the `imdb` id, which `GET {CTRL}/api/library/list` reports for
  every entry, and which is the way in: resolve it through TMDB
  (`/find/<imdb_id>?external_source=imdb_id`, then the credits) and filter on
  that. On a library of thousands this is thousands of API calls, so narrow the
  candidate list by filename first and only then resolve what is left
- TV stubs carry an **empty** `imdb`, so the same trick does not work there. Go
  through the series directory name instead

### Delete

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"path":"movies/<file>.mkv","blacklist":true}' "{CTRL}/api/library/remove"
```

One call per title. `blacklist: true` is what stops the next sync bringing them
all back.

### Before deleting anything

Bulk deletion is destructive, irreversible and operates on someone else's
library. Print the full list of what matches, with a count, and get an explicit
confirmation before the first `rm`. If the criterion needed TMDB lookups, say so
and show what could not be resolved rather than silently excluding it.

Removing a title the automated pipeline manages is legitimate here, since that
is precisely what the blacklist is for. But say which ones they are: the operator
may have wanted them.

## Troubleshooting

Every failure below is what the server tells you; none of them are fixed by
touching files.

- **`504 no metadata`**: the swarm did not answer within `metadata_wait`. Raise
  it (tetto 300s) and retry, or pick a release with more seeders. The torrent
  was already removed, so nothing is left behind
- **`422 the torrent holds no video file`**: the release is a pack of extras, a
  scene folder or the wrong content. Pick another one
- **`422 no episode file could be named`**: a TV pack whose filenames carry no
  `SxxEyy`. Add the episodes one at a time with `episode` and `file_index`
- **`502 gostorm rejected the torrent`**: the engine is down or restarting. Check
  `{CTRL}/api/health`
- **`503 the episode registry is unavailable`**: the state DB is not there, and a
  TV stub written now would be deleted by the next sync. This is an operator
  problem, do not work around it
- **The title is in `list` but not in Plex**: the scan runs about 15 seconds
  after the add and the media server takes its own time. `list` is the authority
  on whether the stub exists
- **Add is slow**: most of the time is the metadata wait. A cold or dead swarm
  uses all of it, and no amount of retrying makes the peers appear

## Do NOT

- Hardcode scoring weights, size bands or seeder minimums: read them from the
  deployment's configuration
- Print, paste or save the full config response: it contains API keys and tokens
  in cleartext. Read the handful of fields you need and keep them in memory
- Create stubs for content already present in the library
- Modify files that the automated pipeline manages
- Write a stub yourself, or reach for the filesystem at all: naming, episode
  registration and the scan are the server's job, and a hand-written stub that
  gets one of them wrong is deleted by the next sync as an orphan
- Remove a title by deleting its file: use `/api/library/remove`, with
  `blacklist: true` when the removal is meant to last
- Trigger a library scan by hand after `/api/library/add`: it already asked for
  one, and a burst of adds is coalesced into a single scan
- Touch the engine API (`POST /torrents`) to add or remove a title: it registers
  a torrent without writing anything into the library, so nothing appears. Its
  `wipe` action removes every torrent on the deployment, and no request phrased
  as "remove these films" ever means that
- Delete in bulk without showing the full list first and having it confirmed
- Run the whole flow when only a verification was asked (check, don't add)

## When you learn something new

You will hit things this file does not cover, or covers wrongly. Two rules for
what to do with them.

**Do not edit this skill on your own.** It is shared, published and versioned,
and a wrong instruction written confidently is worse than a missing one. Every
correction in this file so far came from the same loop: an agent reported what
it observed, a human checked it against the engine source, the version went up.
That verification step is why the corrections are trustworthy, and skipping it
would fill the file with plausible mistakes.

**Write findings down where they survive the round.** Append to a `FINDINGS.md`
in your working directory. Three kinds of thing belong there:

- **the address you were given**, so the next round does not ask again: the
  `CTRL` base
- **what the operator prefers**, learnt from how they answered: the audio
  language they accept, whether they take an oversized remux, which library a
  title should land in
- **obstacles and how they were cleared**: an indexer that times out and needs a
  control search, a season pack whose episode numbering does not follow the file
  ids, a title whose TMDB match is ambiguous

```
## <date> <what you were doing>
- observed: <exactly what happened, with the command and the output>
- expected per the skill: <what this file led you to expect>
- resolved by: <what actually worked>
- guess at cause: <optional, and label it as a guess>
```

**Never put secrets there.** No tokens, no API keys, no config dumps. An address
and a preference are notes; `prowlarr.api_key` is not.

Report the findings to the operator at the end of the run. The ones that turn
out to be general belong in the next version of this skill; the ones local to a
deployment stay in that file.

Say plainly when something did not work, including when you cannot tell why. An
unexplained failure reported as such is useful; the same failure smoothed over
is how a defect survives to the next round.

## Helper scripts

One script, embedded here as the single source of truth. It only reads the
configuration: everything that writes goes through the API. Write it to a
temporary file when you need it and delete it after, or run it inline. It takes
the control base from an env var or a positional arg; no IP or secret is
hardcoded.

### resolve_deployment.py

```python
#!/usr/bin/env python3
"""Read a Tiramisu deployment's scoring profile and configuration.

Usage:
    python3 resolve_deployment.py [ctrl_base]

ctrl_base defaults to http://127.0.0.1:9080, overridable with TIRAMISU_CTRL. It
is the ONLY value that has to come from the operator: the media server, the
library ids and the scoring weights all come back from GET {ctrl}/api/config.

Weights are per-deployment configuration and may have been tuned, so they must
be read, never assumed. When the config carries no quality_scoring block the
engine falls back to its own built-in profile and this script says so instead of
inventing numbers.

SECURITY: the config response also contains API keys and tokens in cleartext.
This script reports whether each one is set, never its value, and never the
whole response. Do not dump it anywhere either.
"""
import json
import os
import sys
import urllib.request


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    ctrl = os.environ.get("TIRAMISU_CTRL") or (
        args[0] if args else "http://127.0.0.1:9080"
    )

    # Fetched fresh every time, and never written to disk. The response carries
    # plex.token, prowlarr.api_key and tmdb_api_key in cleartext, so caching it would
    # leave secrets in a file readable by any local user. One call costs milliseconds.
    with urllib.request.urlopen(ctrl + "/api/config", timeout=15) as r:
        cfg = json.loads(r.read())

    # Both Plex and Jellyfin read their url/token from the "plex" block: the engine
    # hands those same two fields to whichever client media_server_type selects.
    ms = cfg.get("plex") or {}
    kind = cfg.get("media_server_type") or ("plex" if ms.get("url") else "unset")
    print("--- deployment ---")
    print(f"  media server        {kind} {ms.get('url') or 'MISSING'}")
    print(f"  media token         {'set' if ms.get('token') else 'MISSING'}")
    print(f"  library ids         movies={ms.get('library_id')} tv={ms.get('tv_library_id')}  (0 = refresh off)")
    print(f"  torrentio           {cfg.get('torrentio_url')}")
    print(f"  tmdb key            {'set' if cfg.get('tmdb_api_key') else 'MISSING'}")

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
