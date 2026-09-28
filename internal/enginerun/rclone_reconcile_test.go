package enginerun

import (
	"os"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// TestRcloneReconcileSettles: the intents of stopped jobs are settled at the next job's start
// (§3.3) for each crash state, and the reconciliation never deletes: an equal-size update whose
// new version finished keeps its old version in retention and is present; a partial upload
// turns missing and is copied again; a retain found at both paths keeps both objects.
func TestRcloneReconcileSettles(t *testing.T) {
	const run = ".bunkarr/retention/20260901T000000Z-job99/"
	cases := []struct {
		name string
		// setup changes the source and the store after the first sync and returns the intent's
		// reason; live and ret say which objects exist then.
		setup     func(h *harness) (reason string, live, ret []byte)
		wantState syncer.State
		wantRet   int
	}{
		{name: "equal-size update finished", setup: func(h *harness) (string, []byte, []byte) {
			h.writeSrc("a.mkv", content("A", 2000), 50) // same size, new content and mtime
			return syncer.ReasonReplaced, []byte(content("A", 2000)), []byte(content("a", 2000))
		}, wantState: syncer.StatePresent, wantRet: 1},
		{name: "update interrupted", setup: func(h *harness) (string, []byte, []byte) {
			h.writeSrc("a.mkv", content("A", 2500), 50)
			return syncer.ReasonReplaced, nil, []byte(content("a", 2000))
		}, wantState: syncer.StatePresent, wantRet: 1},
		{name: "update partly uploaded", setup: func(h *harness) (string, []byte, []byte) {
			h.writeSrc("a.mkv", content("A", 2500), 50)
			return syncer.ReasonReplaced, []byte("partial"), []byte(content("a", 2000))
		}, wantState: syncer.StatePresent, wantRet: 2},
		{name: "nothing moved", setup: func(h *harness) (string, []byte, []byte) {
			return syncer.ReasonDeleted, []byte(content("a", 2000)), nil
		}, wantState: syncer.StatePresent, wantRet: 0},
		{name: "retain finished", setup: func(h *harness) (string, []byte, []byte) {
			h.removeSrc("a.mkv")
			return syncer.ReasonDeleted, nil, []byte(content("a", 2000))
		}, wantState: syncer.StateRetained, wantRet: 1},
		{name: "retain at both paths", setup: func(h *harness) (string, []byte, []byte) {
			h.removeSrc("a.mkv")
			return syncer.ReasonDeleted, []byte(content("a", 2000)), []byte(content("a", 2000))
		}, wantState: syncer.StateRetained, wantRet: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newRclone(t)
			h.writeSrc("a.mkv", content("a", 2000), 1)
			h.writeSrc("keep.mkv", content("k", 700), 2)
			h.mustSync(jobs.Params{})
			rec := h.mustLive(h.dp("a.mkv"))
			reason, live, ret := c.setup(h)
			retPath := run + h.dp("a.mkv")
			h.exec(`UPDATE destination_files SET retained_path = ?, reason = ? WHERE id = ?`, retPath, reason, rec.ID)
			h.rclone.remove(h.dp("a.mkv"))
			fi, _ := os.Stat(h.srcPath("a.mkv"))
			if live != nil {
				mt := h.now()
				if fi != nil && len(live) == int(fi.Size()) {
					mt = fi.ModTime()
				}
				h.rclone.put(h.dp("a.mkv"), live, mt)
			}
			if ret != nil {
				h.rclone.put(retPath, ret, baseTime)
			}
			before := len(h.rclone.paths(""))
			h.advance(time1h)
			h.mustSync(jobs.Params{})
			if n := len(h.callsOf(proc.Rclone, "delete")) + len(h.callsOf(proc.Rclone, "deletefile")) + len(h.callsOf(proc.Rclone, "purge")); n != 0 {
				t.Errorf("%d deleting commands ran", n)
			}
			if after := len(h.rclone.paths("")); after < before {
				t.Errorf("objects went from %d to %d", before, after)
			}
			got := h.retained()
			if len(got) != c.wantRet {
				t.Errorf("%d retained rows, want %d: %+v", len(got), c.wantRet, got)
			}
			for _, r := range got {
				if _, ok := h.rclone.get(r.RetainedPath); !ok {
					t.Errorf("retained row %s has no object", r.RetainedPath)
				}
			}
			for _, r := range h.records() {
				if r.State.Live() && r.RetainedPath != "" {
					t.Errorf("an intent is left: %+v", r)
				}
			}
			if c.wantState == syncer.StatePresent {
				r := h.mustLive(h.dp("a.mkv"))
				fi, _ := os.Stat(h.srcPath("a.mkv"))
				if r.State != syncer.StatePresent || r.Size != fi.Size() || r.MtimeNs != fi.ModTime().UnixNano() {
					t.Errorf("live record %+v, want present with the source's version", r)
				}
			} else if _, ok := h.live(h.dp("a.mkv")); ok {
				t.Error("the retained file is still live")
			}
		})
	}
}
