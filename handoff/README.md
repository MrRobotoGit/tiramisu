# Handoff — audio projection (issue #25)

Written 2026-09-21 for the maintainer, at branch `feature/audio-projection`
tip `f83810a`. Everything here is documentation: no Go file in this folder, and
nothing in it is imported or built.

## What this is

Phase 1 audio support was built against the specs in `specs/`, in three PRs.
PR 1 is merged. **PR 2 is implemented but not ready to merge** — a third
adversarial review returned REJECT. This folder is what someone picking the
work up needs, without having to reconstruct it from the diff.

| File | What it is |
|---|---|
| `PR2_REMAINING_WORK.md` | **Start here.** The outstanding work, ordered, each item with a file, a line range and a concrete failing input. |
| `PR3_CARRIED_ITEMS.md` | What was deliberately deferred out of PR 2, and why. |
| `specs/` | The engine spec, the controller/skill spec, and the phase-1 roadmap these PRs were built against. |
| `evidence/` | The second and third adversarial review reports, verbatim. |

## How the work was done

Implementation and tests were written by different agents, with different
model providers in the two seats, and no agent both authored tests and
implemented the same slice. Accepted test files were pinned by blob hash so the
implementer could not quietly edit a test into passing. Each slice then got an
adversarial review by a third agent that did not write it.

That mattered: **the review found a real defect in every single slice**, and
none of those defects was caught by the build, the race detector, `go vet`,
`gofmt`, deadcode, staticcheck or the project's own wiring guards, all of which
are green at `f83810a`.

Per CONTRIBUTING's evidence rule, what was run and what it showed is in
`evidence/`, and `PR2_REMAINING_WORK.md` states what the hardware test proved
and — more usefully — what it did not.

## Caveats on reading the evidence

The two review reports are reproduced exactly as written, so they cite a few
paths under `.agent-workflow/` and `.tdd-state/`. Those are local workflow
scaffolding, deliberately kept out of this repository, and are not present in
the tree. The citations to files under `internal/` and to `main.go` are all
live and checkable.

The reports are also blunt about the implementer's own mistakes, including one
case where a fix intended to improve containment introduced a worse hole than
the one it closed (H5 in `PR2_REMAINING_WORK.md`). That is left in rather than
softened, because it is the most useful thing in the report.
