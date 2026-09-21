# Carried into PR 3 from PR 2

Written 2026-09-21 at the close of PR 2. Each item states what is wrong, why it
was not fixed in PR 2, and what "done" looks like, so PR 3 does not have to
re-derive any of it.

---

## Status at the close of the PR 2 hardening branch (2026-09-21)

| # | Item | Status | Where |
|---|------|--------|-------|
| 1 | Staged transaction crash recovery | **done** | `2111423`, rollback-only, wired in `startup.go` |
| 2 | `resolveTargetFile` hot-path cost | **done** | `e0beb0c`, the audio branch skips the engine file-list copy |
| 3 | External identity on replay | **open — decision required** | unchanged: the stored row wins and the disagreement is logged |
| 4 | `Remove` does not unpublish | **done** | `8da6d99`, mark → unpublish → unlink → prune → forget, exact path only |
| 5 | `WriteAudioStub` unreachable | **done** | `db7dfd7`, deleted; `AudioStubBytes` + `SectionWriter` is the only writer |
| 6 | Directory fsync after rename | **done** | `2dcc000`, both directories, `EINVAL`/`ENOTSUP` tolerated on the fallback |

### PR 3 roadmap status (2026-09-21, branch `feature/audio-projection`)

Mapped against `specs/TIRAMISU_PHASE1_ROADMAP_FINAL.md` §1-14. Commits are on
`feature/audio-projection`; the audits quoted in this folder (lotti 4-8) were
requested per point.

| Roadmap item | Status | Evidence |
|---|---|---|
| §1 exact-path Remove | done | `8da6d99` + `removal_test.go` |
| §2 remove state machine | done | `8da6d99`, startup sweep `5c3b814` + `recovery_test.go` |
| §3 open-handle lifetime | done | handle never retargeted; `h4_namespace_test.go`, `livepublish_test.go` |
| §4 read-only semantics | done | `b96abf1`, `0444`/`0555`, EROFS/EPERM + `main_readonly_test.go` |
| §5 cache/dentry invalidation | done | `e0beb0c` (DirCache generation + ancestors), `dc37c2b` (live namespace) |
| §6 stable file metadata | done | inode map + registry `MtimeNS`, no `time.Now()` fallback |
| §7 stable directory semantics | done | `60a5623`, `DirMtime` from `UpdatedAtNS` + `dirmtime_test.go`, `main_dirmtime_test.go` |
| §8 restart readiness | done | `af86710`, `audio_namespace_state`/`_entries` in `/metrics` + `main_readiness_test.go` |
| §9 complete successful readdir | done | committed namespace published as one batch (`284aac2`), DirCache generation test |
| §10 EOF/short-read | done | `startup_read_failure_test.go` (EOF, stalled deadline, absent stream) |
| §11 scanner-safe blocking reads | done | same read path, single injected deadline; `793f89a` bounds the wake by the FUSE context |
| §12 concurrency/fairness measurement | **pending** | Pi 4 reference workload: 4K + 32-part audiobook + full scan |
| §13 downstream compatibility matrix | **partial** | Plex/Plexamp webhook identity verified live on pi-test (`2aaf8ad`/`9b83a3b`/`8ef6db3`); Navidrome/Jellyfin/Audiobookshelf scans pending |
| §14 adversarial security suite | done | `pathvalidation_test.go` (P15-P27), `containment_test.go` (H5-H7, symlinks), `audio_test.go` (portable-key collisions), recovery/removal crash tests |

Audiobooks share every code path with music (section-aware helpers); no separate
audiobook implementation exists to complete, and the functional audiobook
workload is intentionally parked per the maintainer's instruction.

Also closed in the same round: a directory listing can no longer land after its
invalidation (`e0beb0c`, `DirCache` generation), and the live-namespace
consistency fix from the round-3 audit (`dc37c2b`).

Removal crash window closed in `5c3b814`: startup sweeps `removing` rows (stub
away, prune, row delete) before reconciliation. `79e3b79` turns a stub replaced
mid-removal into an explicit 409 instead of a silent `Removed: true`.

---

## 1. BLOCKING — crash recovery for a staged transaction (spec §7.4)

### What is wrong

Startup reconciliation publishes only `committed` rows
(`startup.go`, `globalAudioNamespace.Publish(committed)`). It has **no handling
for `staged` transactions at all**. Spec §7.4 requires it:

> Startup MUST reconcile durable mutation state before advertising audio paths.
>
> For a `staged` transaction:
> - if **all** expected final stubs exist and validate against all staged rows,
>   startup MAY promote the whole transaction to `committed`;
> - otherwise startup MUST roll back the transaction as a unit, deleting only
>   files that can be proven to belong to that transaction, then deleting
>   staged rows.

### Why it became urgent in PR 2

PR 2 reordered publication to satisfy spec §7.2 — the renames now happen
**before** the commit, so the single atomic transaction is last. That removed
the partial-commit window the round-1 review found, and it was the right fix.

It also changed what a crash leaves behind:

| | before the reorder | after |
|---|---|---|
| crash after staging, before renames | staged rows + hidden dot-leading files | same |
| crash after renames, before commit | *not reachable* | **staged rows + final names on disk** |

The new state is **not a correctness break for readers**: audio dispatch
classifies against the committed namespace, so an uncommitted final stub is
inert, which is exactly the invariant §7.2 states one line below its sequence.

The harm is that the path becomes **permanently unusable**. Publication uses
`renameat2(RENAME_NOREPLACE)`, so every retry of that virtual path now fails
with `ErrDestinationExists` (409) against a file no registry row owns. The
caller cannot fix it through the API, because Remove works from the registry and
there is no row. It needs a human with filesystem access.

Orphan staged rows also accumulate and hold `(section, virtual_path)` and
`(hash, file_index)` uniqueness, so they block re-adding by a second route.

### Why PR 2 did not fix it

Two honest reasons, in tension:

- The maintainer's own three-PR split (issue #25) puts **"restart stability"**
  in PR 3, alongside removal and cache invalidation.
- Spec §7.4 sits **inside section 7**, the add/atomicity section this PR owns,
  and PR 2 is what made the failure mode reachable.

The judgement at the time was to document this rather than expand PR 2's diff
further, six remediation slices in. A later review disagreed and argued it
belongs in PR 2, since PR 2's own reordering is what made the state reachable.
See B1 in `PR2_REMAINING_WORK.md` — the fix is not large either way.

### What done looks like

Minimal spec compliance is small, because §7.4 makes promotion optional
(`MAY`) and rollback mandatory (`MUST`):

1. At startup, before publishing the namespace, load every projection in state
   `staged`, grouped by `txn_id` (`AudioProjectionsByState` already exists).
2. For each transaction, **roll it back as a unit**: delete the final name and
   the staging name for each row — both are derivable, `virtual_path` and
   `staging_name` are columns — then `RollbackAudioProjections(txnID)`.
3. Delete **only** files provable to belong to that transaction. §7.4 is
   explicit that a physical audio-looking file with no registry row must not be
   claimed from filename shape alone.
4. Do it through `SectionWriter`, not pathnames. The containment work in PR 2
   exists precisely so recovery cannot be redirected by a symlink, and a
   recovery path that bypasses it reopens the hole.
5. Log every rollback: an operator needs to know a request was undone.

Promotion (the `MAY` half) is a later optimisation and should not be attempted
before rollback works, because promoting on incomplete validation is worse than
rolling back a request the caller can simply retry.

**Tests it needs:** a staged transaction whose final names all exist; one where
some do; one where none do; one where a symlink was planted in a parent
component between the crash and the restart; and proof that a committed
transaction is untouched by any of it.

---

## 2. HIGH — hot-path cost in `resolveTargetFile`

`main.go`'s `resolveTargetFile` copies and sorts the **entire** resident torrent
file list before calling `library.ResolveOpenTarget`, which for audio discards
it immediately and returns the registry's index.

The copy exists for a real reason — the engine's slice is shared state and the
old code sorted it in place, which PR 2 fixed — but for audio it is pure waste
on the most latency-sensitive path in the project, the one Plex hammers during a
library scan.

**Done looks like:** resolve the section first, and build the file list only for
video. Keep the copy for video; the in-place sort must not come back.

---

## 3. MEDIUM — external identity on replay has no contract

`AddAudio` returns `present` for a projection that already exists. If the replay
carries a **different** external identity than the stored row, PR 2 keeps the
stored value and logs the disagreement, because the registry has stage, commit
and rollback but **no update path**.

It is deliberately not a 409: a different MusicBrainz id does not change the
bytes at the path, and conflating it with the content conflict would invent
semantics the maintainer has not ruled on.

**This needs a decision, not an implementation.** Should a replay with a new
identity update the row, conflict, or stay ignored? The first needs an update
path in the registry.

---

## 4. MEDIUM — `Remove` does not unpublish from the live namespace

PR 2 added live publication on add. There is no corresponding unpublish, because
audio Remove is PR 3 work. When Remove lands it must drop the namespace entry
(`AudioNamespace.Remove` already exists) in the same order publication uses:
registry first, then namespace, then cache invalidation.

---

## 5. LOW — `WriteAudioStub` is now unreachable

The add path renders the stub with `AudioStubBytes` and writes it through
`SectionWriter`, so `WriteAudioStub` has no caller. It is recorded as
carried-forward dead code rather than deleted, because the maintainer asked for
it specifically in the PR 1 review. **Deleting his requested API is his call.**

---

## 6. LOW — directory fsync is not performed

Publication renames and then commits. The rename is not followed by an fsync of
the containing directory, so a power loss can leave a committed row whose final
name is not durable. Spec §7.2 lists "fsync as required" in its sequence.

Relevant only to power loss, not process death, and it pairs naturally with
item 1: the same recovery pass that handles staged transactions is what would
repair a committed row whose stub is missing (§7.4 requires exactly that, using
`mtime_ns` rather than recovery time).
