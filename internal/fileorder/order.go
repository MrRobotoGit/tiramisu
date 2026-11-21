package fileorder

import (
	"slices"
	"sort"
	"strconv"
)

// Less is natural order on paths: after the shared byte prefix, two leading numbers compare
// as integers and a leading number sorts first; otherwise plain byte order.
// Not a strict weak order ("06 x" ties "6 y"; cycles exist), so IDs depend on Sort's input.
func Less(a, b string) bool {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	ra, rb := a[i:], b[i:]
	na, okA := leadingInt(ra)
	nb, okB := leadingInt(rb)
	switch {
	case okA && okB:
		return na < nb
	case okA:
		return true
	case okB:
		return false
	}
	return ra < rb
}

// leadingInt parses the ASCII digits at the start of s; false when none or on int overflow.
func leadingInt(s string) (int, bool) {
	j := 0
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:j])
	return n, err == nil
}

// Sort returns paths in ID order (element i has ID i+1). Pass the engine's file order, never
// an already sorted list: with this comparator the result depends on the input order.
func Sort(paths []string) []string {
	out := slices.Clone(paths)
	sort.Slice(out, func(i, j int) bool { return Less(out[i], out[j]) })
	return out
}
