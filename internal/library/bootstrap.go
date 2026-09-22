package library

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// AudioStubBytes renders a stub without touching the filesystem, so the contained
// writer can place it through a pinned directory descriptor instead. It replaced the
// earlier WriteAudioStub, whose own MkdirAll/WriteFile bypassed the containment the
// writer now guarantees.
func AudioStubBytes(streamURL string, size int64, magnet, externalID, externalIDNS string) ([]byte, error) {
	stub := map[string]interface{}{
		"url":    streamURL,
		"size":   size,
		"magnet": magnet,
	}
	// Absent rather than empty, like the imdb field this deliberately omits: a
	// key the caller did not fill is an identity the engine would be inventing.
	if externalID != "" || externalIDNS != "" {
		stub["external_id"] = externalID
		stub["external_id_ns"] = externalIDNS
	}
	return json.Marshal(stub)
}

// AudioCueStubBytes is AudioStubBytes for one cue track: it records which track of
// the file it is and where its frames start.
func AudioCueStubBytes(streamURL string, size int64, magnet, externalID, externalIDNS string, cueTrack int, offset int64) ([]byte, error) {
	data, err := AudioStubBytes(streamURL, size, magnet, externalID, externalIDNS)
	if err != nil || cueTrack == 0 {
		return data, err
	}
	var stub map[string]interface{}
	if err := json.Unmarshal(data, &stub); err != nil {
		return nil, err
	}
	stub["cue_track"], stub["offset"] = cueTrack, offset
	return json.Marshal(stub)
}

// EnsureSectionRoots creates the audio section roots under sourcePath when they are
// missing, so an install that predates audio gains them on update rather than
// needing a manual step. Video roots are install.sh's job and are left alone.
func EnsureSectionRoots(sourcePath string) error {
	if sourcePath == "" {
		// Not a no-op: an empty path would resolve the sections against the
		// process working directory, which for a service install is /.
		return errors.New("library: empty source path")
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("library: source path %q is not a directory", sourcePath)
	}

	for _, section := range []Section{SectionMusic, SectionAudiobooks} {
		root := filepath.Join(sourcePath, string(section))
		// Mkdir rather than MkdirAll: an existing root keeps its own mode and
		// contents, and only the one missing level is ever created.
		if err := os.Mkdir(root, 0755); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			existing, statErr := os.Stat(root)
			if statErr != nil {
				return statErr
			}
			if !existing.IsDir() {
				return fmt.Errorf("library: section root %q is not a directory", root)
			}
		}
	}
	return nil
}
