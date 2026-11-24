package library

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// ErrLibraryFromMaster: a local video change on a node whose library comes from a master.
var ErrLibraryFromMaster = errors.New("library_from_master")

const (
	msgAddBlocked    = "This library comes from %s. Titles can't be added here while it is your library master. Leave in Friends › Library to add titles yourself."
	msgRemoveBlocked = "This library comes from %s. Titles can't be removed here while it is your library master. Leave in Friends › Library to remove titles yourself."
)

// MasterError is ErrLibraryFromMaster with the master's name; its text is the user's message.
type MasterError struct {
	Master string
	Add    bool
}

func (e *MasterError) Error() string {
	if e.Add {
		return fmt.Sprintf(msgAddBlocked, e.Master)
	}
	return fmt.Sprintf(msgRemoveBlocked, e.Master)
}

func (e *MasterError) Is(target error) bool { return target == ErrLibraryFromMaster }

// LocalChange refuses a local video addition or removal while the node has a library master.
func (m *Manager) LocalChange(kind string, add bool) error {
	if section, canonical := SectionForType(kind); canonical && IsAudioSection(section) {
		return nil
	}
	return masterRefusal(m.cfg.LibraryMaster, add)
}

func masterRefusal(master func() string, add bool) error {
	if master == nil {
		return nil
	}
	if name := master(); name != "" {
		return &MasterError{Master: name, Add: add}
	}
	return nil
}

// unlinkGate is what UnlinkAllowed reads: the media dirs and the master of the last Manager built.
type unlinkGate struct {
	dirs   []string
	master func() string
}

var gate atomic.Pointer[unlinkGate]

// GateUnlinks makes UnlinkAllowed answer for m's media dirs and library master (the FUSE's Manager).
func (m *Manager) GateUnlinks() {
	gate.Store(&unlinkGate{dirs: m.mediaDirs(), master: m.cfg.LibraryMaster})
}

// UnlinkAllowed is LocalChange for a stub path under the movie or TV dirs; other paths pass.
func UnlinkAllowed(path string) error {
	g := gate.Load()
	if g == nil {
		return nil
	}
	clean := filepath.Clean(path)
	for _, dir := range g.dirs {
		if rel, err := filepath.Rel(dir, clean); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return masterRefusal(g.master, false)
		}
	}
	return nil
}

// writeMasterError answers a refused local change with the 409 the Control Panel reads.
func writeMasterError(w http.ResponseWriter, err error) bool {
	var me *MasterError
	if !errors.As(err, &me) {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]string{"error": "library_from_master", "master": me.Master, "message": me.Error()})
	return true
}
