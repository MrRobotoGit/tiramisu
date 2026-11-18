package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RootFrom is the root directory from --path, as main resolved it before: a directory is the
// root, a file means its directory, nothing means /home/pi. It needs no configuration.
func RootFrom(pathFlag string) string {
	if pathFlag == "" {
		return "/home/pi"
	}
	if fi, err := os.Stat(pathFlag); err == nil && fi.IsDir() {
		return pathFlag
	}
	return filepath.Dir(pathFlag)
}

// StateDir is <root>/STATE.
func StateDir(root string) string { return filepath.Join(root, "STATE") }

// DBPath is TIRAMISU_STATE_DB, else <state>/tiramisu.db.
func DBPath(stateDir string) string {
	if p := os.Getenv("TIRAMISU_STATE_DB"); p != "" {
		return p
	}
	return filepath.Join(stateDir, "tiramisu.db")
}

// LegacyFiles are the config.json files a start may import, in order of precedence,
// deduplicated: MKV_PROXY_CONFIG_PATH, next to the binary, <root>/config.json.
func LegacyFiles(root string) []string {
	var cands []string
	if p := os.Getenv("MKV_PROXY_CONFIG_PATH"); p != "" {
		cands = append(cands, p)
	}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "config.json"))
	}
	cands = append(cands, filepath.Join(root, "config.json"))
	var out []string
	seen := map[string]bool{}
	for _, c := range cands {
		a, err := filepath.Abs(c)
		if err != nil {
			a = c
		}
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// CheckLegacyDBPath refuses a config.json that moved the DB elsewhere (state_db_path)
// unless TIRAMISU_STATE_DB says where: otherwise the second start would open an empty DB.
func CheckLegacyDBPath(files []string, stateDir string) error {
	if os.Getenv("TIRAMISU_STATE_DB") != "" {
		return nil
	}
	def := filepath.Join(stateDir, "tiramisu.db")
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var v struct {
			StateDBPath string `json:"state_db_path"`
		}
		if json.Unmarshal(stripJSONComments(data), &v) != nil || v.StateDBPath == "" {
			continue
		}
		if !samePath(v.StateDBPath, def) {
			return fmt.Errorf("%s sets state_db_path to %s: start with TIRAMISU_STATE_DB=%s so the configuration is found there on every start", f, v.StateDBPath, v.StateDBPath)
		}
	}
	return nil
}

// samePath compares two paths as absolute, symlinks resolved where they exist.
func samePath(a, b string) bool {
	norm := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		// Resolve symlinks on the deepest ancestor that exists (STATE may not exist yet).
		rest := ""
		for d := filepath.Clean(p); ; d = filepath.Dir(d) {
			if r, err := filepath.EvalSymlinks(d); err == nil {
				return filepath.Join(r, rest)
			}
			if filepath.Dir(d) == d {
				return filepath.Clean(p)
			}
			rest = filepath.Join(filepath.Base(d), rest)
		}
	}
	return norm(a) == norm(b)
}
