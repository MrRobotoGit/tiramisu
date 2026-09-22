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
| 3 | External identity on replay | **done — documented** | decided 2026-09-22: stored row wins, disagreement logged; written into the API contract, no code change |
| 4 | `Remove` does not unpublish | **done** | `8da6d99`, mark → unpublish → unlink → prune → forget, exact path only |
| 5 | `WriteAudioStub` unreachable | **done** | `db7dfd7`, deleted; `AudioStubBytes` + `SectionWriter` is the only writer |
| 6 | Directory fsync after rename | **done** | `2dcc000`, both directories, `EINVAL`/`ENOTSUP` tolerated on the fallback |
| 7 | Album-granular removal | **done** | `840e8bf` prefix removal with resume, `89bc294` skips albums with an open playback session |

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
| §12 concurrency/fairness measurement | **done — production evidence** | a 5.8k-projection library under a live Plex scan concurrent with playback; see the closing note |
| §13 downstream compatibility matrix | **done for the deployed scanners** | Plex/Plexamp verified live (`2aaf8ad`/`9b83a3b`/`8ef6db3`); Navidrome/Jellyfin/Audiobookshelf carried as known debt, see the closing note |
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

## 3. CLOSED — external identity on replay (decided 2026-09-22)

**Decision: keep the current behaviour and state it in the contract.** A replay
that carries a different `external_id` does not change the bytes served, so the
stored row wins and the disagreement is logged. A `409` was considered and
rejected: it would conflate identity with content, failing a request whose
projection is correct. Documented in the manual-add skill's audio section; no
code change.

The original analysis follows.

## 3b. Original writeup — external identity on replay has no contract

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

---

## 7. NEW — album is the unit of removal, and the API has no way to say it

Raised 2026-09-22 from the first production library (397 albums, 5331 tracks).

### What is wrong

`RemoveAudio` is exact-path only, by design: "no hash, prefix or recursive
forms". That is the right primitive, but it is the wrong granularity for the
only removal anyone actually performs.

Nobody removes a track. A dead swarm kills the whole album at once, and an album
with nine of its ten tracks unplayable is not a library entry worth keeping.
Removing one today means N API calls, N transactions and N chances to stop
half-way, with no state that says the album was meant to go.

At the current scale that is ~1900 calls for the dead portion of one library.

### Why music is simpler than TV here, not harder

For a series, a season pack is one torrent whose episodes live independent
lives: that is where the refcount, the per-episode reaper and the gap registry
come from. **For an album the torrent is the album**, so the unit of removal and
the unit of acquisition coincide, which for TV they never do.

Measured on the production registry, excluding one album added by hand with a
flat two-level layout:

| Relation | Violations |
|---|---|
| albums spanning more than one hash | **0** |
| albums carrying more than one external id | **0** |
| hashes covering more than one album | **0** |
| external ids covering more than one album | 1 |

397 albums, 397 hashes. The one duplicated id is the same release imported
twice, not a torrent shared between albums.

### Which key identifies an album

- **hash** — a perfect match today, but that is a property of this data, not an
  invariant. One discography torrent breaks it, and that is exactly the case
  where removing by hash takes away albums the caller did not name.
- **external id** — semantically the right answer, but already not unique here,
  and the contract makes it optional: a caller may register none.
- **path prefix** — what the user sees in the player, what they mean by "this
  album", and the only key always present.

### What done looks like

A prefix form on the existing endpoint:

```json
{"type":"music","prefix":"Artist/Album"}
```

- removes every projection under that prefix **in one transaction**, reusing the
  existing mark → unpublish → unlink → prune → forget sequence per row, so a
  crash mid-album is recovered by the same startup sweep as a single removal;
- refuses with `409` when the rows under the prefix do not share exactly one
  hash: today that never fires, and the day it does it is telling the caller the
  directory is not an album;
- leaves the torrent decision to `dropTorrentGuarded`, unchanged — a torrent
  still referenced by another projection survives, which is what makes the
  discography case safe rather than special;
- keeps the exact-path form as it is. The prefix form is an addition, not a
  replacement: a caller that knows the single path it wants should not have to
  express it as a prefix.

### Guardrails, agreed 2026-09-22 before implementation

- **Component-wise and section-relative.** `Artist/Album` must not match
  `Artist/Album2`: the comparison is on path components, never a string prefix.
  This is the same rule §6.6 already states for `list`'s `prefix` ("safe prefix
  validation") and it should reuse it rather than grow a second definition. The
  section root is never a valid prefix and is never pruned.
- **`409` when the rows under the prefix do not share exactly one hash.** Today
  that never fires (397 albums, 397 hashes); a discography pack is what makes it
  fire, and the caller is being told the directory is not an album.
- **The per-row sequence is unchanged**: mark → unpublish → unlink → prune →
  forget. No new group state is introduced, so an album interrupted half-way is
  finished by the existing startup sweep of `removing` rows, with no recovery
  code of its own.
- **Response carries counts**, `removed` and `absent`, plus
  `torrent_referenced`. A prefix matching no rows is a success with
  `removed: 0`, not a `404`: the operation is idempotent, which is what a reaper
  retrying after a partial run needs.

### Scope: this is a deliberate departure from the Phase 1 spec

The engine spec defers prefix removal to Phase 2 in three places — the Remove
row of the §4 capability table (line 105), the §5 non-goals (line 159), and
§6.7 (line 783: "Hash removal, prefix removal, recursive directory removal, and
`paths: []` batch removal are Phase 2 conveniences").

Production changed the premise: with one library at 397 albums and roughly a
third of it on dead swarms, the per-track form makes the only real removal
operation cost ~1900 calls. The extension is taken knowingly, and the spec text
should record it rather than leave the document contradicting the code.

### What this unblocks

An audio reaper, which does not exist today (`internal/syncer` has no audio
path, and `reapFlaggedTitles` is keyed by IMDB id, which audio has none of). With
1:1 album-to-hash the selection is a `GROUP BY hash` over the projections whose
torrent stopped answering, and the action is one call per album instead of one
per track.

Until then a dead album is rescanned by the media server on every pass, and the
cost is real: sessions against a dead swarm run 1m28s-1m50s before giving up, and
they hold one of the 15 shared concurrency slots while they do — a limit sized
for 4K video, not for a scan walking thousands of tracks.


---

## Closing note — PR 3, 2026-09-22

Closed with every code item done. Two roadmap points are closed on evidence that
is real but not the one the roadmap asked for, and the difference is recorded
here rather than smoothed over.

### §12 — what was actually measured

The reference workload in the roadmap is a 4K stream plus a cold 32-part
audiobook scan plus a full library scan. What ran instead was larger and less
controlled: an import that took the music section to **5,808 projections**, with
a live Plex scan running concurrently with playback, over more than twelve hours.

Held through it:

- no panic, no fatal, no restart caused by the engine;
- startup reconciliation re-read the whole registry with **0 missing stubs and 0
  size mismatches**, across several restarts;
- **5,744 files, 5,744 distinct inodes**, no duplicates and no zero — §6's
  stability measured on production data rather than on unit tests;
- peer ejection stayed at 0 during the scan, so the outlier policy did not
  misfire on a workload it had never seen.

**What was not collected**: the formal numbers the roadmap names — read latency
distribution, `EAGAIN`/`EIO`/`ETIMEDOUT` counts, RAM ceiling, total scan
duration. The evidence is robustness under a heavier workload than specified,
not the measurement itself.

On the scan duration, measured afterwards rather than guessed: Plex reports
progress per completed album, not smoothly — it sat at 4% for 90 seconds between
jumps — so a short window says nothing. Over twenty minutes it moved 1% → 4%,
about seven minutes per point, which puts a full cold scan of 5.8k projections
at **roughly 11-12 hours**. An earlier note in this file said ~50 hours; that
divided by time since service start rather than since the scan began, and was
wrong.

The audiobook half was parked earlier by the maintainer and stays parked.

### §13 — which scanners, and why the rest is debt

Plex and Plexamp are verified live, including webhook identity through
`mbid://`. They are the deployment that exists.

Navidrome, Jellyfin and Audiobookshelf are **not** verified, and that matters
more than "three scanners we do not run", because one design decision rests on
them: `Readdir` blocks on an unready audio namespace instead of returning
`EAGAIN`, and the reason is how those scanners react to the errno — Navidrome
retries once and returns what it has, Audiobookshelf converts the error to `[]`
and marks existing items missing. That analysis came from PR 1's research and
was accepted on its merits. It has never been observed here.

So the behaviour is **inferred, not measured**. If one of those three is ever
deployed, that is the first thing to check, and a disagreement with the research
is a finding, not a surprise.

### Residuals deliberately left

- `marked` is discarded in `RemoveAudioPrefix`. With the update restricted to the
  paths read, a short count only means some rows were already `removing` — the
  resume case. It is information thrown away, not a defect.
- The audio reaper has never condemned anything: at close, the library was less
  than 24 hours old, so nothing could satisfy "3 failures spanning 24h". The
  dry run reported 416 albums, 0 candidates, and 1 album skipped for an active
  session — the guard working on a real case, which is the part worth having
  seen.
