package auth

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tiramisu/internal/metadb"
)

func TestStartWithoutDBAndMarkerFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir)
	var logs []string
	svc, _ := Start(http.NewServeMux(), nil, dir, nil, func(f string, a ...any) { logs = append(logs, f) })
	if !svc.FailClosed() || !strings.Contains(strings.Join(logs, "\n"), "503") {
		t.Fatalf("expected fail-closed, log: %v", logs)
	}
}

func TestStartEnvResetClearsAndBlocksSetup(t *testing.T) {
	dir := t.TempDir()
	db, _ := metadb.New(filepath.Join(dir, "t.db"), nil)
	defer db.Close()
	New(db, dir, false).Setup("admin", "correct horse", "", "")
	t.Setenv("TIRAMISU_RESET_AUTH", "1")
	svc, _ := Start(http.NewServeMux(), db, dir, nil, func(string, ...any) {})
	if st := svc.State(); st.Enabled || st.Configured || !st.ResetPending {
		t.Fatalf("%+v", st)
	}
}

func TestRunResetFlag(t *testing.T) {
	dir := t.TempDir()
	db, _ := metadb.New(filepath.Join(dir, "tiramisu.db"), nil)
	New(db, dir, false).Setup("admin", "correct horse", "", "")
	db.Close()
	if code := RunResetFlag("", dir); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); !os.IsNotExist(err) {
		t.Fatal("marker left behind")
	}
}
