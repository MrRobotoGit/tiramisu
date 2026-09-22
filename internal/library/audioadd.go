package library

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"tiramisu/internal/metadb"
)

// maxAudioFilesPerAdd is the spec §14.4 cap on files per Add.
const maxAudioFilesPerAdd = 512

// AudioFileRequest is one requested projection. Both fields are preserved
// verbatim: the caller owns audio naming.
type AudioFileRequest struct {
	SourcePath string `json:"source_path"`
	Path       string `json:"path"`
	// Opaque caller identity and its namespace ("musicbrainz", "asin"). Stored
	// and returned verbatim; both or neither.
	ExternalID          string `json:"external_id,omitempty"`
	ExternalIDNamespace string `json:"external_id_ns,omitempty"`
	// CueTrack projects one track of a single-file image, as numbered in the cue
	// sheet the torrent carries beside it; 0 projects the whole file. Tags become
	// the track's Vorbis comments, verbatim; without them the cue's are used.
	CueTrack int               `json:"cue_track,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`
}

// AudioAddRequest is a structurally valid audio add with its type resolved.
type AudioAddRequest struct {
	Section Section
	Title   string
	Files   []AudioFileRequest
}

// ValidateAudioAddRequest checks what needs no torrent. Virtual paths are not
// validated here: the _hash8 suffix needs the hash the engine reports.
func ValidateAudioAddRequest(req AddRequest) (AudioAddRequest, error) {
	// SectionForType is the canonical table and does not trim or case-fold, which
	// is exactly roadmap §1's "no aliases" rule for audio.
	section, canonical := SectionForType(req.Type)
	if !canonical || !IsAudioSection(section) {
		// Video keeps validate()'s leniency so routing matches the existing path.
		switch strings.ToLower(strings.TrimSpace(req.Type)) {
		case "", "movie", "movies", "film":
			return AudioAddRequest{Section: SectionMovies}, errf(http.StatusBadRequest, "type %q is not an audio type", req.Type)
		case "tv", "show", "series", "episode":
			return AudioAddRequest{Section: SectionTV}, errf(http.StatusBadRequest, "type %q is not an audio type", req.Type)
		}
		return AudioAddRequest{}, errf(http.StatusBadRequest, "unknown type %q: use music or audiobook", req.Type)
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		return AudioAddRequest{}, errf(http.StatusBadRequest, "title is required")
	}
	if len(req.Files) == 0 {
		return AudioAddRequest{}, errf(http.StatusBadRequest, "files is required for type %q", req.Type)
	}
	if len(req.Files) > maxAudioFilesPerAdd {
		return AudioAddRequest{}, errf(http.StatusBadRequest, "%d files exceeds the limit of %d per request", len(req.Files), maxAudioFilesPerAdd)
	}

	seenPath := make(map[string]bool, len(req.Files))
	seenSource := make(map[string]bool, len(req.Files))
	files := make([]AudioFileRequest, 0, len(req.Files))
	for i, file := range req.Files {
		if strings.TrimSpace(file.SourcePath) == "" {
			return AudioAddRequest{}, errf(http.StatusBadRequest, "files[%d].source_path is required", i)
		}
		if strings.TrimSpace(file.Path) == "" {
			return AudioAddRequest{}, errf(http.StatusBadRequest, "files[%d].path is required", i)
		}
		if seenPath[file.Path] {
			return AudioAddRequest{}, errf(http.StatusBadRequest, "files[%d].path %q is requested twice", i, file.Path)
		}
		if file.CueTrack < 0 {
			return AudioAddRequest{}, errf(http.StatusBadRequest, "files[%d].cue_track %d is negative", i, file.CueTrack)
		}
		if file.CueTrack == 0 && len(file.Tags) > 0 {
			return AudioAddRequest{}, errf(http.StatusBadRequest, "files[%d].tags apply to a cue track only", i)
		}
		sourceKey := fmt.Sprintf("%s\x00%d", file.SourcePath, file.CueTrack)
		if seenSource[sourceKey] {
			return AudioAddRequest{}, errf(http.StatusBadRequest, "files[%d].source_path %q is requested twice", i, file.SourcePath)
		}
		// Half an identity identifies nothing, whichever half is missing. A value
		// that is entirely whitespace counts as absent; padding is preserved.
		blankID := strings.TrimSpace(file.ExternalID) == ""
		blankNS := strings.TrimSpace(file.ExternalIDNamespace) == ""
		if blankID != blankNS {
			return AudioAddRequest{}, errf(http.StatusBadRequest, "files[%d] has external id %q and namespace %q: %v", i, file.ExternalID, file.ExternalIDNamespace, metadb.ErrAudioIdentityIncomplete)
		}
		if blankID {
			file.ExternalID, file.ExternalIDNamespace = "", ""
		}
		seenPath[file.Path] = true
		seenSource[sourceKey] = true
		// Stored as supplied: only title is trimmed, never a path.
		files = append(files, file)
	}
	return AudioAddRequest{Section: section, Title: title, Files: files}, nil
}

// StatusForError maps this package's failures onto the API error model, so a
// caller can tell a bad request from an unsatisfiable one from an engine fault.
func StatusForError(err error) int {
	if err == nil {
		return http.StatusOK
	}
	// An error that already carries a status keeps it.
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Status
	}
	switch {
	case errors.Is(err, ErrPathInvalid),
		errors.Is(err, ErrExtensionUnsupported),
		errors.Is(err, ErrExtensionMismatch),
		errors.Is(err, ErrHashSuffixInvalid),
		errors.Is(err, ErrSourceDuplicate),
		errors.Is(err, metadb.ErrAudioIdentityIncomplete):
		return http.StatusBadRequest
	case errors.Is(err, ErrCueUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrSourceNotFound), errors.Is(err, ErrCueSplit):
		// Well formed but unsatisfiable, as pickFile already answers.
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrSourceAmbiguous):
		// The engine's own answer is malformed, not the caller's request.
		return http.StatusBadGateway
	case errors.Is(err, ErrSourceDrift),
		errors.Is(err, metadb.ErrAudioPathConflict),
		errors.Is(err, metadb.ErrAudioSourceConflict):
		// Same status, distinct sentinels: the caller still learns which of the
		// destination or the release is taken.
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}
