package library

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"tiramisu/internal/metadb"
)

// ErrRequestNotAudio marks a request a video type owns. A caller routes on it
// with errors.Is rather than on the status, which is for clients.
var ErrRequestNotAudio = errors.New("library: request is not an audio type")

type AudioAddedFile struct {
	Path       string `json:"path"`
	SourcePath string `json:"source_path"`
	FileIndex  int    `json:"file_index"`
	Size       int64  `json:"size"`
	// Mtime is the committed projection's mtime, stable across replays: a present
	// result must not look like it rewrote anything.
	Mtime string                `json:"mtime"`
	State AudioProjectionStatus `json:"state"`
	// CueTrack is the image track this projection serves; absent for a whole file.
	CueTrack int `json:"cue_track,omitempty"`

	// Always present, empty when the caller supplied none: List returns the same
	// concept with both keys, and a client should not have to branch on absence.
	ExternalID          string `json:"external_id"`
	ExternalIDNamespace string `json:"external_id_ns"`
}

type AudioAddResponse struct {
	Hash  string `json:"hash"`
	Title string `json:"title"`
	Type  string `json:"type"`
	// AlreadyPresent is true when every requested projection already existed: the
	// request is a 200 replay, not a 201 creation.
	AlreadyPresent bool             `json:"already_present"`
	Files          []AudioAddedFile `json:"files"`
}

// AudioProjectionRegistry is the read and write side of the projection
// registry. *metadb.DB satisfies it.
type AudioProjectionRegistry interface {
	AudioProjectionLookup
	StageAudioProjections(txnID string, ps []metadb.AudioProjection) error
	CommitAudioProjections(txnID string, updatedAtNS int64) (int, error)
	RollbackAudioProjections(txnID string) (int, error)
}

// canonicalHashKey folds a valid base32 info hash onto its 40-hex spelling so
// one torrent has one lock key and one ownership identity.
func canonicalHashKey(hash string) string {
	h := strings.ToLower(strings.TrimSpace(hash))
	if len(h) != 32 {
		return h
	}
	raw, err := base32.StdEncoding.DecodeString(strings.ToUpper(h))
	if err != nil || len(raw) != 20 {
		return h
	}
	return hex.EncodeToString(raw)
}

// audioErr gives a failure its API status while keeping the sentinel reachable
// through errors.Is.
func audioErr(err error) *Error {
	return &Error{Status: StatusForError(err), Message: err.Error(), Err: err}
}

// AddAudio publishes N projections through the registry batch: nothing reaches a
// final name until the registry has committed.
func (m *Manager) AddAudio(ctx context.Context, req AddRequest) (*AudioAddResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, errf(http.StatusRequestTimeout, "request cancelled: %v", err)
	}
	// Routing is decided from the type alone, before validation, so an unknown
	// type keeps the legacy path's canonical error rather than an audio one.
	if section, canonical := SectionForType(req.Type); !canonical || !IsAudioSection(section) {
		return nil, &Error{
			Status:  http.StatusInternalServerError,
			Message: fmt.Sprintf("type %q is not handled by AddAudio", req.Type),
			Err:     ErrRequestNotAudio,
		}
	}
	intent, err := ValidateAudioAddRequest(req)
	if err != nil {
		return nil, err
	}
	if m.cfg.AudioProjections == nil {
		return nil, errf(http.StatusServiceUnavailable, "the audio projection registry is unavailable")
	}
	if m.cfg.AudioRoot == "" {
		return nil, errf(http.StatusServiceUnavailable, "no audio root configured")
	}

	// Identity resolution mirrors validate() and Inspect: the magnet's own hash
	// wins, so cleanup removes what the engine was actually asked to add.
	hash, magnet, err := resolveAudioIdentity(req.Hash, req.Magnet)
	if err != nil {
		return nil, err
	}
	file, err := releaseFile(req.TorrentFile, hash)
	if err != nil {
		return nil, err
	}
	if magnet == "" {
		magnet = BuildMagnet(hash, intent.Title, MergeTrackers(DefaultTrackers(), file.Trackers))
	}

	// Locked on the canonical spelling so a base32 magnet and its hex form are one
	// torrent rather than two lock keys with a window between them.
	lockKey := canonicalHashKey(hash)
	defer m.lockHash(lockKey)()

	known, ok := m.knownTorrentHashes(ctx)
	if !ok {
		return nil, errf(http.StatusBadGateway, "cannot list torrents to establish ownership")
	}
	preexisting := known[lockKey]

	m.uploadReleaseFile(ctx, file, intent.Title)
	addedHash, err := m.cfg.GoStorm.AddTorrent(ctx, magnet, intent.Title)
	if err != nil || addedHash == "" {
		return nil, errf(http.StatusBadGateway, "gostorm rejected the torrent: %v", err)
	}
	engineHash := strings.ToLower(strings.TrimSpace(addedHash))
	if !reInfoHash.MatchString(engineHash) {
		if !preexisting {
			// Drop under the canonical spelling: the engine keys torrents by the hex
			// hash it assigned, and the request's own spelling may be base32.
			m.dropTorrent(ctx, canonicalHashKey(hash))
		}
		return nil, errf(http.StatusBadGateway, "gostorm returned a malformed info hash %q", addedHash)
	}
	if engineKey := canonicalHashKey(engineHash); engineKey != lockKey {
		defer m.lockHash(engineKey)()
		preexisting = preexisting || known[engineKey]
	}
	// Everything after the torrent exists must undo it on the way out, exactly as
	// Add does, or a failed request leaves a torrent hydrated with no projection.
	abandon := func() {
		if !preexisting {
			m.dropTorrent(ctx, engineHash)
		}
	}

	wait := req.MetadataWait
	if wait <= 0 {
		wait = defaultMetadataWait
	} else if wait > maxMetadataWait {
		wait = maxMetadataWait
	}
	info, err := m.cfg.GoStorm.GetTorrentInfo(ctx, engineHash, wait)
	if err != nil || info == nil {
		abandon()
		return nil, errf(http.StatusGatewayTimeout, "no metadata after %ds: %v", wait, err)
	}

	// The cue tracks of one image share its source path: resolve each path once.
	var sourcePaths []string
	seenSource := map[string]bool{}
	for _, file := range intent.Files {
		if !seenSource[file.SourcePath] {
			seenSource[file.SourcePath] = true
			sourcePaths = append(sourcePaths, file.SourcePath)
		}
	}
	resolved, err := ResolveSources(info.FileStats, sourcePaths)
	if err != nil {
		abandon()
		return nil, audioErr(err)
	}
	byPath := make(map[string]ResolvedSource, len(resolved))
	for _, r := range resolved {
		byPath[r.SourcePath] = r
	}
	sources := make([]ResolvedSource, len(intent.Files))
	for i, file := range intent.Files {
		sources[i] = byPath[file.SourcePath]
	}
	var cueCat *cueCatalog
	intent.Files, sources, cueCat, err = m.expandCueImages(ctx, engineHash, info.FileStats, intent.Files, sources)
	if err != nil {
		abandon()
		return nil, audioErr(err)
	}
	// Validated against the hash the engine reported: a base32 magnet comes back
	// in hex, and the _hash8 suffix has to match the spelling that is stored.
	for i, file := range intent.Files {
		if _, err := ValidateProjectionPath(intent.Section, file.Path, sources[i].SourcePath, engineHash); err != nil {
			abandon()
			return nil, audioErr(err)
		}
	}
	// A cue track's projection is its header plus a frame range of the image, so its
	// size is known only once the boundaries are found.
	segments, err := m.cueSegments(ctx, engineHash, info.FileStats, intent.Files, sources, cueCat)
	if err != nil {
		abandon()
		return nil, audioErr(err)
	}
	for i, seg := range segments {
		sources[i].Size = seg.Size()
	}
	plans, err := PlanAudioProjections(m.cfg.AudioProjections, intent.Section, engineHash, intent.Files, sources)
	if err != nil {
		abandon()
		return nil, audioErr(err)
	}

	txnID, err := newAudioTxnID()
	if err != nil {
		abandon()
		return nil, errf(http.StatusInternalServerError, "cannot start an audio transaction: %v", err)
	}
	now := time.Now().UnixNano()
	sectionRoot := filepath.Join(m.cfg.AudioRoot, string(intent.Section))

	var rows []metadb.AudioProjection
	for i, plan := range plans {
		if plan.Status != AudioProjectionCreated {
			continue
		}
		rows = append(rows, metadb.AudioProjection{
			Section:             string(intent.Section),
			VirtualPath:         plan.VirtualPath,
			PortablePathKey:     PortablePathKey(plan.VirtualPath),
			Hash:                engineHash,
			FileIndex:           plan.Source.FileIndex,
			SourcePath:          plan.Source.SourcePath,
			Size:                plan.Source.Size,
			MtimeNS:             now,
			Title:               intent.Title,
			Magnet:              magnet,
			ExternalID:          intent.Files[i].ExternalID,
			ExternalIDNamespace: intent.Files[i].ExternalIDNamespace,
			CueTrack:            intent.Files[i].CueTrack,
			ByteOffset:          segments[i].Offset,
			Header:              segments[i].Header,
			StagingName:         fmt.Sprintf(".tiramisu-%s-%d", txnID, i),
			CreatedAtNS:         now,
			UpdatedAtNS:         now,
		})
	}

	// Anchored to the section root, so a path that passed string validation still
	// cannot be redirected by a symlink planted beneath it (spec §14.3).
	writer, err := OpenSectionWriter(sectionRoot)
	if err != nil {
		abandon()
		return nil, errf(http.StatusServiceUnavailable, "cannot open section %q: %v", intent.Section, err)
	}
	defer writer.Close()

	if len(rows) > 0 {
		// The registry is consulted before the filesystem is touched, so a
		// conflicting batch writes nothing at all.
		if err := m.cfg.AudioProjections.StageAudioProjections(txnID, rows); err != nil {
			abandon()
			return nil, audioErr(err)
		}
	}

	// objects tracks every file this request created, with the identity it was born
	// with: a rollback unlinks a name only while it still holds that object, and
	// leaves anything else that took the name alone.
	type stagedObject struct {
		rel string
		id  FileIdentity
	}
	objects := make([]stagedObject, 0, len(rows))
	unwind := func() error {
		var failed []string
		for _, obj := range objects {
			removed, err := writer.RemoveStagedIfIdentity(obj.rel, obj.id)
			switch {
			case err != nil:
				failed = append(failed, fmt.Sprintf("%s: %v", obj.rel, err))
			case !removed:
				failed = append(failed, fmt.Sprintf("%s: the name no longer holds this request's file", obj.rel))
			}
		}
		// Only directories this writer created are pruned: a pre-existing empty
		// ancestor is not this request's to remove.
		if err := writer.PruneCreatedDirs(); err != nil {
			m.cfg.Logger.Printf("[LibraryAPI] WARNING: cannot prune audio directories created by this request: %v", err)
		}
		if len(failed) > 0 {
			// The rows are the only proof of ownership for what is left on disk, so
			// they stay staged for startup recovery instead of being rolled back.
			abandon()
			return fmt.Errorf("cleanup incomplete (%s); transaction %s left staged for recovery", strings.Join(failed, "; "), txnID)
		}
		if len(rows) > 0 {
			if _, err := m.cfg.AudioProjections.RollbackAudioProjections(txnID); err != nil {
				abandon()
				return fmt.Errorf("cannot roll back audio transaction %s: %w", txnID, err)
			}
		}
		abandon()
		return nil
	}

	for _, row := range rows {
		data, err := AudioCueStubBytes(m.streamURL(row.Hash, row.FileIndex), row.Size, row.Magnet, row.ExternalID, row.ExternalIDNamespace, row.CueTrack, row.ByteOffset)
		if err != nil {
			if cleanupErr := unwind(); cleanupErr != nil {
				return nil, errf(http.StatusInternalServerError, "cannot render audio stub for %s: %v; %v", row.VirtualPath, err, cleanupErr)
			}
			return nil, errf(http.StatusInternalServerError, "cannot render audio stub for %s: %v", row.VirtualPath, err)
		}
		rel := stagingRelPath(row)
		id, err := writer.WriteStagedIdentity(rel, data)
		if err != nil {
			if cleanupErr := unwind(); cleanupErr != nil {
				err = fmt.Errorf("%w; %v", err, cleanupErr)
			}
			return nil, audioErr(err)
		}
		objects = append(objects, stagedObject{rel: rel, id: id})
	}

	// Spec §7.2: renames first, commit last, so the one atomic step cannot be
	// preceded by a failure. An uncommitted final name is inert to dispatch.
	for i, row := range rows {
		if err := writer.Publish(objects[i].rel, row.VirtualPath); err != nil {
			if cleanupErr := unwind(); cleanupErr != nil {
				err = fmt.Errorf("%w; %v", err, cleanupErr)
			}
			return nil, audioErr(err)
		}
		objects[i].rel = row.VirtualPath
	}

	if len(rows) > 0 {
		committed, err := m.cfg.AudioProjections.CommitAudioProjections(txnID, now)
		if err != nil {
			if cleanupErr := unwind(); cleanupErr != nil {
				err = fmt.Errorf("%w; %v", err, cleanupErr)
			}
			return nil, audioErr(err)
		}
		if committed != len(rows) {
			if cleanupErr := unwind(); cleanupErr != nil {
				return nil, errf(http.StatusInternalServerError, "committed %d of %d audio projections; %v", committed, len(rows), cleanupErr)
			}
			return nil, errf(http.StatusInternalServerError, "committed %d of %d audio projections", committed, len(rows))
		}
	}

	// The whole album reaches the namespace in one update: a Readdir interleaved with
	// per-row inserts would list a partial batch as if it were the whole library.
	if m.cfg.PublishAudioPath != nil && len(rows) > 0 {
		batch := make([]AudioProjection, 0, len(rows))
		for _, row := range rows {
			batch = append(batch, AudioProjection{
				Section:             intent.Section,
				VirtualPath:         row.VirtualPath,
				Hash:                row.Hash,
				FileIndex:           row.FileIndex,
				Size:                row.Size,
				MtimeNS:             row.MtimeNS,
				UpdatedAtNS:         row.UpdatedAtNS,
				ExternalID:          row.ExternalID,
				ExternalIDNamespace: row.ExternalIDNamespace,
				CueTrack:            row.CueTrack,
				ByteOffset:          row.ByteOffset,
				Header:              row.Header,
			})
		}
		m.cfg.PublishAudioPath(batch)
	}
	if len(rows) > 0 {
		m.scheduleRefresh(m.audioSection(intent.Section))
	}
	for _, row := range rows {
		// Published before the cache is dropped: a Readdir racing between the two
		// would otherwise refill a cache from a namespace without this path.
		if m.cfg.InvalidatePath != nil {
			m.cfg.InvalidatePath(filepath.Join(sectionRoot, filepath.FromSlash(row.VirtualPath)))
		}
	}

	created := 0
	files := make([]AudioAddedFile, 0, len(plans))
	for i, plan := range plans {
		id, ns := intent.Files[i].ExternalID, intent.Files[i].ExternalIDNamespace
		mtimeNS := now
		// The stored row wins for an existing projection: there is no update path,
		// so a replay with a different identity is reported, not applied, and the
		// response carries the committed mtime rather than this request's clock.
		if plan.Status == AudioProjectionPresent && plan.Existing != nil {
			if plan.Existing.ExternalID != id || plan.Existing.ExternalIDNamespace != ns {
				m.cfg.Logger.Printf("[LibraryAPI] WARNING: %s keeps stored identity %q/%q, request supplied %q/%q", plan.VirtualPath, plan.Existing.ExternalID, plan.Existing.ExternalIDNamespace, id, ns)
			}
			id, ns = plan.Existing.ExternalID, plan.Existing.ExternalIDNamespace
			mtimeNS = plan.Existing.MtimeNS
		} else {
			created++
		}
		files = append(files, AudioAddedFile{
			Path:                plan.VirtualPath,
			SourcePath:          plan.Source.SourcePath,
			FileIndex:           plan.Source.FileIndex,
			Size:                plan.Source.Size,
			Mtime:               time.Unix(0, mtimeNS).UTC().Format(time.RFC3339Nano),
			State:               plan.Status,
			CueTrack:            intent.Files[i].CueTrack,
			ExternalID:          id,
			ExternalIDNamespace: ns,
		})
	}
	return &AudioAddResponse{
		Hash: engineHash, Title: intent.Title, Type: req.Type,
		AlreadyPresent: created == 0,
		Files:          files,
	}, nil
}

func newAudioTxnID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// stagingRelPath puts the hidden staging name beside its destination so the
// publish is a rename within one directory.
func stagingRelPath(row metadb.AudioProjection) string {
	if i := strings.LastIndex(row.VirtualPath, "/"); i >= 0 {
		return row.VirtualPath[:i+1] + row.StagingName
	}
	return row.StagingName
}
