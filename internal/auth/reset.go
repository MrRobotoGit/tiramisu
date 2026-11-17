package auth

import (
	"errors"
	"fmt"
	"os"

	"tiramisu/internal/metadb"
)

// ResetAt opens the state DB at dbPath on its own and clears sign-in. Used by
// --reset-auth, which exits afterwards instead of starting a second engine.
// If the DB cannot be opened it removes only the marker, so a fail-closed :9080 opens
// again; the returned error says the DB was left untouched.
func ResetAt(dbPath, stateDir string) error {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return Reset(nil, stateDir) // no DB: do not create one
	}
	db, err := metadb.New(dbPath, nil)
	if err != nil {
		if rerr := Reset(nil, stateDir); rerr != nil {
			return rerr
		}
		return fmt.Errorf("%w: marker removed, state DB not opened (%v)", ErrDBUntouched, err)
	}
	defer db.Close()
	return Reset(db, stateDir)
}

var ErrDBUntouched = errors.New("state DB untouched")
