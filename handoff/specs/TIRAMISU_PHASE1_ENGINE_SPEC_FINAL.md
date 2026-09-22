# Tiramisu Phase 1 Generic Audio Projection Specification — Maintainer Review Draft

**Status:** Proposed Phase 1 implementation specification for maintainer review  
**Issue:** `MrRobotoGit/tiramisu#25` — Audiobook/Music Support  
**Prepared:** 2026-09-17  
**Scope:** Generic torrent-backed music and audiobook projection only; no implementation is included here.

**Research inputs:** `tiramisu-context-handoff(1).md`, `TIRAMISU_PHASE1_SPEC(1).md`, `Navidrome-scanning(1).md`, `jellyfin-compatibility(1).md`, `compatibility-research(1).md`, `metadata-research(1).md`, and `identity-research(1).md`.

## 0. Authority, terminology, and normative language

This specification synthesizes the Phase 1 draft, the project context handoff, current-source investigations of Navidrome and Jellyfin, and the broader self-hosted-media compatibility research into a **proposed engine/API/filesystem contract** for maintainer review. It is not intended to relocate application policy into Tiramisu.

The issue thread remains the baseline. Where this draft deliberately proposes a research-driven change to a point the maintainer previously accepted, that departure is called out inline and summarized in **Appendix C — Review deltas from prior issue agreement**. The maintainer's decision on this draft determines the final Phase 1 contract.

For questions that do not intentionally propose such a departure, authority is ordered as follows:

1. the latest maintainer decisions in issue #25 and the repository's `CONTRIBUTING.md`;
2. behavior verified against current Tiramisu/GoStorm source during the research passes;
3. ordinary filesystem semantics required by multiple current downstream consumers;
4. convergent findings from the compatibility research;
5. a recommendation in this document where the thread intentionally left a wire-level or numeric detail to the implementation spec.

The terms **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY** are normative in the RFC 2119 sense **for this proposal**. They become project requirements only to the extent the maintainer accepts this specification.

### 0.1 Phase 1 objective

Phase 1 extends Tiramisu's existing torrent-backed regular-file illusion from movie/TV `.mkv` projections to caller-selected music and audiobook files while keeping Tiramisu a low-level projection and streaming engine.

The architecture remains deliberately caller-agnostic:

```text
 deterministic controller     CLI / UI / automation     optional AI agent skill
           \                         |                         /
            \                        |                        /
             +------------------------+-----------------------+
                                      |
                           inspect / add / list / remove
                                      v
                              Tiramisu Library API
                                      |
                            projection registry
                                      |
                               GoStorm + FUSE
                                      |
                       +--------------+--------------+
                       |              |              |
                  Plex/Plexamp     Navidrome   Audiobookshelf/Jellyfin
```

Tiramisu owns byte-accurate projection, persistence, filesystem correctness, torrent reference safety, and the raw Library API. Semantic media policy belongs outside the engine. **Phase 1 does not require an AI agent, an agent skill, or a particular companion controller:** any deterministic client capable of calling the raw API may use audio projection directly.

### 0.2 Responsibility placement (`CONTRIBUTING.md` alignment)

This proposal follows the repository's required architectural split. Phase 1 MUST preserve these boundaries:

| Layer | Responsibilities in this design | Responsibilities that MUST NOT move here |
|---|---|---|
| **Tiramisu engine / Library API / FUSE** | Raw stable primitives and projection state: torrent metadata inspection, exact source-file addressing, add/list/remove, projection registry, safe stub creation, reference counting, immutable byte projection, filesystem semantics, persistence/recovery, and generic validation | Search, release scoring, media identity, provider choice, semantic duplicate policy, naming decisions beyond engine-required path invariants, metadata enrichment, or media-server-specific scan/playback logic |
| **External application / policy layer** | Deterministic or interactive search/discovery orchestration, scoring, release/recording selection, naming policy, verification, duplicate policy, metadata-provider use, and orchestration of downstream operations after an engine mutation. A deterministic companion controller may implement this without AI; when an AI agent is used, Tiramisu's repository-level agent policy remains in the versioned skill as `CONTRIBUTING.md` requires. | FUSE/torrent correctness or authoritative engine state |
| **Media server** | Metadata consumption/enrichment, library scanning, analysis, and playback | Torrent/projection ownership |

The deterministic audio controller described in the companion specification is a **consumer of these raw primitives, not part of the Tiramisu engine contract**. It is intended to provide automatic music/audiobook selection for users who do not use an AI agent. The optional agent skill is an additional interface above or alongside that deterministic layer, not a runtime prerequisite for audio projection.

`POST /api/library/inspect` is in scope only as a **raw engine primitive**: it reports torrent file facts needed to address a source (`source_path`, resolved engine file id/index, size/state). It MUST NOT identify albums/books, score releases, inspect provider metadata, or decide which source the caller should choose.

The caller supplies the final audio path because audio naming is external application policy. Tiramisu may enforce only engine invariants on that path (section containment, extension agreement, mandatory hash suffix, collision rules, and safety constraints); it MUST NOT derive artist/album/author/book naming semantics.

Tiramisu does not own downstream library policy. An external deterministic controller, CLI/UI application, automation client, or optional agent skill MAY orchestrate a media server's own scan API after a committed mutation, but server-specific scan behavior and configuration remain outside the engine.

### 0.3 Terms

- **API type** — `music` or `audiobook` in Library API requests.
- **Section** — the physical/FUSE root corresponding to an API type: `music/` or `audiobooks/`.
- **Projection** — one stable virtual path backed by exactly one torrent file identity.
- **Projection identity** — `(hash, file_index, source_path)` after source resolution.
- **Virtual path** — caller-selected path relative to a section root.
- **Source path** — exact torrent-relative path returned by `inspect`.
- **Stub** — the physical JSON-backed Tiramisu file whose extension causes FUSE to expose the torrent file's bytes rather than the stub bytes.
- **Healthy torrent under supported load** — torrent metadata and data are obtainable and the declared Phase 1 acceptance workload is within the supported concurrency envelope.

---

# 1. Proposed Phase 1 contract

This draft proposes the following as the Phase 1 contract for maintainer approval. Most rows restate the issue agreement; rows that intentionally revise a previously accepted assumption are identified in Appendix C.

| Topic | Phase 1 decision |
|---|---|
| Engine boundary | Tiramisu remains a generic projection/streaming engine; discovery, metadata, naming semantics, scoring, lifecycle policy, and downstream refresh policy remain external. |
| API types | `music`, `audiobook`. No aliases. |
| Roots | Flat sibling roots: `movies/`, `tv/`, `music/`, `audiobooks/`. |
| Public file selector | Exact `source_path`, not caller-supplied GoStorm `file_index`. |
| File index | Tiramisu resolves GoStorm's current 1-based `file_index`, returns it, and persists it with `source_path`. |
| Inspect | New `POST /api/library/inspect` on the Library API. |
| Registry | Persistent metadb audio projection registry is authoritative for audio ownership, List, Remove, idempotency, refcounts, and recovery. |
| Hash suffix | Caller supplies the final path; basename MUST end with `_<hash8>` immediately before the extension. Tiramisu validates but does not append it. |
| Music formats | `.flac` only in Phase 1. |
| Audiobook formats | `.m4b`, `.m4a`, `.mp3` in Phase 1. |
| Format handling | Extension is an input filter, never a rename/transcode. Source and virtual extensions must agree case-insensitively. |
| Stable paths | A stable virtual path identifies stable content. In-place content replacement is out of scope. |
| Same source at two paths | Rejected in Phase 1; one `(hash,file_index)` may have only one audio projection. |
| Add semantics | Multi-file, request-failure-atomic, per-file idempotency (`created` / `present`). |
| Conflicts | Existing path with different identity returns `409 Conflict`. |
| Remove | Exact virtual path only in Phase 1. Batch/hash/prefix removal is deferred. |
| Audio reaper | None. Tiramisu does not semantically reap audio projections. |
| Direct unlink | Managed audio `unlink` through the mount returns `EPERM`; Library API Remove owns lifecycle. |
| File mode | Audio files `0444`; mutation operations return read-only errors. |
| Inode | Existing content identity remains `hash:file_index`; committed audio paths are eagerly registered so basename fallback is not used. |
| Timestamps | Projection mtime is stable and persisted; no `time.Now()` fallback for a valid unchanged projection. |
| Namespace enumeration | A successful audio `readdir` is complete and coherent for the committed namespace at one logical point in time; temporary engine/backing contention MUST NOT produce a successful partial listing. |
| Publication | `Add` success means the path, stat metadata, and backing immutable projection are coherently available to `readdir`/Lookup/Getattr/Open; no half-published entry is observable. |
| Open-handle lifetime | Once Open succeeds, that handle remains bound to the same immutable `(hash,file_index,size)` object until close. API Remove may remove the pathname for new lookups but MUST NOT retarget or invalidate an already-open handle solely because namespace ownership was removed. |
| Hydration invariance | Cache fill, torrent piece completion, reader activity, read-ahead, and playback MUST NOT change visible path, inode, file type, size, or mtime. |
| Directory metadata | Directory mtimes change for committed child-set mutations and remain stable for hydration-only activity. Directory metadata is conventional scanner input, not media identity. |
| Restart readiness | Persisted committed audio namespace is reconciled before it is advertised as scan-ready; a successful enumeration MUST NOT expose a partially restored registry. |
| Warmup | No movie-style head/tail SSD warmup and no new audio-specific metadata warmup in Phase 1. |
| Priority/prefetch | No per-file priority API or next-track prefetch in Phase 1. |
| Downstream refresh | Explicitly triggered by the external caller/application after committed mutations; filesystem watchers are not part of the correctness contract. |
| Movie/TV behavior | Existing movie/TV semantics MUST remain unchanged. |

---

# 2. Scope and non-goals

## 2.1 In scope

Phase 1 includes:

- section-aware generic virtual-file projection for supported audio containers;
- one torrent backing multiple independently visible audio projections;
- persistent projection identity;
- inspect, add, list, and exact-path remove API behavior;
- failure-atomic multi-file adds;
- restart/crash recovery;
- section-aware startup, VFS, caches, monitoring, and torrent reference checks;
- hostile-path validation and race-safe stub creation;
- regular-file correctness for seeking/scanning/playback;
- coherent recursive directory enumeration and atomic namespace publication;
- stable open-handle object identity across namespace removal;
- hydration-invariant file metadata and conventional directory mutation timestamps;
- bounded scanner-safe read behavior;
- compatibility validation against Navidrome, Audiobookshelf, Jellyfin, Plex, and Plexamp;
- preservation of current movie/TV behavior.

## 2.2 Explicitly out of scope

Phase 1 MUST NOT add:

- MusicBrainz, Audible, AudioSilo, TMDB-like, or other audio metadata-provider integration;
- artist, album, author, narrator, series, edition, popularity, recency, or quality-scoring policy;
- internal audio discovery/search engines;
- semantic audio reapers;
- per-file priority controls;
- next-track/next-part prefetch;
- playback-aware audio scheduling classes beyond the minimum correctness fix required by the acceptance workload;
- audio sidecars (`cover.jpg`, `metadata.json`, `.opf`, `.lrc`, `.cue`, `desc.txt`, etc.);
- same-path release replacement;
- hash-, prefix-, directory-, or batch-removal APIs;
- media-server-specific refresh integrations inside Tiramisu;
- a new authentication system for `:9080`.

---

# 3. Section and format model

## 3.1 Section mapping

The engine MUST use this mapping:

```text
API type       physical/FUSE section
---------      ---------------------
music          music/
audiobook      audiobooks/
```

The roots MUST be siblings of the existing roots:

```text
<PhysicalSourcePath>/movies/
<PhysicalSourcePath>/tv/
<PhysicalSourcePath>/music/
<PhysicalSourcePath>/audiobooks/
```

A shared `audio/` parent MUST NOT be introduced in Phase 1.

## 3.2 Extension allowlists

Phase 1 MUST admit only:

```text
music:       .flac
audiobook:   .m4b .m4a .mp3
```

Extension comparison MUST be case-insensitive.

The allowlist is a Phase 1 interoperability and test boundary, not a claim that GoStorm/FUSE cannot project other byte streams. Quality preferences belong to the external application/policy layer.

The source torrent file's extension and requested virtual path extension MUST match case-insensitively. Tiramisu MUST NOT expose one container under another container's extension.

Content sniffing or magic-byte verification MUST NOT be added to the FUSE hot path. Phase 1 MAY rely on the torrent-relative extension supplied by metadata; content verification can be revisited separately.

## 3.3 Hash suffix convention

Every audio virtual filename MUST contain the final eight hexadecimal characters of the canonical infohash as the final underscore-delimited token before the extension.

For canonical hash:

```text
0123456789abcdef0123456789abcdefa1b2c3d4
```

a valid filename is:

```text
01 - Track_a1b2c3d4.flac
```

The validation rule is conceptually:

```regex
_<hash8>\.<extension>$
```

where `hash8` comparison is case-insensitive and equals the last eight hex digits of the normalized 40-hex-character infohash.

Tiramisu MUST validate this suffix and MUST NOT silently append, replace, or rewrite it.

---

# 4. Projection identity and invariants

## 4.1 Identity

A committed projection is identified by:

```text
(section, virtual_path) -> (hash, file_index, source_path)
```

The following invariants MUST hold:

```text
UNIQUE(section, virtual_path)
UNIQUE(section, portable_path_key)
UNIQUE(hash, file_index)       -- across Phase 1 audio projections
```

`portable_path_key` is defined in the security section and protects case-insensitive/Unicode-normalizing downstream filesystems.

## 4.2 Stable path rule

A virtual path MUST NOT be rebound to different content.

- same path + same projection identity => idempotent `present`;
- same path + different `hash` or `file_index` => `409 path_conflict`;
- same `(hash,file_index)` + another virtual path => `409 source_already_projected`.

A caller that changes release/source MUST use a new virtual path in Phase 1.

## 4.3 `source_path` semantics

`source_path` is the public source selector.

It MUST be copied exactly from `inspect` and matched exactly against the GoStorm file list:

- case-sensitive;
- no Unicode normalization;
- no lowercasing;
- no `filepath.Clean` or host-filesystem interpretation;
- no separator rewriting.

If zero files match, Add MUST return `source_not_found`.

If more than one file matches the exact `source_path`, Add MUST return `ambiguous_source_path`; it MUST NOT choose one arbitrarily.

Tiramisu MUST persist both `source_path` and the resolved GoStorm `file_index`.

If later metadata resolution shows that the stored `source_path` and stored `file_index` no longer describe the same torrent file, Tiramisu MUST NOT silently serve the newly indexed content. The projection becomes unhealthy (`source_mismatch`) until explicit reconciliation.

---

# 5. Persistent projection registry

## 5.1 Authority

For audio, the metadb projection registry is the source of truth for:

- ownership;
- per-file idempotency;
- path/source conflicts;
- List;
- exact-path Remove;
- audio torrent reference counting;
- startup reconciliation;
- recovery after interrupted mutation;
- protection from movie/TV orphan cleanup;
- stable mtime;
- health reporting.

Filename parsing alone MUST NOT be authoritative for audio ownership, even though the `_hash8` convention remains required.

## 5.2 Recommended schema

```sql
CREATE TABLE audio_projections (
    id                INTEGER PRIMARY KEY,

    section           TEXT NOT NULL
                      CHECK(section IN ('music', 'audiobooks')),
    virtual_path      TEXT NOT NULL,
    portable_path_key TEXT NOT NULL,

    hash              TEXT NOT NULL,
    file_index        INTEGER NOT NULL CHECK(file_index > 0),
    source_path       TEXT NOT NULL,
    size              INTEGER NOT NULL CHECK(size > 0),
    mtime_ns          INTEGER NOT NULL,

    title             TEXT NOT NULL,
    magnet            TEXT,

    state             TEXT NOT NULL
                      CHECK(state IN ('staged', 'committed', 'removing')),
    txn_id            TEXT,
    staging_name      TEXT,

    created_at_ns     INTEGER NOT NULL,
    updated_at_ns     INTEGER NOT NULL,

    UNIQUE(section, virtual_path),
    UNIQUE(section, portable_path_key),
    UNIQUE(hash, file_index)
);

CREATE INDEX idx_audio_projections_hash
    ON audio_projections(hash);

CREATE INDEX idx_audio_projections_section_path
    ON audio_projections(section, virtual_path);

CREATE INDEX idx_audio_projections_state
    ON audio_projections(state);

CREATE INDEX idx_audio_projections_txn
    ON audio_projections(txn_id);
```

The registry SHOULD NOT duplicate the inode number. Tiramisu's existing inode map remains authoritative for inode assignment; audio commits eagerly register the path against the existing `hash:file_index` identity.

The stream URL SHOULD be recomputed from `(hash,file_index)` using the existing stream URL primitive rather than persisted as a second potentially stale source of truth. For a committed audio projection, VFS Lookup and Open MUST resolve the torrent identity, size, and modification time from the committed registry row. A physical stub is a placeholder and MUST NOT override that identity after reconciliation, including after a metadata-cache eviction or an on-disk stub replacement.

## 5.3 Reference counting

A row in `staged`, `committed`, or `removing` state counts as a live audio reference for torrent-drop decisions.

A torrent MUST NOT be dropped while any valid reference exists in any media kind.

Before dropping a torrent, Tiramisu MUST consider:

1. committed/in-flight audio projection registry rows;
2. current movie references;
3. current TV references;
4. any other existing Tiramisu ownership source that currently protects the torrent.

Existing movie/TV cleanup MUST consult audio ownership before dropping a shared hash.

### 5.3.1 Administrative removal exemption

> **Maintainer decision, 2026-09-19.** Added after an adversarial review found that
> GoStorm's embedded admin API removes torrents without consulting audio ownership.

The obligations above bind **Tiramisu's own automatic lifecycle decisions**: the
Library API, the sync engines, the reapers, the orphan sweeps, and the FUSE unlink
handler. They do **not** bind explicit administrative removal through GoStorm's own
API (`POST /torrents` with `rem`, `drop`, or `wipe`).

That API is the engine's operator surface, not a cleanup path. Requiring it to
consult the audio projection registry would push a Tiramisu-level concept into a
general-purpose torrent engine and invert the layering `CONTRIBUTING.md` protects.
An operator calling `rem` is performing a deliberate act, in the same sense `rm` is
deliberate; it is not Tiramisu inferring that a torrent is unused.

Consequences that MUST be documented rather than guarded against:

- `rem` and `wipe` delete persistent torrent state and can therefore strand a
  committed audio projection, leaving a listed file whose bytes are unavailable
  until the torrent is re-added. `drop` only evicts the running instance, so a
  later Open can re-add it, but an active reader can be interrupted immediately.
- The Library API's `remove` is the supported removal path for anything Tiramisu
  projects. Documentation MUST say so.

If belt-and-braces protection is wanted later, the conforming shape is an optional
engine-side hook (for example `torr.SetDropGuard(func(hash string) bool)`) that
defaults to permitting the drop and that Tiramisu wires at startup — keeping the
registry dependency outside GoStorm. That is a separate change requiring its own
issue.

---

# 6. Library API

## 6.1 General API rules

The audio API uses existing Library API routes where possible.

Canonical audio API types are only:

```text
music
audiobook
```

Aliases such as `audio`, `album`, `track`, `book`, or `audiobooks` MUST NOT be accepted as API types.

For audio requests, JSON decoding MUST reject:

- unknown fields;
- trailing JSON values;
- duplicate keys where the decoder can detect them;
- movie/TV-only semantic fields;
- invalid field types.

This strictness MUST be scoped so that existing movie/TV decoding semantics are not changed incidentally.

The existing request body size limit MUST be preserved or made stricter; Phase 1 MUST NOT increase it.

An audio Add MUST contain between 1 and 512 `files` entries.

Duplicate `source_path` entries inside one request MUST return `400 duplicate_source`.

Duplicate destination `path` entries inside one request MUST return `400 duplicate_destination`.

Movie/TV Add requests containing the newly reserved `files` field SHOULD be rejected as invalid rather than silently ignored, but no other movie/TV request/response semantics may change.

## 6.2 Torrent identity fields

Audio Inspect and Add requests use:

- `hash` — required canonical torrent identity; normalize to lowercase 40-character hexadecimal;
- `title` — required caller label for audio; it does not derive the virtual path, but it is used as the magnet `dn`, persists in the generated stub/magnet state, and is the torrent title shown by GoStorm;
- `magnet` — optional but recommended for a cold torrent; if present, its BTIH MUST identify the same torrent as `hash`;
- `metadata_wait` — optional integer seconds, default `60`, minimum `1`, maximum `300`.

A hash/magnet mismatch MUST return `400 hash_magnet_mismatch`.

## 6.3 Inspect

### Request

```http
POST /api/library/inspect
Content-Type: application/json
```

```json
{
  "title": "Artist - Album",
  "hash": "0123456789abcdef0123456789abcdefa1b2c3d4",
  "magnet": "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdefa1b2c3d4&dn=Artist%20-%20Album",
  "metadata_wait": 60
}
```

`magnet` MAY be omitted when the engine can already resolve the torrent from existing runtime state.

### Semantics

Inspect MUST:

1. validate torrent identity;
2. ensure/wake the torrent as necessary;
3. wait up to `metadata_wait` for metadata;
4. return the GoStorm-resolved file list;
5. create no library projection and no audio registry row.

Inspect MAY leave ordinary transient GoStorm torrent residency to the existing engine lifecycle; torrent residency is not library ownership.

### Success response

```json
{
  "hash": "0123456789abcdef0123456789abcdefa1b2c3d4",
  "metadata_state": "ready",
  "files": [
    {
      "file_index": 1,
      "source_path": "Release/01 - Track.flac",
      "size": 31234567,
      "extension": ".flac"
    },
    {
      "file_index": 2,
      "source_path": "Release/02 - Track.flac",
      "size": 32555123,
      "extension": ".flac"
    }
  ]
}
```

`file_index` is diagnostic/resolved state, not the selector expected back from the caller.

A metadata timeout MUST NOT masquerade as a ready torrent with `files: []`.

Recommended timeout response:

```http
504 Gateway Timeout
```

```json
{
  "error": "torrent metadata was not available before metadata_wait expired",
  "code": "metadata_not_ready",
  "details": {
    "hash": "0123456789abcdef0123456789abcdefa1b2c3d4",
    "metadata_state": "not_ready",
    "retryable": true
  }
}
```

## 6.4 Add

### Request

```http
POST /api/library/add
Content-Type: application/json
```

```json
{
  "type": "music",
  "title": "Artist - Album",
  "hash": "0123456789abcdef0123456789abcdefa1b2c3d4",
  "magnet": "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdefa1b2c3d4&dn=Artist%20-%20Album",
  "metadata_wait": 60,
  "files": [
    {
      "source_path": "Release/01 - Track.flac",
      "path": "Artist/Album/01 - Track_a1b2c3d4.flac"
    },
    {
      "source_path": "Release/02 - Track.flac",
      "path": "Artist/Album/02 - Track_a1b2c3d4.flac"
    }
  ]
}
```

`path` is always section-relative. It MUST NOT include `music/` or `audiobooks/` itself.

### Validation order

Before any new projection becomes visible, Add MUST validate the entire request:

1. JSON/type validity;
2. hash/magnet/title;
3. destination path safety and `_hash8` suffix;
4. duplicate sources/destinations;
5. metadata availability;
6. exact `source_path` resolution;
7. resolved `file_index` and size;
8. section extension allowlist;
9. source/destination extension equality;
10. registry idempotency/conflicts;
11. filesystem destination collision state.

If any requested projection is invalid or conflicts, no new projections from that request may be committed.

Exact pre-existing projections are not modified during rollback.

### Success response

If at least one projection is created, return `201 Created`:

```json
{
  "type": "music",
  "hash": "0123456789abcdef0123456789abcdefa1b2c3d4",
  "already_present": false,
  "files": [
    {
      "path": "Artist/Album/01 - Track_a1b2c3d4.flac",
      "source_path": "Release/01 - Track.flac",
      "file_index": 1,
      "size": 31234567,
      "mtime": "2026-09-16T20:00:00.000000000Z",
      "state": "created"
    },
    {
      "path": "Artist/Album/02 - Track_a1b2c3d4.flac",
      "source_path": "Release/02 - Track.flac",
      "file_index": 2,
      "size": 32555123,
      "mtime": "2026-09-16T20:00:00.000000000Z",
      "state": "created"
    }
  ]
}
```

If all requested projections already exist with identical identity, return `200 OK` and `already_present: true`.

A partially idempotent request may return a mixture of `present` and `created`:

```json
{
  "type": "music",
  "hash": "0123456789abcdef0123456789abcdefa1b2c3d4",
  "already_present": false,
  "files": [
    {
      "path": "Artist/Album/01 - Track_a1b2c3d4.flac",
      "source_path": "Release/01 - Track.flac",
      "file_index": 1,
      "size": 31234567,
      "mtime": "2026-09-16T20:00:00.000000000Z",
      "state": "present"
    },
    {
      "path": "Artist/Album/02 - Track_a1b2c3d4.flac",
      "source_path": "Release/02 - Track.flac",
      "file_index": 2,
      "size": 32555123,
      "mtime": "2026-09-16T20:00:00.000000000Z",
      "state": "created"
    }
  ]
}
```

An idempotent `present` result MUST NOT rewrite the stub, modify mtime, reassign inode, or invalidate the path unnecessarily.

## 6.5 Exact idempotency and conflict table

| Situation | Required result |
|---|---|
| Same section/path + same hash/file index/source path | `present`; no rewrite or metadata churn |
| Same section/path + different hash | `409 path_conflict` |
| Same section/path + same hash but different resolved file index | `409 path_conflict` |
| Same `(hash,file_index)` at another audio path | `409 source_already_projected` |
| Same torrent + additional unprojected source files | Create normally |
| Some `present`, some new | Validate whole request; commit all new or none |
| Any conflict in mixed request | Commit no new rows/files |
| Duplicate source within request | `400 duplicate_source` |
| Duplicate destination within request | `400 duplicate_destination` |
| Stored source path/index no longer agree | `source_mismatch`; never silently rebind |

### Conflict example

```http
409 Conflict
```

```json
{
  "error": "virtual path is already bound to different content",
  "code": "path_conflict",
  "details": {
    "section": "music",
    "path": "Artist/Album/01 - Track_a1b2c3d4.flac",
    "existing": {
      "hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1b2c3d4",
      "file_index": 7
    },
    "requested": {
      "hash": "0123456789abcdef0123456789abcdefa1b2c3d4",
      "file_index": 1
    }
  }
}
```

## 6.6 List

Audio List MUST be scalable to at least tens of thousands of projections without requiring the complete section to be materialized in memory.

Recommended interface:

```http
GET /api/library/list?type=music&prefix=Artist/Album/&limit=200&cursor=<opaque>
```

For audio:

- `limit` default: `200`;
- `limit` maximum: `1000`;
- sorting: `virtual_path` ascending using deterministic binary ordering;
- pagination: cursor/keyset, not large-offset pagination;
- `prefix`: optional section-relative path prefix subject to safe prefix validation;
- cursor: opaque to callers.

Audio response:

```json
{
  "type": "music",
  "items": [
    {
      "path": "Artist/Album/01 - Track_a1b2c3d4.flac",
      "hash": "0123456789abcdef0123456789abcdefa1b2c3d4",
      "source_path": "Release/01 - Track.flac",
      "file_index": 1,
      "size": 31234567,
      "mtime": "2026-09-16T20:00:00.000000000Z",
      "health": "ok",
      "torrent_present": true
    }
  ],
  "next_cursor": null
}
```

Recommended health values:

```text
ok
stub_missing
stub_invalid
source_mismatch
recovery_required
```

`torrent_present: false` is not by itself an unhealthy projection; runtime torrent residency is transient.

Existing movie/TV List response behavior MUST NOT be changed merely to make the audio response scalable.

## 6.7 Remove

Phase 1 audio removal is exact-path only:

```http
POST /api/library/remove
Content-Type: application/json
```

```json
{
  "type": "audiobook",
  "path": "Author/Book/01 - Part_a1b2c3d4.mp3"
}
```

Success:

```json
{
  "type": "audiobook",
  "path": "Author/Book/01 - Part_a1b2c3d4.mp3",
  "removed": true,
  "torrent_referenced": true
}
```

Repeated removal of an already absent audio projection SHOULD be idempotent:

```json
{
  "type": "audiobook",
  "path": "Author/Book/01 - Part_a1b2c3d4.mp3",
  "removed": false,
  "state": "absent"
}
```

Audio Remove MUST:

1. remove exactly one projection;
2. remove/invalidate its namespace and cache state;
3. remove its registry ownership;
4. prune empty parent directories without removing the section root;
5. retain the torrent while any other projection/reference exists;
6. drop/expire the torrent only through the existing engine policy once no references remain.

For audio, `blacklist` MUST NOT be lifecycle state. An audio remove request containing `blacklist` MUST be rejected as inapplicable.

Hash removal, prefix removal, recursive directory removal, and `paths: []` batch removal are Phase 2 conveniences.

> **Amendment, 2026-09-22 — prefix removal is pulled forward.** The first
> production library (397 albums, 5331 projections, roughly a third on dead
> swarms) showed that the album, not the track, is the only unit anyone removes,
> and that the exact-path form makes that operation cost one call per track.
> Album-granular removal by section-relative prefix is therefore in scope, with
> component-wise matching, a `409` when the rows under the prefix do not share
> exactly one hash, and the per-row state machine of §8 unchanged. The other
> three forms above remain Phase 2. See `PR3_CARRIED_ITEMS.md` §7.

## 6.8 Error envelope

New audio/inspect errors SHOULD use this backward-friendly shape:

```json
{
  "error": "human-readable message",
  "code": "stable_machine_code",
  "field": "files[0].path",
  "details": {}
}
```

`error` remains a string for compatibility with current Library API expectations; `code`, `field`, and `details` add machine-readable behavior.

Required stable codes include:

```text
invalid_type
title_required
invalid_hash
hash_magnet_mismatch
metadata_not_ready
missing_files
too_many_files
unknown_field
invalid_path
path_escape
invalid_hash_suffix
invalid_extension
extension_mismatch
duplicate_source
duplicate_destination
source_not_found
ambiguous_source_path
path_conflict
source_already_projected
projection_busy
```

---

# 7. Failure-atomic add and crash recovery

## 7.1 Meaning of atomic

> **Maintainer review delta:** the earlier issue agreement required request failure atomicity but explicitly avoided promising that all final names become visible as one filesystem transaction. This proposal still makes no POSIX multi-rename claim, but strengthens scanner-visible publication by gating the batch on registry commit so an uncommitted partial batch is not enumerated as committed library state.

Phase 1 MUST provide **request failure atomicity and deterministic crash recovery**.

It MUST NOT claim that arbitrary final filenames in multiple directories become visible in one POSIX filesystem transaction.

For audio, FUSE visibility is registry-gated: only `committed` projections are visible. This allows individual filesystem renames to occur before the registry batch becomes visible.

## 7.2 Required add sequence

A safe implementation sequence is:

```text
strict request validation
        |
canonicalize hash / validate magnet
        |
acquire ordered path locks + hash lock
        |
resolve torrent metadata
        |
resolve every source_path -> file_index / size
        |
validate every destination + registry conflict
        |
classify present vs new
        |
allocate txn_id + stable mtime for new rows
        |
transaction: insert NEW rows as STAGED
        |
create hidden per-directory staging stubs
        |
fsync as required
        |
rename each staging stub to final name with NO-REPLACE
        |
transaction: mark the entire new batch COMMITTED
        |
eager inode registration + cache/dentry invalidation
        |
return success
```

A staging filename MUST NOT satisfy the audio VFS predicate and MUST NOT be visible to downstream scanners.

Final stubs whose registry rows are not `committed` MUST NOT be exposed by audio Readdir/Lookup/Open.

## 7.3 Collision creation

Final-name creation MUST be no-replace.

On Linux, `renameat2(..., RENAME_NOREPLACE)` is the preferred commit primitive. A concurrent requester MUST never overwrite an existing final path.

## 7.4 Crash rules

Startup MUST reconcile durable mutation state before advertising audio paths.

For a `staged` transaction:

- if **all** expected final stubs exist and validate against all staged rows, startup MAY promote the whole transaction to `committed`;
- otherwise startup MUST roll back the transaction as a unit, deleting only files that can be proven to belong to that transaction, then deleting staged rows.

For a `committed` row whose stub is missing or invalid:

- Tiramisu MUST recreate/repair the stub from registry identity and current deterministic stub-building logic when safe;
- restored physical and virtual mtime MUST use `mtime_ns`, not the recovery time.

For a physical audio-looking file with no registry row:

- Tiramisu MUST NOT claim it merely from filename shape;
- it MUST NOT expose it as a virtual audio projection;
- it SHOULD log it for reconciliation;
- automatic deletion is allowed only when the filename/state proves it is Tiramisu-owned staging/tombstone debris.

---

# 8. Remove state machine and invalidation

> **Maintainer review delta:** the previously accepted removal sketch closed active handles during removal. The compatibility research instead recommends ordinary open-file lifetime semantics: namespace removal prevents new opens, while a handle that already opened the immutable object remains valid until close. This is an intentional proposed change and requires maintainer approval.

A remove operation SHOULD use a durable `removing` state.

Recommended sequence:

```text
lookup exact committed row
        |
acquire path/hash lock
        |
mark row REMOVING
        |
unpublish namespace; prevent new opens
        |
rename final stub to hidden same-directory tombstone
        |
delete registry ownership row
        |
unlink tombstone
        |
prune empty dirs
        |
invalidate directory/metadata caches
        |
check cross-media + live-handle torrent references
        |
defer runtime-object destruction until last open handle closes
```

A crash with a `removing` row MUST resume/finish removal; startup MUST NOT restore it as a committed projection.

Removal changes **namespace ownership**, not the identity of already-open file descriptions. Once Open has succeeded, the handle MUST continue reading the same immutable `(hash,file_index,size)` object until Release/close. Tiramisu MAY retain a transient runtime reference to the backing torrent after registry ownership is removed so that an existing reader can finish. New Lookup/Open operations after namespace invalidation MUST behave as absent. No open handle may be silently retargeted if a different projection later occupies another path.

---

# 9. Startup and reconciliation

Startup MUST become section-aware without changing existing movie/TV semantics.

For audio it MUST:

1. migrate/open the projection registry;
2. recover `staged` and `removing` transactions;
3. reconcile committed registry rows to physical stubs;
4. validate audio stub metadata against registry size/identity;
5. eagerly register audio inode mappings;
6. build directory metadata/cache state from the same committed predicate used by VFS operations;
7. protect all referenced audio hashes from cleanup/drop logic.

Audio recovery is bidirectional:

```text
registry -> physical stub
physical stub -> registry
```

but the registry remains authoritative. A missing stub can be reconstructed; an unknown stub is not automatically adopted.

If GoStorm metadata is available, startup/recovery SHOULD verify that persisted `source_path` still resolves to persisted `file_index`. A mismatch MUST become `source_mismatch`, not a content substitution.

Startup readiness is externally observable. Tiramisu MUST NOT report the audio mount/section as ready for a downstream refresh while reconciliation can still cause committed projections to appear incrementally. Until the committed namespace is coherent, directory operations MAY fail as a whole or readiness MAY remain false; they MUST NOT succeed with a plausible but incomplete restored tree.

---

# 10. VFS and regular-file contract

## 10.1 Extension-based discriminator

The VFS MUST remain extension/section based.

Conceptually:

```text
movies, tv     => .mkv
music          => .flac
audiobooks     => .m4b | .m4a | .mp3
```

Audio validity additionally requires a committed registry row for the exact section-relative path.

Content sniffing MUST NOT be introduced into Lookup/Readdir/Open.

## 10.2 Predicate parity

Audio Readdir, Lookup, Getattr, and Open MUST use the same committed-projection predicate.

Under stable state it MUST NOT be possible for:

```text
Readdir -> name exists
Lookup  -> ENOENT
```

or for Lookup/Open to expose a path Readdir would never advertise.

## 10.3 Directory enumeration and namespace publication

Projected audio directories are ordinary searchable/readable directories and SHOULD advertise mode `0555`.

For a quiescent committed registry, a successful `readdir` MUST represent the complete committed Tiramisu namespace for that directory at a coherent logical point in time. Internal registry contention, GoStorm activity, cache state, or unavailable torrent payload bytes MUST NOT cause an otherwise committed child to be silently omitted from a successful listing.

If Tiramisu cannot produce a coherent complete listing, failing the directory operation is safer than returning a plausible partial snapshot.

Add and Remove MUST publish namespace changes atomically from an ordinary scanner's perspective:

```text
After Add reports success:
    readdir(parent) contains virtual_path
    Lookup/Getattr(virtual_path) succeeds
    st_size is the persisted immutable source size
    Open(O_RDONLY) addresses that same committed source

After Remove reports success:
    new readdir/Lookup/Open no longer expose the path
    already-open handles remain bound to the old immutable object until close
```

A scanner MUST NOT observe a newly listed entry before its stable attributes and backing projection are usable.

## 10.4 Byte correctness

For every healthy committed projection:

- `st_size` MUST exactly equal the selected torrent file size;
- every byte returned at every offset MUST match the torrent source;
- arbitrary `pread` patterns MUST work;
- head-first, tail-first, and alternating seeks MUST work;
- concurrent opens MUST return identical correct data;
- a read crossing EOF returns only bytes before EOF;
- a read beginning at or beyond EOF returns zero bytes without an I/O error;
- a successful blocking read beginning at offset `< st_size` MUST NOT return zero merely because the required torrent range is not hydrated yet;
- positive short reads are legal regular-file behavior when forward progress is made, but internal piece unavailability MUST become waiting/retry within the absolute deadline or a documented I/O error rather than synthetic EOF.

Tiramisu's existing protection against non-EOF short reads becoming persistent zero-filled page-cache data MUST be preserved for audio.

## 10.5 Stable file and directory metadata

For an unchanged projection across restart:

```text
path   MUST remain identical
inode  MUST remain identical
size   MUST remain identical
mtime  MUST remain identical
```

The inode identity remains derived from `hash:file_index` using the current inode map. Audio commits MUST eagerly register the full path so basename fallback is not used.

Intermediate directory inodes SHOULD remain stable across restart using the existing directory inode strategy.

A valid unchanged audio projection MUST NOT use `time.Now()` as a stat fallback.

Visible file identity is **hydration-invariant**. Cache fill, piece completion, reader activity, read-ahead, Wake operations, playback, and successful probing MUST NOT modify the projection's visible path, inode, file type, size, or mtime.

Directory mtimes SHOULD follow ordinary child-set semantics:

- a committed Add/Remove that changes the directory's children updates the affected directory mtime;
- hydration-only activity does not;
- restart/reconciliation of an unchanged committed tree MUST NOT fabricate child-change mtimes.

Directory mutation time MUST survive restart by a durable mechanism (for example persisted directory metadata or an equivalent deterministic durable representation). Rebuilding a directory with `time.Now()` is not conforming because it would make an unchanged tree look modified after every restart.

`atime`, if exposed, MUST NOT be used by Tiramisu as projection identity and is not part of the Phase 1 stability contract.

## 10.6 File-size validation

> **Maintainer review delta:** the issue previously accepted a smaller per-section audio floor (described as a few MiB / a per-section floor). This proposal replaces an arbitrary minimum-size heuristic with structural validation plus exact registry/source-size agreement so legitimate very short tracks or book parts are not rejected solely for being small.

The current movie-sized `100 MiB` minimum MUST NOT apply to audio.

For audio, the corruption guard is stronger and identity-based:

- persisted size MUST be greater than zero;
- stub size MUST be structurally valid;
- registry size MUST equal the resolved torrent file size;
- VFS `st_size` MUST equal that same size.

Phase 1 MUST NOT impose an arbitrary multi-megabyte minimum that would reject valid short tracks or book parts.

---

# 11. Read-only filesystem semantics

Projected audio files MUST advertise mode `0444` and projected audio directories SHOULD advertise `0555`.

Required behavior:

| Operation | Result for managed audio |
|---|---|
| `open(O_RDONLY)` | success |
| `open(O_WRONLY)` | `EROFS` |
| `open(O_RDWR)` | `EROFS` |
| `open(...O_TRUNC...)` | `EROFS` |
| `Write` | `EROFS` |
| `Create` | `EROFS` |
| `Rename` | `EROFS` |
| `Setattr` attempting mutation | `EROFS` |
| `Mkdir` through FUSE | `EROFS` |
| `Unlink` managed audio | `EPERM` |
| Library API Remove | supported lifecycle mechanism |

The API may create/prune underlying engine-owned directories; clients using the mount may not mutate them.

---

# 12. Read failure, latency, and concurrency contract

## 12.1 Principle

For a healthy torrent under the declared supported load, temporary internal Tiramisu backpressure MUST NOT appear to an ordinary blocking filesystem client as a final `EAGAIN`, `EIO`, or premature `ETIMEDOUT`.

A media scanner has not opted into a nonblocking retry protocol. Internal queue pressure is an implementation detail.

A failed read MUST NOT mutate projection identity or mark the projection semantically absent. A later independent Open/Read or explicit downstream rescan MUST be able to retry if the backing torrent becomes healthy again.

## 12.2 Outward errno contract

| Condition | Required outward result |
|---|---|
| Internal slot/rate/pump contention that resolves before deadline | transparent wait/retry, then successful read |
| Explicit FUSE/client cancellation | `EINTR` |
| Data unavailable through the absolute read deadline | `ETIMEDOUT` |
| Persistent projection/source identity mismatch or unrecoverable corruption | `EIO` |
| Offset at/after EOF | zero-byte success |
| Internal "try again later" while blocking semantics are in use | MUST NOT surface as final `EAGAIN` under supported load |

## 12.3 Absolute deadline

Each FUSE Read MUST operate under one absolute deadline propagated through:

- master slot acquisition;
- rate limiting;
- Wake;
- fetch/pump operations;
- reposition/retry;
- non-EOF short-read refill.

Independent nested timeouts MUST NOT stack into an unexpectedly much longer operation or terminate early solely due internal contention.

**Phase 1 default absolute read deadline: 30 seconds.** It SHOULD be internally configurable without changing the Library API. Acceptance testing may justify changing the default before merge, but the single-deadline model is normative.

## 12.4 No automatic audio deletion from read failure

A dead/slow audio torrent MAY make reads time out. Tiramisu MUST NOT semantically remove the audio projection because of that read result. The external application/lifecycle owner controls replacement or removal policy.

## 12.5 Scanner/playback concurrency acceptance

> **Maintainer review delta:** the issue previously split projection-layer scanner failures (merge-blocking) from scheduler saturation under many small readers (measure first, scope separately). This proposal deliberately tightens that boundary: the reference workload below is treated as a Phase 1 correctness workload, while still limiting any required scheduler work to the smallest change needed to preserve ordinary blocking-file semantics. Broader scheduler redesign remains Phase 2.

The Phase 1 reference stress case is:

```text
active 4K movie playback
+
cold 32-part MP3 audiobook
+
Audiobookshelf full scan of that book
```

The implementation MUST demonstrate:

- all 32 audiobook probes succeed for a healthy torrent;
- scanner-visible `EAGAIN == 0`;
- projection-induced scanner-visible `ETIMEDOUT == 0`;
- projection-induced scanner-visible `EIO == 0`;
- active movie playback does not experience an engine-induced playback stall/buffering event;
- Tiramisu remains responsive.

Phase 1 does not require a general scheduler redesign. If the current scheduler cannot satisfy this correctness workload, the smallest scheduling/fairness correction required to pass it becomes Phase 1; broader tuning remains Phase 2.

The generic capacity interpretation is broader than Audiobookshelf: normal blocking readers MUST remain correct and make progress under **dozens of independent short-lived random-read streams while foreground playback is active**. The 32-part audiobook case is the reference test, not an application-specific scheduler API.

---

# 13. Warmup and caching

Music and audiobook projections MUST skip the movie/TV SSD head and tail warmup path.

Phase 1 MUST NOT add a separate audio metadata-region warmup or mini-prefetch mechanism.

Normal demand-driven fetching and the existing RAM read-ahead/cache machinery MAY operate unchanged where compatible.

Cold audio Open/Wake latency is acceptable as a performance characteristic only while it stays within the bounded read contract and does not cause scanner failures.

Cache hydration MUST NOT change scanner-visible file identity or directory membership. Directory and attribute caches MUST be invalidated by committed namespace mutations, not by ordinary torrent hydration.

Small-file warmup constants, audio-specific prefetch, and next-track prefetch are Phase 2 optimization topics.

---

# 14. Path and filesystem security

Caller-controlled destination paths are hostile input regardless of API authentication.

## 14.1 Destination path syntax

`files[].path` and audio Remove `path` MUST:

- be valid UTF-8;
- be section-relative;
- use `/` as separator;
- be in Unicode NFC form already; Tiramisu rejects rather than silently rewrites non-NFC paths;
- have no empty components;
- have no `.` or `..` components;
- have no leading-dot components;
- contain no NUL;
- contain no C0/C1 control characters;
- contain no `\`, `:`, `*`, `?`, `"`, `<`, `>`, or `|`;
- have no component ending in a period or space;
- avoid Windows reserved device basenames (`CON`, `PRN`, `AUX`, `NUL`, `COM1`…`COM9`, `LPT1`…`LPT9`) case-insensitively;
- contain at most 16 path components;
- have each component at most 255 UTF-8 bytes;
- have a total encoded path length at most 4096 bytes.

The final component must also satisfy the section extension and `_hash8` rules.

## 14.2 Portable collision key

For every accepted destination path, Tiramisu MUST compute a non-user-visible `portable_path_key` from:

1. NFC-normalized path components;
2. Unicode default case folding;
3. `/` component joining.

A new projection whose `portable_path_key` collides in the same section MUST be rejected even on a case-sensitive Linux filesystem. The original caller spelling remains the visible `virtual_path`.

## 14.3 Symlink and TOCTOU defense

String-prefix containment checks alone are insufficient.

All mutation operations MUST be anchored to a pre-opened section-root directory descriptor and MUST prevent symlink traversal/races.

Preferred Linux implementation:

- `openat2` with `RESOLVE_BENEATH` and `RESOLVE_NO_SYMLINKS` for path walking/opens;
- `mkdirat` under retained directory fds;
- exclusive staging creation;
- `renameat2(RENAME_NOREPLACE)` for final commit.

A fallback may walk components using dirfd-relative `openat`/`fstatat` with no-follow semantics. A fallback MUST NOT validate a string path with `Lstat` and later reopen it through an unrelated path string, because that recreates the TOCTOU window.

## 14.4 Resource exhaustion

Phase 1 MUST cap:

- request body size (at or below the current Library API limit);
- files per Add: 512;
- path depth: 16;
- component bytes: 255;
- total path bytes: 4096.

`ENOSPC`, inode exhaustion, fsync failure, or rename failure MUST enter the normal batch rollback/recovery path.

## 14.5 Authentication boundary

Filesystem/path hardening is mandatory in Phase 1.

Adding authentication to `:9080` is a separate project concern. Documentation MUST state that mutation endpoints are to be exposed only on a trusted/restricted network unless a separate authentication layer protects them.

---

# 15. Downstream scanner compatibility

## 15.1 General contract

Tiramisu does not need to optimize every scanner feature in Phase 1, but ordinary scans MUST be functionally correct.

A supported scanner MUST NOT silently lose a healthy item because Tiramisu exposed avoidable transient engine contention as a filesystem error or returned a successful incomplete namespace snapshot.

Sidecars are out of scope; therefore embedded audio metadata/artwork and downstream metadata services are the supported Phase 1 path.

Scanner-specific observations are qualification evidence, not new engine APIs. The reusable contract is ordinary read-only filesystem behavior: coherent recursive enumeration, stable metadata, exact immutable bytes, random access, bounded blocking hydration, correct EOF, stable open handles, and explicit controller-triggered refresh.

## 15.2 Primary qualification matrix

This matrix is **qualification evidence for the engine contract**, not a place to encode media-server policy. Server configuration, naming conventions, provider metadata, and scan orchestration belong to the external application/policy layer and the media server.

| Consumer | Filesystem behavior Phase 1 must qualify |
|---|---|
| Navidrome | Stable path/size/mtime and directory metadata; reliable FLAC tag/art reads; random seek/EOF correctness; no projection-induced transient metadata-read failures |
| Audiobookshelf | Reliable ffprobe-style reads; M4B/M4A tail/random access; multipart MP3; stable metadata; concurrent probes behave like ordinary blocking-file reads |
| Jellyfin | Coherent recursive enumeration; correct ffprobe/chapter/random reads; ordinary playback/seeking; a later explicit scan can recover after a real backing read failure without fabricated identity changes |
| Plex/Plexamp | Stable paths and immutable bytes; arbitrary scanner reads/seeks; correct playback; empirical qualification because scanner internals are comparatively opaque |

## 15.3 Additional natural-compatibility targets

The broader survey found no additional application-specific engine capability that should enter Phase 1. It did identify useful qualification targets that exercise different points in the generic contract:

| Target | Why it is useful | Qualification posture |
|---|---|---|
| Gonic | Straightforward recursive walk + stat + TagLib; path/mtime incremental behavior; low scanner concurrency; explicit Subsonic scan API | High-priority Class A target |
| OwnTone | Ordinary readable directories plus explicit/manual rescan even when filesystem notifications are unavailable | High-priority Class A proof of explicit-refresh architecture |
| Koel | Path/mtime identity with a modest configurable worker pool | Class A mid-concurrency target |
| Lyrion Music Server | Conventional music scan using size/mtime and explicit rescan controls; different player ecosystem | Class A target |
| Lightweight Music Server | Potentially high scanner concurrency and optional MusicNN whole-file analysis | Class A under tag-centric profile; stress target for expensive analysis |
| Ampache | Conventional local-catalog model | Secondary Class A target |
| Emby / Gerbera | Conventional roots, but scanner internals are less certain or less valuable for core qualification | Class B empirical targets |
| mStream | Custom hashing and optional analysis can intentionally hydrate substantial content | Class B adversarial/performance target |
| Funkwhale | Import/in-place-import workflow rather than a pure point-at-root scanner | Class C controller-integration target, not an engine target |
| Storyteller | Transformation/alignment workflow is fundamental | Class D non-target for generic Phase 1 |

No candidate in the broader survey displaces Audiobookshelf as the dedicated audiobook qualification reference.

## 15.4 Metadata, naming, and verification boundary

Tiramisu projects the selected source bytes unchanged. It MUST NOT rewrite audio tags, synthesize media metadata, query MusicBrainz/AudioSilo/retailer metadata, choose a canonical album/book/recording identity, score candidate releases, or enforce server-specific naming conventions. Those are external application/policy responsibilities.

The engine MAY validate only facts necessary to preserve its own projection contract, such as exact torrent-relative source path, resolved file identity, byte size, extension agreement, safe final path, mandatory hash suffix, and collision/immutability rules.

Whether a source contains sufficient embedded metadata for a particular media server is therefore an **external verification/selection concern**, not an Add-time Tiramisu policy. The engine's responsibility is to preserve those source bytes exactly once selected.

## 15.5 Sidecar consequences

Phase 1 does not project:

```text
cover.jpg
folder.jpg
metadata.json
.opf
.lrc
.cue
desc.txt
```

Consequences are expected and MUST be documented:

- external cover-art files are unavailable through Tiramisu; embedded art or downstream metadata must supply art;
- external lyrics files are unavailable;
- CUE-based splitting is unavailable;
- Audiobookshelf folder sidecar metadata is unavailable;
- folder-description sidecars are unavailable.

These limitations do not invalidate Phase 1 if supported audio files with adequate embedded metadata are discoverable and playable.

---

# 16. Downstream refresh ownership

Tiramisu MUST NOT depend on filesystem-watcher propagation from physical stub changes beneath the FUSE view as its correctness contract, and Phase 1 MUST NOT add Plex, Navidrome, Audiobookshelf, Jellyfin, or other media-server credentials/library identifiers to the engine for audio-library administration.

The engine boundary ends when the Library API mutation is committed and observable through its raw projection state. After that:

```text
Tiramisu mutation commits
        |
        v
external application/controller may update its own policy state
        |
        v
media server performs its own scan/metadata/playback behavior
```

An external deterministic controller, CLI/UI application, automation client, or optional agent skill MAY explicitly invoke a media server's supported scan operation as orchestration, but the endpoint choice, credentials, library identifiers, scan policy, retry policy, and server configuration are not Tiramisu responsibilities and MUST NOT be implemented in the FUSE daemon or Library API.

---

# 17. Backward compatibility

Phase 1 MUST preserve existing movie/TV behavior unless a generic internal primitive is extracted with no observable behavior change.

Regression protection includes:

- `.mkv` remains the movie/TV virtual-stub discriminator;
- movie/TV filename generation remains unchanged;
- movie/TV inode identity remains unchanged;
- current movie/TV warmup behavior remains unchanged;
- current movie/TV reapers/sync policy remain unchanged except that they must not drop hashes still referenced by audio;
- movie/TV blacklist behavior remains unchanged;
- existing movie/TV cache behavior and media-server refresh behavior remain unchanged;
- no global strict-JSON decoder change may reject previously accepted movie/TV payloads;
- existing Library API response schemas for movie/TV MUST NOT be reshaped merely for audio pagination.

Where possible, audio-specific behavior SHOULD be implemented behind section/type-aware generic helpers rather than broad mechanical renaming/refactoring of all existing `Mkv*` types.

---

# 18. Deterministic Phase 1 acceptance matrix

Every test below has a measurable pass/fail condition.

## 18.1 API and identity

| Test | Pass criterion |
|---|---|
| Inspect metadata-ready torrent | Complete file list; exact `source_path`, 1-based `file_index`, size; no projection created |
| Inspect metadata timeout | `504 metadata_not_ready`; never a false ready empty list |
| Hash/magnet mismatch | `400 hash_magnet_mismatch`; no state mutation |
| Add with missing `_hash8` | `400 invalid_hash_suffix`; no durable row/stub |
| Add source/dest extension mismatch | `400 extension_mismatch`; no durable row/stub |
| Add unsupported music MP3 | rejected in Phase 1 |
| Add supported audiobook MP3 | accepted if all other rules pass |
| Exact repeat Add | `200`; file state `present`; inode/mtime unchanged |
| Same path/different source | `409 path_conflict`; old projection unchanged |
| Same source at second path | `409 source_already_projected` |
| Mixed present/new request | all new entries created atomically; present entries untouched |
| Mixed request containing conflict | no new projections committed |

## 18.2 Raw virtual-file correctness

| Test | Pass criterion |
|---|---|
| Full sequential SHA-256 | Projected SHA-256 exactly equals source torrent-file SHA-256 |
| 1,000 deterministic random preads | Every byte exactly matches reference; no unexpected short reads |
| Exact size | `stat().st_size` equals torrent file size |
| EOF | last-byte, crossing-EOF, and at-EOF behavior matches regular-file semantics |
| No false EOF | injected backing delay at offset `< st_size` never produces successful zero-byte read; operation waits or returns documented error |
| Legal positive short read | parser/read loop still makes forward progress and receives exact bytes; no zero-fill/page-cache corruption |
| Cold head read | correct bytes; no final `EAGAIN`; completes within read deadline |
| Cold tail read | correct bytes; no final `EAGAIN`; completes within read deadline |
| 8 concurrent readers same file | zero mismatches and zero projection-induced I/O errors |
| Alternating head/tail seeks | all bytes correct; no stale/page-cache corruption |

## 18.3 Read-only semantics

| Test | Pass criterion |
|---|---|
| `O_WRONLY` | `EROFS` |
| `O_RDWR` | `EROFS` |
| `O_TRUNC` | `EROFS` |
| write/create/rename/setattr/mkdir | `EROFS` |
| mount-side unlink | `EPERM`; registry and projection unchanged |
| API Remove | succeeds per exact-path lifecycle contract |

## 18.4 Restart and metadata stability

| Test | Pass criterion |
|---|---|
| Normal restart | path/inode/size/mtime identical before and after |
| Directory restart stability | unchanged intermediate directory inode remains stable where current inode strategy supports it |
| Hydration invariance | reading/hydrating a file does not change path/inode/type/size/mtime |
| Directory mtime | Add/Remove changes affected parent mtime; hydration-only activity does not |
| Restart namespace readiness | scan cannot observe a successful partial restored tree; namespace is coherent before ready |
| Missing committed stub | startup repairs it and restores mtime |
| Unknown unregistered audio stub | not exposed or silently adopted |
| Stored `source_path`/index mismatch | projection reports `source_mismatch`; no wrong content served |

## 18.5 Atomicity and races

| Test | Pass criterion |
|---|---|
| Inject failure before staged rows | no projection state remains |
| Failure during staging | all transaction-owned staging files/rows rolled back |
| Crash after subset of final renames before commit | startup either promotes complete valid batch or rolls back whole new batch; no partial visible set |
| Crash after registry commit before cache invalidation | startup/lookup reconstructs correct visible set |
| Two requests, same absent path, different hashes | exactly one succeeds; other returns 409; no overwrite |
| Concurrent same-hash adds of different files | one torrent identity, correct independent projections, correct refcount |
| Concurrent add/remove same path | deterministic serialization; no stale visible path or lost row |
| Add publication | after Add success, readdir/stat/open all agree immediately on the same projection |
| Enumeration fault | injected registry/internal delay produces either complete successful listing or whole-operation failure, never silent omission |
| Remove while handle open | pathname disappears for new lookup/open; old handle continues reading the exact original bytes until close |

## 18.6 Security

| Test | Pass criterion |
|---|---|
| Absolute path | rejected |
| `..` traversal | rejected |
| Empty/`.` component | rejected |
| Symlink under section pointing outside | no creation/removal outside root, including raced replacement |
| Case-only path collision | second projection rejected |
| Unicode normalization-equivalent collision | second projection rejected |
| SMB-hostile/trailing-space/dot name | rejected |
| >512 files | rejected before filesystem mutation |
| Over-depth/overlength path | rejected before filesystem mutation |

## 18.7 Navidrome

| Test | Pass criterion |
|---|---|
| Cold FLAC album scan | every expected track imported with correct tags/duration; embedded art readable when present; zero Tiramisu read errors |
| Quick scan unchanged | unchanged projections do not trigger unnecessary reprocessing due Tiramisu file or directory timestamp churn |
| Hydration-only activity then quick scan | cached/piece-completed tracks are not reclassified as changed |
| Tiramisu restart then quick scan | unchanged album remains stable from scanner perspective |
| Parent directory mutation | Add/Remove changes appropriate directory state so explicit scan sees the child-set mutation |
| Transient slow-but-healthy torrent | no track silently omitted due Tiramisu internal contention |
| Failed tag read then later scan | later explicit scan can recover the item without requiring artificial metadata mutation |

## 18.8 Audiobookshelf

| Test | Pass criterion |
|---|---|
| Single M4B with chapters | correct duration/chapters; playback and seek work |
| 32-part MP3 book | all 32 files present after scan; deterministic ordering from tags/controller naming; zero failed probes |
| 4K playback + cold 32-part scan | 32/32 probes succeed; final `EAGAIN=0`; projection-induced timeout/EIO=0; movie does not stall |
| Genuinely unavailable torrent | read/probe fails only after bounded deadline; Tiramisu remains responsive; no indefinite hang |
| Torrent later recovers + rescan | item can be discovered without changing projection identity |

## 18.9 Jellyfin

| Test | Pass criterion |
|---|---|
| Cold tagged FLAC album | all items discovered with correct tags/runtime and embedded art; no projection-induced probe errors |
| Stable rescan | unchanged path/size/mtime avoids reprobe attributable solely to Tiramisu metadata drift |
| Tiramisu restart + scan | no item loss or reprobe caused merely by restarted projection metadata |
| Single M4B chapters | correct duration/chapter discovery; playback and random seek work |
| Multipart MP3/M4A | every controller-selected part remains browsable and deterministically ordered; test does not require Jellyfin to synthesize one composite file |
| Slow healthy read | zero final EAGAIN/timeout/EIO within supported deadline; no missing item |
| One failed initial probe then refresh | later ordinary refresh can reprobe and recover without changing projection path/mtime |
| Namespace enumeration fault | no successful partial listing; existing items are not spuriously removed because of hidden committed children |
| Music scan, LUFS disabled | baseline ordinary-scan hydration recorded |
| LUFS enabled | analysis reads correct complete content; no corruption; total file bytes and unique torrent bytes hydrated recorded as performance data |

## 18.10 Plex/Plexamp

| Test | Pass criterion |
|---|---|
| Music scan, expensive analysis disabled | all expected tracks grouped under controller naming policy and playable |
| Plexamp normal playback | continuous playback without projection errors |
| Repeated forward/back seeks | correct audio after every seek |
| Sonic analysis enabled | correct operation; time/bytes measured, not a Phase 1 efficiency gate |
| Loudness analysis enabled | correct operation; time/bytes measured |

## 18.11 Lifecycle/reference counting

| Test | Pass criterion |
|---|---|
| Remove one file from N-file torrent | only target disappears; torrent remains |
| Remove last audio file with no other refs | row/stub gone; torrent eligible for existing drop/expiry policy |
| Remove last audio file while movie/TV ref shares hash | torrent remains |
| Movie/TV cleanup while audio ref exists | audio remains readable; torrent not dropped |
| Remove then restart | removed path does not reappear |
| Remove last file in nested album/book dirs | empty dirs pruned; section root remains |

## 18.12 Qualification instrumentation

Acceptance builds SHOULD record operation-level telemetry sufficient to distinguish filesystem correctness from torrent hydration cost, without logging media payload bytes. At minimum:

```text
timestamp
section / projection identity / hash / file_index
operation: readdir | lookup | getattr | open | read | release
read offset / requested length / returned length
errno
time waiting for internal scheduler capacity
time waiting for torrent piece availability
total operation latency
```

The harness SHOULD separately record:

```text
filesystem bytes requested/read
unique torrent bytes or pieces newly hydrated
```

Those values answer different questions: a scanner may reread cached bytes many times while downloading little new data, or may touch sparse ranges that hydrate many torrent pieces.

## 18.13 Scale

Populate at least 50,000 registry rows and page the entire audio section.

Pass criteria:

- stable deterministic ordering;
- no duplicates or omissions across cursors;
- prefix filter correct;
- memory use bounded by page size rather than total library size;
- query latency does not degrade catastrophically with later pages (keyset pagination).

---

# 19. Implementation impact map

No implementation is prescribed here, but current source analysis identifies the following likely modification surfaces.

| Area | Likely functions/types | Phase 1 reason |
|---|---|---|
| `internal/library/manager.go` | request/response types, `validate`, `pickFile`, `Add`, idempotency, `findByHash`, `dropTorrentIfUnused`, List/Remove dispatch | New audio types, multi-file source resolution, registry idempotency/refcounts |
| `internal/library/handler.go` | Add/Remove/List decode/error paths; new Inspect handler | Strict audio decoding, machine errors, inspect, audio pagination |
| `internal/library/naming.go` | `IsVideoFile`, filename/stub helpers, stub creation | Section-aware extension predicates; caller-owned audio final paths; staging/no-replace support |
| `internal/metadb/*` | migration/schema/transactions | `audio_projections`, batch state/recovery |
| `internal/vfs/metadata.go` | virtual metadata validation | Remove movie-sized floor for audio; section-aware structural/size checks |
| `internal/vfs/inodemap.go` | registration call sites | Keep current `hash:file_index`; eagerly register committed audio paths |
| `startup.go` | startup cache builder/traversal/recovery | Flat audio sections; recursive audio registry reconciliation; shared predicates |
| `main.go` FUSE nodes/handles | Lookup/Readdir/Getattr/Open/Read/Release/Unlink and mutation errno behavior | Section-aware discriminator, coherent complete enumeration, atomic publication, read-only audio, open-handle lifetime, no audio warmup, scanner-safe read contract |
| `internal/vfs/errno.go` | error mapping | Do not expose internal backpressure as terminal `EAGAIN`; preserve EINTR/timeout/corruption distinctions |
| GoStorm/torrent status wrapper | file list/status resolution | Implement Inspect and `source_path -> file_index` mapping using GoStorm's current sorted 1-based IDs |
| `cleanup.go` / `autoremove.go` | orphan/drop checks | Protect hashes referenced by audio registry; no audio semantic reaper |
| `internal/monitor/collector/*` | `.mkv`-specific filtering/metrics | Section-aware audio visibility/monitoring without changing movie/TV metrics semantics |
| cache/directory metadata paths | invalidation, readiness and predicates | Add/Remove publication, complete Readdir, stable directory mtime, restart readiness, Readdir/Lookup/Open parity |
| tests | library, metadb, VFS, startup, security, concurrency, downstream harness | Normative acceptance coverage |

Broad cosmetic renaming of movie-centric internal type names SHOULD be avoided unless required for correctness. Extract generic primitives only where Phase 1 actually needs them.

---

# 20. Recommended landing sequence

Implementation SHOULD land in three reviewable changes, targeting the agreed audio feature branch when used by the project.

## PR 1 — registry, sections, and VFS foundation

- metadb audio projection schema/migration;
- flat `music/` and `audiobooks/` sections;
- section-aware virtual-stub predicates;
- startup/reconciliation scaffolding;
- registry-aware torrent reference protection;
- eager inode registration;
- audio warmup bypass;
- movie/TV regression tests.

## PR 2 — Library API and atomic creation

- `music` / `audiobook` types;
- `POST /api/library/inspect`;
- exact `source_path` resolution and persisted `file_index`;
- extension/hash-suffix/path validation;
- per-projection idempotency and 409 conflicts;
- failure-atomic multi-file Add;
- paged/prefix-filtered audio List;
- API/unit/crash-injection tests.
- bounded startup read failure: when on-demand torrent activation cannot start
  because the engine is unavailable, Open/Read MUST terminate with an ordinary
  non-retryable I/O error within the existing read bound; it MUST NOT expose an
  indefinitely retryable filesystem result to a scanner. A later independent
  attempt after engine readiness MUST remain possible. This is engine-wide and
  covers video as well as audio. The TrueNAS PR 2 Plex scan made this a PR 2
  exit requirement; broader scanner-load scheduling remains PR 3.

## PR 3 — lifecycle hardening and compatibility

- exact-path Remove;
- `0444` / `EROFS` / `EPERM` semantics;
- handle/pump/cache/dentry invalidation;
- stable timestamp/restart behavior;
- hostile-path race hardening;
- scanner-load read scheduling and broader scanner-safe read behavior;
- downstream acceptance harness and concurrency measurements.

A PR MUST NOT claim audio support complete until the Phase 1 acceptance matrix passes.

---

# 21. Phase 2 backlog

The following are explicitly deferred unless a Phase 1 acceptance failure proves that one is the minimum necessary correctness fix:

- additional music codecs (MP3, AAC/M4A, Ogg Vorbis, Opus, WAV, etc.);
- additional audiobook codecs/containers;
- audio playback-priority detection;
- per-file priority API;
- next-track/next-part prefetch;
- audio-specific metadata warmup;
- global read-ahead redesign;
- scheduler redesign/fairness work beyond the Phase 1 acceptance workload;
- small-file cache constant tuning;
- sidecar projection;
- batch remove;
- prefix/directory remove;
- hash-wide remove;
- safe same-path content replacement;
- inode identity redesign;
- internal music/audiobook discovery/sync engines;
- metadata-provider/scoring/quality policy;
- semantic audio reapers;
- media-server refresh credentials/integrations inside Tiramisu.
- optional `mmap` conformance work, xattrs, locks, or writable mappings;
- generic bounded pre-projection source-range reads / media-neutral probe API.

---

# 22. Remaining implementation-time gates

The architecture is closed enough to implement. The remaining values are tunables rather than unresolved architecture:

1. **Read deadline:** this spec selects a 30-second Phase 1 default and a single absolute-deadline model. Before merge, acceptance measurements may justify tuning the default without changing the contract.
2. **Operational caps:** this spec selects 512 files/Add, 16 components, 255 bytes/component, 4096 path bytes, List default 200/max 1000. These may be adjusted downward/upward only with tests demonstrating the new limits still cover normal albums and multipart books and bound resource usage.
3. **Performance thresholds:** bytes fetched and scan wall-clock time for Plex/Jellyfin analysis are measurement outputs, not correctness gates unless they trigger read failures or playback stalls.

No additional architectural decision is required for roots, file addressing, registry ownership, formats, warmup, lifecycle ownership, idempotency, removal shape, namespace coherence, open-handle lifetime, or downstream refresh ownership.

---

# Appendix A — Research reconciliation

This appendix is non-normative. It records the decisions produced by reconciling the supplied research corpus.

| Topic | Evidence across research | Final Phase 1 treatment |
|---|---|---|
| Engine/controller boundary | All studies converge that media identity, metadata providers, release scoring, naming semantics, and server refresh policy belong outside Tiramisu | Generic immutable projection engine only |
| Roots | Maintainer discussion settled flat sibling roots | `music/`, `audiobooks/` |
| File selector | Research and GoStorm behavior favor exact torrent-relative path while index is implementation-derived | caller sends `source_path`; Tiramisu resolves/persists 1-based `file_index` |
| Inspect | Needed by both music and audiobook source-selection workflows | `POST /api/library/inspect` returns generic file-list facts only |
| Registry | Required for ownership, idempotency, refcounts, recovery, stable metadata and reconciliation | authoritative persistent audio projection registry |
| Hash suffix | Registry would technically provide uniqueness, but maintainer retained `_hash8` | caller supplies and Tiramisu validates `_hash8` |
| Formats | Maintainer deliberately narrowed initial scope | FLAC music; M4B/M4A/MP3 audiobooks |
| Readdir semantics | Jellyfin and broad survey show successful partial enumeration can cause destructive scanner conclusions | successful `readdir` is complete/coherent; otherwise fail operation |
| Namespace publication | All scanners depend on recursive discovery before file parsing | Add/Remove publish coherent namespace changes; no listed-but-unopenable file |
| EOF semantics | Jellyfin/mStream and ordinary parsers treat zero-byte success as EOF | no zero-byte success at offset `< st_size` due hydration delay |
| Short reads | Legal positive short reads are ordinary file behavior; false EOF is the dangerous case | positive forward-progress short reads allowed; synthetic zero forbidden |
| Stable metadata | Navidrome and several surveyed scanners use path/size/mtime and sometimes directory mtime | path/inode/size/mtime stable; hydration cannot alter them; child-set mutations update directory mtime |
| Open handle vs Remove | Broad compatibility study identified a missing POSIX-like lifetime guarantee | opened handle stays bound to same immutable object until close; Remove only unpublishes new namespace access |
| Read backpressure | Navidrome/Jellyfin and broad survey show scanners do not implement Tiramisu-specific nonblocking retry protocols | internal contention hidden; no normal final `EAGAIN`; bounded timeout/corruption distinctions preserved |
| Restart | Scanners must not see an incremental reconstruction as a real deletion set | reconcile committed namespace before scan-ready successful enumeration |
| Warmup | No evidence justifies movie-style head/tail or new media-specific prefetch in Phase 1 | demand-driven only; optimize later from measurement |
| Sidecars | All target servers can operate from embedded metadata/provider enrichment, though features are reduced | no sidecar projection in Phase 1 |
| Expensive analysis | Jellyfin LUFS, Plex sonic analysis, LMS MusicNN and mStream hashing are application policy expressed as ordinary reads | bytes must be correct; document/recommend operational profiles; do not invent filesystem shortcuts |
| Broader servers | Gonic/OwnTone/Koel/Lyrion/LMS validate the generic contract; Funkwhale/Storyteller require different workflows | qualify natural consumers; do not redesign engine for import/transformation systems |
| External application policy | Identity graphs, metadata providers, candidate scoring, naming/verification and server-specific operating recommendations are valuable research but are not engine architecture | extracted from this engine spec; belongs in deterministic companion-controller documentation and, where AI interaction is used, the optional versioned agent skill |

---

# Appendix B — Qualification and support model

The support model SHOULD distinguish **filesystem compatibility** from **recommended operational profiles**.

## B.1 Class model

- **Class A — natural filesystem compatibility:** ordinary read-only media root plus scanner; no ingestion/transformation workflow required.
- **Class B — likely compatible but empirically opaque/expensive behavior:** qualify before advertising broad support.
- **Class C — usable only through application import/in-place-import integration:** controller adapter territory, not a Tiramisu engine gap.
- **Class D — transformation workflow fundamentally mismatches Phase 1:** explicit non-target.

## B.2 Common qualification metrics

For every candidate scan, measure:

- max simultaneous opens/readers;
- read-offset distribution and requested byte counts;
- unique torrent bytes/pieces hydrated;
- scheduler wait and piece wait latency;
- final read errors by errno;
- total scan time;
- metadata stability across unchanged rescan/restart;
- foreground playback throughput/stalls during scan;
- recovery after one injected read failure.

A server appearing correctly in its UI is **not sufficient** if the run required partial namespace snapshots, false EOF, routine `EAGAIN`, metadata churn, or hidden playback starvation.

## B.3 Generic acceptance ladder

The most reusable workload ladder is:

```text
Gonic-like low-concurrency tag scan
        ->
Koel-like moderate worker scan
        ->
Lightweight Music Server / Audiobookshelf high-concurrency scan
        ->
high-concurrency scan while foreground movie/audio playback is active
```

Passing this ladder demonstrates a generic blocking-file scheduler/capacity property rather than an application-specific optimization.

---

# Appendix C — Review deltas from prior issue agreement

This appendix is non-normative review aid. It identifies places where this research-synthesized proposal intentionally asks the maintainer to reconsider or strengthen a point that had already been accepted in the issue thread. These are not presented as silently settled changes.

| Topic | Previously accepted issue position | Proposal in this draft | Why the proposal changed |
|---|---|---|---|
| Open handle during Remove | Removal sketch included closing active handles / clearing active state | Remove unpublishes the path for new lookup/open, but an already-open immutable file handle remains valid until close | Ordinary POSIX-like file lifetime avoids a reader changing identity or failing solely because namespace ownership changed mid-read |
| Scheduler saturation under scanner fan-out | Projection-layer scanner failures block merge; scheduler saturation under many small readers is measured and scoped separately | The agreed 4K playback + cold 32-part audiobook scan is a Phase 1 correctness workload; only the minimum scheduler/fairness fix needed to preserve blocking-file semantics enters Phase 1 | Downstream scanners do not understand Tiramisu-internal backpressure, so a healthy source failing under the declared supported workload looks like filesystem corruption/unavailability |
| Audio minimum-size guard | Keep a smaller per-section floor (described as a few MiB / per-section floor) in addition to structural validation | No arbitrary multi-MiB floor; require `size > 0`, structurally valid stub, and exact registry/torrent/VFS size agreement | Exact identity/size validation catches corruption without rejecting legitimate very short tracks/parts by policy |
| Multi-file namespace publication | Request failure atomicity was accepted; no promise that N filesystem renames form one POSIX transaction | Still no POSIX multi-rename guarantee, but registry-gated visibility prevents an uncommitted partial batch from being advertised as committed library state | Successful partial enumeration can cause scanners to infer deletions/missing items; registry state already provides a natural publication boundary |

The following research-derived requirements are **additions rather than reversals of an explicitly accepted opposite** and therefore are not listed as conflicts above: complete/coherent successful `readdir`, hydration-invariant metadata, stable directory mutation timestamps, restart scan-readiness, false-EOF prohibition, and the proposed single absolute FUSE read-deadline model. They remain part of this maintainer-review draft and can be accepted, modified, or rejected independently.
