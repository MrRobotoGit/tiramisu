package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"tiramisu/internal/gostorm/log"
)

type JsonDB struct {
	Path              string
	filenameDelimiter string
	filenameExtension string
	fileMode          fs.FileMode
	xPathDelimeter    string
}

var globalJsonDB GoStormDB
var jsonDbLocks = make(map[string]*sync.Mutex)
var jsonDbLocksMutex sync.Mutex

func NewJsonDB() GoStormDB {
	if globalJsonDB != nil {
		return globalJsonDB
	}
	globalJsonDB = &JsonDB{
		Path:              Path,
		filenameDelimiter: ".",
		filenameExtension: ".json",
		fileMode:          fs.FileMode(0o666),
		xPathDelimeter:    "/",
	}
	return globalJsonDB
}

func (v *JsonDB) CloseDB() {
	// Not necessary
}

func (v *JsonDB) Set(xPath, name string, value []byte) {
	jsonObj := map[string]interface{}{}
	if err := json.Unmarshal(value, &jsonObj); err != nil {
		v.log(fmt.Sprintf("Set: error writing entry %s->%s", xPath, name), err)
		return
	}
	filename, err := v.xPathToFilename(xPath)
	if err != nil {
		v.log(fmt.Sprintf("Set: error writing entry %s->%s", xPath, name), err)
		return
	}
	v.lock(filename)
	defer v.unlock(filename)
	root, err := v.readJsonFileAsMap(filename)
	if err != nil {
		// Never rebuild the file from an unreadable read: that would drop every other
		// entry it holds.
		v.log(fmt.Sprintf("Set: refusing to overwrite unreadable %s for %s->%s", filename, xPath, name), err)
		return
	}
	root[name] = jsonObj
	if err := v.writeMapAsJsonFile(filename, root); err != nil {
		v.log(fmt.Sprintf("Set: error writing entry %s->%s", xPath, name), err)
	}
}

func (v *JsonDB) Get(xPath, name string) []byte {
	filename, err := v.xPathToFilename(xPath)
	if err != nil {
		v.log(fmt.Sprintf("Get: error reading entry %s->%s", xPath, name), err)
		return nil
	}
	v.lock(filename)
	defer v.unlock(filename)
	root, err := v.readJsonFileAsMap(filename)
	if err != nil {
		v.log(fmt.Sprintf("Get: error reading entry %s->%s", xPath, name), err)
		return nil
	}
	jsonData, ok := root[name]
	if !ok {
		// Not an error: no entry is the normal case for a fresh install.
		return nil
	}
	byteData, err := json.Marshal(jsonData)
	if err != nil {
		v.log(fmt.Sprintf("Get: error reading entry %s->%s", xPath, name), err)
		return nil
	}
	data := make([]byte, len(byteData))
	copy(data, byteData)
	return data
}

func (v *JsonDB) List(xPath string) []string {
	filename, err := v.xPathToFilename(xPath)
	if err != nil {
		v.log(fmt.Sprintf("List: error reading entries in xPath %s", xPath), err)
		return nil
	}
	v.lock(filename)
	defer v.unlock(filename)
	root, err := v.readJsonFileAsMap(filename)
	if err != nil {
		v.log(fmt.Sprintf("List: error reading entries in xPath %s", xPath), err)
		return nil
	}
	nameList := make([]string, 0, len(root))
	for k := range root {
		nameList = append(nameList, k)
	}
	return nameList
}

func (v *JsonDB) Rem(xPath, name string) {
	filename, err := v.xPathToFilename(xPath)
	if err != nil {
		v.log(fmt.Sprintf("Rem: error removing entry %s->%s", xPath, name), err)
		return
	}
	v.lock(filename)
	defer v.unlock(filename)
	root, err := v.readJsonFileAsMap(filename)
	if err != nil {
		v.log(fmt.Sprintf("Rem: refusing to overwrite unreadable %s for %s->%s", filename, xPath, name), err)
		return
	}
	delete(root, name)
	if err := v.writeMapAsJsonFile(filename, root); err != nil {
		v.log(fmt.Sprintf("Rem: error removing entry %s->%s", xPath, name), err)
	}
}

func (v *JsonDB) Clear(xPath string) {
	filename, err := v.xPathToFilename(xPath)
	if err != nil {
		v.log(fmt.Sprintf("Clear: error converting xPath %s to filename: %v", xPath, err))
		return
	}

	v.lock(filename)
	defer v.unlock(filename)

	path := filepath.Join(v.Path, filename)
	emptyData := []byte("{}")

	if err := os.WriteFile(path, emptyData, v.fileMode); err != nil {
		v.log(fmt.Sprintf("Clear: error writing empty file for xPath %s: %v", xPath, err))
	}
}

func (v *JsonDB) lock(filename string) {
	jsonDbLocksMutex.Lock()
	mtx, ok := jsonDbLocks[filename]
	if !ok {
		mtx = &sync.Mutex{}
		jsonDbLocks[filename] = mtx
	}
	jsonDbLocksMutex.Unlock()
	mtx.Lock()
}

func (v *JsonDB) unlock(filename string) {
	jsonDbLocksMutex.Lock()
	if mtx, ok := jsonDbLocks[filename]; ok {
		mtx.Unlock()
	}
	jsonDbLocksMutex.Unlock()
}

func (v *JsonDB) xPathToFilename(xPath string) (string, error) {
	if pathComponents := strings.Split(xPath, v.xPathDelimeter); len(pathComponents) > 0 {
		return strings.ToLower(strings.Join(pathComponents, v.filenameDelimiter) + v.filenameExtension), nil
	}
	return "", errors.New("xPath has no components")
}

func (v *JsonDB) readJsonFileAsMap(filename string) (map[string]interface{}, error) {
	jsonData := map[string]interface{}{}
	path := filepath.Join(v.Path, filename)

	fileData, err := os.ReadFile(path)
	if err != nil {
		// A missing file is an empty store. Anything else is a real failure and must not
		// be reported as empty, or the caller rewrites the file from nothing.
		if os.IsNotExist(err) {
			return jsonData, nil
		}
		v.log(fmt.Sprintf("readJsonFileAsMap(%s) read error", filename), err)
		return nil, err
	}
	if len(bytes.TrimSpace(fileData)) == 0 {
		return jsonData, nil
	}
	if err := json.Unmarshal(fileData, &jsonData); err != nil {
		v.log(fmt.Sprintf("readJsonFileAsMap(%s) invalid JSON: %s", filename, fileData), err)
		return nil, err
	}
	return jsonData, nil
}

func (v *JsonDB) writeMapAsJsonFile(filename string, o map[string]interface{}) error {
	path := filepath.Join(v.Path, filename)

	fileData, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		v.log(fmt.Sprintf("writeMapAsJsonFile path: %s marshal error", path), err)
		return err
	}

	// Temp file + rename: os.WriteFile truncates before writing, so a power cut or a hard
	// reboot mid-write leaves a partial settings.json.
	tmp, err := os.CreateTemp(v.Path, filename+".tmp")
	if err != nil {
		v.log(fmt.Sprintf("writeMapAsJsonFile path: %s temp error", path), err)
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(fileData); err != nil {
		tmp.Close()
		v.log(fmt.Sprintf("writeMapAsJsonFile path: %s write error", path), err)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		v.log(fmt.Sprintf("writeMapAsJsonFile path: %s sync error", path), err)
		return err
	}
	if err := tmp.Close(); err != nil {
		v.log(fmt.Sprintf("writeMapAsJsonFile path: %s close error", path), err)
		return err
	}
	if err := os.Chmod(tmpName, v.fileMode); err != nil {
		v.log(fmt.Sprintf("writeMapAsJsonFile path: %s chmod error", path), err)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		v.log(fmt.Sprintf("writeMapAsJsonFile path: %s rename error", path), err)
		return err
	}
	return nil
}

// readable reports whether the file backing xPath can be parsed. It separates a corrupt
// file from an absent one, which the storage decision in InitSets depends on.
func (v *JsonDB) readable(xPath string) bool {
	filename, err := v.xPathToFilename(xPath)
	if err != nil {
		return false
	}
	v.lock(filename)
	defer v.unlock(filename)
	_, err = v.readJsonFileAsMap(filename)
	return err == nil
}

func (v *JsonDB) log(s string, params ...interface{}) {
	if len(params) > 0 {
		log.TLogln(fmt.Sprintf("JsonDB: %s: %s", s, fmt.Sprint(params...)))
	} else {
		log.TLogln(fmt.Sprintf("JsonDB: %s", s))
	}
}
