# Contributing to Tiramisu

Tiramisu is a Torrent engine and a custom FUSE virtual filesystem: bytes are streamed on demand from a swarm to your media server, without ever downloading the file to disk. It ships as a single Go binary and it is built to stay small on purpose.

This document explains what fits the project, and what will be closed. Reading it before opening an issue or a pull request is the difference between a review and a rejection.

## What Tiramisu is not

Tiramisu is not a download manager, not a debrid client, not a media library UI, and not a media player. It is the infrastructure layer between a torrent swarm and a media server. Everything above that layer (library administration, release selection, naming, tradeoffs) is policy, and policy does not live in this repository: it lives in a versioned [agent skill](hermes/SKILL.md) driven by the user's agent of choice.

## What is welcome

- Improvements, bug fixes, data race fixes and optimizations in the FUSE layer, in the GoStorm engine, and in the torrent client layer, including the vendored anacrolix/torrent fork.
- Agentic autonomy built on the skill: the skill is the base. An MCP server is welcome, but never without a skill and guardrails.
- Improvements to the versioned skill, where the project's policy lives.
- Docker: the maintainer does not run Tiramisu in Docker by design, but every improvement to the Docker image is more than welcome.

## Before you open a pull request

1. **Open an issue first** for anything that is not a small, self-contained bug fix: new features, API changes, refactors, behavioral changes, new dependencies. Wait for a maintainer's answer before writing code. PRs that arrive without a prior issue will usually be closed with a pointer to this document.
2. **Read the project's own documentation first.** Many "gaps" are described in the [README](README.md), the [release notes](https://github.com/MrRobotoGit/tiramisu/releases), or the [configuration reference](README.md#configuration-reference). Characterizing the project before reading it wastes your time and ours.
3. **Search existing issues, PRs and forks.** Your idea may already have been proposed, implemented, or explicitly rejected.

## Where things belong

**The engine.** Minimal, stable primitives: torrent add/get/list, FUSE stub creation, blacklist on unlink from the mount, the config API on `:9080`, and the Library API (`add` / `remove` / `list`, raw primitives only). These endpoints exist so clients without filesystem access can do the same operations the mount exposes.

**The skill layer.** Policy: search, scoring, release selection, language and bandwidth tradeoffs, duplicate detection, naming, verification. A server endpoint freezes a policy and cannot ask the user when a tradeoff appears; a skill iterates like documentation. Logic like this will not be merged into the engine, and it will not be merged into the embedded Control Panel either.

**The media server.** Metadata, library scanning, subtitle fetching, playback. Do not build these into the FUSE daemon.

The FUSE mount is a projection of engine state, not an administrable directory. Operations that bypass the engine handlers leave orphaned state: removals go through the API or the media server, never through manual file operations on the mount.

## House rules

- **Architecture.** New Go files go into an existing package under `internal/`. The repository root is reserved for `main.go`, `version.go` and the legacy files still coupled to `main`; it is not a valid place for new code. Global state lives in `main.go`; internal packages export constructors and types, never singletons.
- **Root cause, in Go.** Fixes belong in the Go code path. The embedded HTML/JS is a thin layer; a frontend patch over a backend defect will be rejected.
- **Performance budget.** Raspberry Pi 4 is the baseline. Memory ceilings, read-ahead budget and startup time are part of the contract. Changes to hot paths need measurements: before/after, with the workload and the hardware named.
- **Dependencies.** Do not add a module without discussing it in an issue first. The anacrolix/torrent fork is vendored and patched in-tree; upstreaming those patches is done deliberately, not as a side effect of a PR.
- **API surface.** The endpoint surface is deliberately small and frozen. New endpoints require an issue and an explicit maintainer decision, and they must expose raw primitives, never policy.
- **CI and publishing.** Registry and multi-architecture publishing (Docker Hub, GHCR, amd64 + arm64) is out of scope for PRs. Changes that alter or drop part of that matrix will not be merged.
- **No data loss by design.** Do not add cleanup or deletion that acts on artifacts created by the feature itself, and do not infer live state from a RAM+DB union. If a fix removes files, prove what is actually live before touching it.

## Out of scope, deliberately

Features removed from the codebase stay removed unless a maintainer asked for them in an issue: FFprobe integration, M3U, WebDAV, DLNA, the React UI, Telegram bot, image libraries, and anything else that grew the binary without serving streaming. Resurrecting them in a PR is not a neutral addition: it is a reversal of a design decision that was made on purpose.

## Evidence required

Every PR premise is verified against the actual repository code, not only against the file it modifies. Reviewers will check defaults for behavioral impact, test coverage on semantic changes, and alignment with the project's philosophy.

- **Reproduce before you fix.** State the observed behavior, the expected behavior, and how you reproduced both.
- **Go tests are part of the change.** Behavioral changes and bug fixes come with `*_test.go` coverage: a test that fails without the patch and passes with it. PRs without tests will be asked to add them.
- **Numbers, not adjectives.** "Faster", "cleaner", "more robust" are not evidence. Show before/after measurements.
- **One concern per PR.** A coherent thesis, not a bundle of isolated patches. If two changes cannot be justified together, split them.
- **LLM-assisted contributions are welcome with evidence per premise.** A plausible patch generated without repository context will be asked for verification or closed. Attach what you ran, what you measured, and what you ruled out.

## Review

Review is direct and technical. Expect every claim to be checked, including the ones you are confident about. A PR that conflicts with the scope in this document will be closed with an explanation, even if the code is well written. A fork that diverges in philosophy is fine and welcome under the license: it simply does not set the direction upstream.

This is a single-maintainer project with no SLA: expect days, not hours. If an issue or PR sits without an answer, a ping is fair.

## Licensing

Tiramisu is licensed under GPL-3.0-only. By submitting a contribution you agree that it is licensed under the same terms, and you grant the maintainer a perpetual, worldwide, non-exclusive, royalty-free, irrevocable right to relicense it under other terms, including commercial terms.

Only submit code you have the right to license, and do not copy code from projects whose license is incompatible with GPL-3.0. Forks are welcome under the same license, but the GPL requires them to keep the LICENSE text, keep the copyright notices, and state which files they modified and when: stripping any of these is a license violation, not a fork.

## Security

Do not open a public issue for a vulnerability. Use the repository's private vulnerability reporting in the Security tab, so the report stays private until a fix ships.

## Style

- Public repository text is in English, including commit messages, PR descriptions and release notes.
- No AI or LLM attribution trailers in commits.
- Code comments are at most one or two lines, and only document constraints that are not obvious from the code. Historical rationale and discarded alternatives belong in the changelog, never in the code.
- Run `gofmt` and `go vet` before pushing; the build must be clean.
