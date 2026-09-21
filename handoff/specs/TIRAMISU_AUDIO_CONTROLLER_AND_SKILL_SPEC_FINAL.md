# Tiramisu Deterministic Audio Controller & Optional Agent Skill Specification

**Status:** Proposed companion architecture for automatic music/audiobook selection above the Phase 1 Tiramisu projection API.

This document is **not** an engine/API/FUSE contract and does not propose moving media semantics into the Tiramisu engine. Its primary subject is a **deterministic external audio controller** that makes automatic music and audiobook selection usable without an AI agent. An AI-agent skill is an optional interface/orchestration layer, not a dependency of the controller or of Tiramisu audio projection.

The architectural split is:

```text
      CLI / Web UI / automation          optional AI agent skill
                 \                          /
                  \                        /
                   v                      v
                 Deterministic Audio Controller
                 ------------------------------
                 canonical identity resolution
                 metadata-provider federation
                 torrent discovery
                 deterministic gates/scoring
                 release/recording selection
                 stable naming/path construction
                 reconciliation/lifecycle policy
                 downstream scan orchestration
                              |
                    inspect/add/list/remove
                              v
                          Tiramisu
                 projection + streaming engine
                              |
                              v
             Plex / Navidrome / Audiobookshelf / Jellyfin
```

## Architectural requirements

1. **Deterministic-first.** Automatic selection MUST be implementable and usable without an LLM or AI agent. Given the same provider facts, configuration, controller state, and candidate set, the controller SHOULD produce the same decision.
2. **AI is optional.** The agent skill MAY translate natural-language intent, explain evidence, ask the user to resolve ambiguity, or invoke controller operations. It MUST NOT be the only place where correctness-critical matching, scoring, identity, or reconciliation logic exists.
3. **Ordinary clients are first-class.** A CLI, web UI, scheduled automation, or another application MUST be able to drive the same deterministic controller behavior as an agent.
4. **Ambiguity is explicit.** When deterministic evidence does not meet an automatic-selection threshold, the controller SHOULD return structured alternatives or a review-required state rather than depending on an AI guess.
5. **Tiramisu stays generic.** The controller consumes raw Tiramisu primitives and persists semantic state independently. Tiramisu does not gain MusicBrainz/Audible/AudioSilo concepts, release scoring, narrator logic, or media-server naming policy.
6. **Repository boundary remains respected.** This companion controller is outside the Tiramisu engine. Where Tiramisu's own repository carries AI-facing policy, `CONTRIBUTING.md` places that policy in the versioned agent skill; that does not make the deterministic controller AI-dependent.

The Phase 1 engine proposal intentionally contains only the raw primitives and filesystem guarantees needed by ordinary deterministic callers and optional agent-driven callers alike.

## Deployment/qualification notes moved out of the engine spec

- Navidrome: explicit scan orchestration is external; embedded tags/art are preferable because Phase 1 does not project sidecars.
- Audiobookshelf: folder/book naming and metadata-verification policy are external; explicit scan orchestration is external.
- Jellyfin: analysis choices such as LUFS/audio normalization are media-server configuration, not Tiramisu policy.
- Plex/Plexamp: naming conventions and optional sonic/loudness analysis are media-server/external-application concerns, not Tiramisu policy.

## Source metadata verification moved out of the engine spec

The deterministic controller may prefer sources with sufficient embedded metadata for the intended media server, but Tiramisu itself should not reject a semantically valid source because TITLE/ARTIST/ALBUM/narrator/chapter/provider identifiers are absent. Tiramisu preserves selected source bytes; the external layer decides whether those bytes are suitable.

---

# 1. Deterministic controller interoperability contract

This section is normative for a deterministic controller claiming conformance with this workflow. It does **not** move music/audiobook semantics into the Tiramisu engine. A conforming controller MUST preserve the engine/domain boundary while producing inputs that ordinary media servers can consume reliably, and MUST remain usable without an AI agent.

## 1.1 Common deterministic workflow

The controller is expected to:

1. resolve the user's semantic intent into a canonical music Release or audiobook Recording/Release target;
2. discover torrent candidates using external discovery/indexer policy;
3. call Tiramisu Inspect and use exact returned `source_path` values;
4. evaluate candidate immutable source files using domain-specific identity and representation-quality evidence;
5. construct final section-relative paths following the downstream server's naming policy and containing the required `_hash8` suffix;
6. call Add, preferably batching all desired files from one torrent in one request;
7. persist its semantic mapping independently of Tiramisu;
8. explicitly trigger the affected downstream library scan after committed controller state;
9. reconcile periodically with paged audio List;
10. call exact-path Remove only when its semantic policy retires a projection;
11. create a new path for changed source content rather than rebinding an existing path;
12. treat `torrent_present=false` as runtime residency information, not loss of library ownership.

Tiramisu does not need to know why a projected path represents a track, album, audiobook part, author, narrator, series, edition, remaster, abridgement, or preferred release.

## 1.2 Source immutability

The controller MUST NOT rely on rewriting tags, transcoding containers, or manufacturing sidecars inside Tiramisu. Candidate quality must therefore be evaluated **before projection** as far as practical.

Controller metadata may evolve after projection, but the projected source binding is immutable:

```text
virtual_path
    -> section
    -> torrent infohash
    -> source_path
    -> resolved file_index
    -> exact source byte length
```

Display metadata improvements MUST NOT silently rename or retarget a published path. Renames/replacements are explicit migrations.

## 1.3 Identity confidence and representation quality are separate

Both domain studies converge on the same selection rule:

> establish that the candidate is the correct semantic object first; rank technical representation quality second.

A high-bitrate wrong edition or wrong narrator MUST never defeat a lower-bitrate source that is actually the requested release/recording.

---

# 2. Music controller identity and provider architecture

This appendix is controller guidance, not Tiramisu engine behavior.

## 2.1 Canonical music graph

The controller SHOULD model:

```text
Artist
  -> ArtistCredit
ReleaseGroup                 # conceptual album/work
  -> Release                 # concrete edition/issue
      -> Medium              # disc/medium position
          -> Track           # release-specific occurrence/position
              -> Recording   # underlying recorded audio identity

SourceTorrent(infohash)
  -> SourceFile(file_index, source_path, length)
      -> parsed FLAC evidence

ProjectionBinding
  -> canonical Release/Track
  -> immutable SourceFile
  -> virtual_path
```

MusicBrainz SHOULD be canonical for Artist, Release Group, Release, Track and Recording. Medium identity is local `(release_mbid, medium_position)`. ArtistCredit remains structured ordered credit data rather than a fuzzy string identity.

The critical distinction is:

- **Release Group** = conceptual album family;
- **Release** = exact issue/edition the controller is trying to source;
- **Track** = position-specific occurrence on that release;
- **Recording** = underlying recorded audio, potentially shared by many releases.

Recording MBID or track-set equality MUST NOT be used as the edition key. A remaster/reissue/regional pressing can be a distinct Release even when recordings overlap.

## 2.2 Recommended music provider routing

| Provider | Controller role | Must not decide |
|---|---|---|
| MusicBrainz | canonical identity backbone; release/medium/track/recording graph; labels/catalog/barcodes/status/date | torrent source quality or popularity |
| Cover Art Archive | cover-art lookup for known MBIDs | identity by image alone |
| ListenBrainz | optional popularity/discovery/ranking signals | edition identity |
| AcoustID + Chromaprint | escalation path for untagged/ambiguous recordings | concrete Release identity by itself |
| Discogs | corroborate pressing/edition, label/catalog/format | replace MusicBrainz canonical keys |
| Last.fm | optional discovery/similarity where terms permit | canonical identity |
| Local MusicBrainz mirror | scalable local access to same canonical data | different semantic authority from MusicBrainz |

Provider identifiers SHOULD be stored as external mappings with provenance. Licensing/rate-limit policy belongs in the controller and MUST be tracked per source; expressive artwork/descriptions are enrichment assets, not identity facts.

## 2.3 Music source evaluation

Candidate FLAC sets SHOULD first pass hard structural gates, including:

- contradictory coherent MusicBrainz Release IDs;
- incompatible Release-track IDs/positions;
- duplicate `(discnumber, tracknumber)` positions;
- missing required target tracks;
- album-image FLAC + CUE when Phase 1 requires separately projected tracks;
- hidden-track/file-boundary structure incompatible with the selected Release;
- multidisc sources lacking enough immutable metadata to identify disc positions reliably.

Only after hard gates should positive evidence be scored. Evidence priority SHOULD be approximately:

```text
concrete Release MBID
> Release-track MBIDs
> Recording MBIDs
> exact medium/track structure
> per-track/album duration agreement
> album-artist/title
> barcode/catalog/label/country evidence
> ISRC corroboration
```

Audio fidelity, bit depth, sample rate and bitrate are **representation-quality** inputs and MUST NOT compensate for identity contradictions.

A practical confidence policy is:

- `EXACT`: direct coherent release-level identifiers + structural agreement;
- `HIGH`: strong multi-signal edition match without contradiction;
- `MEDIUM`: plausible but ambiguous, requires review;
- `LOW`: strings/weak evidence only, do not auto-project;
- `REJECT`: hard contradiction or incomplete/wrong structure.

## 2.4 Music embedded metadata target

Because Tiramisu will not rewrite FLAC bytes, the controller SHOULD prefer candidates with interoperable embedded tags, especially:

```text
TITLE
ARTIST
ALBUM
ALBUMARTIST
TRACKNUMBER / TRACKTOTAL
DISCNUMBER / DISCTOTAL
DATE / ORIGINALDATE
COMPILATION where applicable
MUSICBRAINZ_ALBUMID
MUSICBRAINZ_RELEASEGROUPID
MUSICBRAINZ_RELEASETRACKID
MUSICBRAINZ_TRACKID / RECORDINGID alias where present
ISRC
BARCODE / CATALOGNUMBER where present
embedded front-cover picture where available
```

A source may still be the correct release while having thin tags; tag quality is therefore an interoperability/representation score rather than semantic identity.

## 2.5 Music controller state

A useful deterministic state machine is:

```text
REQUESTED
 -> RESOLVED_GROUP
 -> RESOLVED_RELEASE
 -> SEARCHING
 -> INSPECTED
 -> EVALUATED
 -> APPROVED
 -> PROJECTED
 -> SUPERSEDED
```

The controller SHOULD persist the selected Release snapshot, candidate source evidence, hard contradictions, confidence, technical profile, torrent/file identity and projection path so future reconciliation is explainable rather than recomputed fuzzily.

A future **media-neutral bounded source-range read** would be valuable for pre-projection FLAC tag/probe inspection, but it is not a Phase 1 engine requirement. A music-specific `InspectFLACTags` endpoint would violate the desired boundary.

---

# 3. Audiobook controller identity and provider architecture

This appendix is controller guidance, not Tiramisu engine behavior.

## 3.1 Layered audiobook identity

The controller MUST treat audiobook identity as more than book identity. Recommended ontology:

```text
Literary Work
  -> TextEdition / textual basis
  -> AudiobookRecording
       -> narrator/cast credits
       -> spoken language / translation
       -> text completeness / abridgement
       -> performance type
       -> runtime assertions
       -> recording-level chapter assertions
       -> AudiobookRelease(s)
            -> ASIN / audiobook ISBN / territory
            -> publisher / imprint / release date / artwork
            -> SourceRepresentation(s)
                 -> torrent infohash
                 -> selected source paths
                 -> one M4B / many M4A / many MP3
                 -> AudioPart(s)
```

Controller-owned UUIDs SHOULD be primary keys for Work, TextEdition, AudiobookRecording and AudiobookRelease. Provider IDs are mappings with provenance, not database primary keys.

Core invariants:

```text
same recording != same book
same recording != same retail product
same recording != same torrent
same recording != same file packaging
```

Examples:

- same novel + different narrator => different Recording;
- same narrator + abridged vs unabridged => different Recording;
- different translation/textual basis => different Recording target;
- same performance sold in different territories/ASINs => same Recording, possibly different Releases;
- same performance as one M4B vs twenty MP3s => same Recording, different SourceRepresentation.

## 3.2 Recording classification

The Recording model SHOULD carry independent fields rather than one overloaded flag:

```text
text_completeness:
    UNABRIDGED | ABRIDGED | CONDENSED | SUMMARY | UNKNOWN

performance_type:
    SINGLE_NARRATOR | MULTI_NARRATOR | FULL_CAST |
    DRAMATIZED_ADAPTATION | AUTHOR_READ |
    SYNTHETIC_OR_VIRTUAL_VOICE | OTHER | UNKNOWN
```

Narrators/cast MUST be structured Person/RecordingCredit relationships with aliases and provider IDs. String normalization may aid matching but MUST NOT automatically merge people or pseudonyms.

Recording-level logical chapters and representation-encoded chapters SHOULD be stored separately because retailer/source packaging can split or combine the same spoken performance differently.

## 3.3 Identifier hierarchy

| Identifier | Semantic level | Controller treatment |
|---|---|---|
| Open Library Work ID | Work | strong work mapping |
| Open Library Edition / ISBN | textual/publication edition | edition evidence, not recording identity |
| AudioSilo Recording ID | audiobook recording | strong open recording evidence |
| LibriVox project ID | LibriVox recording project | strong recording/project identity within LibriVox |
| Hardcover audio Edition ID | audio manifestation | strong evidence but may conflate release/recording layers |
| ASIN | retail release/product, region-sensitive | Release ID; map Release -> Recording |
| audiobook ISBN | published audio product | Release evidence |
| torrent infohash | immutable source package | SourceRepresentation only |
| `(infohash, source_path)` | immutable source file | AudioPart only |
| `_hash8` | projection discriminator | never bibliographic identity |

An ASIN is powerful evidence but MUST NOT become the global Recording key. An exact ASIN already mapped by trustworthy provenance to a Recording can contribute to `EXACT`; a naked ASIN still resolves through Release -> Recording.

## 3.4 Recommended audiobook provider routing

| Provider | Controller role |
|---|---|
| AudioSilo Meta | preferred open recording-identity provider; Work -> Recording facts including narrators/runtime/abridgement/chapters where present |
| LibriVox | primary authority for LibriVox recording projects, readers, sections and runtime |
| Hardcover | strong fallback/corroborator for audio editions, narrator contributions, ASIN/runtime and series/book data |
| Open Library | primary Work/TextEdition/ISBN resolver; not recording authority |
| Google Books | bibliographic/ISBN/publication/cover/description fallback; not recording identity |
| Dedicated ISBN services | optional publication lookup only |
| Audiobookshelf community providers | discovery adapters whose upstream provenance/terms determine trust |
| Audible-derived aggregators / ASIN evidence | optional commercial-release evidence; not foundational canonical identity |

The controller SHOULD maintain field-level assertions with provider, raw value, normalized value, provider record ID, correlation group, retrieval/update time, license class, confidence/precision and active/superseded/disputed state. Multiple providers that copy one upstream marketplace record MUST NOT count as independent votes.

## 3.5 Audiobook source matching

Hard vetoes SHOULD include:

- explicit incompatible narrator/cast when both sides are authoritative;
- explicit abridged/unabridged conflict;
- incompatible performance type;
- spoken-language mismatch;
- known translation mismatch;
- gross runtime incompatibility;
- provider ID known to map to another Recording;
- missing/duplicated source parts that make the representation incomplete.

Positive evidence can then support confidence, with strongest weight given to recording/release-scoped IDs, narrator/cast agreement, text/language/completeness, runtime, chapter fingerprints and coherent part structure.

Recommended classes:

- `EXACT`: explicit recording-scoped ID or trusted Release ID mapped to target + no hard conflict + audio-specific corroboration;
- `HIGH`: strong narrator/language/completeness/runtime agreement and convincing Work identity;
- `MEDIUM`: plausible but materially ambiguous; human review;
- `LOW`: title/author/ISBN/path evidence without enough recording evidence; do not auto-project;
- `REJECT`: hard conflict or incomplete representation.

Representation ranking occurs only after identity passes. A clean single M4B is normally operationally preferable to multipart audio when both represent the same verified Recording, but a wrong-narrator M4B always loses to the correct multipart MP3 source.

## 3.6 Stable audiobook paths

Readable controller-owned layouts may resemble:

```text
Author/Title/Title_<hash8>.m4b
Author/Series/NN - Title/01 - Part_<hash8>.mp3
```

Narrator SHOULD NOT be included in every path merely as display metadata. When two distinct recordings of the same Work coexist, the controller SHOULD semantically disambiguate their item directories, for example:

```text
Author/Title [Narrator A]/...
Author/Title [Narrator B]/...
```

The first published path is pinned. Later metadata cleanup does not rename it automatically.

A future media-neutral probe/range-read facility could expose duration, codec/container, raw tags, chapters, artwork presence/hash, corruption status or bounded bytes without interpreting audiobook semantics. Those are useful future generic inspection capabilities, not Phase 1 requirements.

---

# 4. Optional AI agent skill

The agent skill is an optional conversational/interface layer over the deterministic system. It MAY:

- translate natural-language requests into structured controller intent;
- ask the user to resolve ambiguous editions, narrators, releases, or tradeoffs;
- explain why deterministic gates accepted or rejected a candidate;
- invoke controller operations and report their results;
- expose manual Tiramisu primitives when an advanced user explicitly wants them.

The skill MUST NOT:

- be required for scheduled or automatic library maintenance;
- contain the only implementation of canonical identity, hard vetoes, scoring, naming, or reconciliation;
- silently override deterministic controller results without an explicit user-directed policy path;
- move media semantics into the Tiramisu engine.

A deployment with no AI system at all remains a complete supported architecture:

```text
CLI / UI / scheduler -> deterministic audio controller -> Tiramisu -> media server
```

The AI-agent path is additive:

```text
user -> optional agent skill -> deterministic audio controller -> Tiramisu -> media server
```
