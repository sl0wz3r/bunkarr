package enginerun

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// fakeRclone is an in-memory rclone object store behind the FakeRunner (§14.2): objects by path
// relative to the destination root, copy with --backup-dir (the replaced object moves into the
// backup dir first, counted against --max-delete), server-side moves as a copy then a delete
// (undoLastDelete leaves both, as a crash between them does on S3), strict crypt (only the
// command's own root exists), listings, check --combined, cat, rcat, delete and purge.
type fakeRclone struct {
	t    testing.TB
	h    *harness
	mu   sync.Mutex
	root string
	objs map[string]fakeObj

	// copyErr fails the upload of a file (relative to the copy's destination) with an ERROR line.
	copyErr map[string]string
	// cutoffAfter stops copy batches after this many uploads with exit 10 (a window cutoff); 0: never.
	cutoffAfter int
	// cutAfterBackup stops a copy with exit 10 right after it moved this file's old object into the
	// backup dir, before the new one arrived (a window cutoff in the middle of an update).
	cutAfterBackup map[string]bool
	// beforeCopy runs before a copy uploads (a test changes the source there).
	beforeCopy func(files []string)
	// lastDeletes are the objects the last move or moveto deleted (undoLastDelete restores them).
	lastDeletes map[string]fakeObj
	cleanups    []string
	copyCalls   []copyCall
	deleteLists [][]string
}

// copyCall is one copy command the fake answered.
type copyCall struct {
	files     []string
	maxDelete int
	backupDir string
}

func (f *fakeRclone) copies() []copyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.copyCalls)
}

type fakeObj struct {
	data  []byte
	mtime time.Time
}

func newFakeRclone(t testing.TB, h *harness, root string) *fakeRclone {
	return &fakeRclone{t: t, h: h, root: root, objs: map[string]fakeObj{}, copyErr: map[string]string{}, cutAfterBackup: map[string]bool{}}
}

func (f *fakeRclone) put(rel string, data []byte, mtime time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[rel] = fakeObj{data: slices.Clone(data), mtime: mtime}
}

func (f *fakeRclone) get(rel string) (fakeObj, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs[rel]
	return o, ok
}

func (f *fakeRclone) remove(rel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objs, rel)
}

// paths returns every object path under prefix ("" = all), sorted.
func (f *fakeRclone) paths(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p := range f.objs {
		if prefix == "" || strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// undoLastDelete restores the objects the last move deleted: a server-side move that copied and
// did not delete (the crash point rclone.afterMove).
func (f *fakeRclone) undoLastDelete() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for p, o := range f.lastDeletes {
		f.objs[p] = o
	}
	f.lastDeletes = nil
}

func (f *fakeRclone) install(r *enginetest.FakeRunner) {
	for _, sub := range []string{"lsjson", "copy", "copyto", "moveto", "move", "delete", "deletefile", "purge", "rmdirs", "cat", "rcat",
		"check", "backend cleanup", "lsf", "about"} {
		r.Handle(proc.Rclone, sub, f.handle)
	}
}

// rcloneValueFlags are the rclone flags that take a value.
var rcloneValueFlags = map[string]bool{"--files-from-raw": true, "--backup-dir": true, "--max-delete": true, "--stats": true,
	"--stats-log-level": true, "--transfers": true, "--checkers": true, "--max-duration": true, "--cutoff-mode": true, "--count": true,
	"--size": true, "--combined": true, "--max-depth": true, "-o": true, "--ca-cert": true, "--exclude": true}

// positionals returns a command's positional arguments after its subcommand words.
func positionals(args []string, words int, valueFlags map[string]bool) []string {
	var out []string
	for i := words; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if valueFlags[a] {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

// rel maps a remote path of the command to a root-relative path (ok false: not on the root).
func (f *fakeRclone) rel(remote string) (string, bool) {
	if !strings.HasPrefix(remote, f.root) {
		return "", false
	}
	r := strings.TrimPrefix(strings.TrimPrefix(remote, f.root), "/")
	return r, true
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func logLine(level, msg, object string) string {
	m := map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "level": level, "msg": msg, "source": "fake"}
	if object != "" {
		m["object"], m["objectType"] = object, "*s3.Object"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// statsLine is rclone's stats line. Like rclone, bytes counts the server-side copies (serverSide,
// the old versions --backup-dir moved on S3) as well as the uploads.
func statsLine(bytes, serverSide, transfers, deletes int64, transferring []string) string {
	st := map[string]any{"bytes": bytes, "serverSideCopyBytes": serverSide, "transfers": transfers, "deletes": deletes}
	if len(transferring) > 0 {
		var tr []map[string]any
		for _, n := range transferring {
			tr = append(tr, map[string]any{"name": n})
		}
		st["transferring"] = tr
	}
	b, _ := json.Marshal(map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "level": "notice", "msg": "stats", "stats": st})
	return string(b)
}

func readList(c *enginetest.Call) []string {
	p, ok := c.Flag("--files-from-raw")
	if !ok {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (f *fakeRclone) handle(c *enginetest.Call) enginetest.Script {
	sub := c.Subcommand()
	words := 1
	if sub == "backend cleanup" {
		words = 2
	}
	pos := positionals(c.Args, words, rcloneValueFlags)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch sub {
	case "lsjson":
		return f.lsjson(c, pos)
	case "copy":
		return f.copy(c, pos)
	case "copyto":
		return f.copyto(pos)
	case "moveto":
		return f.moveto(pos)
	case "move":
		return f.move(c, pos)
	case "delete":
		return f.deleteList(c, pos)
	case "deletefile":
		r, _ := f.rel(pos[0])
		delete(f.objs, r)
		return enginetest.Script{}
	case "purge":
		r, _ := f.rel(pos[0])
		for p := range f.objs {
			if p == r || strings.HasPrefix(p, r+"/") {
				delete(f.objs, p)
			}
		}
		return enginetest.Script{}
	case "rmdirs", "about":
		if sub == "about" {
			return enginetest.Script{Stdout: []string{`{"free": 1000000000}`}}
		}
		return enginetest.Script{}
	case "cat":
		r, _ := f.rel(pos[0])
		o, ok := f.objs[r]
		if !ok {
			return enginetest.Script{Stderr: []string{logLine("error", "object not found", r)}, Exit: 3}
		}
		return enginetest.Script{Stdout: strings.Split(strings.TrimSuffix(string(o.data), "\n"), "\n")}
	case "rcat":
		r, _ := f.rel(pos[0])
		f.objs[r] = fakeObj{data: c.Stdin, mtime: f.h.now()}
		return enginetest.Script{}
	case "check":
		return f.check(c, pos)
	case "backend cleanup":
		f.cleanups = append(f.cleanups, strings.Join(c.Args, " "))
		return enginetest.Script{}
	case "lsf":
		return enginetest.Script{}
	}
	return enginetest.Script{Exit: 2}
}

func (f *fakeRclone) lsjson(c *enginetest.Call, pos []string) enginetest.Script {
	dir, ok := f.rel(pos[0])
	if !ok {
		return enginetest.Script{Exit: 3}
	}
	var names []string
	if list := readList(c); list != nil {
		for _, n := range list {
			if _, ok := f.objs[joinRel(dir, n)]; ok {
				names = append(names, n)
			}
		}
	} else {
		prefix := dir
		if prefix != "" {
			prefix += "/"
		}
		for p := range f.objs {
			if strings.HasPrefix(p, prefix) {
				n := strings.TrimPrefix(p, prefix)
				if c.Has("-R") || !strings.Contains(n, "/") {
					names = append(names, n)
				}
			}
		}
	}
	slices.Sort(names)
	out := []string{"["}
	for i, n := range names {
		o := f.objs[joinRel(dir, n)]
		b, _ := json.Marshal(map[string]any{"Path": n, "Name": path.Base(n), "Size": len(o.data), "ModTime": o.mtime.Format(time.RFC3339Nano),
			"IsDir": false})
		line := string(b)
		if i < len(names)-1 {
			line += ","
		}
		out = append(out, line)
	}
	out = append(out, "]")
	return enginetest.Script{Stdout: out}
}

func (f *fakeRclone) copy(c *enginetest.Call, pos []string) enginetest.Script {
	srcRoot := pos[0]
	dst, _ := f.rel(pos[1])
	backup := ""
	if b, ok := c.Flag("--backup-dir"); ok {
		backup, _ = f.rel(b)
	}
	maxDelete := -1
	if v, ok := c.Flag("--max-delete"); ok {
		maxDelete, _ = strconv.Atoi(v)
	}
	files := readList(c)
	f.copyCalls = append(f.copyCalls, copyCall{files: files, maxDelete: maxDelete, backupDir: backup})
	if f.beforeCopy != nil {
		f.mu.Unlock()
		f.beforeCopy(files)
		f.mu.Lock()
	}
	var lines []string
	var bytes, ssc, transfers, deletes int64
	errs, exit := 0, 0
	for i, name := range files {
		if f.cutoffAfter > 0 && int(transfers) >= f.cutoffAfter {
			lines = append(lines, statsLine(bytes, ssc, transfers, deletes, files[i:i+1]),
				logLine("error", "Max duration reached - stopping soft", ""))
			return enginetest.Script{Stderr: lines, Exit: 10}
		}
		if msg, bad := f.copyErr[name]; bad {
			lines = append(lines, logLine("error", "Failed to copy: "+msg, name))
			errs++
			continue
		}
		p := filepath.Join(srcRoot, filepath.FromSlash(name))
		fi, err := os.Lstat(p)
		if err != nil {
			lines = append(lines, logLine("error", "Failed to copy: "+err.Error(), name))
			errs++
			continue
		}
		data, _ := os.ReadFile(p)
		target := joinRel(dst, name)
		cur, exists := f.objs[target]
		if exists && len(cur.data) == len(data) && cur.mtime.Equal(fi.ModTime()) {
			continue // matches: rclone skips it
		}
		if exists {
			deletes++
			if maxDelete >= 0 && deletes > int64(maxDelete) {
				lines = append(lines, logLine("error", "Got fatal error on delete: --max-delete threshold reached", name))
				return enginetest.Script{Stderr: lines, Exit: 7}
			}
			f.objs[joinRel(backup, name)] = cur
			delete(f.objs, target)
			bytes += int64(len(cur.data))
			ssc += int64(len(cur.data))
			lines = append(lines, logLine("info", "Moved into backup dir", name))
			if f.cutAfterBackup[name] {
				lines = append(lines, statsLine(bytes, ssc, transfers, deletes, []string{name}), logLine("error", "Max duration reached - stopping soft", ""))
				return enginetest.Script{Stderr: lines, Exit: 10}
			}
		}
		f.objs[target] = fakeObj{data: data, mtime: fi.ModTime()}
		msg := "Copied (new)"
		if exists {
			msg = "Copied (replaced existing)"
		}
		lines = append(lines, logLine("info", msg, name))
		bytes += int64(len(data))
		transfers++
	}
	lines = append(lines, statsLine(bytes, ssc, transfers, deletes, nil))
	if errs > 0 {
		exit = 1
	}
	return enginetest.Script{Stderr: lines, Exit: exit}
}

func (f *fakeRclone) copyto(pos []string) enginetest.Script {
	if r, ok := f.rel(pos[1]); ok { // upload
		fi, err := os.Lstat(pos[0])
		if err != nil {
			return enginetest.Script{Stderr: []string{logLine("error", err.Error(), path.Base(pos[0]))}, Exit: 1}
		}
		data, _ := os.ReadFile(pos[0])
		f.objs[r] = fakeObj{data: data, mtime: fi.ModTime()}
		return enginetest.Script{}
	}
	r, _ := f.rel(pos[0]) // download
	o, ok := f.objs[r]
	if !ok {
		return enginetest.Script{Exit: 3}
	}
	if err := os.WriteFile(pos[1], o.data, 0o600); err != nil {
		return enginetest.Script{Exit: 1}
	}
	return enginetest.Script{}
}

func (f *fakeRclone) moveto(pos []string) enginetest.Script {
	from, _ := f.rel(pos[0])
	to, _ := f.rel(pos[1])
	o, ok := f.objs[from]
	if !ok {
		return enginetest.Script{Stderr: []string{logLine("error", "object not found", path.Base(from))}, Exit: 4}
	}
	f.objs[to] = o
	delete(f.objs, from)
	f.lastDeletes = map[string]fakeObj{from: o}
	return enginetest.Script{Stderr: []string{logLine("info", "Moved (server-side) to: "+to, path.Base(from))}}
}

func (f *fakeRclone) move(c *enginetest.Call, pos []string) enginetest.Script {
	src, _ := f.rel(pos[0])
	dst, _ := f.rel(pos[1])
	f.lastDeletes = map[string]fakeObj{}
	var lines []string
	for _, name := range readList(c) {
		o, ok := f.objs[joinRel(src, name)]
		if !ok {
			continue
		}
		f.objs[joinRel(dst, name)] = o
		delete(f.objs, joinRel(src, name))
		f.lastDeletes[joinRel(src, name)] = o
		lines = append(lines, logLine("info", "Moved (server-side)", name))
	}
	return enginetest.Script{Stderr: lines}
}

func (f *fakeRclone) deleteList(c *enginetest.Call, pos []string) enginetest.Script {
	dir, _ := f.rel(pos[0])
	maxDelete := -1
	if v, ok := c.Flag("--max-delete"); ok {
		maxDelete, _ = strconv.Atoi(v)
	}
	n := 0
	f.deleteLists = append(f.deleteLists, readList(c))
	for _, name := range readList(c) {
		p := joinRel(dir, name)
		if _, ok := f.objs[p]; !ok {
			continue
		}
		n++
		if maxDelete >= 0 && n > maxDelete {
			return enginetest.Script{Stderr: []string{logLine("error", "Got fatal error on delete: --max-delete threshold reached", name)}, Exit: 7}
		}
		delete(f.objs, p)
	}
	return enginetest.Script{}
}

func (f *fakeRclone) check(c *enginetest.Call, pos []string) enginetest.Script {
	srcRoot := pos[0]
	dst, _ := f.rel(pos[1])
	combined, _ := c.Flag("--combined")
	list := readList(c)
	if list == nil {
		b, _ := os.ReadFile(c.Cmd.Dir.DataPath("sample"))
		list = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
	var out bytes.Buffer
	differ := false
	for _, name := range list {
		local, err := os.ReadFile(filepath.Join(srcRoot, filepath.FromSlash(name)))
		o, ok := f.objs[joinRel(dst, name)]
		mark := "="
		switch {
		case err != nil:
			mark = "!"
		case !ok:
			mark, differ = "+", true
		case !bytes.Equal(local, o.data):
			mark, differ = "*", true
		}
		fmt.Fprintf(&out, "%s %s\n", mark, name)
	}
	if err := os.WriteFile(combined, out.Bytes(), 0o600); err != nil {
		return enginetest.Script{Exit: 2}
	}
	if differ {
		return enginetest.Script{Exit: 1}
	}
	return enginetest.Script{}
}
