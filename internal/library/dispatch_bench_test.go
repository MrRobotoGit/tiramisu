package library

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

var (
	dispatchBenchmarkBool  bool
	dispatchBenchmarkClass VFSClass
	dispatchBenchmarkCount int
	dispatchBenchmarkPath  string
)

func BenchmarkLegacyBeforeStringsHasSuffix(b *testing.B) {
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	inputs := []struct {
		name string
		path string
	}{
		{"shallow_video_inside_movies", filepath.Join(source, "movies", "Film.mkv")},
		{"flat_video_outside_sections", filepath.Join(source, "Film.mkv")},
		{"deep_committed_audio", filepath.Join(source, "music", "Artist", "Album", "Disc 1", "01 - Track_a1b2c3d4.flac")},
	}

	for _, input := range inputs {
		b.Run(input.name, func(b *testing.B) {
			b.ReportAllocs()
			dispatchBenchmarkPath = input.path
			b.ResetTimer()
			var result bool
			for i := 0; i < b.N; i++ {
				result = strings.HasSuffix(dispatchBenchmarkPath, ".mkv")
			}
			dispatchBenchmarkBool = result
		})
	}
}

func BenchmarkClassifyPathVideoInsideMovies(b *testing.B) {
	b.ReportAllocs()
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	path := filepath.Join(source, "movies", "Film.mkv")
	b.ResetTimer()
	var result VFSClass
	for i := 0; i < b.N; i++ {
		result = ClassifyPath(source, path)
	}
	dispatchBenchmarkClass = result
}

func BenchmarkClassifyPathVideoOutsideSections(b *testing.B) {
	b.ReportAllocs()
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	path := filepath.Join(source, "Film.mkv")
	b.ResetTimer()
	var result VFSClass
	for i := 0; i < b.N; i++ {
		result = ClassifyPath(source, path)
	}
	dispatchBenchmarkClass = result
}

func BenchmarkClassifyProjectionAudioCommittedDeep(b *testing.B) {
	b.ReportAllocs()
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	relative := "Artist/Album/Disc 1/01 - Track_a1b2c3d4.flac"
	path := filepath.Join(source, "music", filepath.FromSlash(relative))
	namespace := NewAudioNamespace()
	namespace.Replace([]AudioPath{{Section: SectionMusic, VirtualPath: relative}})
	b.ResetTimer()
	var result VFSClass
	for i := 0; i < b.N; i++ {
		result = ClassifyProjection(source, path, namespace)
	}
	dispatchBenchmarkClass = result
}

func BenchmarkClassifyProjectionAudioUncommittedDeep(b *testing.B) {
	b.ReportAllocs()
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	relative := "Artist/Album/Disc 1/01 - Rejected_a1b2c3d4.flac"
	path := filepath.Join(source, "music", filepath.FromSlash(relative))
	namespace := NewAudioNamespace()
	namespace.Replace([]AudioPath{{Section: SectionMusic, VirtualPath: "Artist/Album/Disc 1/Other_a1b2c3d4.flac"}})
	b.ResetTimer()
	var result VFSClass
	for i := 0; i < b.N; i++ {
		result = ClassifyProjection(source, path, namespace)
	}
	dispatchBenchmarkClass = result
}

func BenchmarkClassifyProjectionVideoDoesNotConsultOwnership(b *testing.B) {
	b.ReportAllocs()
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	path := filepath.Join(source, "movies", "Film.mkv")
	namespace := NewAudioNamespace()
	namespace.Replace([]AudioPath{{Section: SectionMusic, VirtualPath: "Artist/Track.flac"}})
	b.ResetTimer()
	var result VFSClass
	for i := 0; i < b.N; i++ {
		result = ClassifyProjection(source, path, namespace)
	}
	dispatchBenchmarkClass = result
}

func BenchmarkAudioNamespaceHasProjectionBySize(b *testing.B) {
	for _, size := range []int{1, 100, 10_000} {
		b.Run(fmt.Sprintf("entries_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			paths := make([]AudioPath, size)
			for i := range paths {
				paths[i] = AudioPath{
					Section:     SectionMusic,
					VirtualPath: fmt.Sprintf("Artist/Album/Track-%05d.flac", i),
				}
			}
			namespace := NewAudioNamespace()
			namespace.Replace(paths)
			target := paths[len(paths)-1]
			b.ResetTimer()
			var result bool
			for i := 0; i < b.N; i++ {
				result = namespace.HasProjection(target.Section, target.VirtualPath)
			}
			dispatchBenchmarkBool = result
		})
	}
}

func BenchmarkClassifyProjectionWholeDirectory1000DeepAudio(b *testing.B) {
	const entries = 1000
	b.ReportAllocs()
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	paths := make([]string, entries)
	projections := make([]AudioProjection, entries)
	for i := 0; i < entries; i++ {
		relative := fmt.Sprintf("Artist/Album/Disc 1/%04d - Track_%08x.flac", i, i)
		paths[i] = filepath.Join(source, "music", filepath.FromSlash(relative))
		projections[i] = AudioProjection{
			Section:     SectionMusic,
			VirtualPath: relative,
			Hash:        fmt.Sprintf("%040x", i+1),
			FileIndex:   i,
			Size:        int64(1_000_000 + i),
			MtimeNS:     int64(1_700_000_000_000_000_000 + i),
		}
	}
	namespace := NewAudioNamespace()
	namespace.Publish(projections)
	b.ResetTimer()
	count := 0
	for i := 0; i < b.N; i++ {
		for _, path := range paths {
			class := ClassifyProjection(source, path, namespace)
			if class.Stub {
				count++
			}
		}
	}
	b.StopTimer()
	dispatchBenchmarkCount = count
}
