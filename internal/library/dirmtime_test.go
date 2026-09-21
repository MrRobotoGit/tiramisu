package library

import (
	"testing"
	"time"
)

// Spec 7: a directory's mtime is derived from the newest committed child below it, so
// only a committed add or replace can move it.
func TestAudioNamespaceDirMtime(t *testing.T) {
	older := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	newer := older.Add(time.Hour)
	other := newer.Add(time.Hour)

	ns := NewAudioNamespace()
	ns.Publish([]AudioProjection{
		{Section: SectionMusic, VirtualPath: "Artist/Album/01 - Track.flac", UpdatedAtNS: older.UnixNano()},
		{Section: SectionMusic, VirtualPath: "Artist/Album/Disc 2/01 - Track.flac", UpdatedAtNS: newer.UnixNano()},
		{Section: SectionMusic, VirtualPath: "Other/Album/01 - Track.flac", UpdatedAtNS: newer.UnixNano()},
		{Section: SectionAudiobooks, VirtualPath: "Artist/Album/01 - Part.m4b", UpdatedAtNS: other.UnixNano()},
	})

	for _, tc := range []struct {
		name    string
		section Section
		dir     string
		want    time.Time
		found   bool
	}{
		{"artist directory", SectionMusic, "Artist", newer, true},
		{"album below the artist", SectionMusic, "Artist/Album", newer, true},
		{"disc below the album", SectionMusic, "Artist/Album/Disc 2", newer, true},
		{"other top-level directory", SectionMusic, "Other", newer, true},
		{"empty prefix spans the whole section", SectionMusic, "", newer, true},
		{"no committed projection below", SectionMusic, "Missing", time.Time{}, false},
		{"prefix is component-wise, not string-wise", SectionMusic, "Art", time.Time{}, false},
		{"sections do not leak into each other", SectionAudiobooks, "Artist", other, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ns.DirMtime(tc.section, tc.dir)
			if ok != tc.found {
				t.Fatalf("DirMtime(%s, %q) found = %v, want %v", tc.section, tc.dir, ok, tc.found)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("DirMtime(%s, %q) = %v, want %v", tc.section, tc.dir, got, tc.want)
			}
		})
	}
}
