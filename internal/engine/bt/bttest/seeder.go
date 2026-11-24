// Package bttest runs a real seeding client for engine tests, so packages above engine/bt
// never import the library themselves.
package bttest

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

// Seeder seeds one multi-file torrent over loopback.
type Seeder struct {
	TorrentFile []byte // the .torrent
	InfoBytes   []byte
	InfoHash    string
	Name        string
	Addr        string // 127.0.0.1:port
	Payload     []byte // every file concatenated, in info order

	dir string
	mi  *metainfo.MetaInfo
}

// NewSeeder writes files of the given sizes under dir/name and seeds them.
func NewSeeder(t testing.TB, name string, sizes []int, pieceLen int64) *Seeder {
	t.Helper()
	return NewSeederRate(t, name, sizes, pieceLen, 0)
}

// NewSeederRate is NewSeeder with the upload capped at upBytesPerSec (0 = unlimited).
func NewSeederRate(t testing.TB, name string, sizes []int, pieceLen int64, upBytesPerSec int) *Seeder {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Seeder{Name: name}
	for i, n := range sizes {
		b := make([]byte, n)
		_, _ = rand.Read(b)
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("E%d.bin", len(sizes)-i)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: pieceLen}
	if err := info.BuildFromFilePath(root); err != nil {
		t.Fatal(err)
	}
	for _, fi := range info.UpvertedFiles() {
		b, err := os.ReadFile(filepath.Join(append([]string{root}, fi.BestPath()...)...))
		if err != nil {
			t.Fatal(err)
		}
		s.Payload = append(s.Payload, b...)
	}
	var err error
	if s.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: s.InfoBytes, Announce: "udp://tracker.invalid:1/announce"}
	if s.TorrentFile, err = bencode.Marshal(mi); err != nil {
		t.Fatal(err)
	}
	s.InfoHash = mi.HashInfoBytes().HexString()

	s.dir, s.mi = dir, mi
	s.Addr = seed(t, dir, mi, upBytesPerSec)
	return s
}

// Reseed seeds s again from another client capped at upBytesPerSec and returns its address.
func Reseed(t testing.TB, s *Seeder, upBytesPerSec int) string {
	t.Helper()
	return seed(t, s.dir, s.mi, upBytesPerSec)
}

func seed(t testing.TB, dir string, mi *metainfo.MetaInfo, upBytesPerSec int) string {
	t.Helper()
	cfg := torrent.NewDefaultClientConfig()
	cfg.ListenPort = 0
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisableIPv6 = true
	cfg.NoDefaultPortForwarding = true
	cfg.Seed = true
	// The library stalls a connection whose queued requests exceed this allocation (1MiB by
	// default): a leecher pipelining more than 64 blocks would get nothing past the fast set.
	cfg.MaxAllocPeerRequestDataPerConn = 64 << 20
	cfg.DataDir = dir
	if upBytesPerSec > 0 {
		// The burst must hold one 16 KiB block, or the library panics on upload.
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(upBytesPerSec), max(upBytesPerSec/4, 64<<10))
	}
	cfg.DefaultStorage = storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, PieceCompletion: storage.NewMapPieceCompletion()})
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	tor, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	<-tor.GotInfo()
	tor.VerifyData()
	for deadline := time.Now().Add(30 * time.Second); tor.BytesMissing() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("seeder verification timeout")
		}
	}
	return fmt.Sprintf("127.0.0.1:%d", cl.LocalPort())
}

// TorrentFile builds a single-file .torrent with random content and no seeder.
func TorrentFile(t testing.TB, name string, size int, pieceLen int64) (file []byte, infoHash string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	b := make([]byte, size)
	_, _ = rand.Read(b)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: pieceLen}
	if err := info.BuildFromFilePath(path); err != nil {
		t.Fatal(err)
	}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: ib}
	if file, err = bencode.Marshal(mi); err != nil {
		t.Fatal(err)
	}
	return file, mi.HashInfoBytes().HexString()
}
