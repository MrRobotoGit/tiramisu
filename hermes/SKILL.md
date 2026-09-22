---
name: tiramisu-manual-content-add
description: "Use when adding a specific movie, TV, music or audiobook release to a Tiramisu library by hand. Picks a release with the deployment's own scoring and files it through the Library API, which needs no access to the filesystem."
version: 4.5.0
author: MrRobotoGit
license: GPL-3.0-only
metadata:
  hermes:
    tags: [tiramisu, torrent, manual-add, mkv, flac, audiobook, library, plex, jellyfin, plexamp, prowlarr]
    related_skills: [tiramisu-development]
---

# Tiramisu Manual Content Addition

How to add a specific movie, TV series, album or audiobook to a Tiramisu library
when the automated indexer sync missed it, replicating the same quality-scoring and
virtual-file logic the sync engine uses. Most of this skill is about video, because
that is where the scoring lives; audio has its own contract and its own section. Deployment, operation and debugging live in
`tiramisu-development`; this skill only covers getting one release into the library.

Everything happens over HTTP against the control port: the server writes the
files, so this runs from anywhere that can reach the deployment and needs no
access to its filesystem. One helper script is embedded in
[Helper scripts](#helper-scripts) and only reads the configuration. No host, IP
or secret is embedded anywhere: fill the placeholders per deployment, never
hardcode.

**Requires Tiramisu v1.9.64 or later; the audio contract needs v1.10.0.** On an
older build the audio endpoints answer `404`, and an old `list` reads the wrong
library without saying so. Say which version you found and stop. Updating is the
fix: writing the stub by hand is never the answer, wherever you happen to be
running.

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

If `media_server_type` is empty, do not infer it from the `plex` block: Jellyfin
deployments carry `plex.url` and an API key there too, so the inference reads Plex
on a Jellyfin box. Report the missing field instead of guessing.

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

Step 3 is one HTTP call for video. **For audio the naming moves to you**: there
is no metadata source to derive a filename from, so it is `inspect` then `add`,
and you name every projection — see
[Audio: music and audiobooks](#audio-music-and-audiobooks). Knowing what the
engine does server-side is still worth it: it is what lets you tell a bad pick
from a broken deployment.

## What to report

Three things, and nothing else. Brevity is never the rule before something
destructive: what [Before deleting anything](#before-deleting-anything)
prescribes, the full list with a count and what could not be resolved, stands
whole. Trimming a report costs the reader time; trimming the list they are about
to approve costs them files.

**The choice, when there is one.** Once the candidates are scored, say what
survived and ask which release to file. One line each: the release name as the
indexer gives it, its size, its seeders, and whatever would change the decision
(a cut that is not labelled, a swarm that has gone cold). Ask once, with the
options and your recommendation. The candidates the gates rejected, the scoring
behind the order and the searches that produced them are not part of the
question.

One surviving candidate is still a choice: present it and wait. The confirmation
is not about which release wins, it is about writing into a library that is not
yours, and it is worth one line even when the answer looks obvious. A candidate
that only just cleared the gates, or one you would not have picked yourself, is
exactly the case where the operator wants to see it before it is filed.

**The outcome, in one line.** What `add` returned: the stub that was written, or
the error. Nothing about the calls that led there.

**Anything that did not go as this file describes.** An indexer suspended, every
candidate rejected by the same gate, a release that turned out to be
unreachable, a reply that does not match the documented one. Say what came back,
as it came back.

A run that narrows thirty candidates to one is reported by the one. The log costs
the reader the time the run was meant to save, and buries the two lines they
need: what to choose, and what happened.

Everything you leave out you still keep: rejected candidates, individual scores,
timings, raw responses. Hand any of it over the moment it is asked for, without
making the operator ask twice.

## Library API: the whole add in one call

Available from v1.9.64 on (video; audio from v1.10.0), on the control port. It does what the sync engine does for
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
against that rather than the fragment; when it is empty, only the path
identifies the entry.

Only `movie`, `tv`, `gaps`, `music` and `audiobook` mean anything: any other
value, a typo included, silently falls back to the movie library. **The plural
`audiobooks` is not a value**: it is routed as a video type and answers `200`
with the movie list — a wrong answer that looks like a right one. `musics` too.
Ask for the library you are about to write into, spelled the way this file
spells it.

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
  **movies use the LAST 8, episodes the FIRST 8.** That is what the server writes
  today, not a rule every stub on disk obeys: legacy entries can carry the other
  half. Match on the `hash` field of the entry, never on the filename fragment

## Audio: music and audiobooks

Two more libraries, `music` and `audiobooks`, siblings of `movies` and `tv` rather
than children of a shared `audio/` parent. They take the same endpoints, but the
contract is not the video one with a different word in `type`, and the differences
are the part worth reading.

**You name the files, the engine does not.** For a movie you pass metadata and the
server derives the filename. For audio there is no metadata source to derive from:
you supply the virtual path of every projection, and the engine only validates it.
Nothing derives an album layout for you.

**One torrent backs many projections.** An album is one torrent and N tracks, so an
add carries a `files` array and either all of it lands or none of it does.

**It is two calls, not one.** You cannot name a file you have not seen, so
`inspect` first to get the source paths, then `add` mapping each one to the
virtual path you want.

**The torrent is given, not found here.** Audio sourcing starts from a hash or
magnet the caller already has; building it from a title is the controller's job.
The `external_id` is what your media server will send back, so match its side:
Plex sends the MusicBrainz *release track* id, Jellyfin the MusicBrainz
*recording* id.

### Step 1: inspect

```bash
curl -s -X POST -H 'Content-Type: application/json' --max-time 120 \
  -d '{"hash":"<40 hex>","title":"20Ten","metadata_wait":60}' \
  "{CTRL}/api/library/inspect"
```

```json
{"hash":"9b3103673dfce0841cb41afa3a9f944f9ac7684d",
 "files":[{"source_path":"Prince - 20Ten (2010) FLAC/01. Compassion.flac",
           "file_index":1,"size":28461945}]}
```

`title` is required even here: a cold torrent has to be added to the engine to be
read, and it needs a name. Metadata that never arrives is an error, not an empty
list — "not ready" and "no files" are different answers, and the engine keeps them
apart. `metadata_wait` is the same knob as in `add`: seconds to wait for metadata,
default 60, capped at 300; keep `--max-time` above it.

### Step 2: add

```bash
curl -s -X POST -H 'Content-Type: application/json' --max-time 120 \
  -d '{"type":"music","hash":"9b3103673dfce0841cb41afa3a9f944f9ac7684d","title":"20Ten",
       "files":[
         {"source_path":"Prince - 20Ten (2010) FLAC/01. Compassion.flac",
          "path":"Prince - 20Ten (2010)/01. Compassion_9ac7684d.flac",
          "external_id":"f4a1...","external_id_ns":"musicbrainz"}
       ]}' \
  "{CTRL}/api/library/add"
```

The `_9ac7684d` on the path is the last 8 of the hash on the line above. It is
not optional and the engine will not add it for you.

| Field | Meaning |
|-------|---------|
| `type` | `music` or `audiobook`. **No aliases**: `musics`, `Music`, `audiobooks` are all `400`. Unlike the video types, a typo here is refused rather than silently falling back |
| `hash` / `magnet` | as for video, but a `hash` that disagrees with the magnet's own BTIH is `400 hash_magnet_mismatch` and nothing is added |
| `title` | the torrent's display name in the engine. It is not the folder name: that comes from the paths you pass |
| `files[].source_path` | a `source_path` from `inspect`, verbatim |
| `files[].path` | the virtual path inside the section. The directories are yours to choose and are created as needed, but the **filename must end in `_<hash8>` before the extension** — see below. A path without it is a `400` |
| `files[].external_id` / `external_id_ns` | optional, both or neither. Opaque to the engine, stored and returned verbatim. `musicbrainz` for music, `asin` for audiobooks — this is the audio counterpart of the IMDB id, and what the webhook matcher pairs a Plexamp event against |

**The extension is enforced per section** and is not a suggestion: `music` accepts
only `.flac`, `audiobooks` accepts `.m4b`, `.m4a` and `.mp3`. The extension of
`path` must also agree with the one of `source_path`, so a `.flac` source cannot
be filed as `.mp3`.

```json
{"hash":"9b31...","title":"20Ten","type":"music","already_present":false,
 "files":[{"path":"Prince - 20Ten (2010)/01. Compassion_9ac7684d.flac",
           "source_path":"Prince - 20Ten (2010) FLAC/01. Compassion.flac",
           "file_index":1,"size":28461945,"mtime":"2026-09-21T12:09:48Z",
           "state":"created","external_id":"f4a1...","external_id_ns":"musicbrainz"}]}
```

**In the audio response `path` is virtual** — the section-relative path you sent,
which is also what `list` and `remove` use. The video response is the one that
reports a physical `/mnt/torrserver/...` path; do not copy that habit here.

`201` when it created them, `200` with `"already_present": true` when **every**
requested projection was already there — a replay, and `mtime` stays what it was,
so a present result never looks like it rewrote anything. A mixed request — some
paths present, some new — still answers `201`, creates only the missing entries
and leaves the present ones alone, stored identity included. Each file's `state`
says which happened: `created` for what this call wrote, `present` for what was
already there.

**No rescan follows an audio add.** The rescan hook belongs to the video path:
the config carries library ids for movies and TV, none for music, and no audio
call asks the media server to look. Scan the `music/` and `audiobooks/` sections
manually once a batch is filed — that is the intended flow, not a missing
feature.

**Album images are split by the engine.** Many lossless rips are one FLAC holding
the whole album plus a cue sheet (`FLAC (image+.cue)`). Send the image like any
other file — one `files[]` entry, the path where the album should live — and the
engine does the rest: when a cue sheet in the torrent describes that FLAC with two
or more tracks, the add files **one projection per track** beside the requested
path, named `NN - Title_<hash8>.flac` from the sheet, each carrying your
`external_id`, with the tags (title, artist, album, track number) written into the
track itself. The response lists the tracks it created, each with its
`cue_track`, so it holds more files than you sent. `inspect` shows what will
happen: its `cue_tracks` array lists `source_path`, `track`, `title` and
`performer` for every image a sheet describes.

If the torrent has not delivered the cue sheet (or the image bytes the cut needs)
within three minutes, the add answers **`503`** and files nothing: retry later,
never fall back to filing the image whole — Plex would show one 45-minute track.
A cut the image cannot support (a damaged or non-FLAC image, a sheet without the
track) is a `422`.

To give each track its own MusicBrainz id, name the tracks yourself instead:
one entry per track with the image as `source_path`, the track number as
`cue_track`, and optionally `tags` (a map of Vorbis comments written verbatim,
e.g. `{"TITLE":"Lovesong","MUSICBRAINZ_RELEASETRACKID":"..."}`). Entries that
name a `cue_track` are filed exactly as sent.

**On a replay the stored identity wins.** If your `external_id` disagrees with
the one already registered for that projection, the request still succeeds, the
stored value is returned unchanged, and the disagreement is logged. This is
deliberate: the identity does not select the bytes — `(hash, file_index)` does —
so refusing the call would fail a request whose projection is correct. To change
a registered identity, remove the projection and add it again. `external_id` and
`external_id_ns` are always in the response, empty when you sent none, so a client
never has to branch on a missing key.

**Audio requests are strict**: an unknown field is a `400`, where the video
decoder stays lenient for callers that predate the endpoint. In `files[]` the
only accepted keys are `source_path`, `path`, `external_id`, `external_id_ns`,
`cue_track` and `tags`:
`file_index`, `size` and `mtime` come back from `inspect` but must not be echoed
into the `add`, or the whole request is refused. Bodies are capped at 1 MiB.

### Listing audio

```bash
curl -s "{CTRL}/api/library/list?type=music&limit=100"
```

**This answers an object, not an array.** The video types return a bare list;
audio pages, because a library is thousands of tracks rather than hundreds of
films:

```json
{"items":[{"type":"music","path":"Prince - 20Ten (2010)/01. Compassion_9ac7684d.flac",
           "hash":"9b31...","source_path":"...","file_index":1,
           "size":28461945,"mtime_ns":1758456588000000000,"state":"committed",
           "external_id":"f4a1...","external_id_ns":"musicbrainz"}],
 "next_cursor":"Prince - 20Ten (2010)/01. Compassion_9ac7684d.flac"}
```

Pass `next_cursor` back as `cursor` until it comes back empty, and `prefix` to
scope to one album instead of walking the section. `limit` defaults to 200 and
caps at 1000; `path` and `prefix` are mutually exclusive. The items are
reconciliation data, not media metadata: enough to diff what you sent against
what the engine holds, no more. `state` is normally `committed`; `staged` is an
add not yet published and `removing` a projection the engine has claimed for
deletion. Add `failures=1` to attach the metadata-failure counters — `fail_count`,
`first_fail_ns`, `last_fail_ns`, `active_session` — to every entry.

### What the audio library looks like

```
music/
└── <dirs you chose>/
    └── <name you chose>_<HASH8>.flac
audiobooks/
└── <dirs you chose>/
    └── <name you chose>_<HASH8>.m4b
```

The directory shape is the one you sent: nothing derives an album layout. The
**filename is not**, because of one mandatory token.

**`_<HASH8>` is required, and the engine never adds it for you.** It validates
the name you sent and refuses it otherwise:

```
400  path "... - I-Cube Remix.flac" does not end in _<hash8>
     for hash "4164a01e...388702de": library: invalid hash suffix
```

`HASH8` is the **last 8** lowercase hex characters of the info hash, the movie
rule rather than the episode one, immediately before the extension:

```
hash  4164a01e677359c47d71f9e1982e58a8388702de
path  Daft Punk - Homework (Remixes) (2022)/01 - Around The World_388702de.flac
```

Identity is still the `hash` and `external_id` of the entry, never the path: the
suffix is a naming rule the engine enforces, not the key anything is looked up by.

### Removing audio

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"music","path":"Prince - 20Ten (2010)/01. Compassion_9ac7684d.flac"}' \
  "{CTRL}/api/library/remove"
```

**One projection, or one album.** `path` removes exactly that file; `prefix`
removes every projection under an album directory in a single transaction. The two
are mutually exclusive, and one of them is required. The video shortcuts still do
not apply: no `hash` form that removes every stub of a release, no recursion below
the album. And `blacklist` is a `400` here, not a no-op — it is video lifecycle
state and audio has no sync engine that would re-add a title.

```json
{"type":"music","path":"Prince - 20Ten (2010)/01. Compassion_9ac7684d.flac",
 "removed":true,"torrent_referenced":false}
```

**The album form is the one you usually want.** Nobody removes a single track: a
dead swarm takes the whole album, and an album missing nine of its ten tracks is
not worth keeping.

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"music","prefix":"Prince - 20Ten (2010)"}' \
  "{CTRL}/api/library/remove"
```

```json
{"type":"music","prefix":"Prince - 20Ten (2010)","removed":10,
 "torrent_referenced":false}
```

`removed` counts the projections, so it is a number here and a boolean in the
exact-path form. Matching is on whole path components: `Artist/Album` never
reaches `Artist/Album2`. A prefix holding projections from more than one torrent
is a `409` — that directory is not one album. A prefix matching nothing is a
success with `removed: 0`, not a `404`, so a reaper retrying after a partial run
does not have to special-case it.

`torrent_referenced` says whether other projections still point at that torrent.
The torrent is never dropped by this call: while any row references it a
fail-closed guard keeps it alive, and once none does it expires through the
engine's own idle policy. Removing one track of an album therefore does not
break the others, the same way removing one episode does not break a season pack.

## Episode gaps (TV)

The TV reaper removes an episode when its release stops resolving its metadata
and a complete search finds nothing live to replace it. The hole is recorded,
not forgotten, and `type=gaps` is how you see it:

```bash
curl -s "{CTRL}/api/library/list?type=gaps" | \
  python3 -c 'import sys,json; [print(g["show"], g["season"], g["episode_key"], g["dead_hash"][:8]) for g in json.load(sys.stdin)]'
```

The response is an array like the other types, but the objects are gaps, not
stubs: `episode_key`, `show`, `season`, `show_imdb`, `path` (where the stub was),
`dead_hash`, `removed_at` (unix seconds) and `last_attempt`, zero when the engine
has never tried that hole. It is capped at 500 entries and carries no total, so
treat a full page as "there may be more".

**The engine retries the open gaps on its own.** Every TV sync re-searches them,
skipping the ones younger than six hours, at most five shows per run, oldest
first, with a show whose resolution failed moved to the back of the queue. A
gap disappears as soon as the episode is back. Doing nothing is therefore a
valid outcome: an episode missing from `list` for a while is not a fault to
repair by hand.

What the skill is for here:

- **explaining the disappearance**: the episode was in a release whose swarm
  died, and no live release existed at that moment
- **filling the hole on request**: run the normal search, score and add flow for
  a *live* release of that episode. Never re-file `dead_hash`: it is the exact
  release the reaper discarded
- **checking, not editing**: gaps ride on `list`, there is no separate endpoint
  and nothing in the database should be touched

An episode that comes back gets a new HASH8, so Plex sees a new item and the
watched/resume state for that episode does not carry over. That is the same
trade-off as any upgrade, not a symptom of the repair.

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
   (see [What the language gate actually does](#what-the-language-gate-actually-does))
3. **blacklisted title**, then **blacklisted hash** — the one gate that does not
   apply to you, see below
4. **seeders below `min_seeders`**
5. **resolution neither 4K nor 1080p** — anything else is rejected outright,
   there is no 720p path
6. **size out of band**, and the two resolutions differ here: a 4K release with
   an unknown size (0) is ACCEPTED and merely takes `unknown_size_4k_penalty`,
   while a 1080p release with an unknown size is REJECTED
7. **final score <= 0**

**The blacklist is not one of your gates.** It exists so the unattended sync
does not keep re-proposing titles the operator threw out, night after night,
with nobody there to say no. You are the opposite case: someone asked for this
title, now, and that request outranks a standing rule written for decisions made
without anyone watching. File what was asked for. You cannot read the blacklist
over the API anyway, and you do not need to.

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

Filing a release the gates rejected is a departure from the deployment's own
policy, so it takes the operator's explicit go-ahead, never your judgement
alone. When it happens, say in the report which gate was overridden and what
follows from it: the unattended sync would not have picked this release, and a
later discovery run can still replace it once something scores above the upgrade
threshold.

### What the language gate actually does

It is a blacklist, and it reads only the title.

- It matches **flag emoji and language words alike**: `excluded_flags: ["ES"]`
  rejects a title carrying 🇪🇸 and one carrying `SPANISH`, `CASTELLANO` or
  `LATINO`. So a multi-language release is rejected for the language you do not
  want even when it also carries the one you do
- The **preferred language is the opposite kind of rule**: a positive weight, not
  a requirement, and it is scored over the title *and* the indexer name. Nothing
  is ever kept because it announces your language, and nothing is ever rejected
  for lacking it

The consequence is worth stating out loud when it bites: a Torrentio release
listing 🇮🇹 alongside 🇫🇷 disappears silently, and the operator never learns it
existed. That is the same tradeoff as the size band above, and it deserves the
same treatment: show it and ask.

**A flag is not an audio track.** Torrentio lists the languages of a release
without separating audio from subtitles, so a WEB-DL announcing twenty-seven of
them is mostly subtitles. Never tell the operator "it has Italian audio" on the
strength of a flag, and do not read the `preferred_language` bonus as evidence
either: it is a heuristic over a filename. The only thing that settles it is
reading the real stream layout, which is [what the ffprobe check does](#4-verify)
once the stub exists.

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
# type picks the library: movie, tv or gaps (see "Episode gaps"). Ask for the
# one you are about to write into.
curl -s "{CTRL}/api/library/list?type=movie" | \
  python3 -c 'import sys,json; [print(i["fuse_path"]) for i in json.load(sys.stdin)]' | grep -i '<title fragment>'
```
For TV, match the series fragment on the same output and read `season`/`episode`
from the entries.

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

One call does the add, the file pick, the stub and the library scan; see
[Library API](#library-api-the-whole-add-in-one-call) for the full field list
and the response. Two fields carry the silent failures: `release_title`
verbatim, or `_DV`/`_Atmos`/`_REMUX` never reach the filename, and
`first_air_date` for TV, or the show lands in a second folder. A pack is
`"season":<N>` alone; a single episode adds `"episode":<N>` and keeps the
season. `hash` alone is enough: the server builds the magnet and the torrent
does not start DHT-only; pass `magnet` when its trackers must be kept.

**Build the payload as a file, not inline.** One apostrophe in a title closes
the shell string and the call dies on `unexpected EOF while looking for matching
quote`. In a library that is not English-only this is the common case, not an
edge one: `L'immortale`, `Non c'è altra scelta`.

```bash
python3 -c '
import json
json.dump({"type":"movie","hash":"<HASH>","title":"Non c'"'"'è altra scelta",
           "year":2024,"release_title":"<raw release name>","imdb":"tt..."},
          open("/tmp/add.json","w"), ensure_ascii=False)
'
curl -s -X POST -H 'Content-Type: application/json' --max-time 120 \
  --data-binary @/tmp/add.json "{CTRL}/api/library/add"
```

Send the title as the operator wrote it, accents included, and do not clean it
up first: the server sanitises the filename itself and anything outside
`[a-zA-Z0-9._-]` becomes `_`, so `Non c'è altra scelta` is filed as
`Non_c_altra_scelta_<year>_...`. Note what that means: an accented letter is
dropped, not transliterated. The name in the response is the truth, and it is
what a later `list` will match on.

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

The call is the one in [Removing](#removing):
`{"path":"<fuse_path from the add response>","blacklist":true}`. Setting
`blacklist: true` keeps the sync engines from adding the title back; leave it
out only when you are removing to make room for a better release of the same
title.

**Replacing a release is a remove and an add, and the media server is the last
to know.** `list` answers whether the stub exists, which is what it is the
authority on. It says nothing about what a player would open: the library scan
is asynchronous, and until it has run and settled the media server can still
hold the old file, in the same entry, and play from it. When the point of the
operation was an upgrade, say so and check the media server itself before
calling it done:

```bash
# Plex: the Part under the entry must name the new stub
curl -s "{plex.url}/library/sections/{plex.library_id}/all?X-Plex-Token={plex.token}" | \
  tr '>' '>\n' | grep -A2 -i '<title fragment>' | grep '<Part'
```

Do not trigger a scan to hurry it along: `remove` and `add` already asked for
one. If the old file is still there minutes later, that is worth reporting, not
working around.

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

One [remove](#removing) call per title, `path` from the selection above.
`blacklist: true` is what stops the next sync bringing them all back.

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
- **An episode vanished from `list`**: it was reaped, not lost. Check
  `type=gaps` for the hole; the engine retries it every sync, so the only action
  is filing a live replacement if the operator wants one sooner
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
- Narrate the run: present the choice, then the outcome, then anything that
  deviated — not the searches in between
- File a release the gates rejected without the operator's explicit go-ahead,
  or report it afterwards as if it were an ordinary pick
- Call a language present because a flag says so: flags cover subtitles too
- Re-file the `dead_hash` of an open gap: it is the release the reaper discarded
  for being dead

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

Report the findings to the operator at the end of the run: they are the third of
the [three things worth reporting](#what-to-report), so a run with none of them
ends at the outcome line. The ones that turn out to be general belong in the next
version of this skill; the ones local to a deployment stay in that file.

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

Usage: python3 resolve_deployment.py [ctrl_base]
ctrl_base defaults to http://127.0.0.1:9080, overridable with TIRAMISU_CTRL:
the media server, the library ids and the weights all come back from
GET {ctrl}/api/config. Weights are per-deployment and may have been tuned, so
read them, never assume them; with no quality_scoring block the built-in
profile applies and this script says so instead of inventing numbers.

The config response carries API keys and tokens in cleartext: report whether
each is set, never its value, and never dump the response.
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

    # Fetched fresh, never written to disk: caching it would leave secrets in a file.
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
