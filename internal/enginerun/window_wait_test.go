package enginerun

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// uploadsTake makes the fake engines take the time their uploads need at kib KiB/s: the harness
// clock advances by the bytes each backup adds or each copy uploads.
func (h *harness) uploadsTake(kib int64) {
	took := func(n int64) time.Duration {
		return time.Duration(float64(n) / float64(kib*1024) * float64(time.Second))
	}
	switch {
	case h.restic != nil:
		h.restic.onData = func(n int64) { h.advance(took(n)) }
	case h.rclone != nil:
		h.rclone.beforeCopy = func(files []string) {
			var n int64
			for _, f := range files {
				if fi, err := os.Stat(h.srcPath(f)); err == nil {
					n += fi.Size()
				}
			}
			h.advance(took(n))
		}
	}
}

// oneFileBatches makes every rclone copy batch one file (the fake has no --max-duration, so a
// batch must not carry a later file past the window's end).
func (h *harness) oneFileBatches() {
	if h.rclone == nil {
		return
	}
	h.settings(func(s *destinations.Settings) {
		if s.Rclone != nil {
			s.Rclone.BatchFiles = 1
		}
	})
}

// nightly runs the destination's scheduled sync on each of days nights at the given time after
// the window's 01:00 opening: every night's job plans afresh (the scheduled fire supersedes the
// deferred job of the night before), and before each run newFile (if set) adds a file that sorts
// before the others. It stops at the first job that completes and returns it with its night
// (0-based); day is -1 when every night's job deferred.
func (h *harness) nightly(days int, after time.Duration, newFile func(day int)) (j jobs.Job, day int) {
	h.t.Helper()
	for n := 0; n < days; n++ {
		h.setClock(time.Date(2026, 9, 28+n, 1, 0, 0, 0, time.UTC).Add(after))
		if newFile != nil {
			newFile(n)
		}
		j = h.newJob(jobs.TypeSync, false, jobs.Params{})
		_, err := h.runJob(j)
		h.closeJob(j.ID) // completed, or superseded by the next night's scheduled run
		if err == nil {
			return j, n
		}
		mustDefer(h.t, err)
	}
	return jobs.Job{}, -1
}

func (h *harness) present(rel string) bool {
	r, ok := h.live(h.dp(rel))
	return ok && r.State == syncer.StatePresent
}

// TestWindowOverrunWaitSurvivesSupersession: with allowOverrun a file larger than the window that
// waited for an opening starts in the next window even when every night's scheduled run plans
// afresh (it supersedes the deferred job) and begins late (another job held the upload slot), or
// puts a new file before it: the wait is kept in the destination's state and the file runs first
// (§9.2: a job never defers forever).
func TestWindowOverrunWaitSurvivesSupersession(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		for _, tc := range []struct {
			name     string
			after    time.Duration
			newFirst bool
		}{{"late start", 45 * time.Minute, false}, {"a new file first", 0, true}} {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				h := newHarness(t, harnessOptions{kind: kind, bandwidth: nightWindow(true, 1)}) // 21.1 MiB per window
				h.uploadsTake(1)
				h.oneFileBatches()
				h.writeSrc("huge.mkv", content("h", 22<<20), 1)
				var newFile func(int)
				if tc.newFirst {
					newFile = func(day int) { h.writeSrc(fmt.Sprintf("a%d.mkv", day), content(fmt.Sprintf("a%d", day), 2000), 2+day) }
				}
				h.nightly(3, tc.after, newFile)
				if !h.present("huge.mkv") {
					t.Errorf("huge.mkv is not backed up after 3 nights\n%s", h.rep.dump())
				}
			})
		}
	}
}

// TestWindowFitWaitSurvivesSupersession: a file that fits a whole window (with the grace) but not
// the time left at its turn waits once; the next night's plan puts a new file before it, yet it
// runs first at the opening and is backed up (§9.2).
func TestWindowFitWaitSurvivesSupersession(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(kind), func(t *testing.T) {
			h := newHarness(t, harnessOptions{kind: kind, bandwidth: graceWindow(1)}) // 01:00-01:10 + 5 min
			h.uploadsTake(1)
			h.oneFileBatches()
			h.writeSrc("w.mkv", content("w", 14*60*1024), 1) // 14 minutes
			h.nightly(3, 0, func(day int) {
				h.writeSrc(fmt.Sprintf("a%d.mkv", day), content(fmt.Sprintf("a%d", day), 2*60*1024), 2+day) // 2 minutes
			})
			if !h.present("w.mkv") {
				t.Errorf("w.mkv is not backed up after 3 nights\n%s", h.rep.dump())
			}
		})
	}
}

// TestWindowLateStartNeverFits: a file whose turn always comes with too little time left (every
// night's run begins late) does not wait forever: without allowOverrun it fails with a warning in
// its third window and the job completes; with allowOverrun it starts after its first wait and may
// run past the end (§9.2).
func TestWindowLateStartNeverFits(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		for _, overrun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/overrun=%v", kind, overrun), func(t *testing.T) {
				bw := graceWindow(1)
				bw.Window.AllowOverrun = overrun
				h := newHarness(t, harnessOptions{kind: kind, bandwidth: bw})
				h.uploadsTake(1)
				h.oneFileBatches()
				h.writeSrc("w.mkv", content("w", 14*60*1024), 1) // 14 minutes; 7 + 5 are left at 01:03
				j, day := h.nightly(5, 3*time.Minute, nil)
				if day < 0 {
					t.Fatalf("the sync still defers after 5 nights\n%s", h.rep.dump())
				}
				s, msg := itemStatus(h.items(j.ID), h.dp("w.mkv"))
				switch {
				case overrun && (s != jobs.ItemDone || !h.present("w.mkv")):
					t.Errorf("w.mkv: %s %q", s, msg)
				case !overrun && (s != jobs.ItemFailed || msg != notStartedMessage(3, "1.0 KiB/s")):
					t.Errorf("w.mkv: %s %q", s, msg)
				case !overrun && day != 2:
					t.Errorf("it failed on night %d, want its third window (night 2)", day)
				case overrun && day != 1:
					t.Errorf("it ran on night %d, want the night after its first wait (night 1)", day)
				}
			})
		}
	}
}
