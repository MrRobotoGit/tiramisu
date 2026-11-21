package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"tiramisu/internal/metadb"
)

// importSettingsJSON copies <dir>/settings.json ({"BitTorr":{…}}) into the settings row once,
// for installs still on the JSON backend. A stored row wins; the file is then set aside.
func importSettingsJSON(db *metadb.DB, dir, stamp string, logf func(string, ...any)) error {
	src := filepath.Join(dir, "settings.json")
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	row, err := db.GoStormKVGet(kvXPath, kvName)
	if err != nil {
		return err
	}
	if row != nil {
		// Unread: the row wins whatever the file holds, even when it cannot be read.
		setAside(src, src+".migrated-"+stamp, "the stored settings are kept", logf)
		return nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("settings: reading %s: %w", src, err)
	}
	doc, perr := legacyDocument(b)
	if perr != nil {
		// Never read as empty: the file is kept aside and the defaults are written later.
		dst := src + ".corrupt-" + stamp
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("settings: %s unreadable (%v) and cannot be set aside: %w", src, perr, err)
		}
		logf("[Settings] WARN %s unreadable (%v): moved to %s, not imported", src, perr, dst)
		return nil
	}
	if err := db.ImportGoStorm(func(im *metadb.GoStormImporter) error {
		_, err := im.PutKV(kvXPath, kvName, doc)
		return err
	}); err != nil {
		return fmt.Errorf("settings: importing %s: %w", src, err)
	}
	logf("[Settings] %s imported", src)
	setAside(src, src+".migrated-"+stamp, "imported", logf)
	return nil
}

// legacyDocument extracts the BitTorr object, checked like a stored document, compacted.
func legacyDocument(b []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, err
	}
	inner, ok := top[kvName]
	if !ok {
		return nil, errors.New("no BitTorr object")
	}
	if _, _, err := parseDocument(inner); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, inner); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// setAside renames a legacy file that is no longer read; a failure only warns, since the
// data is already in the DB and refusing to start would loop under systemd.
func setAside(src, dst, why string, logf func(string, ...any)) {
	if err := os.Rename(src, dst); err != nil {
		logf("[Settings] WARN %s (%s) cannot be set aside: %v", src, why, err)
		return
	}
	logf("[Settings] %s (%s) moved to %s", src, why, dst)
}
