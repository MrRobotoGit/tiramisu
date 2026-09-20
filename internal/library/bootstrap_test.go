package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"tiramisu/internal/vfs"
)

func TestWriteAudioStub(t *testing.T) {
	const (
		streamURL = "https://example.invalid/stream?link=audio-hash&index=7"
		size      = int64(4 * 1024 * 1024)
		magnet    = "magnet:?xt=urn:btih:audio-hash"
	)

	t.Run("B1_B2_B3_B6 creates the tree and writes audio metadata", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, string(SectionMusic), "Artist", "Album", "01 - Track_a1b2c3d4.flac")

		if err := WriteAudioStub(path, streamURL, size, magnet); err != nil {
			t.Fatalf("WriteAudioStub: %v", err)
		}

		meta, err := vfs.ReadMetadataFromFileWithLimits(path, vfs.AudioSizeLimits)
		if err != nil {
			t.Fatalf("B2 read audio metadata: %v", err)
		}
		if meta.URL != streamURL || meta.Size != size || meta.Path != path {
			t.Errorf("B2 metadata = %#v, want URL %q, size %d, path %q", meta, streamURL, size, path)
		}
		if meta.ImdbID != "" {
			t.Errorf("B3 metadata IMDb ID = %q, want empty", meta.ImdbID)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read written stub: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatalf("B2 stub is not valid JSON: %v", err)
		}
		if _, ok := fields["imdb"]; ok {
			t.Errorf("B3 stub fields = %v, must not contain imdb", fieldNames(fields))
		}
		if want := []string{"magnet", "size", "url"}; !reflect.DeepEqual(fieldNames(fields), want) {
			t.Errorf("B2/B3 stub fields = %v, want exactly %v", fieldNames(fields), want)
		}
		var gotMagnet string
		if err := json.Unmarshal(fields["magnet"], &gotMagnet); err != nil {
			t.Fatalf("B2 decode magnet: %v", err)
		}
		if gotMagnet != magnet {
			t.Errorf("B2 magnet = %q, want %q", gotMagnet, magnet)
		}

		for _, check := range []struct {
			name string
			path string
			mode os.FileMode
		}{
			{"B6 artist directory", filepath.Join(root, string(SectionMusic), "Artist"), 0o755},
			{"B6 album directory", filepath.Join(root, string(SectionMusic), "Artist", "Album"), 0o755},
			{"B6 stub file", path, 0o644},
		} {
			info, err := os.Stat(check.path)
			if err != nil {
				t.Fatalf("%s stat: %v", check.name, err)
			}
			if got := info.Mode().Perm(); got != check.mode {
				t.Errorf("%s mode = %04o, want %04o", check.name, got, check.mode)
			}
		}
	})

	t.Run("B4 overwrites the complete previous stub", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), string(SectionAudiobooks), "Author", "Book", "Part 01.m4b")
		if err := WriteAudioStub(path, streamURL+"&long-unused-suffix=must-disappear", size, magnet+"-long-unused-suffix"); err != nil {
			t.Fatalf("write longer stub: %v", err)
		}
		if err := WriteAudioStub(path, "https://e.invalid/s", 1, "m"); err != nil {
			t.Fatalf("overwrite with shorter stub: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read overwritten stub: %v", err)
		}
		var got struct {
			URL    string `json:"url"`
			Size   int64  `json:"size"`
			Magnet string `json:"magnet"`
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("B4 overwritten stub is not a single valid JSON value: %v", err)
		}
		want := struct {
			URL    string `json:"url"`
			Size   int64  `json:"size"`
			Magnet string `json:"magnet"`
		}{"https://e.invalid/s", 1, "m"}
		if got != want {
			t.Errorf("B4 overwritten metadata = %#v, want %#v", got, want)
		}
	})

	t.Run("B5 unusable parent returns an error", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "regular-file")
		if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
			t.Fatalf("create regular-file parent: %v", err)
		}
		path := filepath.Join(parent, "Album", "Track.flac")
		if err := WriteAudioStub(path, streamURL, size, magnet); err == nil {
			t.Fatal("B5 WriteAudioStub error = nil for a parent that is a regular file")
		}
	})
}

func TestEnsureSectionRoots(t *testing.T) {
	t.Run("B7_B10 creates exactly both audio roots", func(t *testing.T) {
		root := t.TempDir()
		if err := EnsureSectionRoots(root); err != nil {
			t.Fatalf("EnsureSectionRoots: %v", err)
		}

		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read source root: %v", err)
		}
		got := make([]string, 0, len(entries))
		for _, entry := range entries {
			got = append(got, entry.Name())
			if !entry.IsDir() {
				t.Errorf("B7 section %q is not a directory", entry.Name())
			}
		}
		want := []string{string(SectionAudiobooks), string(SectionMusic)}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("B7/B10 source entries = %v, want exactly %v", got, want)
		}
	})

	t.Run("B8_B9 repeated calls preserve existing roots and contents", func(t *testing.T) {
		root := t.TempDir()
		music := filepath.Join(root, string(SectionMusic))
		books := filepath.Join(root, string(SectionAudiobooks))
		if err := os.Mkdir(music, 0o700); err != nil {
			t.Fatalf("create music root: %v", err)
		}
		if err := os.Mkdir(books, 0o711); err != nil {
			t.Fatalf("create audiobooks root: %v", err)
		}
		stub := filepath.Join(music, "existing.flac")
		wantContent := []byte("existing stub must survive")
		if err := os.WriteFile(stub, wantContent, 0o600); err != nil {
			t.Fatalf("create existing stub: %v", err)
		}
		beforeMusic, err := os.Stat(music)
		if err != nil {
			t.Fatal(err)
		}
		beforeBooks, err := os.Stat(books)
		if err != nil {
			t.Fatal(err)
		}

		for call := 1; call <= 2; call++ {
			if err := EnsureSectionRoots(root); err != nil {
				t.Fatalf("B8 EnsureSectionRoots call %d: %v", call, err)
			}
		}

		afterMusic, err := os.Stat(music)
		if err != nil {
			t.Fatalf("stat music after calls: %v", err)
		}
		afterBooks, err := os.Stat(books)
		if err != nil {
			t.Fatalf("stat audiobooks after calls: %v", err)
		}
		if !os.SameFile(beforeMusic, afterMusic) || !os.SameFile(beforeBooks, afterBooks) {
			t.Error("B8 an existing section root was recreated")
		}
		if got := afterMusic.Mode().Perm(); got != 0o700 {
			t.Errorf("B8 existing music mode = %04o, want preserved 0700", got)
		}
		if got := afterBooks.Mode().Perm(); got != 0o711 {
			t.Errorf("B8 existing audiobooks mode = %04o, want preserved 0711", got)
		}
		gotContent, err := os.ReadFile(stub)
		if err != nil {
			t.Fatalf("B9 read preserved stub: %v", err)
		}
		if !reflect.DeepEqual(gotContent, wantContent) {
			t.Errorf("B9 existing stub content = %q, want %q", gotContent, wantContent)
		}
	})

	t.Run("B11 unusable paths return errors", func(t *testing.T) {
		tests := []struct {
			name  string
			setup func(t *testing.T) string
		}{
			{
				name: "source path is a regular file",
				setup: func(t *testing.T) string {
					path := filepath.Join(t.TempDir(), "source-file")
					if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
						t.Fatal(err)
					}
					return path
				},
			},
			{
				name: "section root is a regular file",
				setup: func(t *testing.T) string {
					root := t.TempDir()
					if err := os.WriteFile(filepath.Join(root, string(SectionMusic)), []byte("not a directory"), 0o600); err != nil {
						t.Fatal(err)
					}
					return root
				},
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if err := EnsureSectionRoots(tc.setup(t)); err == nil {
					t.Fatal("B11 EnsureSectionRoots error = nil for unusable path")
				}
			})
		}
	})

	t.Run("B12 empty source path is rejected without relative writes", func(t *testing.T) {
		cwd := t.TempDir()
		t.Chdir(cwd)
		if err := EnsureSectionRoots(""); err == nil {
			t.Fatal("B12 EnsureSectionRoots(\"\") error = nil")
		}
		for _, section := range []Section{SectionMusic, SectionAudiobooks} {
			if _, err := os.Stat(filepath.Join(cwd, string(section))); !os.IsNotExist(err) {
				t.Errorf("B12 relative section %q was created or stat returned unexpected error: %v", section, err)
			}
		}
	})
}

func fieldNames(fields map[string]json.RawMessage) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
