package settings

import (
	"io"
	"os"
	"path/filepath"

	"tiramisu/internal/gostorm/log"
)

// workdirState lists the files this package resolves against Path. They used to land in the
// process working directory, because Path was never assigned and filepath.Join("", name)
// yields a relative name.
var workdirState = []string{
	"config.db",     // bbolt: torrents, and settings when StoreSettingsInJson is false
	"settings.json", // BTSets when StoreSettingsInJson is true (the default)
	"torrents.json",
	"trackers.txt",
	"accs.db",
	"bip.txt",
	"wip.txt",
	"server.pem",
	"server.key",
	"blocklist",
	"blocklist.url",
}

// MigrateFromWorkdir moves state left in the working directory into dst, once. Under systemd
// the working directory happens to be the install directory and this finds nothing to do; in
// Docker it is the image's WORKDIR, outside every mounted volume, which is how torrents and
// GoStorm settings disappeared on "docker rm". A file already present in dst always wins.
func MigrateFromWorkdir(dst string) {
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
			continue // dst already has it; leave the stray copy alone
		}
		if err := moveFile(src, target); err != nil {
			log.TLogln("Migrate state: could not move", name, "->", target, ":", err)
			continue
		}
		log.TLogln("Migrate state: recovered", name, "from the working directory")
		moved++
	}
	if moved > 0 {
		log.TLogln("Migrate state: recovered", moved, "file(s) into", dstAbs)
	}
}

// moveFile renames when it can and copies otherwise: in a container the working directory and
// the mounted volume are different devices, so rename fails with EXDEV.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
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
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Remove(src)
	return nil
}
