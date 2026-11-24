// Command linesim measures how many significant lines of the Go files in a tree also appear
// in an upstream tree (the licence check of design.md §6 against TorrServer MatriX.137).
//
//	go run ./internal/tools/linesim -upstream /path/to/TorrServer-MatriX.137 -tree .
//
// A line is significant when, with its spaces collapsed, it has at least -min (20) characters and
// is neither a comment nor a package clause. Identical common Go lines (if err != nil {) set
// a noise floor: compare a file's share with it, not with zero.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type fileScore struct {
	path          string
	lines, shared int
}

func (f fileScore) pct() float64 {
	if f.lines == 0 {
		return 0
	}
	return 100 * float64(f.shared) / float64(f.lines)
}

func normalize(line string) string { return strings.Join(strings.Fields(line), " ") }

func significant(l string, min int) bool {
	return len(l) >= min && !strings.HasPrefix(l, "//") && !strings.HasPrefix(l, "/*") &&
		!strings.HasPrefix(l, "*") && !strings.HasPrefix(l, "package ")
}

func goFiles(root string, skip []string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			for _, s := range skip {
				if rel == s || d.Name() == ".git" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

func lines(path string, min int, each func(string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if l := normalize(sc.Text()); significant(l, min) {
			each(l)
		}
	}
	return sc.Err()
}

// score returns the per-file scores of tree against the upstream line set.
// The shared lines of the file show (relative to tree) are printed.
func score(upstream, tree string, skip []string, min int, show string) ([]fileScore, error) {
	up := map[string]bool{}
	ufiles, err := goFiles(upstream, nil)
	if err != nil {
		return nil, err
	}
	for _, p := range ufiles {
		if err := lines(p, min, func(l string) { up[l] = true }); err != nil {
			return nil, err
		}
	}
	tfiles, err := goFiles(tree, skip)
	if err != nil {
		return nil, err
	}
	var out []fileScore
	for _, p := range tfiles {
		rel, _ := filepath.Rel(tree, p)
		fsc := fileScore{path: rel}
		if err := lines(p, min, func(l string) {
			fsc.lines++
			if up[l] {
				fsc.shared++
				if rel == show {
					fmt.Println("  shared:", l)
				}
			}
		}); err != nil {
			return nil, err
		}
		out = append(out, fsc)
	}
	return out, nil
}

func main() {
	upstream := flag.String("upstream", "", "upstream source tree (TorrServer MatriX.137 checkout)")
	tree := flag.String("tree", ".", "tree to check")
	skipList := flag.String("skip", "internal/anacrolix-torrent,docs", "comma-separated dirs, relative to -tree, left out")
	min := flag.Int("min", 20, "minimum significant line length")
	top := flag.Int("top", 25, "files listed, highest share first")
	minLines := flag.Int("min-lines", 20, "files with fewer significant lines are not listed")
	show := flag.String("show", "", "print the shared lines of this file (path relative to -tree)")
	flag.Parse()
	if *upstream == "" {
		fmt.Fprintln(os.Stderr, "linesim: -upstream is required")
		os.Exit(2)
	}
	scores, err := score(*upstream, *tree, strings.Split(*skipList, ","), *min, *show)
	if err != nil {
		fmt.Fprintln(os.Stderr, "linesim:", err)
		os.Exit(1)
	}
	var total, shared int
	for _, s := range scores {
		total, shared = total+s.lines, shared+s.shared
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].pct() > scores[j].pct() })
	fmt.Printf("files %d, significant lines %d, shared with upstream %d (%.1f%%)\n", len(scores), total, shared, 100*float64(shared)/float64(max(total, 1)))
	n := 0
	for _, s := range scores {
		if s.lines < *minLines || n == *top {
			continue
		}
		n++
		fmt.Printf("%6.1f%%  %5d/%-5d  %s\n", s.pct(), s.shared, s.lines, s.path)
	}
}
