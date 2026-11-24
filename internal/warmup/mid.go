package warmup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	midSuffix = ".warmup-mid" // sparse file of verified pieces past the head, at their file offsets
	covSuffix = ".cov"        // next to a mid file: its ranges, written after the data is synced
)

// midFile is the coverage of one mid file; mu serialises its writes.
type midFile struct {
	mu  sync.Mutex
	cov tailRange
}

func (tr *tailRange) size() (n int64) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, s := range tr.ranges {
		n += s.end - s.start
	}
	return n
}

func (tr *tailRange) list() []tailSpan {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]tailSpan(nil), tr.ranges...)
}

func (d *DiskWarmupCache) midPath(hash string, fileID int) string {
	return filepath.Join(d.dir, hash+"-"+strconv.Itoa(fileID)+midSuffix)
}

func (d *DiskWarmupCache) midFor(path string) *midFile {
	v, _ := d.mids.LoadOrStore(path, &midFile{})
	return v.(*midFile)
}

// WriteMidPiece stores one verified piece of a file at its file offset; coverage is recorded only
// after the data is synced, so a crash never leaves unverified bytes marked as written. A piece
// that extends the head file densely goes there; any other stays in the mid file. Room comes only
// from other mids: without it the piece is not kept.
func (d *DiskWarmupCache) WriteMidPiece(hash string, fileID int, fileOff int64, data []byte) {
	if d == nil || len(data) == 0 || fileOff < 0 {
		return
	}
	// Only an existing head grows: creating one would make room by evicting library heads.
	if head, avail := d.headCap(), d.GetAvailableRange(hash, fileID); fileOff < head && avail > 0 && fileOff <= avail {
		k := min(int64(len(data)), head-fileOff)
		d.WriteChunk(hash, fileID, data[:k], fileOff)
		data, fileOff = data[k:], fileOff+k
		if len(data) == 0 {
			return
		}
	}
	path := d.midPath(hash, fileID)
	d.midWriting.Store(path, true) // the quota never evicts a file being written
	defer d.midWriting.Delete(path)
	var mf *midFile
	for { // a removal between the lookup and the lock leaves a stale entry: take the new one
		mf = d.midFor(path)
		mf.mu.Lock()
		if cur, ok := d.mids.Load(path); ok && cur == mf {
			break
		}
		mf.mu.Unlock()
	}
	defer mf.mu.Unlock()
	end := fileOff + int64(len(data))
	if mf.cov.covers(fileOff, end) {
		return
	}
	d.mu.Lock()
	room := d.roomForMidLocked(int64(len(data)))
	d.mu.Unlock()
	if !room {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return
	}
	_, err = f.WriteAt(data, fileOff)
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		return
	}
	before := mf.cov.size()
	mf.cov.add(fileOff, end) // the data is synced: true in memory even if the .cov write fails
	atomic.AddInt64(&d.totalSize, mf.cov.size()-before)
	d.indexAdd(path)
	if err := writeCov(path, mf.cov.list()); err != nil { // a restart then keeps the older ranges only
		logf.Printf("[DiskWarmup] mid coverage of %s: %v", filepath.Base(path), err)
	}
}

// writeCov replaces path's coverage file atomically.
func writeCov(path string, spans []tailSpan) error {
	tmp := path + covSuffix + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, s := range spans {
		fmt.Fprintf(w, "%d %d\n", s.start, s.end)
	}
	if err = w.Flush(); err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path+covSuffix)
}

// readCov loads a mid file's coverage; false without a valid coverage file.
func readCov(path string) (*midFile, bool) {
	b, err := os.ReadFile(path + covSuffix)
	if err != nil {
		return nil, false
	}
	mf := &midFile{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var s, e int64
		if _, err := fmt.Sscanf(line, "%d %d", &s, &e); err != nil || s < 0 || e <= s {
			return nil, false
		}
		mf.cov.add(s, e)
	}
	return mf, true
}

// loadMid restores a mid file at start: kept with its coverage, removed without; returns its bytes.
func (d *DiskWarmupCache) loadMid(path string) int64 {
	mf, ok := readCov(path)
	if !ok {
		os.Remove(path)
		os.Remove(path + covSuffix)
		return 0
	}
	d.mids.Store(path, mf)
	return mf.cov.size()
}

// removeMid deletes a mid file and its coverage, after any write in progress on it.
func (d *DiskWarmupCache) removeMid(path string) {
	if v, ok := d.mids.Load(path); ok {
		mf := v.(*midFile)
		mf.mu.Lock()
		defer mf.mu.Unlock()
		d.removeMidLocked(path, mf)
		return
	}
	if _, busy := d.midWriting.Load(path); !busy {
		d.removeMidFiles(path)
	}
}

// evictMid removes a mid file for the quota, under d.mu: a file whose writer holds it is skipped,
// as that writer may be waiting for d.mu.
func (d *DiskWarmupCache) evictMid(path string) bool {
	v, ok := d.mids.Load(path)
	if !ok {
		if _, busy := d.midWriting.Load(path); busy {
			return false // its writer is about to create the entry
		}
		d.removeMidFiles(path)
		return true
	}
	mf := v.(*midFile)
	if !mf.mu.TryLock() {
		return false
	}
	defer mf.mu.Unlock()
	d.removeMidLocked(path, mf)
	return true
}

// removeMidLocked deletes path with mf.mu held, only if mf is still its entry: a newer entry owns
// the files now.
func (d *DiskWarmupCache) removeMidLocked(path string, mf *midFile) {
	if !d.mids.CompareAndDelete(path, mf) {
		return
	}
	atomic.AddInt64(&d.totalSize, -mf.cov.size())
	d.removeMidFiles(path)
}

func (d *DiskWarmupCache) removeMidFiles(path string) {
	os.Remove(path)
	os.Remove(path + covSuffix)
	d.indexRemove(path)
}

// midSpans lists the covered ranges of a mid file.
func (d *DiskWarmupCache) midSpans(path string) []tailSpan {
	v, ok := d.mids.Load(path)
	if !ok {
		return nil
	}
	return v.(*midFile).cov.list()
}
