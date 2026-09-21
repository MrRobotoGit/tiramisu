package library

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

const (
	openIdentityHash      = "0123456789abcdef0123456789abcdef01234567"
	openIdentityShortHash = "01234567"
)

var (
	openIdentityAudioSections = []Section{SectionMusic, SectionAudiobooks}
	openIdentityVideoSections = []Section{SectionMovies, SectionTV}
)

func openIdentityCopy(files []TorrentFile) []TorrentFile {
	if files == nil {
		return nil
	}
	return append([]TorrentFile{}, files...)
}

func requireOpenTarget(t *testing.T, got OpenTarget, err error, wantIndex int) {
	t.Helper()
	if err != nil {
		t.Fatalf("ResolveOpenTarget error = %v, want index %d", err, wantIndex)
	}
	if got.FileIndex != wantIndex {
		t.Fatalf("FileIndex = %d, want %d", got.FileIndex, wantIndex)
	}
	if got.Hash != openIdentityHash {
		t.Fatalf("Hash = %q, want %q", got.Hash, openIdentityHash)
	}
}

func requireOpenUnresolved(t *testing.T, got OpenTarget, err error) {
	t.Helper()
	if !errors.Is(err, ErrOpenTargetUnresolved) {
		t.Fatalf("error = %v, want ErrOpenTargetUnresolved", err)
	}
	if got != (OpenTarget{}) {
		t.Fatalf("target = %+v on error, want zero OpenTarget", got)
	}
}

// openIdentityTrapFiles builds the O1 trap: file 1 has the caller's size and a
// name that normalises onto the caller's virtual path, so the heuristic answers
// 1, while the registry row (urlIndex) says 2.
func openIdentityTrapFiles() (files []TorrentFile, size int64, virtualPath string) {
	size = 4096
	files = []TorrentFile{
		{Index: 1, Path: "Album/01_Track.flac", Length: size},
		{Index: 2, Path: "Album/02_Other.flac", Length: size},
		{Index: 3, Path: "Album/03_Third.flac", Length: 999},
	}
	virtualPath = "/Artist/Album/01 Track.flac"
	return files, size, virtualPath
}

func TestResolveOpenTarget_AudioIdentityIsAuthoritative(t *testing.T) {
	t.Run("O1 urlIndex wins over a size and name match on another file", func(t *testing.T) {
		files, size, vpath := openIdentityTrapFiles()
		for _, section := range openIdentityAudioSections {
			t.Run(string(section), func(t *testing.T) {
				got, err := ResolveOpenTarget(section, openIdentityHash, 2, size, vpath, files)
				requireOpenTarget(t, got, err, 2)
			})
		}
	})

	t.Run("O1 control: the same trap makes the video heuristic pick another file", func(t *testing.T) {
		files, size, vpath := openIdentityTrapFiles()
		for _, section := range openIdentityVideoSections {
			t.Run(string(section), func(t *testing.T) {
				got, err := ResolveOpenTarget(section, openIdentityHash, 2, size, vpath, files)
				requireOpenTarget(t, got, err, 1)
			})
		}
	})

	t.Run("O1 urlIndex wins when its own file matches neither name nor size", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "Book/chapter_01.m4b", Length: 500},
			{Index: 2, Path: "Book/chapter_02.m4b", Length: 600},
			{Index: 3, Path: "Book/chapter_03.m4b", Length: 700},
		}
		got, err := ResolveOpenTarget(SectionAudiobooks, openIdentityHash, 3, 500, "/Author/Book/chapter 01.m4b", files)
		requireOpenTarget(t, got, err, 3)
	})

	t.Run("O1 hash suffix in the caller name does not redirect audio", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "Album/01_Track.flac", Length: 4096},
			{Index: 2, Path: "Album/02_Other.flac", Length: 4096},
		}
		vpath := "/Artist/Album/01 Track_" + openIdentityShortHash + ".flac"
		got, err := ResolveOpenTarget(SectionMusic, openIdentityHash, 2, 4096, vpath, files)
		requireOpenTarget(t, got, err, 2)
	})

	t.Run("O2 urlIndex wins over a lone size match on another file", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "a.flac", Length: 100},
			{Index: 2, Path: "b.flac", Length: 200},
			{Index: 3, Path: "c.flac", Length: 300},
		}
		for _, section := range openIdentityAudioSections {
			t.Run(string(section), func(t *testing.T) {
				got, err := ResolveOpenTarget(section, openIdentityHash, 2, 100, "/x/renamed.flac", files)
				requireOpenTarget(t, got, err, 2)
			})
		}
	})

	t.Run("O3 nil or empty resident list still resolves to urlIndex", func(t *testing.T) {
		for name, files := range map[string][]TorrentFile{
			"nil":   nil,
			"empty": {},
		} {
			for _, section := range openIdentityAudioSections {
				t.Run(name+"/"+string(section), func(t *testing.T) {
					got, err := ResolveOpenTarget(section, openIdentityHash, 7, 123, "/x/y.flac", files)
					requireOpenTarget(t, got, err, 7)
				})
			}
		}
	})

	t.Run("O4 urlIndex is honoured when its file size differs from the stub", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "a.flac", Length: 100},
			{Index: 2, Path: "b.flac", Length: 200},
		}
		for name, size := range map[string]int64{
			"stub matches another file": 100,
			"stub matches no file":      31337,
			"stub size zero":            0,
		} {
			t.Run(name, func(t *testing.T) {
				got, err := ResolveOpenTarget(SectionMusic, openIdentityHash, 2, size, "/x/b.flac", files)
				requireOpenTarget(t, got, err, 2)
			})
		}
	})
}

func TestResolveOpenTarget_AudioWithoutUsableIndexErrors(t *testing.T) {
	sizedFiles := []TorrentFile{
		{Index: 1, Path: "Album/01_Track.flac", Length: 4096},
		{Index: 2, Path: "Album/02_Other.flac", Length: 8192},
	}
	fileSets := map[string][]TorrentFile{
		"nil list":                 nil,
		"empty list":               {},
		"lone size and name match": sizedFiles,
	}
	indexes := map[string]int{
		"zero":     0,
		"negative": -1,
		"min int":  math.MinInt,
	}
	for _, section := range openIdentityAudioSections {
		for fname, files := range fileSets {
			for iname, idx := range indexes {
				t.Run("O5 "+string(section)+"/"+fname+"/"+iname, func(t *testing.T) {
					got, err := ResolveOpenTarget(section, openIdentityHash, idx, 4096, "/Artist/Album/01 Track.flac", files)
					requireOpenUnresolved(t, got, err)
				})
			}
		}
	}

	t.Run("O12 audio index 0 never means the first file", func(t *testing.T) {
		files := []TorrentFile{{Index: 1, Path: "only.flac", Length: 10}}
		got, err := ResolveOpenTarget(SectionMusic, openIdentityHash, 0, 10, "/x/only.flac", files)
		requireOpenUnresolved(t, got, err)
	})
}

func TestResolveOpenTarget_VideoKeepsHeuristic(t *testing.T) {
	const size = int64(7_000_000)

	t.Run("O6 size and name match wins over urlIndex", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "Movie/Sample.mkv", Length: size},
			{Index: 2, Path: "Movie/Some.Movie.2020.mkv", Length: size},
			{Index: 3, Path: "Movie/Extras.mkv", Length: 1},
		}
		for _, section := range openIdentityVideoSections {
			t.Run(string(section), func(t *testing.T) {
				got, err := ResolveOpenTarget(section, openIdentityHash, 1, size, "/x/Some.Movie.2020.mkv", files)
				requireOpenTarget(t, got, err, 2)
			})
		}
	})

	nameCases := []struct {
		name        string
		virtualPath string
		torrentPath string
	}{
		{"short hash suffix after underscore is stripped", "/x/Some.Movie.2020_" + openIdentityShortHash + ".mkv", "Some.Movie.2020.mkv"},
		{"short hash suffix after dot is stripped", "/x/Some.Movie.2020." + openIdentityShortHash + ".mkv", "Some.Movie.2020.mkv"},
		{"full hash suffix after underscore is stripped", "/x/Some.Movie.2020_" + openIdentityHash + ".mkv", "Some.Movie.2020.mkv"},
		{"full hash suffix after dot is stripped", "/x/Some.Movie.2020." + openIdentityHash + ".mkv", "Some.Movie.2020.mkv"},
		{"hash suffix match is case-insensitive", "/x/Some.Movie.2020_0123456789ABCDEF0123456789ABCDEF01234567.mkv", "Some.Movie.2020.mkv"},
		{"underscores in the virtual name match dots", "/x/Some_Movie_2020.mkv", "Some.Movie.2020.mkv"},
		{"spaces in the virtual name match dots", "/x/Some Movie 2020.mkv", "Some.Movie.2020.mkv"},
		{"underscores in the torrent path match dots", "/x/Some.Movie.2020.mkv", "Some_Movie_2020.mkv"},
		{"name comparison is case-insensitive", "/x/SOME.MOVIE.2020.MKV", "some.movie.2020.mkv"},
		{"torrent path suffix matches the virtual base name", "/x/Some Movie 2020.mkv", "Folder/Sub/Some.Movie.2020.mkv"},
		{"virtual base name suffix matches a shorter torrent path", "/x/Prefix.Some.Movie.2020.mkv", "Movie.2020.mkv"},
		{"virtual directories are ignored", "/deep/er/dirs/Some Movie 2020.mkv", "Some.Movie.2020.mkv"},
	}
	for _, tc := range nameCases {
		t.Run("O6 "+tc.name, func(t *testing.T) {
			files := []TorrentFile{
				{Index: 1, Path: "Decoy.One.mkv", Length: size},
				{Index: 2, Path: tc.torrentPath, Length: size},
				{Index: 3, Path: "Decoy.Three.mkv", Length: size},
			}
			for _, section := range openIdentityVideoSections {
				got, err := ResolveOpenTarget(section, openIdentityHash, 1, size, tc.virtualPath, files)
				requireOpenTarget(t, got, err, 2)
			}
		})
	}

	t.Run("O6 a name match on a different size does not count", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "Some.Movie.2020.mkv", Length: size + 1},
			{Index: 2, Path: "Other.mkv", Length: size},
			{Index: 3, Path: "Another.mkv", Length: size},
		}
		got, err := ResolveOpenTarget(SectionMovies, openIdentityHash, 3, size, "/x/Some.Movie.2020.mkv", files)
		requireOpenTarget(t, got, err, 3)
	})

	t.Run("O6 name match wins over urlIndex even for the lone size file", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "One.mkv", Length: 10},
			{Index: 2, Path: "Some.Movie.2020.mkv", Length: size},
		}
		got, err := ResolveOpenTarget(SectionTV, openIdentityHash, 1, size, "/x/Some Movie 2020.mkv", files)
		requireOpenTarget(t, got, err, 2)
	})

	t.Run("O7 lone size match is trusted when the name does not normalise", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "Alpha.mkv", Length: 10},
			{Index: 2, Path: "Beta.mkv", Length: size},
			{Index: 3, Path: "Gamma.mkv", Length: 30},
		}
		for _, section := range openIdentityVideoSections {
			t.Run(string(section), func(t *testing.T) {
				got, err := ResolveOpenTarget(section, openIdentityHash, 1, size, "/x/Plex Renamed Title.mkv", files)
				requireOpenTarget(t, got, err, 2)
			})
		}
	})

	t.Run("O7 lone size match is trusted when urlIndex is absent", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "Alpha.mkv", Length: 10},
			{Index: 2, Path: "Beta.mkv", Length: size},
		}
		got, err := ResolveOpenTarget(SectionMovies, openIdentityHash, 0, size, "/x/Renamed.mkv", files)
		requireOpenTarget(t, got, err, 2)
	})

	t.Run("O7 several size matches without a name match are not trusted", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "Alpha.mkv", Length: size},
			{Index: 2, Path: "Beta.mkv", Length: size},
			{Index: 3, Path: "Gamma.mkv", Length: 30},
		}
		got, err := ResolveOpenTarget(SectionMovies, openIdentityHash, 3, size, "/x/Renamed.mkv", files)
		requireOpenTarget(t, got, err, 3)
	})

	t.Run("O8 no resident list falls back to urlIndex", func(t *testing.T) {
		for name, files := range map[string][]TorrentFile{"nil": nil, "empty": {}} {
			for _, section := range openIdentityVideoSections {
				t.Run(name+"/"+string(section), func(t *testing.T) {
					got, err := ResolveOpenTarget(section, openIdentityHash, 5, size, "/x/Some.Movie.mkv", files)
					requireOpenTarget(t, got, err, 5)
				})
			}
		}
	})

	t.Run("O8 resident list with no size match falls back to urlIndex", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "Some.Movie.mkv", Length: 1},
			{Index: 2, Path: "Other.mkv", Length: 2},
		}
		got, err := ResolveOpenTarget(SectionMovies, openIdentityHash, 2, size, "/x/Some.Movie.mkv", files)
		requireOpenTarget(t, got, err, 2)
	})

	t.Run("O9 no match and no usable urlIndex is unresolved", func(t *testing.T) {
		noMatch := []TorrentFile{
			{Index: 1, Path: "Alpha.mkv", Length: 1},
			{Index: 2, Path: "Beta.mkv", Length: 2},
		}
		ambiguous := []TorrentFile{
			{Index: 1, Path: "Alpha.mkv", Length: size},
			{Index: 2, Path: "Beta.mkv", Length: size},
		}
		fileSets := map[string][]TorrentFile{
			"nil list":            nil,
			"empty list":          {},
			"no size match":       noMatch,
			"ambiguous size only": ambiguous,
		}
		for fname, files := range fileSets {
			for iname, idx := range map[string]int{"zero": 0, "negative": -3} {
				for _, section := range openIdentityVideoSections {
					t.Run(fname+"/"+iname+"/"+string(section), func(t *testing.T) {
						got, err := ResolveOpenTarget(section, openIdentityHash, idx, size, "/x/Renamed.mkv", files)
						requireOpenUnresolved(t, got, err)
					})
				}
			}
		}
	})

	t.Run("O12 video index 0 with no match never means the first file", func(t *testing.T) {
		files := []TorrentFile{{Index: 1, Path: "Only.mkv", Length: 1}}
		got, err := ResolveOpenTarget(SectionMovies, openIdentityHash, 0, size, "/x/Only.mkv", files)
		requireOpenUnresolved(t, got, err)
	})
}

func TestResolveOpenTarget_ReturnsSuppliedHashUnchanged(t *testing.T) {
	const upper = "ABCDEF0123456789ABCDEF0123456789ABCDEF01"
	files := []TorrentFile{{Index: 1, Path: "Some.Movie.mkv", Length: 50}}
	cases := []struct {
		name      string
		section   Section
		urlIndex  int
		wantIndex int
	}{
		{"audio urlIndex", SectionMusic, 4, 4},
		{"audiobook urlIndex", SectionAudiobooks, 4, 4},
		{"video heuristic", SectionMovies, 4, 1},
		{"tv lone size match", SectionTV, 4, 1},
	}
	for _, tc := range cases {
		t.Run("O10 "+tc.name, func(t *testing.T) {
			got, err := ResolveOpenTarget(tc.section, upper, tc.urlIndex, 50, "/x/Some.Movie.mkv", files)
			if err != nil {
				t.Fatalf("ResolveOpenTarget error = %v", err)
			}
			if got.Hash != upper {
				t.Fatalf("Hash = %q, want %q unchanged", got.Hash, upper)
			}
			if got.FileIndex != tc.wantIndex {
				t.Fatalf("FileIndex = %d, want %d", got.FileIndex, tc.wantIndex)
			}
		})
	}

	t.Run("O10 video fallback keeps the hash", func(t *testing.T) {
		got, err := ResolveOpenTarget(SectionMovies, upper, 9, 50, "/x/Nope.mkv", nil)
		if err != nil || got.Hash != upper || got.FileIndex != 9 {
			t.Fatalf("got %+v, %v; want {%s 9}", got, err, upper)
		}
	})
}

func TestResolveOpenTarget_DoesNotMutateFiles(t *testing.T) {
	// Deliberately out of path order: sorting this slice in place would reorder it.
	original := []TorrentFile{
		{Index: 3, Path: "c/Some.Movie.mkv", Length: 30},
		{Index: 1, Path: "b/Other.mkv", Length: 30},
		{Index: 2, Path: "a/Third.mkv", Length: 10},
	}
	cases := []struct {
		name     string
		section  Section
		urlIndex int
		size     int64
		vpath    string
	}{
		{"audio with urlIndex", SectionMusic, 2, 30, "/x/Other.flac"},
		{"audiobook with urlIndex", SectionAudiobooks, 1, 30, "/x/Other.m4b"},
		{"audio unresolved", SectionMusic, 0, 30, "/x/Other.flac"},
		{"video name match", SectionMovies, 2, 30, "/x/Some Movie.mkv"},
		{"video lone size match", SectionTV, 3, 10, "/x/Renamed.mkv"},
		{"video ambiguous fallback", SectionMovies, 2, 30, "/x/Renamed.mkv"},
		{"video unresolved", SectionMovies, 0, 30, "/x/Renamed.mkv"},
	}
	for _, tc := range cases {
		t.Run("O11 "+tc.name, func(t *testing.T) {
			files := openIdentityCopy(original)
			_, _ = ResolveOpenTarget(tc.section, openIdentityHash, tc.urlIndex, tc.size, tc.vpath, files)
			if !reflect.DeepEqual(files, original) {
				t.Fatalf("files mutated:\n got  %+v\n want %+v", files, original)
			}
		})
	}

	t.Run("O11 result reports the engine-assigned Index of an unsorted list", func(t *testing.T) {
		files := openIdentityCopy(original)
		got, err := ResolveOpenTarget(SectionMovies, openIdentityHash, 2, 30, "/x/Some Movie.mkv", files)
		requireOpenTarget(t, got, err, 3)
	})
}

func TestResolveOpenTarget_IndexesAreOneBased(t *testing.T) {
	t.Run("O12 successful results are never zero", func(t *testing.T) {
		files := []TorrentFile{
			{Index: 1, Path: "a.mkv", Length: 10},
			{Index: 2, Path: "b.mkv", Length: 20},
		}
		cases := []struct {
			name     string
			section  Section
			urlIndex int
			size     int64
		}{
			{"audio", SectionMusic, 1, 10},
			{"video match", SectionMovies, 0, 10},
			{"video fallback", SectionMovies, 1, 99},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := ResolveOpenTarget(tc.section, openIdentityHash, tc.urlIndex, tc.size, "/x/a.mkv", files)
				if err != nil {
					t.Fatalf("ResolveOpenTarget error = %v", err)
				}
				if got.FileIndex < 1 {
					t.Fatalf("FileIndex = %d, want >= 1", got.FileIndex)
				}
			})
		}
	})

	t.Run("O12 the first file is addressed as index 1", func(t *testing.T) {
		files := []TorrentFile{{Index: 1, Path: "a.flac", Length: 10}, {Index: 2, Path: "b.flac", Length: 20}}
		got, err := ResolveOpenTarget(SectionMusic, openIdentityHash, 1, 20, "/x/b.flac", files)
		requireOpenTarget(t, got, err, 1)
	})
}
