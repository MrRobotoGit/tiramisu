package config

import (
	"errors"
	"fmt"
	"os"
)

// RunExport writes the saved configuration to dest ("-" = stdout), owner-only; exit code.
func RunExport(s *Store, dest string) int {
	doc, ver, err := s.Current()
	if errors.Is(err, ErrNoConfig) {
		fmt.Fprintln(os.Stderr, "export-config:", err)
		return 1
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "export-config:", err)
		return 1
	}
	if dest == "-" {
		os.Stdout.Write(append(doc, '\n'))
		return 0
	}
	if err := writeAtomic(dest, append(doc, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, "export-config:", err)
		return 1
	}
	fmt.Printf("Configuration version #%d written to %s (it contains keys and tokens: keep it private).\n", ver, dest)
	return 0
}

// RunImport saves file as a new version; it takes effect when Tiramisu (re)starts.
func RunImport(s *Store, file string) int {
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "import-config:", err)
		return 1
	}
	doc, err := ParseDocument(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import-config: %s is not a valid configuration: %v\n", file, err)
		return 1
	}
	id, changed, err := s.Save(doc, "cli", file, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "import-config:", err)
		return 1
	}
	if !changed {
		fmt.Printf("%s matches the configuration in use (version #%d): nothing to do.\n", file, id)
		return 0
	}
	fmt.Printf("Imported %s as version #%d. It takes effect when Tiramisu restarts: if it is running, restart it "+
		"before saving from the Control Panel, or that save is built on the configuration still in use.\n", file, id)
	return 0
}

// WarnOwner warns when the CLI ran as a different user than the one owning the DB (sudo):
// files it created would be unreadable to the service.
func WarnOwner(dbPath string) {
	if owner, ok := fileOwner(dbPath); ok && owner != os.Geteuid() {
		fmt.Fprintf(os.Stderr, "WARNING: %s belongs to uid %d but this ran as uid %d: run it as the service user, "+
			"or check that the service can still read the DB and its -wal/-shm files.\n", dbPath, owner, os.Geteuid())
	}
}
