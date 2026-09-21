# PR 2 (audio projection) — what is left

Branch `feature/audio-projection`, tip `f83810a`. The branch builds clean, the
full suite passes with `-race`, `go vet` and `gofmt` are clean, and the pinned
analyzers report zero new findings. **It is not ready to merge.** A third
adversarial review returned REJECT with four HIGH findings and two blockers;
the report is in `evidence/adversarial-report-3.md`.

This document is the remaining work, ordered by what to do first. Every item
names a file and a line range. Nothing here is speculative — each was traced to
a concrete failing input.

## State of the branch

| | |
|---|---|
| Slices implemented | 8 feature + 6 remediation |
| Tests | 13 suites, all passing, written by a separate agent from the implementer |
| Verified on hardware | TrueNAS + Plex, full music scan, live add without restart, external identity round-trip |
| Reviews | 3 adversarial rounds; each one found a real defect in every slice |

What the hardware run proved: Plex read FLAC tags through FUSE (so real bytes
streamed from the swarm), a track added through the API appeared without a
restart, and `external_id`/`external_id_ns` survived the round trip.

What it did not prove: crash recovery, containment against a planted symlink,
and concurrency under the acceptance workload (a cold 32-part audiobook scanned
while a 4K movie plays). That last one needs measuring on the Pi.

---

## Blockers

### B1 — a crash between rename and commit bricks the path permanently

`internal/library/atomicadd.go:265-299`, `internal/metadb/audio.go:174-188`,
`startup.go:239-323`

Spec §7.2 requires the renames to happen before the registry commit, so the one
atomic step is last. That is implemented and correct. The consequence is that a
crash after the final `Renameat2` and before `CommitAudioProjections` leaves
final stub names on disk with every row still `staged`.

Startup reconciliation reads only `committed` rows, so it neither promotes nor
rolls back that transaction. Publication uses `RENAME_NOREPLACE`, so every
retry of that virtual path returns 409 against a file no row owns — and Remove
works from the registry, where there is no row. **The path cannot be recovered
through the API at all; it needs filesystem access.** Orphan staged rows also
hold the `(section, virtual_path)` and `(hash, file_index)` uniqueness
constraints, blocking a re-add by any other route.

Spec §7.4 already specifies the fix, and makes promotion `MAY` and rollback
`MUST`. Implementing only the rollback half is enough and is small:

1. At startup, before publishing the namespace, load projections in state
   `staged` grouped by `txn_id`. `AudioProjectionsByState` already exists and
   currently has no production caller.
2. For each transaction, through a `SectionWriter` rather than pathnames:
   remove the final name, remove the hidden staging name, prune empty
   directories, then `RollbackAudioProjections(txnID)`.
3. Delete only names derivable from the row. §7.4 is explicit that an
   audio-looking file with no registry row must not be claimed from its
   filename shape.
4. Do the filesystem work before the registry rollback, so a crash *during*
   recovery leaves the rows that prove ownership.
5. Log each rollback — an operator needs to know a request was undone.

A note on scope: the three-PR split puts restart stability in PR 3, and on that
reading this belongs there. The argument for doing it here is that §7.4 sits
inside section 7, the add/atomicity section this PR owns, and that PR 2's own
reordering is what made this state reachable. Your call. If it moves to PR 3,
PR 2 should not claim §7.2 compliance, because §7.2's ordering is only safe
given §7.4's recovery.

### B2 — a hash/magnet disagreement mutates the wrong torrent

`internal/library/atomicadd.go:95-115`, `internal/library/inspect.go:41-60`

Send a valid `hash=A` together with a magnet whose BTIH is `B`. Both Add and
Inspect silently discard A and add, wake and mutate `B`. Spec §§6.2/18 require
`400 hash_magnet_mismatch` with no mutation of any kind.

Fix: at both sites, if `req.Hash` is non-empty and the magnet carries a
non-empty BTIH and the two differ under `canonicalHashKey`, return 400 before
the engine, database, namespace or filesystem is touched. Magnet precedence
when `hash` is empty is correct and tested — keep it. The block is currently
duplicated across the two files; extract it once.

---

## High

### H3 — live add and restart publish different identities

`main.go:4741-4744`, `internal/vfs/reconcile.go:76-108`,
`internal/library/atomicadd.go:301-318`

Restart calls `im.AddFile(full, hash, fileIndex)` **before** publishing, so
every path has a content-derived inode. A live add publishes the projection and
performs no inode registration. A `Readdir` that runs before the first `Lookup`
therefore emits a basename-derived fallback inode — or, via `fastFileMap`, the
inode of a *different* projection that happens to share a basename. The cached
directory entry and the later `Lookup` then disagree about the file's identity.

This is the category a media scanner turns into silent corruption: two tracks
de-duplicated, or one file's metadata cached against another's inode. The
hardware run could not catch it because adding a single track and rescanning
goes through `Lookup`; it needs a `Readdir` before any `Lookup`.

Fix: give the live path the same eager inode registration restart performs.

### H4 — publication is per-row, and startup can silently undo a live add

`internal/library/atomicadd.go:301-318`, `startup.go:253-313`

Two independent defects:

- A multi-track album is inserted into the namespace **one projection per
  call**, so a `Readdir` interleaved between calls returns a partial album.
  Give `AudioNamespace` a batch insert taking `[]AudioProjection` under a
  single mutex acquisition. `PublishAudioPath` has one production call site and
  is already a `Config` seam, so the signature change is contained.
- Startup's second pass runs in a goroutine while the HTTP server is already
  accepting requests, and its whole-set `Publish` **replaces** the namespace.
  A live add landing between that pass's query and its publish is lost — a
  successful, committed add silently disappears from the mount. Merge rather
  than replace, or hold the namespace lock across query-and-publish. Merging is
  safer: a live add is authoritative for its own path.

### H5 — the containment walk can delete outside the section

`internal/library/containment.go:85-112,203-233`

`openParent` walks the path one component at a time, and each intermediate
directory descriptor becomes a **new** `RESOLVE_BENEATH` root. `RESOLVE_BENEATH`
constrains resolution relative to the descriptor passed to that individual
call; it does not keep an already-open directory attached to its original tree.

So: begin pruning `A/B/track.flac`; after `openParent` has opened `root/A` but
before the `Unlinkat`, rename `root/A` outside the section. The descriptor
follows, and `Unlinkat(fd, "B", AT_REMOVEDIR)` removes `outside/A/B`. The
earlier pathname-based implementation would have taken `ENOENT` and touched
nothing — this is a regression introduced by the commit that was meant to
*improve* containment (`37d760b`).

Fix: stop re-rooting. Resolve the whole parent path in a single `Openat2` from
the section root descriptor with `RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS`, so the
kernel enforces the boundary against the root across the entire walk. For the
create case, create components one at a time but re-resolve from the root with
the accumulated path each time, never from the previous component's descriptor.

Honest limit: a rename landing between the final resolution and the syscall is
a TOCTOU the kernel cannot close. This fix makes the window one syscall wide
instead of spanning the whole remaining walk. Closing it entirely is a design
question about holding the section root immutable, not a patch.

### H6 — rollback deletes by name, and drops ownership when cleanup fails

`internal/library/atomicadd.go:235-263`, `internal/library/containment.go:180-233`

Three defects in `unwind()`:

1. **Deletes by name.** `RemoveStaged` unlinks whatever currently occupies the
   name. Replace a published final with another file before inducing a commit
   failure and the replacement is destroyed. Retain object identity — `fstat`
   at create and publish, compare `st_dev`/`st_ino` before unlinking, skip on
   mismatch.
2. **Drops ownership after failed cleanup.** Cleanup errors are logged, then
   `RollbackAudioProjections` deletes the only rows that prove the leftover
   file belongs to this transaction. If any cleanup step failed, **do not roll
   back the rows** — leave them staged for B1's recovery to finish, and return
   an error saying so.
3. **Deletes pre-existing directories.** `PruneEmptyDirs` walks up blindly, so
   an `Artist/Album` that existed before the request is removed by its
   rollback. Track which directories the request created (`Mkdirat` returning
   nil rather than `EEXIST`) and prune only those.

Note: a current test asserts that deleting a pre-existing empty ancestor is
correct. That test encodes the wrong behavior and needs changing with the fix.

### H7 — a failed write leaves a hidden file nothing owns

`internal/library/containment.go:117-145`

`WriteStaged` creates the leaf exclusively, then can fail in `Write` or `Sync`
(ENOSPC, I/O error, fsync failure) and return with the file still present. The
caller appends the name to its cleanup list only on success, so rollback never
removes it, and the now non-empty parent also defeats directory pruning.

Fix inside `WriteStaged`: on any error after the `Openat2` succeeds, unlink the
leaf through the same directory descriptor before returning. The function
created the object, so it owns it — do not push this onto the caller.

---

## Medium

### M8 — the all-present replay response contradicts the spec and the README

`internal/library/handler.go:23-52`, `internal/library/atomicadd.go:22-38,320-341`

Replaying a request whose projections are all already committed returns `201`
with no `already_present`, uses `status` where spec §6.4 says `state`, and
carries no stable `mtime`. §6.4 wants `200` with `already_present: true`, with
`201` reserved for new or mixed requests. The README also promises 200 for an
already-filed release. `AddResponse` already carries `AlreadyPresent` for the
video path — reuse it.

### M9 — the request body cap does not actually bound the request

`internal/library/handler.go:139-145`

The body is read through `io.LimitReader(r.Body, maxBodyBytes)`, which bounds
the *allocation* but not the request: a valid JSON value that ends inside the
first 1 MiB followed by arbitrary excess is accepted, and mutates state. Read
one byte past the cap and reject if it is present.

Separately, audio requests accept unknown fields (`{"filez":123}` parses).
Apply `DisallowUnknownFields` to the audio decode path only — leave the legacy
video decoding semantics alone.

### M10 — a cancelled cold Open leaks activation work and a semaphore token

`main.go:943-970`, `internal/gostorm/native/native.go:72-158`

`Open` races activation against the FUSE context in a goroutine, which fixed a
scanner wedge. But `Wake` takes no context and `AddTorrent` has no deadline on
this path, so a cancelled Open returns `EINTR` while its detached goroutine
keeps running and holds a `wakeSemaphore` token for up to 45 seconds. Eleven
cancelled cold Opens can exhaust the ten-token semaphore and make the next Open
fail with `wake semaphore exhausted`.

### M11 — absent external-identity keys are omitted rather than empty

`internal/library/atomicadd.go:22-31`

`omitempty` drops `external_id` and `external_id_ns` from an Add response when
neither was supplied. The agreed shape is both keys always present as empty
strings, matching `List`. One-line fix; it is a client-visible inconsistency
between two endpoints that return the same concept.

---

## Deliberately not addressed

These are real and are carried to PR 3 (see `PR3_CARRIED_ITEMS.md`):

- A directory cached before an add keeps a stale listing for the 10s TTL.
  Self-healing and bounded.
- A cache-miss `Readdir` can lose the publish/invalidate race and write a stale
  listing back *after* invalidation. This one does **not** self-heal on the TTL
  argument and needs a generation counter on `DirCache`. It is a correctness
  gap, not a performance nicety.
- `Remove` does not unpublish from the live namespace; audio Remove is PR 3.
- `WriteAudioStub` is now unreachable — the add path renders through
  `AudioStubBytes` and writes through `SectionWriter`. Left in place rather
  than deleted because it was specifically requested in the PR 1 review;
  deleting it is your call.
- No directory `fsync` after rename, so a power loss can leave a committed row
  whose directory entry is not durable. Pairs naturally with B1.
- `resolveTargetFile` copies and sorts the entire resident file list before
  the audio branch discards it — pure waste on the hottest path, which is what
  a library scan hammers. The copy itself must stay; the old code sorted the
  engine's shared slice in place, which was a race.

---

## Two things worth knowing before you start

**The automated checks have never caught any of this.** Build, `-race`, vet,
`gofmt`, deadcode, staticcheck and every wiring guard are green at `f83810a`,
and the third review still returned REJECT with four HIGH findings. The checks
are necessary and they prove nothing about this feature's correctness.

**Every review round, a fix introduced something worse.** Round 1's publication
fix published a zero identity; round 2's fix of that shipped a containment
regression (H5). Treat each fix here as new code rather than as a correction
that inherits the trust of what it replaced.
