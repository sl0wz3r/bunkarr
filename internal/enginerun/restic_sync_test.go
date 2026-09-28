package enginerun

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

func newRestic(t *testing.T) *harness {
	return newHarness(t, harnessOptions{kind: engines.Restic})
}

// assertResticRefs checks D31's invariants: the path of every live record with engine_ref NULL
// is in its source's base (newest recorded snapshot) with its size and mtime, and every set
// reference (live or retained) names a snapshot that holds the version.
func (h *harness) assertResticRefs(t *testing.T) {
	t.Helper()
	rows, err := h.svc.snapshotRows(h.ctx, h.dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	bases := basesOf(rows)
	holds := func(snapID, abs string, size, mtimeNs int64) bool {
		s := h.restic.snapshot(snapID)
		if s == nil {
			return false
		}
		o, ok := s.files[abs]
		return ok && int64(len(o.data)) == size && mtimeHolds(o.mtime.UnixNano(), mtimeNs)
	}
	for _, r := range h.records() {
		abs := h.srcDir + "/" + r.SourceRelPath
		switch {
		case r.State == syncer.StateMissing:
		case r.State == syncer.StateRetained:
			if r.EngineRef == "" {
				t.Errorf("retained %s has no reference", r.RelPath)
			} else if !holds(r.EngineRef, r.RetainedPath, r.Size, r.MtimeNs) {
				t.Errorf("retained %s: snapshot %s does not hold %s (%d bytes)", r.RelPath, short(r.EngineRef), r.RetainedPath, r.Size)
			}
		case r.EngineRef == "":
			if !holds(bases[r.SourceID], abs, r.Size, r.MtimeNs) {
				t.Errorf("live %s (NULL ref): the base %s does not hold its version", r.RelPath, short(bases[r.SourceID]))
			}
		default:
			if !holds(r.EngineRef, abs, r.Size, r.MtimeNs) {
				t.Errorf("live %s: its reference %s does not hold its version", r.RelPath, short(r.EngineRef))
			}
		}
	}
}

// TestResticSyncLifecycle: an initial sync, then a change, an addition, a deletion and a rename
// (acceptance 3): one snapshot per batch, recorded through the read-back; the old version of the
// update is a replaced row and the deleted file's version stays referenced (S5); no unlock in a
// sync; a further sync makes no snapshot.
func TestResticSyncLifecycle(t *testing.T) {
	h := newRestic(t)
	h.writeSrc("a.mkv", content("a1", 3000), 1)
	h.writeSrc("b.mkv", content("b", 2000), 2)
	h.writeSrc("c.mkv", content("c", 1500), 3)
	h.writeSrc("d/e.srt", content("e", 100), 4)
	st, _ := h.mustSync(jobs.Params{})
	if st.FilesCopied != 4 || st.Batches != 1 || len(st.Snapshots) != 1 || st.Unchanged {
		t.Fatalf("initial sync stats: %+v\n%s", st, h.rep.dump())
	}
	s1 := st.Snapshots[0].SnapshotID
	backups := h.callsOf(proc.Restic, "backup")
	if len(backups) != 1 {
		t.Fatalf("%d backups", len(backups))
	}
	if list := strings.Split(strings.TrimSuffix(string(h.restic.lastInclude(backups[0])), "\x00"), "\x00"); len(list) != 1 || list[0] != h.srcDir {
		t.Errorf("include list without tier rules = %q, want the source root alone", list)
	}
	if backups[0].Has("--parent") {
		t.Error("the first backup has a parent")
	}
	if v, _ := backups[0].Flag("--exclude-if-present"); v != restic.KeyFile {
		t.Errorf("--exclude-if-present %q", v)
	}
	h.assertResticRefs(t)

	h.advance(time1h)
	h.writeSrc("a.mkv", content("a2", 3300), 20)
	h.writeSrc("new.mkv", content("n", 1300), 21)
	h.removeSrc("c.mkv")
	h.renameSrc("b.mkv", "moved/b.mkv")
	st, _ = h.mustSync(jobs.Params{})
	if st.FilesCopied != 1 || st.FilesUpdated != 1 || st.FilesMoved != 1 || st.FilesRetained != 1 || len(st.Snapshots) != 1 {
		t.Fatalf("second sync stats: %+v\n%s", st, h.rep.dump())
	}
	backups = h.callsOf(proc.Restic, "backup")
	if p, _ := backups[1].Flag("--parent"); p != s1 {
		t.Errorf("--parent %q, want the base %s", p, short(s1))
	}
	var replaced, deleted syncer.Record
	for _, r := range h.retained() {
		switch r.Reason {
		case syncer.ReasonReplaced:
			replaced = r
		case syncer.ReasonDeleted:
			deleted = r
		}
	}
	if replaced.EngineRef != s1 || replaced.SourceRelPath != "a.mkv" || replaced.Size != 3000 {
		t.Errorf("replaced row %+v, want a.mkv's old version in %s", replaced, short(s1))
	}
	if deleted.EngineRef != s1 || deleted.SourceRelPath != "c.mkv" {
		t.Errorf("deleted row %+v, want c.mkv in %s", deleted, short(s1))
	}
	h.assertResticRefs(t)
	if n := len(h.callsOf(proc.Restic, "unlock")); n != 0 {
		t.Errorf("a sync ran restic unlock %d times", n)
	}

	h.advance(time1h)
	st, _ = h.mustSync(jobs.Params{})
	if !st.Unchanged || len(st.Snapshots) != 0 || st.FilesPlanned != 0 {
		t.Errorf("a further sync: %+v", st)
	}
	if n := len(h.callsOf(proc.Restic, "backup")); n != 2 {
		t.Errorf("%d backups, want no new one", n)
	}
}

// TestResticSyncStaleExclusiveLock: an exclusive lock left behind by a check, prune or forget that
// was stopped (here by a container stop before this process started) refuses the backup with
// exit 11; the sync removes it with the guarded unlock and backs up (§6.7, §20.3).
func TestResticSyncStaleExclusiveLock(t *testing.T) {
	h := newRestic(t)
	h.writeSrc("a.mkv", content("a", 1000), 1)
	h.restic.locks[fmt.Sprintf("%064x", 0xdead)] = restic.Lock{Time: h.now().Add(-2 * time.Hour), Exclusive: true, Hostname: "bunkarr-test",
		PID: 4242}
	st, _ := h.mustSync(jobs.Params{})
	if st.FilesCopied != 1 || len(st.Snapshots) != 1 {
		t.Errorf("stats %+v", st)
	}
	if len(h.restic.locks) != 0 || len(h.callsOf(proc.Restic, "unlock")) != 1 {
		t.Errorf("locks %v, unlocks %d", h.restic.locks, len(h.callsOf(proc.Restic, "unlock")))
	}
	// A lock the guard refuses (another container with this host name) fails the sync.
	h.writeSrc("b.mkv", content("b", 1000), 2)
	h.restic.locks[fmt.Sprintf("%064x", 0xbeef)] = restic.Lock{Time: h.now(), Exclusive: true, Hostname: "bunkarr-test", PID: 4343}
	h.advance(time1h)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	_, err := h.runJob(j)
	h.closeJob(j.ID)
	if !errors.Is(err, engines.ErrLocked) || !strings.Contains(err.Error(), "another restic process") {
		t.Errorf("err = %v", err)
	}
}

// TestResticThroughputSample: the upload rate is measured on batches that upload new data, not on
// batches of renamed files (restic reads them in full and uploads nothing), which would understate
// the uplink and fail every sizeable file as larger than the window (§9.2).
func TestResticThroughputSample(t *testing.T) {
	h := newRestic(t)
	h.restic.beforeBackup = func(*enginetest.Call) { h.advance(100 * time.Second) }
	h.writeSrc("in/a.mkv", content("a", 4<<20), 1)
	h.mustSync(jobs.Params{})
	st, err := h.svc.State(h.ctx, h.dest.ID)
	if err != nil || st.ThroughputBps == nil {
		t.Fatalf("no throughput after an upload: %+v %v", st, err)
	}
	measured := *st.ThroughputBps
	h.renameSrc("in/a.mkv", "moved/a.mkv")
	h.advance(time1h)
	h.mustSync(jobs.Params{AllowChanges: true})
	if st, _ := h.svc.State(h.ctx, h.dest.ID); st.ThroughputBps == nil || *st.ThroughputBps != measured {
		t.Errorf("a batch of renamed files changed the throughput from %.0f to %v", measured, st.ThroughputBps)
	}
}

// TestResticProgressTotals: restic's progress takes its totals from the status lines as its done
// counts (every file restic processes, §10.4), so done never exceeds the total on an incremental
// sync.
func TestResticProgressTotals(t *testing.T) {
	h := newRestic(t)
	h.writeSrc("a.mkv", content("a", 5000), 1)
	h.writeSrc("b.mkv", content("b", 6000), 2)
	h.mustSync(jobs.Params{})
	h.writeSrc("c.mkv", content("c", 100), 3)
	h.advance(time1h)
	h.rep.mu.Lock()
	h.rep.progress = nil
	h.rep.mu.Unlock()
	h.mustSync(jobs.Params{})
	backingUp := 0
	for _, p := range h.rep.progress {
		if p.Phase != "backing-up" {
			continue
		}
		backingUp++
		if p.BytesDone > p.BytesTotal || p.FilesDone > p.FilesTotal {
			t.Fatalf("progress done %d/%d bytes, %d/%d files", p.BytesDone, p.BytesTotal, p.FilesDone, p.FilesTotal)
		}
	}
	if backingUp == 0 {
		t.Error("no backing-up progress")
	}
}

// TestResticBaseAfterClockJump: a source's base is the snapshot recorded last, whatever restic's
// snapshot time says: after the host clock was set back between two syncs, the second sync's
// snapshot is the base (its records are recorded in it) and the retention keeps it (§6.2, D31).
func TestResticBaseAfterClockJump(t *testing.T) {
	h := newHarness(t, harnessOptions{kind: engines.Restic, retention: &destinations.Retention{DeletedDays: 30,
		SnapshotDaily: intp(0), SnapshotWeekly: intp(0), SnapshotMonthly: intp(0), SnapshotYearly: intp(0)}})
	h.writeSrc("a.mkv", content("a1", 1000), 1)
	h.writeSrc("b.mkv", content("b", 1000), 2)
	h.mustSync(jobs.Params{})
	h.advance(-3 * time1h) // the clock is corrected backwards
	h.writeSrc("a.mkv", content("a2", 1100), 3)
	st, _ := h.mustSync(jobs.Params{})
	if len(st.Snapshots) != 1 {
		t.Fatalf("snapshots %+v", st.Snapshots)
	}
	second := st.Snapshots[0].SnapshotID
	if base, _ := h.svc.baseOf(h.ctx, h.dest.ID, h.src.ID); base != second {
		t.Errorf("base %s, want the last recorded %s", short(base), short(second))
	}
	h.assertResticRefs(t)
	h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	if h.restic.snapshot(second) == nil {
		t.Error("the retention forgot the source's last recorded snapshot")
	}
}
