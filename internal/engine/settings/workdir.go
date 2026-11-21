package settings

import (
	"io"
	"os"
	"path/filepath"
)

// workdirState lists the state files older Docker installs left in the working directory.
var workdirState = []string{
	"config.db", "settings.json", "torrents.json", "trackers.txt", "accs.db", "bip.txt", "wip.txt",
	"server.pem", "server.key", "blocklist", "blocklist.url",
}

// MigrateFromWorkdir moves state left in the working directory into dst, once; a file
// already in dst wins. Under systemd both are the install directory and nothing moves.
func MigrateFromWorkdir(dst string, logf func(string, ...any)) {
	wd, err := os.Getwd()
	if err != nil {
		return
	}
	wdAbs, err1 := filepath.Abs(wd)
	dstAbs, err2 := filepath.Abs(dst)
	if err1 != nil || err2 != nil || wdAbs == dstAbs {
		return
	}
	moved := 0
	for _, name := range workdirState {
		src := filepath.Join(wdAbs, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		target := filepath.Join(dstAbs, name)
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if err := moveFile(src, target); err != nil {
			logf("[Settings] migrate state: could not move %s -> %s: %v", name, target, err)
			continue
		}
		logf("[Settings] migrate state: recovered %s from the working directory", name)
		moved++
	}
	if moved > 0 {
		logf("[Settings] migrate state: recovered %d file(s) into %s", moved, dstAbs)
	}
}

// renameOnce is the first, same-device attempt of moveFile; tests force the copy path.
var renameOnce = os.Rename

// moveFile renames, or copies across devices (EXDEV between WORKDIR and a mounted volume).
func moveFile(src, dst string) error {
	if err := renameOnce(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	// On disk before the source goes: a power cut must not lose both.
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(src)
}
