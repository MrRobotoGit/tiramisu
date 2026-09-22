package library

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Section is a physical/FUSE root directly under the configured source path. Its
// string value is the directory name: music/ and audiobooks/ are siblings of
// movies/ and tv/, never children of a shared audio/ parent.
type Section string

const (
	SectionMovies     Section = "movies"
	SectionTV         Section = "tv"
	SectionMusic      Section = "music"
	SectionAudiobooks Section = "audiobooks"
)

// sectionExtensions is the whole Phase 1 allowlist. Keeping it in one table is
// what stops extension switches from spreading back through the VFS call sites.
var sectionExtensions = map[Section][]string{
	SectionMovies:     {".mkv"},
	SectionTV:         {".mkv"},
	SectionMusic:      {".flac"},
	SectionAudiobooks: {".m4b", ".m4a", ".mp3"},
}

// apiTypeSections holds the canonical Library API types only. Aliases are
// rejected so an audio request cannot fall back to movie handling.
var apiTypeSections = map[string]Section{
	"movie":     SectionMovies,
	"tv":        SectionTV,
	"music":     SectionMusic,
	"audiobook": SectionAudiobooks,
}

// caseFolder applies Unicode default case folding. cases.Fold is documented as
// stateless and safe to share, unlike Caser in general.
var caseFolder = cases.Fold()

// SectionForType maps a canonical Library API type to its section. It does not
// trim or case-fold: normalising the request field stays with the caller.
func SectionForType(apiType string) (Section, bool) {
	s, ok := apiTypeSections[apiType]
	return s, ok
}

// IsAudioSection reports whether the section holds audio projections.
func IsAudioSection(s Section) bool {
	return s == SectionMusic || s == SectionAudiobooks
}

// SectionExtensions returns the extensions the section projects, lowercase and
// dot-prefixed. The copy keeps a caller from editing the allowlist itself.
func SectionExtensions(s Section) []string {
	exts, ok := sectionExtensions[s]
	if !ok {
		return nil
	}
	return append([]string(nil), exts...)
}

// splitStub returns the final component's stem and extension. ok is false when
// the name cannot be a projection at all: no extension, no stem, or a trailing
// dot, which also covers "", "." and "..".
func splitStub(name string) (stem, ext string, ok bool) {
	base := name
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	i := strings.LastIndexByte(base, '.')
	if i <= 0 || i == len(base)-1 {
		return "", "", false
	}
	return base[:i], base[i:], true
}

// sectionAdmits reports whether ext is on the section's allowlist, compared
// verbatim: the caller decides whether to lowercase first.
func sectionAdmits(s Section, ext string) bool {
	for _, allowed := range sectionExtensions[s] {
		if ext == allowed {
			return true
		}
	}
	return false
}

// ExtensionAllowed reports whether name's extension is admitted by the section,
// compared case-insensitively.
func ExtensionAllowed(s Section, name string) bool {
	_, ext, ok := splitStub(name)
	if !ok {
		return false
	}
	return sectionAdmits(s, strings.ToLower(ext))
}

// ExtensionsAgree reports whether two paths carry the same extension. Tiramisu
// must never expose one container under another container's extension.
func ExtensionsAgree(sourcePath, virtualPath string) bool {
	_, source, okSource := splitStub(sourcePath)
	_, virtual, okVirtual := splitStub(virtualPath)
	return okSource && okVirtual && strings.EqualFold(source, virtual)
}

// HashSuffixToken returns the last 8 hex characters of a canonical infohash,
// lowercased. Unlike HashSuffix it validates, so the 32-character base32 form
// reInfoHash also accepts cannot produce a filename token.
func HashSuffixToken(hash string) (string, bool) {
	if len(hash) != 40 {
		return "", false
	}
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return "", false
		}
	}
	return strings.ToLower(hash[32:]), true
}

// HasHashSuffix reports whether name carries the mandatory "_<hash8>" token
// immediately before its extension. Tiramisu validates this convention and
// never appends or rewrites it.
func HasHashSuffix(name, hash string) bool {
	token, ok := HashSuffixToken(hash)
	if !ok {
		return false
	}
	stem, _, ok := splitStub(name)
	if !ok || len(stem) < len(token)+1 {
		return false
	}
	return stem[len(stem)-len(token)-1] == '_' &&
		strings.EqualFold(stem[len(stem)-len(token):], token)
}

// IsVirtualStub reports whether a name inside the section is a Tiramisu virtual
// stub, from its extension alone: no stat, no content sniffing, no registry
// lookup, because every VFS hot path shares this predicate.
//
// The two sides are deliberately asymmetric. Video keeps the exact
// case-sensitive ".mkv" match the existing FUSE call sites use, so movie/TV
// recognition does not widen. Audio paths come from the caller, so the
// comparison is case-insensitive, and a dot-leading basename is excluded to keep
// the hidden staging namespace out of any listing.
func IsVirtualStub(s Section, name string) bool {
	stem, ext, ok := splitStub(name)
	if !ok {
		return false
	}
	if !IsAudioSection(s) {
		return sectionAdmits(s, ext)
	}
	if strings.HasPrefix(stem, ".") {
		return false
	}
	return sectionAdmits(s, strings.ToLower(ext))
}

// PortablePathKey returns the non-user-visible collision key for a
// section-relative virtual path: two paths share a key exactly when a
// case-insensitive, normalising downstream filesystem would treat them as one
// file. The caller's own spelling stays the visible virtual_path.
//
// Each component is normalised, folded, then normalised again. The second pass
// is required, not tidiness: case folding does not preserve a normal form, and
// without it U+1E9B U+0323 and U+1E69 fold to different keys while a downstream
// filesystem collides them.
func PortablePathKey(virtualPath string) string {
	parts := strings.Split(virtualPath, "/")
	for i, part := range parts {
		parts[i] = norm.NFC.String(caseFolder.String(norm.NFC.String(part)))
	}
	return strings.Join(parts, "/")
}

// AudioRegistry is the subset of the audio projection registry the manager needs
// before dropping a torrent. *metadb.DB satisfies it.
type AudioRegistry interface {
	AudioHashReferenced(hash string) (bool, error)
}

// SectionForPath reports which section a physical path belongs to. Matching is on
// path components, not a string prefix: musicians/ is not music/.
func SectionForPath(sourcePath, fullPath string) (Section, bool) {
	if sourcePath == "" || fullPath == "" {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(sourcePath), filepath.Clean(fullPath))
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	head, rest, nested := strings.Cut(rel, "/")
	// A bare section name is the directory itself, and ".." means the path
	// resolved outside the source root: neither is a projection in a section.
	if !nested || rest == "" || head == ".." {
		return "", false
	}
	section := Section(head)
	if _, ok := sectionExtensions[section]; !ok {
		return "", false
	}
	return section, true
}

// UsesSSDWarmup reports whether a section uses the head/tail SSD warmup built for
// movie and TV playback. Audio skips it and adds nothing in its place: a track is
// small enough that demand fetch plus the existing read-ahead is the whole policy.
func UsesSSDWarmup(s Section) bool {
	if _, known := sectionExtensions[s]; !known {
		return false
	}
	return !IsAudioSection(s)
}

// VFSClass describes how the FUSE layer should treat one physical path.
type VFSClass struct {
	// Section is the section the path lives in, or "" when it is outside all of them.
	Section Section
	// Stub exposes the path as a virtual regular file rather than a passthrough.
	Stub bool
	// Audio selects audio stub validation and disables SSD warmup.
	Audio bool
}

// ClassifyPath decides how the VFS should treat a physical path. It does no I/O.
//
// A .mkv outside every known section stays a stub: the FUSE layer has always
// treated any .mkv as virtual regardless of where it sits, and narrowing that
// would make stubs in a non-standard layout vanish from the mount.
func ClassifyPath(sourcePath, fullPath string) VFSClass {
	section, inSection := SectionForPath(sourcePath, fullPath)
	if !inSection {
		_, ext, ok := splitStub(fullPath)
		return VFSClass{Stub: ok && ext == ".mkv"}
	}
	if !IsVirtualStub(section, fullPath) {
		return VFSClass{Section: section}
	}
	return VFSClass{Section: section, Stub: true, Audio: IsAudioSection(section)}
}

// MayDropTorrent reports whether a torrent may be removed from the engine as far
// as audio ownership is concerned. Callers stay responsible for their own media
// kind: this answers only the audio question.
//
// It fails closed. An answer that could not be obtained is not evidence of
// absence, and a torrent dropped under a live projection strands it.
func MayDropTorrent(reg AudioRegistry, hash string) (bool, error) {
	if hash == "" {
		return false, nil
	}
	if reg == nil {
		return true, nil
	}
	referenced, err := reg.AudioHashReferenced(hash)
	if err != nil {
		return false, fmt.Errorf("audio ownership check for %s: %w", hash, err)
	}
	return !referenced, nil
}

// UnavailableAudioRegistry answers every ownership question with an error. A
// caller passes it when audio ownership is configured but its registry could not
// be opened, so "unknown" is never mistaken for "no audio owns this".
type UnavailableAudioRegistry struct{ Err error }

func (r UnavailableAudioRegistry) AudioHashReferenced(hash string) (bool, error) {
	if r.Err != nil {
		return false, fmt.Errorf("audio registry unavailable: %w", r.Err)
	}
	return false, errors.New("audio registry unavailable")
}

// AudioPath identifies one committed projection.
type AudioPath struct {
	Section     Section
	VirtualPath string
}

// AudioOwnership answers whether the committed audio namespace holds a
// section-relative path. It is consulted on the FUSE hot path, so an
// implementation must not perform I/O.
type AudioOwnership interface {
	HasProjection(section Section, virtualPath string) bool
}

// AudioNamespace is an in-memory view of the committed audio namespace. The
// registry stays authoritative, but a query per Readdir entry would not survive
// the Pi 4 budget, so the VFS reads this snapshot. The zero value is empty.
type AudioNamespace struct {
	mu      sync.RWMutex
	entries map[AudioPath]AudioProjection
	// live holds entries added after the last full publish, so a reconciliation pass
	// that started before them can keep them (see PublishMerged).
	live    map[AudioPath]AudioProjection
	state   NamespaceState
	failErr error
	// wake is closed when this generation leaves Unreconciled, releasing every
	// blocked WaitServable at once. MarkUnready installs a fresh one rather than
	// reopening a closed channel.
	wake chan struct{}
}

// ErrAudioReconcileFailed marks a terminal reconciliation failure, so a caller
// can map it to a hard I/O error rather than reporting absence. A scanner that
// reads a failure as a deletion will remove the user's library.
var ErrAudioReconcileFailed = errors.New("audio reconciliation failed")

func NewAudioNamespace() *AudioNamespace { return &AudioNamespace{} }

// NamespaceState is why a reader may or may not be served. A boolean cannot
// express it: "not reconciled yet" and "there is no registry at all" need
// opposite answers from a blocking reader, and conflating them hangs any install
// that has an audio directory but no state DB.
type NamespaceState int

const (
	// Unreconciled is the zero value on purpose: a namespace nobody has published
	// must never look servable.
	Unreconciled NamespaceState = iota
	Ready
	Unavailable
	Failed
)

func (s NamespaceState) String() string {
	switch s {
	case Unreconciled:
		return "Unreconciled"
	case Ready:
		return "Ready"
	case Unavailable:
		return "Unavailable"
	case Failed:
		return "Failed"
	}
	return "NamespaceState(?)"
}

// transitionLocked moves to a servable or terminal state and releases every
// blocked waiter. Closing is guarded because a transition may repeat - two
// consecutive Publish calls are legal - and closing a closed channel panics.
func (n *AudioNamespace) transitionLocked(state NamespaceState, failErr error) {
	n.state = state
	n.failErr = failErr
	if n.wake == nil {
		return
	}
	select {
	case <-n.wake:
		// Already closed by an earlier transition in this generation.
	default:
		close(n.wake)
	}
}

// blockLocked returns to Unreconciled on a fresh generation, so readers arriving
// during the next reconciliation block again instead of seeing the previous
// generation's already-closed channel.
func (n *AudioNamespace) blockLocked() {
	n.state = Unreconciled
	n.failErr = nil
	n.wake = make(chan struct{})
}

// State reports what the namespace can currently answer.
func (n *AudioNamespace) State() NamespaceState {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.state
}

// MarkUnavailable records that there is no authority source at all - no state DB,
// so no projection can exist. Readers must proceed against an empty set rather
// than block: an install with a music/ directory and StateDB disabled would
// otherwise hang forever on the first listing.
func (n *AudioNamespace) MarkUnavailable() {
	n.mu.Lock()
	n.entries = nil
	n.transitionLocked(Unavailable, nil)
	n.mu.Unlock()
}

// MarkFailed records that reconciliation ran and errored. Readers get a terminal
// error, never a block and never an empty success.
func (n *AudioNamespace) MarkFailed(err error) {
	wrapped := ErrAudioReconcileFailed
	if err != nil {
		wrapped = fmt.Errorf("%w: %w", ErrAudioReconcileFailed, err)
	}
	n.mu.Lock()
	n.entries = nil
	n.transitionLocked(Failed, wrapped)
	n.mu.Unlock()
}

// WaitServable blocks while the namespace is Unreconciled and returns once it can
// be served or has terminally failed.
//
// This is what lets the VFS block a directory operation instead of failing it.
// Pass 4 established that the alternative is worse: on a readdir error Navidrome
// returns the entries it has, i.e. none, and Audiobookshelf marks existing items
// missing. Scanners tolerate slow; they do not tolerate empty.
//
// State is checked before cancellation, so an already-servable namespace answers
// even under a cancelled context - the caller asked a question that is already
// answered.
func (n *AudioNamespace) WaitServable(ctx context.Context) error {
	for {
		n.mu.RLock()
		state, failErr, wake := n.state, n.failErr, n.wake
		n.mu.RUnlock()

		switch state {
		case Ready, Unavailable:
			return nil
		case Failed:
			if failErr == nil {
				return ErrAudioReconcileFailed
			}
			return failErr
		}

		// Unreconciled. The zero value carries no channel, so install one before
		// waiting, and re-check state in case it moved while the lock was handed
		// over.
		if wake == nil {
			n.mu.Lock()
			if n.wake == nil {
				n.wake = make(chan struct{})
			}
			wake, state = n.wake, n.state
			n.mu.Unlock()
			if state != Unreconciled {
				continue
			}
		}

		select {
		case <-wake:
			// Transitioned; loop to read the new state.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Replace swaps the whole namespace at once, so a reader sees either the old set
// or the new one and a scan never observes a half-built library.
func (n *AudioNamespace) Replace(paths []AudioPath) {
	next := make(map[AudioPath]AudioProjection, len(paths))
	for _, p := range paths {
		next[p] = AudioProjection{Section: p.Section, VirtualPath: p.VirtualPath}
	}
	n.mu.Lock()
	n.entries = next
	// A full rebuild carries the whole set, so it supersedes any live addition that
	// was pending: keeping them would resurrect paths the rebuild left out.
	n.live = nil
	n.transitionLocked(Ready, nil)
	n.mu.Unlock()
}

// Add records membership for one path. It carries no identity, so it is not a live
// publication: a live addition must go through AddProjections, or a later
// PublishMerged will visibly drop what Add put there.
func (n *AudioNamespace) Add(p AudioPath) {
	n.mu.Lock()
	if n.entries == nil {
		n.entries = make(map[AudioPath]AudioProjection)
	}
	n.entries[p] = AudioProjection{Section: p.Section, VirtualPath: p.VirtualPath}
	n.mu.Unlock()
}

// Remove drops a path from the namespace. It must clear the live set too: a path
// removed from entries but left in live would be re-applied by the next PublishMerged
// and reappear on the mount.
func (n *AudioNamespace) Remove(p AudioPath) {
	n.mu.Lock()
	delete(n.entries, p)
	delete(n.live, p)
	n.mu.Unlock()
}

func (n *AudioNamespace) HasProjection(section Section, virtualPath string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	_, ok := n.entries[AudioPath{Section: section, VirtualPath: virtualPath}]
	return ok
}

func (n *AudioNamespace) Len() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.entries)
}

// ClassifyProjection is ClassifyPath plus the committed-row requirement spec
// §10.1 places on audio. Extension alone must never publish a projection, or a
// stub the registry disowned would still be listed and served. Video is
// classified exactly as ClassifyPath does and never consults ownership.
func ClassifyProjection(sourcePath, fullPath string, owned AudioOwnership) VFSClass {
	class := ClassifyPath(sourcePath, fullPath)
	if !class.Audio {
		return class
	}
	if owned == nil {
		return VFSClass{Section: class.Section}
	}
	rel, ok := sectionRelativePath(sourcePath, fullPath)
	if !ok || !owned.HasProjection(class.Section, rel) {
		return VFSClass{Section: class.Section}
	}
	return class
}

// sectionRelativePath returns the path below the section directory in slash
// form: the spelling the registry stores as virtual_path.
func sectionRelativePath(sourcePath, fullPath string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(sourcePath), filepath.Clean(fullPath))
	if err != nil {
		return "", false
	}
	_, after, found := strings.Cut(filepath.ToSlash(rel), "/")
	if !found || after == "" {
		return "", false
	}
	return after, true
}

// AudioProjection is a committed projection as the registry defines it. The VFS
// builds an audio file's identity from this rather than from the physical stub,
// so a stub that changes underneath cannot change what is served.
type AudioProjection struct {
	Section     Section
	VirtualPath string
	Hash        string
	FileIndex   int
	Size        int64
	MtimeNS     int64
	// UpdatedAtNS is the registry's last commit time for this projection. Directory
	// mtimes are derived from the newest one below them, so a directory changes when
	// its committed child set does and at no other time.
	UpdatedAtNS int64
	// Caller-supplied identity, carried so the VFS can put it on the playback
	// state a webhook later matches against. Empty when the caller supplied none.
	ExternalID          string
	ExternalIDNamespace string
	// A cue track serves Header, then the torrent file from ByteOffset.
	CueTrack   int
	ByteOffset int64
	Header     []byte
}

// Path returns the projection's namespace key.
func (p AudioProjection) Path() AudioPath {
	return AudioPath{Section: p.Section, VirtualPath: p.VirtualPath}
}

// Publish replaces the namespace with the committed set and marks it ready. It is
// how reconciliation says "this is the whole committed namespace"; an empty
// publish is a coherent answer, which an unpublished namespace is not. Startup uses
// PublishMerged instead, which keeps live additions that landed during the pass;
// this one is the rebuild path for tests and for callers that already hold the whole
// set, and it deliberately supersedes anything still pending in live.
func (n *AudioNamespace) Publish(projections []AudioProjection) {
	next := make(map[AudioPath]AudioProjection, len(projections))
	for _, p := range projections {
		next[p.Path()] = p
	}
	n.mu.Lock()
	n.entries = next
	n.live = nil
	n.transitionLocked(Ready, nil)
	n.mu.Unlock()
}

// PublishMerged replaces the committed set with projections and then re-applies the
// entries added live while the pass that produced projections was running. That pass
// reads the registry before it publishes, so a live add committed in between is absent
// from its rows; without the merge a successful, committed add would silently vanish
// from the mount. A live add is authoritative for its own path.
func (n *AudioNamespace) PublishMerged(projections []AudioProjection) {
	next := make(map[AudioPath]AudioProjection, len(projections))
	for _, p := range projections {
		next[p.Path()] = p
	}
	n.mu.Lock()
	for path, p := range n.live {
		next[path] = p
	}
	n.entries = next
	n.live = nil
	n.transitionLocked(Ready, nil)
	n.mu.Unlock()
}

// Lookup returns the committed projection for a path.
func (n *AudioNamespace) Lookup(section Section, virtualPath string) (AudioProjection, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	p, ok := n.entries[AudioPath{Section: section, VirtualPath: virtualPath}]
	return p, ok
}

// Ready reports whether a coherent committed namespace has been published. Until
// it is, the namespace holds no opinion, and a caller must fail an audio
// enumeration rather than answer from an empty set: spec §9 forbids serving a
// plausible but incomplete tree.
func (n *AudioNamespace) Ready() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.state == Ready
}

// MarkUnready drops published authority, so a failed re-reconciliation cannot
// leave a stale namespace being served as current.
func (n *AudioNamespace) MarkUnready() {
	n.mu.Lock()
	n.entries = nil
	n.blockLocked()
	n.mu.Unlock()
}

// AddProjections inserts a committed batch in one namespace update, so a reader can
// never observe half an album: a Readdir interleaved with per-row inserts would list a
// partial batch as if it were the whole library. The entries also count as live
// additions for a reconciliation pass that is already running (see PublishMerged).
func (n *AudioNamespace) AddProjections(projections []AudioProjection) {
	if len(projections) == 0 {
		return
	}
	n.mu.Lock()
	if n.entries == nil {
		n.entries = make(map[AudioPath]AudioProjection)
	}
	if n.live == nil {
		n.live = make(map[AudioPath]AudioProjection)
	}
	for _, p := range projections {
		n.entries[p.Path()] = p
		n.live[p.Path()] = p
	}
	n.mu.Unlock()
}

// AddProjection inserts one projection with its identity. Add keeps taking a bare
// path for callers that only need membership.
func (n *AudioNamespace) AddProjection(p AudioProjection) {
	n.AddProjections([]AudioProjection{p})
}

// CommittedProjectionFor returns the committed projection a physical path maps
// to, if the namespace owns it. It is what the VFS asks before serving an audio
// file: the registry row, not the stub on disk, decides identity.
func CommittedProjectionFor(sourcePath, fullPath string, owned AudioOwnership) (AudioProjection, bool) {
	if owned == nil {
		return AudioProjection{}, false
	}
	class := ClassifyProjection(sourcePath, fullPath, owned)
	if !class.Stub || !class.Audio {
		return AudioProjection{}, false
	}
	rel, ok := sectionRelativePath(sourcePath, fullPath)
	if !ok {
		return AudioProjection{}, false
	}
	type looker interface {
		Lookup(Section, string) (AudioProjection, bool)
	}
	l, ok := owned.(looker)
	if !ok {
		return AudioProjection{}, false
	}
	return l.Lookup(class.Section, rel)
}

// DirMtime returns the newest commit time among the committed projections below
// dirRel in section: the directory's deterministic modification time. Hydration, reads,
// restarts and rolled-back staging cannot move it; a committed add or replace does. ok
// is false when no committed projection lives below the directory, and the caller then
// keeps the filesystem's own value.
func (n *AudioNamespace) DirMtime(section Section, dirRel string) (time.Time, bool) {
	prefix := ""
	if dirRel != "" && dirRel != "." {
		prefix = dirRel + "/"
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	var newest int64
	found := false
	for path, p := range n.entries {
		if path.Section != section || p.UpdatedAtNS == 0 {
			continue
		}
		if prefix != "" && !strings.HasPrefix(path.VirtualPath, prefix) {
			continue
		}
		if !found || p.UpdatedAtNS > newest {
			newest, found = p.UpdatedAtNS, true
		}
	}
	if !found {
		return time.Time{}, false
	}
	return time.Unix(0, newest), true
}
