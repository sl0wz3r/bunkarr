package restic

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

var restarts int

// restartProcess makes the children files look as they do to the next Bunkarr process: another
// process id, and nothing read yet.
func restartProcess(t *testing.T) {
	t.Helper()
	prev := thisProcess
	restarts++
	thisProcess = "restarted-" + strconv.Itoa(restarts)
	childFiles.Lock()
	clear(childFiles.byPath)
	childFiles.Unlock()
	t.Cleanup(func() {
		thisProcess = prev
		childFiles.Lock()
		clear(childFiles.byPath)
		childFiles.Unlock()
	})
}

func readChildren(t *testing.T, path string) []childRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recs []childRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		t.Fatal(err)
	}
	return recs
}

func fakeChildren(t *testing.T, pids ...int) {
	t.Helper()
	prev := findChildren
	findChildren = func() []int { return pids }
	t.Cleanup(func() {
		findChildren = prev
		ownChildren.Lock()
		for _, pid := range pids {
			delete(ownChildren.spans, pid)
		}
		ownChildren.Unlock()
	})
}

// TestGuardedUnlockEarlierProcessLeftover is acceptance 3's kill and resume (§14.6 item 3):
// docker kill -s KILL during a restic backup leaves its lock (this host name, the killed child's
// PID, refreshed a minute before the kill), and after docker start the verify job's GuardedUnlock
// must unlock at once rather than wait until the lock is LiveLockAge old (the Docker test allows
// 15 minutes), because the children file names that PID as a child of the process before this
// one. The unlock endpoint unlocks too. Another PID, a time outside the child's run, and a time
// after this process's start keep §6.7's rules.
func TestGuardedUnlockEarlierProcessLeftover(t *testing.T) {
	childStart := time.Date(2026, 9, 27, 23, 5, 0, 0, time.UTC)
	processStart := time.Date(2026, 9, 27, 23, 12, 0, 0, time.UTC)
	refreshed := time.Date(2026, 9, 27, 23, 11, 16, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		job      int64
		pid      int
		at       time.Time
		wantErr  string
		unlocked bool
		waited   bool
	}{
		{"verify job after the restart", 6, 4744, refreshed, "", true, false},
		{"the unlock endpoint after the restart", 0, 4744, refreshed, "", true, false},
		{"a PID the earlier process did not start", 6, 4745, refreshed, "", true, true},
		{"the endpoint and a PID the earlier process did not start", 0, 4745, refreshed, "counts as stale from", false, false},
		{"a time before the child's run", 0, 4744, childStart.Add(-time.Minute), "counts as stale from", false, false},
		{"a time after this process started", 6, 4744, processStart.Add(30 * time.Second), "another restic process with host name bunkarr-e2e-offsite", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restartProcess(t)
			d, f := newTestDriver(t)
			b, _ := json.Marshal([]childRecord{
				{Process: "killed", PID: 4744, Start: childStart},
				{Process: "killed", PID: 4700, Start: childStart.Add(-time.Hour), End: childStart.Add(-50 * time.Minute)},
			})
			if err := os.WriteFile(filepath.Join(d.CacheRoot, childrenFileName), b, 0o600); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 27, 23, 13, 18, 0, time.UTC)
			waited := false
			d.sleep = func(_ context.Context, dur time.Duration) error {
				waited = true
				now = now.Add(dur)
				return nil
			}
			dest, sec := s3Repo()
			r, err := d.Connect(dest, sec, engines.Runtime{JobID: tc.job, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			f.Handle(proc.Restic, "list", func(*enginetest.Call) enginetest.Script {
				return enginetest.Script{Stdout: []string{id(1)}}
			})
			f.Handle(proc.Restic, "cat", func(*enginetest.Call) enginetest.Script {
				return enginetest.Script{Stdout: []string{`{"time":"` + tc.at.Format(time.RFC3339Nano) +
					`","exclusive":false,"hostname":"bunkarr-e2e-offsite","username":"root","pid":` + strconv.Itoa(tc.pid) + `}`}}
			})
			unlocks := 0
			f.Handle(proc.Restic, "unlock", func(*enginetest.Call) enginetest.Script { unlocks++; return enginetest.Script{} })
			err = r.GuardedUnlock(context.Background(), "bunkarr-e2e-offsite", processStart, func(int) bool { return false })
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (!errors.Is(err, ErrForeignLock) || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("GuardedUnlock = %v, want %q", err, tc.wantErr)
			}
			if (unlocks == 1) != tc.unlocked || waited != tc.waited {
				t.Fatalf("unlock ran %d times, waited %v; want unlocked %v, waited %v", unlocks, waited, tc.unlocked, tc.waited)
			}
		})
	}
}

// TestChildrenFileAcrossRestart: a child of a command that may take a lock is written to the
// children file when it is found (before its end), and its end when the command ends; a child
// whose process was killed keeps no end. The next process reads them as an earlier process's
// children: a killed child's run lasts until the next process started, an ended child's until
// its end. --no-lock commands are not written.
func TestChildrenFileAcrossRestart(t *testing.T) {
	restartProcess(t)
	fakeChildren(t, 4744)
	d, f := newTestDriver(t)
	file := d.childrenPath()

	// A --no-lock command (restic list locks) writes nothing.
	f.Expect(proc.Restic, enginetest.Prefix("list", "locks", "--no-lock"), enginetest.Script{})
	dest, sec := s3Repo()
	if _, err := connect(t, d, dest, sec).ListLocks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a --no-lock command wrote the children file: %v", err)
	}

	// A command that may take a lock (restic unlock here) is written with its end.
	f.Expect(proc.Restic, enginetest.Prefix("unlock"), enginetest.Script{})
	before := time.Now()
	if err := connect(t, d, dest, sec).Unlock(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	recs := readChildren(t, file)
	if len(recs) != 1 || recs[0].PID != 4744 || recs[0].Start.Before(before.Add(-time.Second)) || recs[0].End.IsZero() {
		t.Fatalf("children file after an ended command: %+v", recs)
	}
	ended := recs[0]

	// A child of a killed process: found and written, but its run never ends.
	done := make(chan struct{})
	killedStart := time.Now().Add(time.Minute) // after the ended run, so the two runs do not overlap
	var errs []error
	end := trackChildren(killedStart, done, file, func(err error) { errs = append(errs, err) })
	deadline := time.Now().Add(5 * time.Second)
	for len(readChildren(t, file)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the running child was not written before its command ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(done)
	_ = end // the kill: the end is never recorded
	if recs := readChildren(t, file); len(recs) != 2 || !recs[1].End.IsZero() || len(errs) > 0 {
		t.Fatalf("children file with a running child: %+v, %v", recs, errs)
	}

	restartProcess(t)
	processStart := killedStart.Add(10 * time.Minute)
	for _, tc := range []struct {
		name string
		pid  int
		at   time.Time
		want bool
	}{
		{"the killed child's lock", 4744, killedStart.Add(5 * time.Minute), true},
		{"the killed child's lock, refreshed just before the restart", 4744, processStart, true},
		{"the ended child's lock", 4744, ended.Start.Add(time.Millisecond), true},
		{"between the two runs", 4744, ended.End.Add(30 * time.Second), false},
		{"after this process started", 4744, processStart.Add(time.Second), false},
		{"another PID", 4745, killedStart.Add(5 * time.Minute), false},
	} {
		if got := leftByEarlierProcess(file, tc.pid, tc.at, processStart); got != tc.want {
			t.Errorf("%s: leftByEarlierProcess = %v, want %v", tc.name, got, tc.want)
		}
	}
	if leftByEarlierProcess("", 4744, killedStart.Add(5*time.Minute), processStart) {
		t.Error("no children file named a child")
	}

	// The next write of this process keeps the earlier records (with an end now: they were gone by
	// the time this process read them) until they are LiveLockAge past, and drops them then.
	if err := noteChildStart(file, 900, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	recs = readChildren(t, file)
	if len(recs) != 3 || recs[1].End.IsZero() || recs[2].Process != thisProcess {
		t.Fatalf("children file after a write of the next process: %+v", recs)
	}
	later := time.Now().Add(3 * LiveLockAge)
	if err := noteChildEnd(file, 900, recs[2].Start, later); err != nil {
		t.Fatal(err)
	}
	if recs := readChildren(t, file); len(recs) != 1 || recs[0].PID != 900 {
		t.Fatalf("children file after LiveLockAge: %+v", recs)
	}
}
