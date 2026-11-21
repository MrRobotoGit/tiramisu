// Package bench measures data-path chains block by block, for A/B decisions between them.
package bench

import (
	"fmt"
	"slices"
	"time"
)

// Result of one or more sequential passes.
type Result struct {
	Blocks        int
	P50, P95, Max time.Duration
	MiBps         float64
}

func (r Result) String() string {
	return fmt.Sprintf("blocks %4d  p50 %9v  p95 %9v  max %9v  %7.1f MiB/s", r.Blocks, r.P50.Round(time.Microsecond),
		r.P95.Round(time.Microsecond), r.Max.Round(time.Microsecond), r.MiBps)
}

// Reader reads one block at off; a short block without error is a failure.
type Reader func(p []byte, off int64) (int, error)

// Run makes passes sequential passes over [0, size), each through a fresh reader from open,
// and times every block. Throughput is the total over the passes.
func Run(open func() (Reader, func()), size int64, block, passes int) (Result, error) {
	lat, busy, err := run(open, size, block, passes, nil)
	if err != nil {
		return Result{}, err
	}
	return summarize(lat, size*int64(passes), busy), nil
}

// Passes is Run with one Result per pass.
func Passes(open func() (Reader, func()), size int64, block, passes int) ([]Result, error) {
	var out []Result
	_, _, err := run(open, size, block, passes, func(lat []time.Duration, busy time.Duration) {
		out = append(out, summarize(lat, size, busy))
	})
	return out, err
}

func run(open func() (Reader, func()), size int64, block, passes int, each func([]time.Duration, time.Duration)) ([]time.Duration, time.Duration, error) {
	buf := make([]byte, block)
	var lat []time.Duration
	var busy time.Duration
	for range passes {
		read, done := open()
		from := len(lat)
		start := time.Now()
		for off := int64(0); off < size; {
			want := int(min(int64(block), size-off))
			t0 := time.Now()
			n, err := read(buf[:want], off)
			lat = append(lat, time.Since(t0))
			if err == nil && n != want {
				err = fmt.Errorf("%d of %d bytes", n, want)
			}
			if err != nil {
				done()
				return nil, 0, fmt.Errorf("block at %d: %w", off, err)
			}
			off += int64(n)
		}
		took := time.Since(start)
		busy += took
		done()
		if each != nil {
			each(lat[from:], took)
		}
	}
	return lat, busy, nil
}

// Summary is the per-field median and range of several Results.
type Summary struct {
	N              int
	Median, Lo, Hi Result
}

func (s Summary) String() string {
	return fmt.Sprintf("n=%d  p50 %v [%v–%v]  p95 %v [%v–%v]  %.1f MiB/s [%.1f–%.1f]", s.N,
		s.Median.P50.Round(time.Microsecond), s.Lo.P50.Round(time.Microsecond), s.Hi.P50.Round(time.Microsecond),
		s.Median.P95.Round(time.Microsecond), s.Lo.P95.Round(time.Microsecond), s.Hi.P95.Round(time.Microsecond),
		s.Median.MiBps, s.Lo.MiBps, s.Hi.MiBps)
}

// Summarize takes the median (upper middle for an even count) and range of every field.
func Summarize(rs []Result) Summary {
	s := Summary{N: len(rs)}
	if len(rs) == 0 {
		return s
	}
	field := func(get func(Result) float64, set func(*Result, float64)) {
		v := make([]float64, len(rs))
		for i, r := range rs {
			v[i] = get(r)
		}
		slices.Sort(v)
		set(&s.Median, v[len(v)/2])
		set(&s.Lo, v[0])
		set(&s.Hi, v[len(v)-1])
	}
	dur := func(get func(Result) time.Duration, set func(*Result, time.Duration)) {
		field(func(r Result) float64 { return float64(get(r)) }, func(r *Result, x float64) { set(r, time.Duration(x)) })
	}
	dur(func(r Result) time.Duration { return r.P50 }, func(r *Result, d time.Duration) { r.P50 = d })
	dur(func(r Result) time.Duration { return r.P95 }, func(r *Result, d time.Duration) { r.P95 = d })
	dur(func(r Result) time.Duration { return r.Max }, func(r *Result, d time.Duration) { r.Max = d })
	field(func(r Result) float64 { return r.MiBps }, func(r *Result, x float64) { r.MiBps = x })
	field(func(r Result) float64 { return float64(r.Blocks) }, func(r *Result, x float64) { r.Blocks = int(x) })
	return s
}

// Sequential is one pass through read.
func Sequential(read Reader, size int64, block int) (Result, error) {
	return Run(func() (Reader, func()) { return read, func() {} }, size, block, 1)
}

func summarize(lat []time.Duration, bytes int64, total time.Duration) Result {
	s := slices.Clone(lat)
	slices.Sort(s)
	q := func(p float64) time.Duration { return s[min(len(s)-1, int(p*float64(len(s))))] }
	return Result{Blocks: len(s), P50: q(0.50), P95: q(0.95), Max: s[len(s)-1], MiBps: float64(bytes) / (1 << 20) / total.Seconds()}
}

// Accept applies the acceptance rule of plan 3: the new chain's p95 may exceed the old one's
// by less than 1ms per block, and its throughput may be at most 5% lower.
func Accept(old, new Result) error {
	if d := new.P95 - old.P95; d >= time.Millisecond {
		return fmt.Errorf("p95 per block +%v (limit 1ms)", d)
	}
	if new.MiBps < old.MiBps*0.95 {
		return fmt.Errorf("throughput %.1f MiB/s < 95%% of %.1f", new.MiBps, old.MiBps)
	}
	return nil
}
