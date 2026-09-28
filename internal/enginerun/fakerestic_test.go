package enginerun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
)

// fakeRestic is a stateful restic repository behind the FakeRunner (§14.2): snapshots with tags,
// paths and their content (read from the source tree at backup time), ls, forget by id, prune,
// check, restore, locks and unlock. A test changes what a backup sees through beforeBackup (a
// file that vanishes or changes after the scan) and unreadable (a file restic reports with an
// error line and leaves out, exit 3).
type fakeRestic struct {
	t  testing.TB
	h  *harness
	mu sync.Mutex
	id string

	snaps   []*fakeSnap
	counter int
	locks   map[string]restic.Lock
	// liveLocks are locks unlock does not remove (another live process).
	liveLocks map[string]bool

	beforeBackup func(c *enginetest.Call)
	// onData runs after a backup read its files, with the bytes it adds (a test advances its
	// clock there: the time the upload takes).
	onData     func(dataAdded int64)
	unreadable map[string]bool
	// hangBackup keeps backups running until they are interrupted (a window's end).
	hangBackup bool
	// hangNewOver keeps a backup running until it is interrupted when it backs up more than this
	// many new or changed files (a batch too large for the window); 0: never.
	hangNewOver int64
	// hangExclusive keeps these exclusive commands (check, prune) running until they are
	// interrupted; they leave their exclusive lock behind, with host name exclusiveHost (restic's
	// rclone backend stops with restic, §20.3).
	hangExclusive map[string]bool
	exclusiveHost string
	// checkErrors is num_errors of the next checks.
	checkErrors int
	// damaged are files (absolute paths) whose restored content is corrupted.
	damaged map[string]bool
	// unrestorable are files (absolute paths) whose data sits in a damaged pack: restore skips
	// them and ends "There were N errors" (exit 1), as restic does.
	unrestorable map[string]bool
	// restoreFails makes every restore fail as a repository restic cannot open (exit 1).
	restoreFails bool
	prunes       int
	// snapshotCalls counts the snapshots listings; onSnapshots runs before each (with its number).
	snapshotCalls int
	onSnapshots   func(n int)
}

type fakeSnap struct {
	ID      string
	Time    time.Time
	Tags    []string
	Paths   []string
	Parent  string
	files   map[string]fakeObj
	summary restic.Summary
}

func newFakeRestic(t testing.TB, h *harness) *fakeRestic {
	return &fakeRestic{t: t, h: h, id: strings.Repeat("ab", 32), locks: map[string]restic.Lock{}, liveLocks: map[string]bool{},
		unreadable: map[string]bool{}, damaged: map[string]bool{}, unrestorable: map[string]bool{}, hangExclusive: map[string]bool{},
		exclusiveHost: "bunkarr-test"}
}

func (f *fakeRestic) install(r *enginetest.FakeRunner) {
	for _, sub := range []string{"cat", "snapshots", "backup", "ls", "forget", "prune", "check", "restore", "list", "unlock", "dump"} {
		r.Handle(proc.Restic, sub, f.handle)
	}
}

func (f *fakeRestic) nextID() string {
	f.counter++
	sum := sha256.Sum256([]byte(fmt.Sprintf("snapshot-%d", f.counter)))
	return hex.EncodeToString(sum[:])
}

// addSnapshot adds a snapshot directly (another install, a retention table).
func (f *fakeRestic) addSnapshot(t time.Time, tags []string, files map[string]fakeObj) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID()
	f.snaps = append(f.snaps, &fakeSnap{ID: id, Time: t, Tags: tags, files: files})
	return id
}

func (f *fakeRestic) removeSnapshot(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeLocked(id)
}

func (f *fakeRestic) removeLocked(id string) {
	f.snaps = slices.DeleteFunc(f.snaps, func(s *fakeSnap) bool { return s.ID == id })
}

func (f *fakeRestic) snapshot(id string) *fakeSnap {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.snaps {
		if s.ID == id || strings.HasPrefix(s.ID, id) {
			return s
		}
	}
	return nil
}

func (f *fakeRestic) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.snaps {
		out = append(out, s.ID)
	}
	return out
}

var resticValueFlags = map[string]bool{"--host": true, "--tag": true, "--files-from-raw": true, "--exclude-file": true,
	"--iexclude-file": true, "--exclude-if-present": true, "--parent": true, "--limit-upload": true, "--limit-download": true,
	"--pack-size": true, "--retry-lock": true, "--time": true, "--max-unused": true, "--read-data-subset": true, "--target": true,
	"--include-file": true, "-o": true, "--repository-version": true}

func (f *fakeRestic) handle(c *enginetest.Call) enginetest.Script {
	if c.Subcommand() == "backup" && f.beforeBackup != nil {
		f.beforeBackup(c)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pos := positionals(c.Args, 1, resticValueFlags)
	if sub := c.Subcommand(); f.hangExclusive[sub] {
		f.counter++
		f.locks[fmt.Sprintf("%064x", 0xe0+f.counter)] = restic.Lock{Time: f.h.now(), Exclusive: true, Hostname: f.exclusiveHost, PID: 4242}
		return enginetest.Script{UntilInterrupted: true, InterruptExit: 130}
	}
	switch c.Subcommand() {
	case "cat":
		if len(pos) > 0 && pos[0] == "config" {
			return enginetest.Script{Stdout: []string{fmt.Sprintf(`{"version":2,"id":%q,"chunker_polynomial":"3d8e8c2e5e7a3b"}`, f.id)}}
		}
		l, ok := f.locks[pos[1]]
		if !ok {
			return enginetest.Script{Exit: 1}
		}
		b, _ := json.Marshal(l)
		return enginetest.Script{Stdout: []string{string(b)}}
	case "list":
		var out []string
		for id := range f.locks {
			out = append(out, id)
		}
		slices.Sort(out)
		return enginetest.Script{Stdout: out}
	case "unlock":
		for id := range f.locks {
			if c.Has("--remove-all") || !f.liveLocks[id] {
				delete(f.locks, id)
			}
		}
		return enginetest.Script{}
	case "snapshots":
		f.snapshotCalls++
		if f.onSnapshots != nil {
			f.onSnapshots(f.snapshotCalls)
		}
		return f.snapshots(c, pos)
	case "backup":
		return f.backup(c)
	case "ls":
		return f.ls(pos)
	case "forget":
		for _, id := range pos {
			f.snaps = slices.DeleteFunc(f.snaps, func(s *fakeSnap) bool { return s.ID == id })
		}
		return enginetest.Script{Stdout: []string{"[]"}}
	case "prune":
		f.prunes++
		return enginetest.Script{Stdout: []string{"repository contains 3 packs", "done"}}
	case "check":
		b, _ := json.Marshal(map[string]any{"message_type": "summary", "num_errors": f.checkErrors, "broken_packs": nil})
		exit := 0
		if f.checkErrors > 0 {
			exit = 1
		}
		return enginetest.Script{Stdout: []string{string(b)}, Exit: exit}
	case "restore":
		return f.restore(c, pos)
	}
	return enginetest.Script{Exit: 1}
}

func (f *fakeRestic) snapshots(c *enginetest.Call, ids []string) enginetest.Script {
	var want []string
	if v, ok := c.Flag("--tag"); ok {
		want = strings.Split(v, ",")
	}
	var list []map[string]any
	for _, s := range f.snaps {
		if len(ids) > 0 && !slices.ContainsFunc(ids, func(id string) bool { return strings.HasPrefix(s.ID, id) }) {
			continue
		}
		if !(restic.Snapshot{Tags: s.Tags}).HasTags(want...) {
			continue
		}
		m := map[string]any{"id": s.ID, "short_id": s.ID[:8], "time": s.Time.Format(time.RFC3339Nano), "tags": s.Tags, "paths": s.Paths,
			"hostname": "bunkarr", "tree": s.ID}
		if s.summary.SnapshotID != "" {
			m["summary"] = s.summary
		}
		list = append(list, m)
	}
	if list == nil {
		return enginetest.Script{Stdout: []string{"[]"}}
	}
	b, _ := json.Marshal(list)
	return enginetest.Script{Stdout: []string{string(b)}}
}

// backup walks the include list (whole directories recursively) and stores what it finds.
func (f *fakeRestic) backup(c *enginetest.Call) enginetest.Script {
	for _, l := range f.locks {
		if l.Exclusive {
			return enginetest.Script{Stderr: []string{`{"message_type":"exit_error","code":11,"message":"repository is already locked exclusively by PID 4242 on nas by root (UID 0, GID 0)\nthe ` + "`unlock`" + ` command can be used to remove stale locks"}`}, Exit: 11}
		}
	}
	if f.hangBackup {
		return enginetest.Script{UntilInterrupted: true, InterruptExit: 130}
	}
	listPath, _ := c.Flag("--files-from-raw")
	raw, err := os.ReadFile(listPath)
	if err != nil {
		f.t.Errorf("fake restic: no include list: %v", err)
		return enginetest.Script{Exit: 1}
	}
	marker, _ := c.Flag("--exclude-if-present")
	var includes []string
	for _, p := range strings.Split(string(raw), "\x00") {
		if p != "" {
			includes = append(includes, p)
		}
	}
	files := map[string]fakeObj{}
	var errLines []string
	add := func(p string, fi fs.FileInfo) {
		if f.unreadable[p] {
			b, _ := json.Marshal(map[string]any{"message_type": "error", "error": map[string]string{"message": "open " + p + ": permission denied"},
				"during": "archival", "item": p})
			errLines = append(errLines, string(b))
			return
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return
		}
		files[p] = fakeObj{data: data, mtime: fi.ModTime()}
	}
	for _, inc := range includes {
		fi, err := os.Lstat(inc)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if fi.Mode().IsRegular() {
				add(inc, fi)
			}
			continue
		}
		_ = filepath.WalkDir(inc, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if marker != "" {
					if _, err := os.Lstat(filepath.Join(p, marker)); err == nil {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
				add(p, fi)
			}
			return nil
		})
	}
	parent, _ := c.Flag("--parent")
	var prev map[string]fakeObj
	for _, s := range f.snaps {
		if s.ID == parent {
			prev = s.files
		}
	}
	var sum restic.Summary
	for p, o := range files {
		old, ok := prev[p]
		switch {
		case !ok:
			sum.FilesNew++
			sum.DataAdded += int64(len(o.data))
		case string(old.data) != string(o.data) || !old.mtime.Equal(o.mtime):
			sum.FilesChanged++
			sum.DataAdded += int64(len(o.data))
		default:
			sum.FilesUnmodified++
		}
		sum.TotalFilesProcessed++
		sum.TotalBytesProcessed += int64(len(o.data))
	}
	sum.DataAddedPacked = sum.DataAdded
	if f.onData != nil {
		f.onData(sum.DataAdded)
	}
	if f.hangNewOver > 0 && sum.FilesNew+sum.FilesChanged > f.hangNewOver {
		return enginetest.Script{UntilInterrupted: true, InterruptExit: 130}
	}
	id := f.nextID()
	sum.SnapshotID = id
	f.snaps = append(f.snaps, &fakeSnap{ID: id, Time: f.h.now(), Tags: c.FlagValues("--tag"), Paths: includes, Parent: parent, files: files,
		summary: sum})
	status, _ := json.Marshal(map[string]any{"message_type": "status", "percent_done": 1, "total_files": len(files), "files_done": len(files),
		"total_bytes": sum.TotalBytesProcessed, "bytes_done": sum.TotalBytesProcessed})
	line, _ := json.Marshal(struct {
		MessageType string `json:"message_type"`
		restic.Summary
	}{"summary", sum})
	exit := 0
	if len(errLines) > 0 {
		exit = 3
	}
	return enginetest.Script{Stdout: []string{string(status), string(line)}, Stderr: errLines, Exit: exit}
}

func (f *fakeRestic) ls(pos []string) enginetest.Script {
	var s *fakeSnap
	for _, x := range f.snaps {
		if x.ID == pos[0] {
			s = x
		}
	}
	if s == nil {
		return enginetest.Script{Stderr: []string{`{"message_type":"exit_error","code":1,"message":"no matching ID found"}`}, Exit: 1}
	}
	head, _ := json.Marshal(map[string]any{"message_type": "snapshot", "id": s.ID, "time": s.Time, "paths": s.Paths})
	out := []string{string(head)}
	var names []string
	for p := range s.files {
		names = append(names, p)
	}
	slices.Sort(names)
	for _, p := range names {
		o := s.files[p]
		b, _ := json.Marshal(map[string]any{"message_type": "node", "name": path.Base(p), "type": "file", "path": p, "size": len(o.data),
			"mtime": o.mtime.Format(time.RFC3339Nano)})
		out = append(out, string(b))
	}
	return enginetest.Script{Stdout: out}
}

func (f *fakeRestic) restore(c *enginetest.Call, pos []string) enginetest.Script {
	id, sub, _ := strings.Cut(pos[0], ":")
	var s *fakeSnap
	for _, x := range f.snaps {
		if x.ID == id {
			s = x
		}
	}
	if s == nil {
		return enginetest.Script{Exit: 1}
	}
	if f.restoreFails {
		return enginetest.Script{Stderr: []string{`{"message_type":"exit_error","code":1,"message":"unable to open config file: Stat: connection refused"}`}, Exit: 1}
	}
	target, _ := c.Flag("--target")
	var errs []string
	var include []string
	if p, ok := c.Flag("--include-file"); ok {
		b, _ := os.ReadFile(p)
		for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
			include = append(include, strings.TrimPrefix(strings.ReplaceAll(l, `\`, ""), "/"))
		}
	}
	for p, o := range s.files {
		rel, ok := strings.CutPrefix(p, sub+"/")
		if !ok || (include != nil && !slices.Contains(include, rel)) {
			continue
		}
		data := o.data
		if f.damaged[p] {
			data = append(slices.Clone(data), 'X')
		}
		if f.unrestorable[p] {
			b, _ := json.Marshal(map[string]any{"message_type": "error", "error": map[string]string{"message": "load blob: ciphertext verification failed"},
				"during": "restore", "item": p})
			errs = append(errs, string(b))
			continue
		}
		dst := filepath.Join(target, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(dst), 0o700)
		_ = os.WriteFile(dst, data, 0o600)
	}
	if len(errs) > 0 {
		errs = append(errs, fmt.Sprintf(`{"message_type":"exit_error","code":1,"message":"There were %d errors\n"}`, len(errs)))
		return enginetest.Script{Stderr: errs, Exit: 1}
	}
	return enginetest.Script{}
}

// lastInclude returns a backup call's include list (recorded while the command ran).
func (f *fakeRestic) lastInclude(c *enginetest.Call) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.snaps {
		if slices.Equal(s.Tags, c.FlagValues("--tag")) {
			return []byte(strings.Join(s.Paths, "\x00"))
		}
	}
	return nil
}
