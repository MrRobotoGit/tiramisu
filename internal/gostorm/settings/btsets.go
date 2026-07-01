package settings

import (
	"encoding/json"
	"io"
	"io/fs"

	"path/filepath"
	"strings"
	"sync"

	"gostream/internal/gostorm/log"
)

type BTSets struct {
	// Cache
	CacheSize       int64 // in byte, def 128 MB
	ReaderReadAHead int   // in percent, 5%-100%, [...S__X__E...] [S-E] not clean
	PreloadCache    int   // in percent

	// Disk
	UseDisk           bool
	TorrentsSavePath  string
	RemoveCacheOnDrop bool

	// Torrent
	ForceEncrypt             bool
	RetrackersMode           int  // 0 - don`t add, 1 - add retrackers (def), 2 - remove retrackers 3 - replace retrackers
	TorrentDisconnectTimeout int  // in seconds
	EnableDebug              bool // debug logs

	// BT Config
	EnableIPv6        bool
	DisableTCP        bool
	DisableUTP        bool
	DisableUPNP       bool
	DisableDHT        bool
	DisablePEX        bool
	DisableUpload     bool
	DownloadRateLimit int // in kb, 0 - inf
	UploadRateLimit   int // in kb, 0 - inf
	ConnectionsLimit  int
	// AggressivePeerManagement enables warmup-phase PEX churn and tail-hedging for
	// streaming cold-start latency. Zero-value false is already the safe default,
	// no explicit default assignment needed (unlike ConnectionsLimit).
	AggressivePeerManagement bool
	PeersListenPort          int
	BlockListURL             string

	// HTTPS
	SslPort int
	SslCert string
	SslKey  string

	// Reader
	ResponsiveMode bool // enable Responsive reader (don't wait pieceComplete)
	AdaptiveShield bool // enable auto-STRICT on corruption (V303); default false

	// FS
	ShowFSActiveTorr bool

	// Storage preferences
	StoreSettingsInJson bool
}

func (v *BTSets) String() string {
	buf, _ := json.Marshal(v)
	return string(buf)
}

var (
	BTsets   *BTSets
	btsetsMu sync.RWMutex
)

func SetBTSets(sets *BTSets) {
	btsetsMu.Lock()
	defer btsetsMu.Unlock()

	if ReadOnly {
		return
	}
	// V301: Failsafe checks - only set defaults if the value is truly missing/invalid (<= 0)
	if sets.CacheSize <= 0 {
		sets.CacheSize = 128 * 1024 * 1024
	}
	if sets.ConnectionsLimit <= 0 {
		sets.ConnectionsLimit = 25
	}
	if sets.TorrentDisconnectTimeout <= 0 {
		sets.TorrentDisconnectTimeout = 30
	}

	if sets.ReaderReadAHead < 5 {
		sets.ReaderReadAHead = 5
	}
	if sets.ReaderReadAHead > 100 {
		sets.ReaderReadAHead = 100
	}

	if sets.PreloadCache < 0 {
		sets.PreloadCache = 0
	}
	if sets.PreloadCache > 100 {
		sets.PreloadCache = 100
	}

	if sets.TorrentsSavePath == "" {
		sets.UseDisk = false
	} else if sets.UseDisk {
		BTsets = sets

		go func(s *BTSets) {
			rootPath := filepath.Clean(s.TorrentsSavePath)
			filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() && strings.ToLower(d.Name()) == ".tsc" {
					btsetsMu.Lock()
					s.TorrentsSavePath = path
					btsetsMu.Unlock()
					log.TLogln("Find directory \"" + s.TorrentsSavePath + "\", use as cache dir")
					return io.EOF
				}
				// Limit depth: if we are more than 1 level deep from rootPath, skip
				if d.IsDir() {
					rel, err := filepath.Rel(rootPath, path)
					if err == nil && strings.Count(rel, string(filepath.Separator)) > 0 {
						return filepath.SkipDir
					}
				}
				if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			})
		}(sets)
	}

	BTsets = sets
	buf, err := json.Marshal(BTsets)
	if err != nil {
		log.TLogln("Error marshal btsets", err)
		return
	}
	tdb.Set("Settings", "BitTorr", buf)
}

func SetDefaultConfig() {
	btsetsMu.Lock()
	defer btsetsMu.Unlock()

	sets := new(BTSets)
	sets.CacheSize = 128 * 1024 * 1024 // 128 MB
	sets.PreloadCache = 0
	sets.ConnectionsLimit = 25
	sets.RetrackersMode = 1
	sets.TorrentDisconnectTimeout = 15
	sets.ReaderReadAHead = 75 // 75%
	sets.EnableIPv6 = true
	sets.ResponsiveMode = true
	sets.AdaptiveShield = true
	sets.DisableUTP = true
	sets.ShowFSActiveTorr = true
	sets.StoreSettingsInJson = true
	BTsets = sets
	if !ReadOnly {
		buf, err := json.Marshal(BTsets)
		if err != nil {
			log.TLogln("Error marshal btsets", err)
			return
		}
		tdb.Set("Settings", "BitTorr", buf)
	}
}

// GetResponsiveMode returns the current ResponsiveMode setting with mutex protection.
func GetResponsiveMode() bool {
	btsetsMu.RLock()
	defer btsetsMu.RUnlock()
	return BTsets.ResponsiveMode
}

// GetAdaptiveShield returns the current AdaptiveShield setting with mutex protection.
func GetAdaptiveShield() bool {
	btsetsMu.RLock()
	defer btsetsMu.RUnlock()
	return BTsets.AdaptiveShield
}

func loadBTSets() {
	btsetsMu.Lock()
	defer btsetsMu.Unlock()

	buf := tdb.Get("Settings", "BitTorr")
	if len(buf) > 0 {
		err := json.Unmarshal(buf, &BTsets)
		if err == nil {
			if BTsets.ReaderReadAHead < 5 {
				BTsets.ReaderReadAHead = 5
			}
			// V301: Do NOT override TorrentDisconnectTimeout here if already loaded from DB
			return
		}
		log.TLogln("Error unmarshal btsets", err)
	}
	// initialize defaults only on error
	sets := new(BTSets)
	sets.CacheSize = 128 * 1024 * 1024 // 128 MB
	sets.PreloadCache = 0
	sets.ConnectionsLimit = 25
	sets.RetrackersMode = 1
	sets.TorrentDisconnectTimeout = 15
	sets.ReaderReadAHead = 75 // 75%
	sets.EnableIPv6 = true
	sets.ResponsiveMode = true
	sets.AdaptiveShield = true
	sets.DisableUTP = true
	sets.ShowFSActiveTorr = true
	sets.StoreSettingsInJson = true
	BTsets = sets
	if !ReadOnly {
		buf, err := json.Marshal(BTsets)
		if err != nil {
			log.TLogln("Error marshal btsets", err)
			return
		}
		tdb.Set("Settings", "BitTorr", buf)
	}
}
