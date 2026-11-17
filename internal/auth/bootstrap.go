package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"

	"tiramisu/internal/metadb"
)

// RunResetFlag implements --reset-auth: it runs before the engine starts and returns
// the process exit code. dbPath empty means STATE/tiramisu.db.
func RunResetFlag(dbPath, stateDir string) int {
	if dbPath == "" {
		dbPath = filepath.Join(stateDir, "tiramisu.db")
	}
	if err := ResetAt(dbPath, stateDir); errors.Is(err, ErrDBUntouched) {
		fmt.Println("reset-auth:", err)
		fmt.Println("Only the marker was removed. Stop the service, check the DB path and permissions, and run it again.")
		return 1
	} else if err != nil {
		fmt.Println("reset-auth failed:", err)
		return 1
	}
	fmt.Println("Sign-in reset: user, sessions and tokens cleared. Run this with the service stopped, then start it.")
	return 0
}

// Start builds the service on the state DB (nil when it is off), applies
// TIRAMISU_RESET_AUTH, logs the state and mounts /api/auth/* and /login on mux.
func Start(mux *http.ServeMux, db *metadb.DB, stateDir string, trustedProxies []string, logf func(string, ...any)) (*Service, []netip.Prefix) {
	resetEnv := os.Getenv("TIRAMISU_RESET_AUTH") == "1"
	var store Store // stays a nil interface when the StateDB is off
	if db != nil {
		store = db
	}
	if resetEnv {
		if err := Reset(store, stateDir); err != nil {
			logf("[Auth] reset failed: %v", err)
		} else if store == nil {
			logf("[Auth] reset: marker removed, but the state DB is unavailable; the user and tokens in it are untouched")
		} else {
			logf("[Auth] reset: user, sessions and tokens cleared, sign-in off")
		}
		logf("[Auth] WARNING: TIRAMISU_RESET_AUTH=1 is set: remove it, sign-in cannot be enabled while it is")
	}
	svc := New(store, stateDir, resetEnv)
	svc.SetLogger(logf)
	trusted := ParseTrusted(trustedProxies)
	if st := svc.State(); st.FailClosed {
		logf("[Auth] sign-in was on but its state is unavailable: :9080 answers 503 until it is back, or stop the service, run tiramisu --reset-auth and start it again")
	} else if st.Enabled {
		logf("[Auth] sign-in on")
	} else {
		logf("[Auth] sign-in off")
	}
	Register(mux, svc, trusted)
	return svc, trusted
}
