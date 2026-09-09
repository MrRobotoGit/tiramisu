package library

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tiramisu/internal/metadb"
)

const (
	defaultMetadataWait = 60
	maxMetadataWait     = 300
)

var reInfoHash = regexp.MustCompile(`^[a-f0-9]{40}$|^[a-z2-7]{32}$`)

// EpisodeRegistry is the subset of the state DB the manager writes to. A TV stub that
// is not registered is deleted by the next TV sync as orphaned, so this is required for
// TV adds.
type EpisodeRegistry interface {
	UpsertEpisode(key string, entry metadb.EpisodeEntry) error
	GetEpisode(key string) (*metadb.EpisodeEntry, bool, error)
	DeleteEpisode(key string) error
	EpisodesByFilePath(path string) ([]metadb.EpisodeEntry, error)
}

// Config wires the manager to the on-disk layout and the engine.
type Config struct {
	MoviesDir  string
	TVDir      string
	GoStormURL string
	GoStorm    GoStorm
	Registry   EpisodeRegistry
	// InvalidatePath, when set, drops the FUSE layer's cached state for a removed stub.
	InvalidatePath func(string)
	// Blacklist, when set, records a removed release the way the FUSE unlink handler
	// does, so the sync engines do not add it back. Called only when the caller asks.
	Blacklist func(path, hash string)
	Logger    *log.Logger
	// MediaServer, when set, is asked to rescan the section a stub was added to or
	// removed from. MovieSection and TVSection are its library ids.
	MediaServer  MediaServer
	MovieSection int
	TVSection    int
	// RefreshDelay is how long refreshes are coalesced for; 0 means the default.
	RefreshDelay time.Duration
}

// Manager adds and removes library entries on behalf of external clients: it does what
// the sync engines do for one title, without any of the discovery.
type Manager struct {
	cfg Config

	mu        sync.Mutex
	hashLocks *keyLocks // one lock per info hash
	showLocks *keyLocks // one per show: exclusive for packs, shared for single episodes

	refreshPending map[int]bool
}

func New(cfg Config) *Manager {
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stdout, "", log.LstdFlags)
	}
	cfg.GoStormURL = strings.TrimRight(cfg.GoStormURL, "/")
	return &Manager{cfg: cfg, hashLocks: newKeyLocks(), showLocks: newKeyLocks()}
}

// Error carries the HTTP status the handler should answer with.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func errf(status int, format string, args ...interface{}) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// AddRequest describes one title to file into the library. Either Hash or Magnet is
// required; everything else shapes the filename and the stub.
type AddRequest struct {
	Type         string `json:"type"` // "movie" (default) or "tv"
	Hash         string `json:"hash"`
	Magnet       string `json:"magnet"`
	Title        string `json:"title"`
	ReleaseTitle string `json:"release_title"`
	ReleaseDate  string `json:"release_date"`
	Year         int    `json:"year"`
	// IMDB is written into a movie stub. Episode stubs carry none: see addEpisodes.
	IMDB string `json:"imdb"`
	// Is4K overrides the resolution read out of ReleaseTitle.
	Is4K         *bool  `json:"is_4k"`
	Season       int    `json:"season"`
	Episode      int    `json:"episode"` // 0 with Season set means "season pack"
	FirstAirDate string `json:"first_air_date"`
	// FileIndex picks one file inside the torrent; 0 means the largest video file.
	FileIndex int `json:"file_index"`
	// QualityScore is stored in the TV registry. Left at 0, the next TV sync is free
	// to replace the episode with any release it scores above zero.
	QualityScore int `json:"quality_score"`
	MetadataWait int `json:"metadata_wait"`
}

// AddedFile is one stub written to disk.
type AddedFile struct {
	Path      string `json:"path"`
	FusePath  string `json:"fuse_path"`
	Size      int64  `json:"size"`
	FileIndex int    `json:"file_index"`
	Season    int    `json:"season,omitempty"`
	Episode   int    `json:"episode,omitempty"`
}

type AddResponse struct {
	Hash           string      `json:"hash"`
	Title          string      `json:"title"`
	Type           string      `json:"type"`
	Files          []AddedFile `json:"files"`
	AlreadyPresent bool        `json:"already_present"`
}

type RemoveRequest struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	// Blacklist keeps the release out: without it the sync engines are free to add the
	// title back on their next run, which is what you want when removing to upgrade
	// and not what you want when removing for good.
	Blacklist bool `json:"blacklist"`
}

type RemoveResponse struct {
	Removed []string `json:"removed"`
}

// Item is one stub as reported by List.
type Item struct {
	Path      string `json:"path"`
	FusePath  string `json:"fuse_path"`
	Size      int64  `json:"size"`
	Hash      string `json:"hash"`
	IMDB      string `json:"imdb,omitempty"`
	FileIndex int    `json:"file_index,omitempty"`
	Season    int    `json:"season,omitempty"`
	Episode   int    `json:"episode,omitempty"`
}

var (
	reStubHash  = regexp.MustCompile(`link=([a-f0-9]{40})`)
	reStubIndex = regexp.MustCompile(`index=(\d+)`)
)

// cleanupCtx is what the rollback paths use: the request context may already be
// cancelled (the client hung up during the metadata wait) and the cleanup still has to
// reach the engine, or the torrent stays hydrated with no stub pointing at it.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

func (m *Manager) dropTorrent(ctx context.Context, hash string) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if err := m.cfg.GoStorm.RemoveTorrent(cctx, hash); err != nil {
		m.cfg.Logger.Printf("[LibraryAPI] WARNING: cannot remove torrent %s: %v", hash, err)
	}
}

// Add registers the torrent with GoStorm, waits for its file list, and writes the stub
// files the FUSE layer serves. On any failure after the torrent was added it is removed
// again, so a failed call leaves nothing hydrated.
func (m *Manager) Add(ctx context.Context, req AddRequest) (*AddResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, errf(http.StatusRequestTimeout, "request cancelled: %v", err)
	}
	// Registered first so it runs after the unlocks below: dropping a torrent takes its
	// own hash lock, and nesting that inside the one this add holds is how two adds
	// replacing each other's episodes would deadlock. Every drop is reference-checked -
	// an earlier add of the same hash may still have stubs streaming from it.
	dropped := map[string]bool{}
	defer func() {
		for h := range dropped {
			m.dropTorrentIfUnused(ctx, h)
		}
	}()

	kind, hash, err := m.validate(&req)
	if err != nil {
		return nil, err
	}

	// Two clients asking for the same release at once would otherwise both pay the
	// metadata wait and write two stubs; the second one waits here and finds the
	// title already present.
	unlock := m.lockHash(hash)
	defer unlock()

	// Two releases of the same episode arriving at once carry different hashes, so the
	// lock above does not cover them: they would both write and race on the registry
	// key, leaving the loser's stub unregistered and its torrent running.
	// Two releases of the same episode carry different hashes, so the lock above does
	// not cover them: they would both write and race on the registry key. A pack takes
	// the show exclusively, because it writes episodes for seasons it discovers in the
	// file names and the requested season does not bound them; a single episode only
	// shares the show and locks its own key, so filing a season episode by episode
	// still runs in parallel.
	if kind == "tv" {
		show := "show:" + EpisodeKey(req.Title, 0, 0)
		if req.Episode > 0 {
			defer m.showLocks.RLock(show)()
			defer m.hashLocks.Lock("ep:" + EpisodeKey(req.Title, req.Season, req.Episode))()
		} else {
			defer m.showLocks.Lock(show)()
		}
	}

	// The stub already on disk is the cheap answer: it saves the metadata wait, and
	// re-adding would duplicate the title under a second name. For TV only an exact
	// episode counts, because one multi-season pack is a single hash behind several
	// seasons: matching by hash alone would report season 2 as already filed.
	if existing, err := m.alreadyPresent(kind, hash, req); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		return &AddResponse{Hash: hash, Title: req.Title, Type: kind, Files: existing, AlreadyPresent: true}, nil
	}

	magnet := req.Magnet
	if magnet == "" {
		magnet = BuildMagnet(hash, req.Title, DefaultTrackers())
	}

	requestedHash := hash
	addedHash, err := m.cfg.GoStorm.AddTorrent(ctx, magnet, req.Title)
	if err != nil || addedHash == "" {
		return nil, errf(http.StatusBadGateway, "gostorm rejected the torrent: %v", err)
	}
	// The filename builders slice the hash, so a malformed answer must stop here.
	hash = strings.ToLower(strings.TrimSpace(addedHash))
	if !reInfoHash.MatchString(hash) {
		// The torrent is hydrated under the hash we asked for; drop it rather than
		// leaving it running with no stub pointing at it.
		dropped[requestedHash] = true
		return nil, errf(http.StatusBadGateway, "gostorm returned a malformed info hash %q", addedHash)
	}
	// A base32 magnet comes back in hex: everything from here on is keyed on the
	// spelling the engine reported, so the lock has to cover it too, or a cleanup
	// elsewhere sees the hash free and removes the torrent from under this add.
	if hash != requestedHash {
		defer m.lockHash(hash)()
	}

	wait := req.MetadataWait
	if wait <= 0 {
		wait = defaultMetadataWait
	} else if wait > maxMetadataWait {
		wait = maxMetadataWait
	}

	info, err := m.cfg.GoStorm.GetTorrentInfo(ctx, hash, wait)
	if err != nil || info == nil {
		dropped[hash] = true
		return nil, errf(http.StatusGatewayTimeout, "no metadata after %ds: %v", wait, err)
	}

	var files []AddedFile
	if kind == "tv" {
		files, err = m.addEpisodes(ctx, req, hash, magnet, info, dropped)
	} else {
		files, err = m.addMovie(req, hash, magnet, info)
	}
	if err != nil {
		dropped[hash] = true
		return nil, err
	}

	m.scheduleRefresh(m.section(kind))
	m.cfg.Logger.Printf("[LibraryAPI] Added %s %q (%s): %d file(s)", kind, req.Title, hash[:8], len(files))
	return &AddResponse{Hash: hash, Title: req.Title, Type: kind, Files: files}, nil
}

// alreadyPresent reports the stubs this request would create that are already on disk.
// A TV pack is never short-circuited: which episodes it holds is only known once the
// file list arrives.
func (m *Manager) alreadyPresent(kind, hash string, req AddRequest) ([]AddedFile, error) {
	if kind != "tv" {
		return m.findByHash(kind, hash)
	}
	if req.Episode <= 0 {
		return nil, nil
	}
	path := filepath.Join(m.cfg.TVDir, ShowFolderName(req.Title, req.FirstAirDate),
		fmt.Sprintf("Season.%02d", req.Season),
		EpisodeFilename(req.Title, req.Season, req.Episode, hash[:8]))
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	st := readStub(path)
	return []AddedFile{{
		Path: path, FusePath: m.fusePath(path), Size: st.Size, FileIndex: st.FileIndex,
		Season: req.Season, Episode: req.Episode,
	}}, nil
}

// lockHash serialises work on one info hash.
func (m *Manager) lockHash(hash string) func() { return m.hashLocks.Lock(hash) }

// validate normalises the request and returns the content kind and the info hash.
func (m *Manager) validate(req *AddRequest) (string, string, error) {
	kind := strings.ToLower(strings.TrimSpace(req.Type))
	switch kind {
	case "", "movie", "movies", "film":
		kind = "movie"
	case "tv", "show", "series", "episode":
		kind = "tv"
	default:
		return "", "", errf(http.StatusBadRequest, "unknown type %q: use movie or tv", req.Type)
	}

	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		return "", "", errf(http.StatusBadRequest, "title is required")
	}
	if req.ReleaseTitle == "" {
		req.ReleaseTitle = req.Title
	}
	if req.ReleaseDate == "" && req.Year > 0 {
		req.ReleaseDate = fmt.Sprintf("%d", req.Year)
	}

	hash := strings.ToLower(strings.TrimSpace(req.Hash))
	if req.Magnet != "" {
		if h := HashFromMagnet(req.Magnet); h != "" {
			hash = h
		} else if hash == "" {
			return "", "", errf(http.StatusBadRequest, "magnet carries no info hash")
		}
	}
	if hash == "" {
		return "", "", errf(http.StatusBadRequest, "hash or magnet is required")
	}
	if !reInfoHash.MatchString(hash) {
		return "", "", errf(http.StatusBadRequest, "malformed info hash %q", hash)
	}

	if kind == "tv" {
		if req.Episode <= 0 && req.FileIndex > 0 {
			return "", "", errf(http.StatusBadRequest,
				"file_index needs an episode: a season pack files every file it can name")
		}
		if req.Season <= 0 {
			return "", "", errf(http.StatusBadRequest, "season is required for tv")
		}
		if m.cfg.Registry == nil {
			return "", "", errf(http.StatusServiceUnavailable,
				"the episode registry is unavailable: a tv stub added now would be deleted by the next sync")
		}
		if m.cfg.TVDir == "" {
			return "", "", errf(http.StatusServiceUnavailable, "no tv directory configured")
		}
	} else if m.cfg.MoviesDir == "" {
		return "", "", errf(http.StatusServiceUnavailable, "no movies directory configured")
	}
	return kind, hash, nil
}

func (m *Manager) addMovie(req AddRequest, hash, magnet string, info *TorrentStats) ([]AddedFile, error) {
	file, err := m.pickFile(req.FileIndex, info.FileStats)
	if err != nil {
		return nil, err
	}

	is4K := reIs4K.MatchString(req.ReleaseTitle)
	if req.Is4K != nil {
		is4K = *req.Is4K
	}

	name := BuildMovieFilename(MovieName{
		Title: req.Title, ReleaseDate: req.ReleaseDate, ReleaseTitle: req.ReleaseTitle,
		Is4K: is4K, Hash: hash,
	})
	path := filepath.Join(m.cfg.MoviesDir, name)
	if err := WriteStub(path, m.streamURL(hash, file.ID), file.Length, magnet, req.IMDB); err != nil {
		return nil, errf(http.StatusInternalServerError, "cannot write %s: %v", name, err)
	}
	return []AddedFile{{
		Path: path, FusePath: m.fusePath(path), Size: file.Length, FileIndex: file.ID,
	}}, nil
}

var reIs4K = regexp.MustCompile(`(?i)2160p|\buhd\b|\b4k\b`)

// pickFile returns the requested file, or the largest video file when no index is given.
func (m *Manager) pickFile(index int, files []FileStat) (*FileStat, error) {
	if index > 0 {
		for i := range files {
			if files[i].ID == index {
				if !IsVideoFile(files[i].Path) {
					return nil, errf(http.StatusBadRequest, "file %d (%s) is not a video file", index, files[i].Path)
				}
				return &files[i], nil
			}
		}
		return nil, errf(http.StatusBadRequest, "the torrent has no file %d", index)
	}

	var best *FileStat
	for i := range files {
		if IsVideoFile(files[i].Path) && (best == nil || files[i].Length > best.Length) {
			best = &files[i]
		}
	}
	if best == nil {
		return nil, errf(http.StatusUnprocessableEntity, "the torrent holds no video file")
	}
	return best, nil
}

func (m *Manager) addEpisodes(ctx context.Context, req AddRequest, hash, magnet string, info *TorrentStats, dropped map[string]bool) ([]AddedFile, error) {
	type episodeFile struct {
		file    FileStat
		season  int
		episode int
	}
	var wanted []episodeFile

	if req.Episode > 0 {
		file, err := m.pickFileForEpisode(req, info.FileStats)
		if err != nil {
			return nil, err
		}
		wanted = append(wanted, episodeFile{*file, req.Season, req.Episode})
	} else {
		for _, f := range info.FileStats {
			if !IsVideoFile(f.Path) {
				continue
			}
			season, episode := ParseSeasonEpisode(filepath.Base(f.Path))
			if episode == 0 {
				continue
			}
			if season == 0 {
				season = req.Season
			}
			wanted = append(wanted, episodeFile{f, season, episode})
		}
		if len(wanted) == 0 {
			return nil, errf(http.StatusUnprocessableEntity, "no episode file could be named in this torrent")
		}
	}

	// One episode, one stub: a pack carrying a proper next to the original would
	// otherwise write the same path twice, and a rollback could only half-undo it.
	best := map[[2]int]episodeFile{}
	for _, w := range wanted {
		k := [2]int{w.season, w.episode}
		if cur, ok := best[k]; !ok || w.file.Length > cur.file.Length {
			best[k] = w
		}
	}
	wanted = wanted[:0]
	for _, w := range best {
		wanted = append(wanted, w)
	}

	sort.Slice(wanted, func(i, j int) bool {
		if wanted[i].season != wanted[j].season {
			return wanted[i].season < wanted[j].season
		}
		return wanted[i].episode < wanted[j].episode
	})

	showDir := filepath.Join(m.cfg.TVDir, ShowFolderName(req.Title, req.FirstAirDate))
	var out []AddedFile
	// What this call found already in place. The stubs it overwrote must survive a
	// rollback, and the episodes a previous release left elsewhere are only deleted
	// once the whole pack is written: doing it as we go would leave a failure halfway
	// with neither the old episode nor the new one.
	type previous struct {
		entry    metadb.EpisodeEntry
		hadEntry bool
		hadStub  bool
		path     string
	}
	var prior []previous

	rollback := func() {
		for i, f := range out {
			if !prior[i].hadStub {
				if err := m.deleteStub(ctx, f.Path); err != nil {
					m.cfg.Logger.Printf("[LibraryAPI] WARNING: cannot undo %s: %v", f.Path, err)
				}
			}
			key := EpisodeKey(req.Title, f.Season, f.Episode)
			var err error
			if prior[i].hadEntry {
				err = m.cfg.Registry.UpsertEpisode(key, prior[i].entry)
			} else {
				err = m.cfg.Registry.DeleteEpisode(key)
			}
			if err != nil {
				m.cfg.Logger.Printf("[LibraryAPI] WARNING: cannot undo the registry entry for %s: %v", key, err)
			}
		}
	}

	for _, w := range wanted {
		seasonDir := filepath.Join(showDir, fmt.Sprintf("Season.%02d", w.season))
		path := filepath.Join(seasonDir, EpisodeFilename(req.Title, w.season, w.episode, hash[:8]))
		key := EpisodeKey(req.Title, w.season, w.episode)

		// Read the previous state before touching anything: a registry we cannot read
		// is a registry we cannot restore, so the add stops here rather than
		// overwriting a row blind.
		p := previous{path: path}
		prev, ok, err := m.cfg.Registry.GetEpisode(key)
		if err != nil {
			rollback()
			return nil, errf(http.StatusServiceUnavailable, "cannot read the registry entry for %s: %v", key, err)
		}
		if ok && prev != nil {
			p.entry, p.hadEntry = *prev, true
		}
		if _, err := os.Stat(path); err == nil {
			p.hadStub = true
		}

		// Episode stubs carry no imdb id, as the TV sync writes them: the webhook
		// matcher pairs a Plex episode event with an open file by looking at states
		// whose id is empty, and a series id here takes the episode out of it.
		if err := WriteStub(path, m.streamURL(hash, w.file.ID), w.file.Length, magnet, ""); err != nil {
			rollback()
			return nil, errf(http.StatusInternalServerError, "cannot write the stub for S%02dE%02d: %v", w.season, w.episode, err)
		}
		out = append(out, AddedFile{
			Path: path, FusePath: m.fusePath(path), Size: w.file.Length,
			FileIndex: w.file.ID, Season: w.season, Episode: w.episode,
		})
		prior = append(prior, p)

		if err := m.cfg.Registry.UpsertEpisode(key, metadb.EpisodeEntry{
			EpisodeKey: key, QualityScore: req.QualityScore, Hash: hash,
			FilePath: path, Source: "api", Created: time.Now().Unix(),
		}); err != nil {
			rollback()
			return nil, errf(http.StatusInternalServerError, "cannot register S%02dE%02d: %v", w.season, w.episode, err)
		}
	}

	// The pack is complete: now the releases it replaced can go.
	for _, p := range prior {
		if !p.hadEntry || p.entry.FilePath == "" || p.entry.FilePath == p.path {
			continue
		}
		if err := m.deleteStub(ctx, p.entry.FilePath); err != nil {
			// Leaving it there keeps its torrent alive, which is the safe half of the
			// failure, but it is a leak either way.
			m.cfg.Logger.Printf("[LibraryAPI] WARNING: cannot remove the replaced stub %s: %v", p.entry.FilePath, err)
			continue
		}
		if p.entry.Hash != "" && p.entry.Hash != hash {
			dropped[p.entry.Hash] = true
		}
	}
	return out, nil
}

// deleteStub removes one stub and drops the FUSE layer's cached state for it.
func (m *Manager) deleteStub(_ context.Context, path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if m.cfg.InvalidatePath != nil {
		m.cfg.InvalidatePath(path)
	}
	return nil
}

// dropTorrentIfUnused removes a torrent only once no stub points at it any more: a
// season pack is one torrent behind many episodes, and removing one episode must not
// make the others unstreamable.
//
// It holds the hash lock while it looks and decides. Without it an Add of the same hash,
// which holds that lock across the whole metadata wait with its stub not yet written,
// would look absent here and end up registered against a torrent this call had just
// removed; two concurrent Removes would both see the last stub gone and drop twice.
func (m *Manager) dropTorrentIfUnused(ctx context.Context, hash string) {
	// Never wait: whoever holds this hash is adding that same release right now, so
	// either it needs the torrent or its own cleanup will drop it.
	unlock, ok := m.hashLocks.TryLock(hash)
	if !ok {
		return
	}
	defer unlock()

	for _, kind := range []string{"movie", "tv"} {
		found, err := m.findByHash(kind, hash)
		if err != nil {
			m.cfg.Logger.Printf("[LibraryAPI] WARNING: keeping torrent %s, cannot check its stubs: %v", hash, err)
			return
		}
		if len(found) > 0 {
			return
		}
	}
	m.dropTorrent(ctx, hash)
}

// pickFileForEpisode prefers the file whose name carries the requested episode number;
// a single-episode torrent that does not name it falls back to the largest video file.
func (m *Manager) pickFileForEpisode(req AddRequest, files []FileStat) (*FileStat, error) {
	if req.FileIndex > 0 {
		return m.pickFile(req.FileIndex, files)
	}
	for i := range files {
		if !IsVideoFile(files[i].Path) {
			continue
		}
		if season, episode := ParseSeasonEpisode(filepath.Base(files[i].Path)); episode == req.Episode &&
			(season == req.Season || season == 0) {
			return &files[i], nil
		}
	}
	return m.pickFile(0, files)
}

func (m *Manager) section(kind string) int {
	if kind == "tv" {
		return m.cfg.TVSection
	}
	return m.cfg.MovieSection
}

// kindOf tells which media directory a path belongs to.
func (m *Manager) kindOf(path string) string {
	if m.cfg.TVDir != "" {
		if rel, err := filepath.Rel(filepath.Clean(m.cfg.TVDir), path); err == nil && !strings.HasPrefix(rel, "..") {
			return "tv"
		}
	}
	return "movie"
}

func (m *Manager) streamURL(hash string, fileID int) string {
	return fmt.Sprintf("%s/stream?link=%s&index=%d&play", m.cfg.GoStormURL, hash, fileID)
}

// fusePath is the stub's path as seen under the FUSE mount: the media directory's own
// name plus whatever is below it.
func (m *Manager) fusePath(path string) string {
	for _, dir := range m.mediaDirs() {
		if rel, err := filepath.Rel(dir, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(filepath.Join(filepath.Base(dir), rel))
		}
	}
	return filepath.ToSlash(path)
}

func (m *Manager) mediaDirs() []string {
	var dirs []string
	if m.cfg.MoviesDir != "" {
		dirs = append(dirs, filepath.Clean(m.cfg.MoviesDir))
	}
	if m.cfg.TVDir != "" {
		dirs = append(dirs, filepath.Clean(m.cfg.TVDir))
	}
	return dirs
}

// findByHash returns the stubs already on disk for this info hash. Movie stubs carry the
// last 8 hash chars, episode stubs the first 8: both conventions predate this API.
func (m *Manager) findByHash(kind, hash string) ([]AddedFile, error) {
	dir := m.cfg.MoviesDir
	suffix := HashSuffix(hash)
	if kind == "tv" {
		dir = m.cfg.TVDir
		suffix = hash[:8]
	}
	var out []AddedFile
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// A directory we cannot read would silently shrink the answer, and a
			// missing stub here means a duplicate add.
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, "_"+suffix+".mkv") {
			return nil
		}
		st := readStub(path)
		season, episode := ParseSeasonEpisode(filepath.Base(path))
		out = append(out, AddedFile{
			Path: path, FusePath: m.fusePath(path), Size: st.Size,
			FileIndex: st.FileIndex, Season: season, Episode: episode,
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, errf(http.StatusInternalServerError, "cannot read %s: %v", dir, err)
	}
	return out, nil
}

// Remove deletes the stubs for a title, the torrent behind them, and the registry
// entries pointing at them.
func (m *Manager) Remove(ctx context.Context, req RemoveRequest) (*RemoveResponse, error) {
	var targets []string

	switch {
	case req.Path != "":
		path, err := m.resolveInsideMediaDirs(req.Path)
		if err != nil {
			return nil, err
		}
		targets = []string{path}
	case req.Hash != "":
		hash := strings.ToLower(strings.TrimSpace(req.Hash))
		if !reInfoHash.MatchString(hash) {
			return nil, errf(http.StatusBadRequest, "malformed info hash %q", hash)
		}
		for _, kind := range []string{"movie", "tv"} {
			found, err := m.findByHash(kind, hash)
			if err != nil {
				return nil, err
			}
			for _, f := range found {
				targets = append(targets, f.Path)
			}
		}
		if len(targets) == 0 {
			return nil, errf(http.StatusNotFound, "no stub carries hash %s", hash)
		}
	default:
		return nil, errf(http.StatusBadRequest, "path or hash is required")
	}

	resp := &RemoveResponse{Removed: []string{}}
	var failed []string
	hashes := map[string]bool{}
	for _, path := range targets {
		hash := readStub(path).Hash
		if err := m.deleteStub(ctx, path); err != nil {
			m.cfg.Logger.Printf("[LibraryAPI] WARNING: cannot delete %s: %v", path, err)
			failed = append(failed, path)
			continue
		}
		if hash != "" {
			hashes[hash] = true
		}
		if req.Blacklist && m.cfg.Blacklist != nil {
			m.cfg.Blacklist(path, hash)
		}
		m.forgetEpisode(path)
		resp.Removed = append(resp.Removed, path)
	}

	// Only now, with the stubs gone, is it safe to decide which torrents nobody needs.
	for hash := range hashes {
		m.dropTorrentIfUnused(ctx, hash)
	}

	for _, path := range resp.Removed {
		m.scheduleRefresh(m.section(m.kindOf(path)))
	}
	m.cfg.Logger.Printf("[LibraryAPI] Removed %d stub(s)", len(resp.Removed))
	if len(failed) > 0 {
		return nil, errf(http.StatusInternalServerError,
			"removed %d stub(s), cannot delete: %s", len(resp.Removed), strings.Join(failed, ", "))
	}
	return resp, nil
}

func (m *Manager) forgetEpisode(path string) {
	if m.cfg.Registry == nil {
		return
	}
	entries, err := m.cfg.Registry.EpisodesByFilePath(path)
	if err != nil {
		m.cfg.Logger.Printf("[LibraryAPI] WARNING: registry lookup for %s: %v", path, err)
		return
	}
	for _, e := range entries {
		if err := m.cfg.Registry.DeleteEpisode(e.EpisodeKey); err != nil {
			m.cfg.Logger.Printf("[LibraryAPI] WARNING: cannot unregister %s: %v", e.EpisodeKey, err)
		}
	}
}

// resolveInsideMediaDirs accepts an absolute path, a path relative to a media
// directory, or the fuse_path the add response returned ("movies/x.mkv"), and refuses
// anything that escapes the configured directories.
func (m *Manager) resolveInsideMediaDirs(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errf(http.StatusBadRequest, "empty path")
	}
	candidates := []string{path}
	if !filepath.IsAbs(path) {
		for _, dir := range m.mediaDirs() {
			candidates = append(candidates,
				filepath.Join(filepath.Dir(dir), path),
				filepath.Join(dir, path))
		}
	}

	inBounds := ""
	for _, c := range candidates {
		clean := filepath.Clean(c)
		if !filepath.IsAbs(clean) || !m.insideMediaDirs(clean) {
			continue
		}
		if info, err := os.Stat(clean); err == nil {
			if info.IsDir() || !strings.HasSuffix(strings.ToLower(clean), ".mkv") {
				return "", errf(http.StatusBadRequest, "%s is not a stub file", path)
			}
			return clean, nil
		}
		if inBounds == "" {
			inBounds = clean
		}
	}
	if inBounds != "" {
		return "", errf(http.StatusNotFound, "no stub at %s", path)
	}
	return "", errf(http.StatusBadRequest, "%s is outside the media directories", path)
}

func (m *Manager) insideMediaDirs(clean string) bool {
	for _, dir := range m.mediaDirs() {
		if rel, err := filepath.Rel(dir, clean); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// List reports the stubs on disk. It is how a client with no filesystem access knows
// what the library already holds.
func (m *Manager) List(kind string) ([]Item, error) {
	dir := m.cfg.MoviesDir
	if strings.HasPrefix(strings.ToLower(kind), "tv") {
		dir = m.cfg.TVDir
	}
	items := []Item{}
	if dir == "" {
		return items, nil
	}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		st := readStub(path)
		season, episode := ParseSeasonEpisode(filepath.Base(path))
		items = append(items, Item{
			Path: path, FusePath: m.fusePath(path), Size: st.Size, Hash: st.Hash,
			IMDB: st.IMDB, FileIndex: st.FileIndex, Season: season, Episode: episode,
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, errf(http.StatusInternalServerError, "cannot read %s: %v", dir, err)
	}
	return items, nil
}

// stub is what a virtual .mkv on disk says about itself.
type stub struct {
	Size      int64
	Hash      string
	IMDB      string
	FileIndex int
}

// readStub reads a stub file. Legacy line-based stubs carry the stream URL on the first
// line and nothing else.
func readStub(path string) stub {
	data, err := os.ReadFile(path)
	if err != nil {
		return stub{}
	}
	content := strings.TrimSpace(string(data))

	url := ""
	out := stub{}
	if strings.HasPrefix(content, "{") {
		var obj struct {
			URL  string `json:"url"`
			Size int64  `json:"size"`
			IMDB string `json:"imdb"`
		}
		if err := json.Unmarshal(data, &obj); err != nil {
			return stub{}
		}
		url, out.Size, out.IMDB = obj.URL, obj.Size, obj.IMDB
	} else {
		url = strings.SplitN(content, "\n", 2)[0]
	}

	if m := reStubHash.FindStringSubmatch(url); len(m) > 1 {
		out.Hash = m[1]
	}
	if m := reStubIndex.FindStringSubmatch(url); len(m) > 1 {
		out.FileIndex, _ = strconv.Atoi(m[1])
	}
	return out
}
