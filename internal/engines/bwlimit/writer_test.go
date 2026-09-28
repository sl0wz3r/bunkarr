package bwlimit

import (
	"io"
	"math"
	"testing"
	"time"
)

// fakeClock advances only when the writer sleeps.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time             { return c.now }
func (c *fakeClock) Sleep(d time.Duration)      { c.now = c.now.Add(d) }
func (c *fakeClock) since(t0 time.Time) float64 { return c.now.Sub(t0).Seconds() }

// countWriter counts what reaches it.
type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

func writeAll(t *testing.T, w io.Writer, total, chunk int) {
	t.Helper()
	buf := make([]byte, chunk)
	for done := 0; done < total; done += chunk {
		n, err := w.Write(buf[:min(chunk, total-done)])
		if err != nil || n != min(chunk, total-done) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
}

func within(got, want, frac float64) bool { return math.Abs(got-want) <= want*frac }

func TestWriterRateFakeClock(t *testing.T) {
	const mib = 1 << 20
	tests := []struct {
		name  string
		rate  func(c *fakeClock, t0 time.Time) int64
		total int
		want  float64 // seconds
	}{
		{"1 MiB/s for 5 MiB", func(*fakeClock, time.Time) int64 { return mib }, 5 * mib, 5},
		{"small rate", func(*fakeClock, time.Time) int64 { return 1024 }, 5 * 1024, 5},
		{"rate raised after 2 s (a timetable change point)", func(c *fakeClock, t0 time.Time) int64 {
			if c.since(t0) < 2 {
				return mib
			}
			return 4 * mib
		}, 10 * mib, 2 + 8.0/4},
		{"unlimited", func(*fakeClock, time.Time) int64 { return 0 }, 50 * mib, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeClock{now: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
			t0 := c.now
			var out countWriter
			w := newWriter(&out, func() int64 { return tc.rate(c, t0) }, c.Now, c.Sleep)
			writeAll(t, w, tc.total, 64*1024)
			if out.n != int64(tc.total) {
				t.Fatalf("wrote %d of %d bytes", out.n, tc.total)
			}
			if got := c.since(t0); (tc.want == 0 && got != 0) || (tc.want > 0 && !within(got, tc.want, 0.05)) {
				t.Fatalf("took %.3f s, want %.3f s ± 5 %%", got, tc.want)
			}
		})
	}
}

// TestWriterRateRealTime: 2 MiB/s over 5 s of real time stays within 5 % (§14.1 bwlimit).
func TestWriterRateRealTime(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 5 s")
	}
	const rate = 2 << 20
	var out countWriter
	w := NewWriter(&out, func() int64 { return rate })
	start := time.Now()
	writeAll(t, w, 5*rate, 32*1024)
	if got := time.Since(start).Seconds(); !within(got, 5, 0.05) {
		t.Fatalf("5 s of data at %d B/s took %.3f s", rate, got)
	}
}
