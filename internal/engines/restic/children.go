package restic

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// The restic processes this Bunkarr process started, for GuardedUnlock (§6.7). An interrupt
// signals a command's whole process group, so restic's rclone backend (`rclone serve restic`)
// dies with restic, and restic cannot remove its lock from the repository (make test-engines:
// the lock of a SIGINT'd backup stays). Such a lock has this container's host name, is newer
// than this process's start, and its PID is gone: exactly what §6.7 refuses as "another restic
// process with this host name". GuardedUnlock therefore also accepts a lock whose PID was one of
// this process's restic children and whose time lies within that child's run: a lock only that
// child can have left. The children are found through /proc while each command runs (Linux;
// elsewhere none are found, and §6.7's rule applies unchanged).
//
// Across restarts. A kill of the container or of Bunkarr during a restic command (docker kill, an
// OOM kill, a power loss) leaves that command's lock behind, and the restarted process (the same
// host name) could not tell it from a live lock of another container with this host name that
// was refreshed shortly before it started: GuardedUnlock would wait until the lock is LiveLockAge
// old. Each child of a command that may take a lock (one without --no-lock) is therefore also
// written to CacheRoot/children.json (<config>/cache/restic): its PID, the start of its run and,
// once it has ended, the end. The record is written as soon as the child is found, before restic
// can take its lock (it opens the repository and derives the key first). A later process on the
// same config directory reads the records of the processes before it once. Those processes are
// gone: <config>/bunkarr.lock admits one process per config directory. A lock with such a
// child's PID and a time within its run (a run without an end, the child of a killed process:
// until this process started) can only be that child's, and GuardedUnlock treats it like a lock of
// an ended child of this process. A lost or unreadable file only brings the wait back.

// childSpan is one restic child: from just before its command started to just after it ended
// (zero while it runs).
type childSpan struct {
	start, end time.Time
}

var ownChildren = struct {
	sync.Mutex
	spans map[int][]childSpan
}{spans: map[int][]childSpan{}}

// maxChildSpans bounds the history kept per PID.
const maxChildSpans = 16

// childSlack widens a child's run for comparisons with lock times (restic writes the lock right
// after it starts; clocks are the same host's).
const childSlack = 2 * time.Second

// findChildren lists this process's restic children (resticChildren; tests replace it).
var findChildren = resticChildren

// trackChildren watches for restic children of this process while one command runs, from start
// until done is closed, and records each one found with the command's run, in memory and, when
// file is set, in that children file (errors go to onErr). It returns a function that marks the
// end of the run.
func trackChildren(start time.Time, done <-chan struct{}, file string, onErr func(error)) func(end time.Time) {
	var mu sync.Mutex
	found := map[int]bool{}
	finished := make(chan struct{})
	report := func(err error) {
		if err != nil && onErr != nil {
			onErr(err)
		}
	}
	go func() {
		defer close(finished)
		interval := 20 * time.Millisecond
		deadline := time.Now().Add(2 * time.Second)
		for {
			for _, pid := range findChildren() {
				mu.Lock()
				if !found[pid] {
					found[pid] = true
					ownChildren.Lock()
					spans := append(ownChildren.spans[pid], childSpan{start: start})
					if len(spans) > maxChildSpans {
						spans = spans[len(spans)-maxChildSpans:]
					}
					ownChildren.spans[pid] = spans
					ownChildren.Unlock()
					if file != "" {
						report(noteChildStart(file, pid, start, time.Now()))
					}
				}
				mu.Unlock()
			}
			if time.Now().After(deadline) {
				interval = time.Second
			}
			select {
			case <-done:
				return
			case <-time.After(interval):
			}
		}
	}()
	return func(end time.Time) {
		<-finished
		mu.Lock()
		defer mu.Unlock()
		ownChildren.Lock()
		for pid := range found {
			spans := ownChildren.spans[pid]
			for i := range spans {
				if spans[i].start.Equal(start) && spans[i].end.IsZero() {
					spans[i].end = end
				}
			}
		}
		ownChildren.Unlock()
		if file != "" {
			for pid := range found {
				report(noteChildEnd(file, pid, start, end))
			}
		}
	}
}

// leftByOwnChild reports whether a lock with pid and time can only have been left by a restic
// child of this process that has ended: the PID was such a child, and the lock's time lies within
// its run.
func leftByOwnChild(pid int, at time.Time) bool {
	ownChildren.Lock()
	defer ownChildren.Unlock()
	for _, s := range ownChildren.spans[pid] {
		if s.end.IsZero() {
			continue
		}
		if !at.Before(s.start.Add(-childSlack)) && !at.After(s.end.Add(childSlack)) {
			return true
		}
	}
	return false
}

// childrenFileName is the children file's name in CacheRoot.
const childrenFileName = "children.json"

// maxChildRecords bounds this process's records in the children file (the oldest ended go first).
const maxChildRecords = 256

// childRecord is one restic child in the children file.
type childRecord struct {
	// Process tells the records of this process from those of earlier ones.
	Process string    `json:"process"`
	PID     int       `json:"pid"`
	Start   time.Time `json:"start"`
	// End is zero while the child runs, and stays zero when its process was killed.
	End time.Time `json:"end,omitzero"`
}

// thisProcess is this process's Process in the children file.
var thisProcess = func() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

// childFile is one children file as this process knows it.
type childFile struct {
	// earlier are the records of earlier processes, read once; those processes are gone.
	earlier []childRecord
	// own are this process's records.
	own []childRecord
	// loadedAt is when earlier was read: an earlier child without an end had ended by then.
	loadedAt time.Time
}

var childFiles = struct {
	sync.Mutex
	byPath map[string]*childFile
}{byPath: map[string]*childFile{}}

// childFileLocked returns what this process knows of path, reading the file the first time
// (childFiles is locked). A missing or unreadable file has no earlier records.
func childFileLocked(path string, now time.Time) *childFile {
	if f := childFiles.byPath[path]; f != nil {
		return f
	}
	f := &childFile{loadedAt: now}
	if b, err := os.ReadFile(path); err == nil {
		var recs []childRecord
		if json.Unmarshal(b, &recs) == nil {
			for _, rec := range recs {
				if rec.Process != thisProcess && rec.PID > 0 && !rec.Start.IsZero() {
					f.earlier = append(f.earlier, rec)
				}
			}
		}
	}
	childFiles.byPath[path] = f
	return f
}

// noteChildStart records in path that a child with pid started its run at start.
func noteChildStart(path string, pid int, start, now time.Time) error {
	childFiles.Lock()
	defer childFiles.Unlock()
	f := childFileLocked(path, now)
	f.own = append(f.own, childRecord{Process: thisProcess, PID: pid, Start: start})
	return f.saveLocked(path, now)
}

// noteChildEnd records in path that the run of the child with pid that started at start ended.
func noteChildEnd(path string, pid int, start, end time.Time) error {
	childFiles.Lock()
	defer childFiles.Unlock()
	f := childFileLocked(path, end)
	for i := range f.own {
		if r := &f.own[i]; r.PID == pid && r.Start.Equal(start) && r.End.IsZero() {
			r.End = end
		}
	}
	return f.saveLocked(path, end)
}

// saveLocked writes f to path (replacing it atomically). A record whose run ended more than
// LiveLockAge ago is dropped: a lock it could explain is left to restic's own rules by then. An
// earlier record without an end is written with loadedAt as its end, so the next process has a
// bound too.
func (f *childFile) saveLocked(path string, now time.Time) error {
	horizon := now.Add(-(LiveLockAge + childSlack))
	f.earlier = slices.DeleteFunc(f.earlier, func(r childRecord) bool {
		end := r.End
		if end.IsZero() {
			end = f.loadedAt
		}
		return end.Before(horizon)
	})
	f.own = slices.DeleteFunc(f.own, func(r childRecord) bool { return !r.End.IsZero() && r.End.Before(horizon) })
	for len(f.own) > maxChildRecords {
		i := slices.IndexFunc(f.own, func(r childRecord) bool { return !r.End.IsZero() })
		if i < 0 {
			i = 0
		}
		f.own = slices.Delete(f.own, i, i+1)
	}
	recs := make([]childRecord, 0, len(f.earlier)+len(f.own))
	for _, r := range f.earlier {
		if r.End.IsZero() {
			r.End = f.loadedAt
		}
		recs = append(recs, r)
	}
	recs = append(recs, f.own...)
	b, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// writeFileAtomic replaces path with b (a synced temporary file renamed over it).
func writeFileAtomic(path string, b []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".children-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// leftByEarlierProcess reports whether a lock with pid and time at can only have been left by a
// restic child of an earlier Bunkarr process on this config directory (the children file at
// path): such a child had this PID and at lies within its run, which ended at the latest when
// this process started (that process was gone by then). A lock newer than processStart never
// counts.
func leftByEarlierProcess(path string, pid int, at, processStart time.Time) bool {
	if path == "" || at.After(processStart) {
		return false
	}
	childFiles.Lock()
	defer childFiles.Unlock()
	f := childFileLocked(path, time.Now())
	for _, r := range f.earlier {
		if r.PID != pid {
			continue
		}
		end := r.End
		if end.IsZero() || end.After(processStart) {
			end = processStart
		}
		if !at.Before(r.Start.Add(-childSlack)) && !at.After(end.Add(childSlack)) {
			return true
		}
	}
	return false
}

// childrenPath is the children file of d (none without CacheRoot).
func (d *Driver) childrenPath() string {
	if d.CacheRoot == "" {
		return ""
	}
	return filepath.Join(d.CacheRoot, childrenFileName)
}
