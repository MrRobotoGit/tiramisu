# Tiramisu Phase 1 Implementation Roadmap

Given full maintainer approval of the revised implementation spec, Phase 1 should be treated as **one feature delivered through three dependency-ordered PRs into `feature/audio-projection`**, rather than three independently releasable features.

The sequencing should deliberately make PR 1 establish the invariants, PR 2 expose them through the Library API, and PR 3 prove that the resulting filesystem behaves like ordinary storage under real media-server workloads.

## Overall target

By the end of Phase 1, Tiramisu should expose immutable torrent-backed music and audiobook files through:

```text
movies/
tv/
music/
audiobooks/
```

with:

```text
music:      .flac
audiobooks: .m4b .m4a .mp3
```

and provide raw primitives usable by ordinary deterministic clients, including the companion audio controller:

```text
inspect
add
list
remove
```

The engine owns projection identity, persistence, filesystem correctness, and torrent reference safety.

The external deterministic application/controller layer continues to own:

```text
search
release selection
scoring
metadata/provider decisions
naming policy
verification
downstream media-server refresh
```

No Phase 1 PR should move those responsibilities into the engine. An optional AI-agent skill may sit above the deterministic controller or call the same raw primitives, but AI is not required for Phase 1 use.

The companion deterministic controller is outside the three engine PRs. The three-PR roadmap therefore builds a stable substrate that can be consumed by the controller, a CLI, a UI, automation, or an agent without changing engine semantics.

---

# PR 1 — Projection registry, sections, and VFS foundation

**Target:** `feature/audio-projection`

**Purpose:** Establish the persistent data model and low-level filesystem machinery on which every later operation depends.

This is the PR where a wrong architectural decision would be most expensive to unwind, so it should avoid exposing the complete public audio API yet.

## 1. Persistent audio projection registry

Add the metadb migration/schema for authoritative audio projection records.

Conceptually each committed projection needs at least:

```text
section
virtual_path
hash
source_path
file_index
size
mtime
state
```

with uniqueness on:

```text
(section, virtual_path)
(hash, file_index)
```

The registry becomes authoritative for:

- audio ownership;
- projection identity;
- audio reference counts;
- recovery;
- idempotency groundwork;
- List/Remove groundwork;
- cleanup protection;
- stable metadata.

Define transaction/lifecycle states needed by later PRs, such as:

```text
staged
committed
removing
```

even if PR 1 does not yet expose every transition publicly.

## 2. Add the two flat sections

Teach the engine about:

```text
music/
audiobooks/
```

as peers of:

```text
movies/
tv/
```

Do not introduce a shared `audio/` parent.

Centralize the mapping:

```text
API type    section         allowed virtual/source extensions
music       music/          .flac
audiobook   audiobooks/     .m4b .m4a .mp3
```

Avoid spreading new extension switch statements throughout the codebase.

## 3. Generalize virtual-stub recognition

Replace audio-relevant `.mkv` assumptions with a section-aware predicate while preserving movie/TV behavior exactly.

The VFS hot path should remain extension/section based.

No:

- content sniffing;
- ffprobe;
- media metadata parsing;
- xattrs;
- policy logic.

Establish one predicate that later becomes shared by:

```text
Readdir
Lookup
Getattr
Open
startup scan
cache population
monitoring
```

This is important groundwork for the later complete/coherent namespace requirement.

## 4. Make torrent-reference accounting section-aware

Update:

```text
findByHash
dropTorrentIfUnused
orphan/cleanup protection
```

so audio projections count as live references.

The registry, rather than a filesystem walk, should become authoritative for audio.

Required invariant:

```text
removing a movie/episode/track/book part
must never drop a torrent
while any other committed projection still references it
```

This must work across media types where hashes overlap.

## 5. Preserve inode identity

Keep the existing:

```text
inode = hash:file_index
```

model.

Do not redesign inode generation.

For audio, eagerly register the full committed path so basename fallback is never used for identity.

Add collision tests using normal audio filenames such as:

```text
01 - Intro.flac
01 - Intro.flac
```

under different directories.

## 6. Build startup/reconciliation foundations

Startup becomes aware of persisted audio registry state and audio sections.

Establish recovery primitives for both directions:

```text
registry -> stub
stub -> registry validation
```

with the registry authoritative.

Do not automatically adopt arbitrary unknown files.

Lay the groundwork for:

```text
staged recovery
removing recovery
missing-stub reconstruction
source mismatch detection
```

Full externally observable restart-readiness semantics can be completed in PR 3.

## 7. Audio warmup policy

Music and audiobook projections must bypass movie/TV head/tail SSD warmup.

Do not add another audio-specific prefetch system.

Audio should initially use:

```text
demand fetch
+
existing RAM/read-ahead behavior
```

This should be implemented at the point where warmup policy is selected, not through scattered special cases.

## 8. Directory/VFS scaffolding

Set up the shared machinery needed eventually to guarantee:

```text
Readdir
Lookup
Getattr
Open
```

all agree about whether an audio projection exists.

PR 1 should already prevent obvious visible-but-unopenable discrepancies even if the stronger transactional publication semantics arrive in PR 2.

The PR 1 adversarial review found that tested registry identity was never used by
the live VFS. Before PR 1 exits, a separate TDD slice must exercise the production
Lookup/Open path without a real FUSE mount: committed audio identity and metadata
must survive physical-stub replacement and a cold metadata cache, while existing
movie/TV behavior remains intact. Readiness and ownership fail-closed remediation
follow as separate slices. Run the local analyzer/wiring gate after targeted GREEN.

## PR 1 test package

At minimum:

```text
migration forward/open tests
registry uniqueness tests
section/type mapping tests
extension predicate tests
source/path case handling
inode collision tests
cross-media torrent-reference tests
startup registry loading tests
audio warmup bypass tests
movie/TV regression tests
Readdir/Lookup predicate parity tests
```

Hot-path before/after measurements should also be captured here because this PR touches VFS dispatch.

### PR 1 exit condition

At the end of PR 1:

> Tiramisu understands persistent audio projections internally and can represent them correctly in the VFS without changing existing movie/TV semantics.

But the feature is **not yet a complete public workflow**.

---

# PR 2 — Library API, source inspection, and atomic projection creation

**Depends on PR 1.**

**Purpose:** Expose the approved raw primitives so ordinary deterministic clients—especially the companion audio controller—can create and reconcile audio projections without requiring an AI agent.

## 1. Add the API types

Extend Library API validation with exactly:

```text
music
audiobook
```

No aliases.

Keep `title` required, but for audio it must not determine the virtual path.

Its existing engine-facing purposes may remain, such as magnet display name / GoStorm title.

## 2. Implement `POST /api/library/inspect`

`inspect` should accept raw torrent identity information and return factual source-file information only.

It should expose fields such as:

```json
{
  "hash": "...",
  "files": [
    {
      "source_path": "Release/01 - Track.flac",
      "file_index": 4,
      "size": 31234567
    }
  ]
}
```

It must:

- wait for torrent metadata with a bounded wait;
- distinguish metadata-not-ready from an actual empty file list;
- expose no album/book semantics;
- perform no release scoring;
- perform no metadata identification;
- not choose files for the caller.

The API caller uses `source_path`; GoStorm's generated `file_index` remains resolved engine state.

## 3. Implement audio Add request shape

One torrent may create N projections.

Each requested file identifies:

```text
source_path
final section-relative path
```

Tiramisu resolves:

```text
source_path -> file_index -> exact size
```

before commit.

## 4. Validate final virtual paths

The path supplied by the caller is the final path.

Validate:

- section-relative only;
- no absolute path;
- no `..`;
- no empty components;
- no hidden/staging namespace collision;
- no symlink escape;
- agreed depth/component/path limits;
- case-collision behavior;
- correct extension;
- source extension and virtual extension agree;
- required final `_<hash8>` token is present and correct.

The engine validates naming safety and the hash convention.

It does **not** decide artist/album/author/book naming.

## 5. Resolve source identity before publication

For every requested projection:

```text
source_path
   ↓
current GoStorm file
   ↓
file_index
size
```

Persist both:

```text
source_path
file_index
```

so future disagreement is detectable.

Never silently substitute another torrent file if resolution changes.

## 6. Per-projection idempotency

Implement:

```text
same path + same source identity
    -> present

new path + unused source identity
    -> created

same path + different identity
    -> 409 Conflict

same (hash,file_index) + different path
    -> reject
```

A partially already-present album should work naturally:

```text
3 present
9 created
```

rather than treating the entire torrent as already present.

## 7. Failure-atomic multi-file Add

Use the registry transaction as the logical publication boundary.

Rough sequence:

```text
validate whole request
resolve all sources
check conflicts
create staged registry state
create hidden/staged physical stubs
validate staged artifacts
commit registry batch
publish final names
invalidate namespace caches
return success
```

If anything fails before commit:

```text
rollback everything newly created by that request
```

Already-present projections remain untouched.

Do not claim that several POSIX renames literally form one filesystem transaction.

The externally important guarantee is:

> scanners never see an uncommitted partial request represented as committed library state.

## 8. Complete/coherent publication

After Add returns success:

```text
Readdir(parent) sees path
Lookup succeeds
Getattr succeeds
st_size is correct
Open(O_RDONLY) opens that same immutable projection
```

No successful Add should produce a listed-but-not-usable file.

## 9. Implement audio List

Return reconciliation data, not media metadata:

```text
section/type
virtual_path
hash
source_path
file_index
size
mtime
state where appropriate
torrent_present/runtime status if useful
```

Support the agreed:

```text
pagination
path-prefix filter
```

using scalable pagination rather than loading the whole library.

## 10. API error model

Make important failures machine-distinguishable:

```text
malformed request
invalid path
unsupported extension
hash suffix mismatch
source not found
metadata not ready
source conflict
path conflict
registry/storage failure
```

Avoid silently falling back to movie behavior for unknown audio types.

## PR 2 test package

Add:

```text
inspect metadata-ready/not-ready tests
source_path resolution tests
file-index persistence tests
multipart Add tests
mixed present/created tests
409 identity-conflict tests
duplicate source tests
hash suffix validation
extension mismatch tests
path-validation tests
batch rollback tests
crash injection around staging/commit
successful Add -> immediate VFS visibility test
List pagination/prefix tests
movie/TV API regression tests
startup engine-unavailable Open/Read tests: bounded terminal filesystem error,
no success with an unhydrated handle, and a later independent recovery attempt;
run targeted race verification without a live swarm or FUSE mount
```

### PR 2 exit condition

At the end of PR 2:

> An ordinary deterministic client can inspect a torrent, select exact source files, and create/reconcile persistent audio projections exclusively through raw Tiramisu API primitives. The companion audio controller is the primary automatic consumer; an AI agent is optional.

The filesystem still needs lifecycle and scanner hardening before declaring Phase 1 supported.

The TrueNAS PR 2 Plex scan exposed a startup read that could leave a scanner
blocked while GoStorm's BT client was unavailable. PR 2 must close that narrow
engine-wide startup failure before exit. It does not absorb PR 3's broader
scanner concurrency and fairness work.

---

# PR 3 — Lifecycle hardening, POSIX-like semantics, and qualification

**Depends on PRs 1 and 2.**

**Purpose:** Make the complete workflow trustworthy to conventional media scanners and playback clients.

This is where the research-driven filesystem guarantees become externally testable.

## 1. Exact-path Remove

Implement API-owned removal by exact section-relative virtual path.

No Phase 1:

```text
batch remove
prefix remove
directory remove
hash-wide audio remove
```

Those remain later work.

## 2. Remove state machine

Use durable removal state.

Suggested flow:

```text
lookup committed projection
lock identity/path
mark REMOVING
unpublish from new namespace lookups
move/delete physical stub safely
remove committed ownership
invalidate relevant caches
prune engine-owned empty directories
re-evaluate torrent references
finish recovery state
```

Startup must finish interrupted removals rather than resurrect them.

## 3. Open-handle lifetime

Implement the approved revised behavior:

```text
open file
API Remove unpublishes pathname
existing handle still references same immutable object
new Lookup/Open sees ENOENT
existing reader may finish
backing runtime object survives until final handle closes
```

Never retarget an existing open handle.

Torrent dropping must account for those transient live references.

## 4. Explicit read-only semantics

Audio files:

```text
0444
```

Projected audio directories:

```text
0555
```

Mutation attempts should return deliberate errors.

For example:

```text
write/open writable -> EROFS
truncate           -> EROFS
rename             -> EROFS
mkdir              -> EROFS
setattr mutation   -> EROFS
direct unlink      -> EPERM
```

Lifecycle remains API-owned.

## 5. Cache and dentry invalidation

Complete audio-aware invalidation for:

```text
Add
Remove
startup recovery
```

Cover:

- directory caches;
- metadata/stat caches;
- inode/path mappings;
- runtime handles where appropriate;
- pump state;
- projection lookup caches.

Hydration itself must **not** cause namespace invalidation or identity changes.

## 6. Stable file metadata

For an unchanged projection across restart:

```text
path  unchanged
inode unchanged
size  unchanged
mtime unchanged
bytes unchanged
```

No valid projection should fall back to:

```go
time.Now()
```

merely because Tiramisu restarted.

## 7. Stable directory semantics

Persist or deterministically reconstruct conventional directory mutation metadata.

A directory's mtime changes because:

```text
committed child set changed
```

not because:

```text
torrent hydrated
file was read
Tiramisu restarted
scanner probed metadata
```

## 8. Restart readiness

Do not expose a plausible but partially reconstructed audio namespace as successfully ready for scanning.

Conceptually:

```text
start
  ↓
recover registry transactions
  ↓
reconcile committed projections
  ↓
rebuild caches/inodes/directory state
  ↓
protect referenced torrents
  ↓
namespace becomes scan-ready
```

If the namespace cannot yet be represented coherently, failure or not-ready is preferable to successful partial enumeration.

## 9. Complete successful `readdir`

For a committed/quiescent namespace:

> successful directory enumeration represents the complete committed child set at a coherent point in time.

Temporary GoStorm/torrent/cache contention must not make committed entries silently vanish from an otherwise successful listing.

This protects downstream scanners from interpreting engine trouble as real deletion.

## 10. Correct EOF and short-read behavior

Make these invariants explicit and tested:

```text
offset >= st_size
    -> normal zero-byte EOF

offset < st_size
torrent range temporarily unavailable
    -> wait/retry or real error
    -> NEVER false zero-byte EOF

positive short read
    -> legal when forward progress occurs
```

Returned bytes must always equal source torrent bytes.

## 11. Scanner-safe blocking reads

Hide internal engine backpressure from ordinary blocking clients.

Under supported load:

```text
temporary pump/slot/rate contention
    -> internal wait/retry

client cancellation
    -> EINTR

source fails to progress through agreed deadline
    -> ETIMEDOUT

unrecoverable corruption/identity failure
    -> EIO

internal transient retry state
    -> not final EAGAIN
```

Implement the approved single absolute read-deadline model rather than stacking unrelated nested timeouts.

Tune the exact default only from measurement.

## 12. Concurrency/fairness qualification

Run the reference workload:

```text
active 4K movie playback
+
cold 32-part MP3 audiobook
+
Audiobookshelf full metadata scan
```

Collect:

```text
simultaneous opens
read offsets/patterns
requested bytes
unique torrent bytes hydrated
blocked-read latency
EAGAIN count
EIO count
ETIMEDOUT count
scan duration
movie throughput/buffering
memory/cache pressure
pump/slot utilization
```

If projection correctness works but the current scheduler cannot provide ordinary blocking-file semantics under the approved workload, make only the **smallest required scheduling/fairness correction** in this PR.

Do not turn this into an opportunistic global scheduler rewrite.

## 13. Downstream compatibility matrix

Cold and warm qualification should cover at least:

```text
Navidrome
Jellyfin
Audiobookshelf
```

and Plex/Plexamp where practical.

Core scenarios:

```text
cold album scan
cold multipart audiobook scan
unchanged rescan
restart + rescan
slow but healthy torrent
injected read failure
Remove during open read
scan during active video playback
large-library List
```

The expected result is filesystem correctness, not identical scanner behavior across products.

## 14. Security hardening

Finish adversarial tests for:

```text
..
absolute paths
symlink races
hidden staging collisions
case collisions
overlong components
excessive nesting
duplicate sources
registry/stub disagreement
crash during Add
crash during Remove
```

The implementation mechanism can remain idiomatic Go/Linux; the security invariant matters more than prescribing one syscall design.

## PR 3 exit condition

Phase 1 is complete when:

> conventional media servers can recursively discover, stat, seek, probe, open concurrently, rescan, survive restart, and consume supported immutable torrent-backed audio projections without learning anything about Tiramisu-specific scheduler or lifecycle behavior.

---

# Final Phase 1 gate

After PR 3 lands on `feature/audio-projection`, do a final integration pass before promoting the feature toward `main`.

The gate should include:

```text
go test ./...
go vet ./...
gofmt clean
race-sensitive tests where practical

movie regression suite
TV regression suite
audio API suite
startup/crash recovery suite
path/security suite
VFS semantics suite
scanner compatibility suite
Pi 4 performance measurements
```

Then verify the architectural boundary one last time:

```text
Engine
✓ raw torrent/file inspection
✓ immutable projection
✓ registry/persistence
✓ filesystem semantics
✓ add/list/remove primitives

Deterministic external controller
✓ discovery
✓ metadata providers
✓ identity reconciliation
✓ deterministic release scoring/selection
✓ artist/album/book naming
✓ source verification
✓ reconciliation/lifecycle policy
✓ explicit media-server refresh

Optional interfaces (AI agent skill / UI / CLI)
✓ user interaction and intent capture
✓ invoke the same deterministic controller/raw API primitives
✓ not required for automatic operation

Media server
✓ metadata library
✓ scanning
✓ analysis
✓ playback
```

Nothing owned by the deterministic controller, optional interface layer, or media server should migrate into Tiramisu merely because it is convenient during implementation.

## Dependency graph

The whole Phase 1 sequence is therefore:

```text
                 Issue #25 + approved spec
                           |
                           v
             feature/audio-projection
                           |
                           v
      ┌─────────────────────────────────┐
      │ PR 1                            │
      │ Registry + sections + VFS base │
      └───────────────┬─────────────────┘
                      |
                      v
      ┌─────────────────────────────────┐
      │ PR 2                            │
      │ Inspect + API + atomic Add      │
      └───────────────┬─────────────────┘
                      |
                      v
      ┌─────────────────────────────────┐
      │ PR 3                            │
      │ Remove + semantics + hardening  │
      │ + compatibility qualification   │
      └───────────────┬─────────────────┘
                      |
                      v
             Full Phase 1 matrix
                      |
                      v
           Maintainer final review
                      |
                      v
               merge toward main
```

The key organizational principle is:

> **PR 1 defines identity, PR 2 defines publication, PR 3 proves the illusion.**

That gives each review a coherent thesis while following the maintainer's requested three-PR structure.
