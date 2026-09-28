//go:build e2e

package engines_test

import (
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/testhooks"
)

// TestWindowForTestHook: in an e2e build BUNKARR_TEST_WINDOW replaces the named destination's
// window with absolute times (closed, open, closed, open again) and keeps its grace.
func TestWindowForTestHook(t *testing.T) {
	t.Setenv(testhooks.EnvWindow, "7,2026-09-27T10:00:00Z,2026-09-27T10:00:15Z,2026-09-27T10:01:00Z")
	at := func(s string) time.Time { return time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC).Add(mustDuration(t, s)) }
	w := engines.WindowFor(7, bwlimit.Config{Window: &bwlimit.Window{Days: bwlimit.Days, From: "01:00", To: "07:00", GraceMinutes: 0}}, time.UTC)
	if w.Always() || w.Grace() != 0 || w.Length() != 15*time.Second || w.Describe() != "10:00:00–10:00:15" {
		t.Fatalf("window %v %v %v %q", w.Always(), w.Grace(), w.Length(), w.Describe())
	}
	tests := []struct {
		at        string
		open      bool
		next, end string
	}{
		{"-1s", false, "0s", ""},
		{"0s", true, "0s", "15s"},
		{"14s", true, "14s", "15s"},
		{"15s", false, "60s", ""},
		{"60s", true, "60s", ""}, // reopened, with no end
		{"1h", true, "1h", ""},
	}
	for _, tc := range tests {
		now := at(tc.at)
		if w.IsOpen(now) != tc.open || !w.NextOpen(now).Equal(at(tc.next)) {
			t.Errorf("at %s: open %v next %v", tc.at, w.IsOpen(now), w.NextOpen(now))
		}
		if end := w.EndsAt(now); (tc.end == "" && !end.IsZero()) || (tc.end != "" && !end.Equal(at(tc.end))) {
			t.Errorf("at %s: ends %v", tc.at, end)
		}
	}
	if other := engines.WindowFor(8, bwlimit.Config{}, time.UTC); !other.Always() {
		t.Error("the hook applied to another destination")
	}
}

func mustDuration(t *testing.T, s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
