package rclone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// The commands of the rclone engine (docs/design/phase4.md §7.3, §7.5, §7.6, §8.3, §4.5). Each
// builds the exact command line of the design, runs it through the exec layer and parses what it
// printed. Removing commands (copy with --backup-dir, move, moveto, delete, deletefile, purge)
// always carry --max-delete (S23); their paths are checked against the fences first.

// MarkerLimit bounds how much of a marker is read.
const MarkerLimit = 64 << 10

// Object is one entry of an lsjson listing: its path relative to the listed directory, size and
// modification time.
type Object struct {
	Path    string    `json:"Path"`
	Name    string    `json:"Name"`
	Size    int64     `json:"Size"`
	ModTime time.Time `json:"ModTime"`
	IsDir   bool      `json:"IsDir"`
}

// MtimeNs is the modification time in nanoseconds since the epoch.
func (o Object) MtimeNs() int64 { return o.ModTime.UnixNano() }

// ParseLsJSONLine parses one stdout line of rclone lsjson, which prints "[", one object per line
// (each but the last followed by a comma) and "]". ok is false for the bracket lines.
func ParseLsJSONLine(line string) (Object, bool, error) {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "[")
	t = strings.TrimSuffix(t, "]")
	t = strings.TrimSuffix(strings.TrimSpace(t), ",")
	if t == "" {
		return Object{}, false, nil
	}
	var o Object
	if err := json.Unmarshal([]byte(t), &o); err != nil {
		return Object{}, false, fmt.Errorf("lsjson: %w", err)
	}
	return o, true, nil
}

// TransferResult is how a transfer command (copy, move, moveto, delete) ended besides a fatal
// error. It is filled also when the command returned an error: whatever the exit, the caller
// lists the batch's paths (§7.3).
type TransferResult struct {
	Code  int
	Class ExitClass
	// Events are the per-object INFO and ERROR lines, in order (informational, §7.7).
	Events []ObjectEvent
	// ObjectErrors maps each object an ERROR line named to its last message: the item's error.
	ObjectErrors map[string]string
	// Stats is the last stats line.
	Stats Stats
	// Cutoff: exit 10, --max-duration reached at a window's end (§9.2).
	Cutoff bool
	// Interrupted: Bunkarr interrupted the command (Interrupt closed).
	Interrupted bool
}

// transfer runs a transfer command. Per-object errors (exit 1 with objects, 4, 6), a cutoff
// (exit 10) and Bunkarr's interrupt are results; the rest is an *Error (or the context's error).
func (c *Conn) transfer(ctx context.Context, cmd command, onStats func(Stats)) (TransferResult, error) {
	if onStats != nil {
		cmd.onLog = func(l LogLine) {
			if l.Stats != nil {
				onStats(*l.Stats)
			}
		}
	}
	out, err := c.run(ctx, cmd)
	res := TransferResult{Code: out.status.Code, Events: out.events, ObjectErrors: out.objectErrors}
	res.Class = ClassifyExit(out.status.Code, len(out.objectErrors) > 0)
	if out.stats != nil {
		res.Stats = *out.stats
	}
	if err != nil {
		return res, err
	}
	st := out.status
	switch {
	case st.Interrupted:
		res.Interrupted = true
		return res, nil
	case st.Stopped():
		return res, fail(cmd, out)
	}
	switch res.Class {
	case ExitOK, ExitObjects, ExitItemNotFound:
		return res, nil
	case ExitCutoff:
		res.Cutoff = true
		return res, nil
	}
	return res, fail(cmd, out)
}

// maxDuration renders --max-duration, rounded up to whole seconds and at least 1 s (0 would mean
// no limit to rclone).
func maxDuration(d time.Duration) string {
	s := (d + time.Second - 1) / time.Second
	return (max(s, 1) * time.Second).String()
}

func checkLocal(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("%w: %q is not a clean absolute local path", ErrFence, p)
	}
	return nil
}

func checkMaxDelete(n int) error {
	if n < 0 {
		return fmt.Errorf("rclone: --max-delete %d is negative", n)
	}
	return nil
}

// CopyInput is one copy batch (§7.3).
type CopyInput struct {
	// SourceRoot is the source's recorded root (absolute); Files are catalog paths relative to it,
	// which are also their paths under DestFolder.
	SourceRoot string
	DestFolder string
	Files      []string
	// RetentionDir is the job's retention directory (filecopy.RetentionDir); the backup dir is
	// <RetentionDir>/<DestFolder> (S23).
	RetentionDir string
	// MaxDelete is the batch's copy and update items (S23): every one can displace an object.
	MaxDelete int
	// MaxDuration is the time left in the transfer window (0: no window): --max-duration with
	// --cutoff-mode soft.
	MaxDuration time.Duration
	// OnStats receives each stats line (progress, §10.4).
	OnStats func(Stats)
	// Interrupt, when closed, interrupts the command (the window's end plus grace).
	Interrupt <-chan struct{}
}

// Copy runs one copy batch, exactly the command line of §7.3:
//
//	rclone copy <source root> <root>/<destFolder> --files-from-raw <run>/files --no-traverse
//	  --backup-dir <root>/.bunkarr/retention/<run>/<destFolder> --max-delete <n>
//	  --use-json-log -v --stats 5s --stats-log-level NOTICE
//	  --transfers <t> --checkers <2t> [--max-duration <left> --cutoff-mode soft]
//
// An object that would be overwritten is moved into the backup dir first (S23), and rclone stops
// with exit 7 when that takes more than MaxDelete deletes. The result is valid whatever the
// exit; the caller lists the batch afterwards (§7.3).
func (c *Conn) Copy(ctx context.Context, in CopyInput) (TransferResult, error) {
	if err := checkLocal(in.SourceRoot); err != nil {
		return TransferResult{}, err
	}
	if err := CheckDestFolder(in.DestFolder); err != nil {
		return TransferResult{}, err
	}
	if err := CheckRetentionDir(in.RetentionDir); err != nil {
		return TransferResult{}, err
	}
	if err := checkMaxDelete(in.MaxDelete); err != nil {
		return TransferResult{}, err
	}
	list, err := fileList(in.Files)
	if err != nil {
		return TransferResult{}, err
	}
	t, ch := c.transfers()
	cmd := command{words: []string{"copy"}, data: map[string][]byte{"files": list}, interrupt: in.Interrupt,
		args: func(rd *proc.RunDir) []string {
			a := []string{in.SourceRoot, c.remote(in.DestFolder), "--files-from-raw", rd.DataPath("files"), "--no-traverse",
				"--backup-dir", c.remote(in.RetentionDir + "/" + in.DestFolder), "--max-delete", strconv.Itoa(in.MaxDelete)}
			a = append(a, transferLog...)
			a = append(a, "--transfers", t, "--checkers", ch)
			if in.MaxDuration > 0 {
				a = append(a, "--max-duration", maxDuration(in.MaxDuration), "--cutoff-mode", "soft")
			}
			return a
		}}
	return c.transfer(ctx, cmd, in.OnStats)
}

// CopyTo uploads one local file to rel (rclone copyto). copyto replaces an existing object and
// removes nothing, so it is not a removing command; the caller makes sure the target is free
// (a displaced live object, a new version directory; S23).
func (c *Conn) CopyTo(ctx context.Context, localPath, rel string) error {
	if err := checkLocal(localPath); err != nil {
		return err
	}
	if err := CheckArgName(rel); err != nil {
		return err
	}
	cmd := command{words: []string{"copyto"}, args: func(*proc.RunDir) []string {
		return append([]string{localPath, c.remote(rel)}, transferLog...)
	}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// MaxDownloadCap is the largest cap Download takes (the allow-list's bound, 1 PiB).
const MaxDownloadCap = 1 << 50

// Download copies the object at rel to the local file localPath (rclone copyto; Fetch and
// ReadFile of config versions), capped at maxBytes: rclone copyto <root>/<rel> <localPath>
// --max-transfer <maxBytes>B --cutoff-mode hard. Whatever the remote serves (an object swapped
// for a larger one after the caller listed it, a prefix copyto copies whole, an SFTP server that
// lists one size and serves more), rclone stops at the cap, removes its partial file and exits 8
// (ErrMaxTransfer). A transfer that reaches the cap exactly is refused too, so the cap must
// exceed the expected size (transferCap).
func (c *Conn) Download(ctx context.Context, rel, localPath string, maxBytes int64) error {
	if err := checkLocal(localPath); err != nil {
		return err
	}
	if err := CheckArgName(rel); err != nil {
		return err
	}
	if maxBytes <= 0 || maxBytes > MaxDownloadCap {
		return fmt.Errorf("rclone copyto: cap %d is not 1-%d bytes", maxBytes, int64(MaxDownloadCap))
	}
	cmd := command{words: []string{"copyto"}, args: func(*proc.RunDir) []string {
		return append([]string{c.remote(rel), localPath, "--max-transfer", strconv.FormatInt(maxBytes, 10) + "B",
			"--cutoff-mode", "hard"}, jsonLog...)
	}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// MoveTo moves one object server-side (§7.3: promote, move, a displacement into retention):
// rclone moveto <root>/<from> <root>/<to> --max-delete 1. A server-side move over an existing
// object replaces it, so the caller lists the target first and displaces an occupied one (S23).
func (c *Conn) MoveTo(ctx context.Context, from, to string) (TransferResult, error) {
	for _, p := range []string{from, to} {
		if err := CheckArgName(p); err != nil {
			return TransferResult{}, err
		}
		if p == filecopy.MarkerRel {
			return TransferResult{}, fmt.Errorf("%w: moveto of the marker", ErrFence)
		}
	}
	if from == to {
		return TransferResult{}, fmt.Errorf("%w: moveto %q onto itself", ErrFence, from)
	}
	cmd := command{words: []string{"moveto"}, args: func(*proc.RunDir) []string {
		return append([]string{c.remote(from), c.remote(to), "--max-delete", "1"}, transferLog...)
	}}
	return c.transfer(ctx, cmd, nil)
}

// MoveInput is one batch of retains or releases (§7.3 step 5).
type MoveInput struct {
	// SrcDir is the directory the files are in (a destFolder); DstDir the retention directory
	// they move to (<RetentionDir>/<destFolder>). Files are relative to both.
	SrcDir, DstDir string
	Files          []string
	// MaxDelete is the batch's file count (S23).
	MaxDelete   int
	MaxDuration time.Duration
	OnStats     func(Stats)
	Interrupt   <-chan struct{}
}

// Move moves a batch of files server-side from a destFolder into a job retention directory:
// rclone move <root>/<src> <root>/<dst> --files-from-raw <run>/files --no-traverse
// --max-delete <n>. It never names a local path (S1) and only moves into retention (S23); the
// targets are listed first by the caller, and a taken name gets a numbered one.
func (c *Conn) Move(ctx context.Context, in MoveInput) (TransferResult, error) {
	if err := CheckDestFolder(in.SrcDir); err != nil {
		return TransferResult{}, err
	}
	if err := CheckArgName(in.DstDir); err != nil {
		return TransferResult{}, err
	}
	run, _, _ := strings.Cut(strings.TrimPrefix(in.DstDir, filecopy.RetentionRoot+"/"), "/")
	if CheckRetentionDir(filecopy.RetentionRoot+"/"+run) != nil || in.DstDir == filecopy.RetentionRoot+"/"+run {
		return TransferResult{}, fmt.Errorf("%w: move only into a folder of a job retention directory (not %q)", ErrFence, in.DstDir)
	}
	if err := checkMaxDelete(in.MaxDelete); err != nil {
		return TransferResult{}, err
	}
	list, err := fileList(in.Files)
	if err != nil {
		return TransferResult{}, err
	}
	t, ch := c.transfers()
	cmd := command{words: []string{"move"}, data: map[string][]byte{"files": list}, interrupt: in.Interrupt,
		args: func(rd *proc.RunDir) []string {
			a := []string{c.remote(in.SrcDir), c.remote(in.DstDir), "--files-from-raw", rd.DataPath("files"), "--no-traverse",
				"--max-delete", strconv.Itoa(in.MaxDelete)}
			a = append(a, transferLog...)
			a = append(a, "--transfers", t, "--checkers", ch)
			if in.MaxDuration > 0 {
				a = append(a, "--max-duration", maxDuration(in.MaxDuration), "--cutoff-mode", "soft")
			}
			return a
		}}
	return c.transfer(ctx, cmd, in.OnStats)
}

// Delete deletes expired retained files (§7.5): rclone delete <root>/.bunkarr/retention
// --files-from-raw <run>/files --max-delete <n>. Every file must be under
// .bunkarr/retention/<run>/ (root-relative paths, checked before the command is built, S23).
func (c *Conn) Delete(ctx context.Context, files []string, maxDelete int) (TransferResult, error) {
	if err := checkMaxDelete(maxDelete); err != nil {
		return TransferResult{}, err
	}
	rel := make([]string, 0, len(files))
	for _, f := range files {
		if err := CheckRetentionFile(f); err != nil {
			return TransferResult{}, err
		}
		rel = append(rel, strings.TrimPrefix(f, filecopy.RetentionRoot+"/"))
	}
	list, err := fileList(rel)
	if err != nil {
		return TransferResult{}, err
	}
	cmd := command{words: []string{"delete"}, data: map[string][]byte{"files": list},
		args: func(rd *proc.RunDir) []string {
			return append([]string{c.remote(filecopy.RetentionRoot), "--files-from-raw", rd.DataPath("files"),
				"--max-delete", strconv.Itoa(maxDelete)}, transferLog...)
		}}
	return c.transfer(ctx, cmd, nil)
}

// DeleteFile deletes one expired retained file under .bunkarr/retention/<run>/ (S23):
// rclone deletefile <root>/<rel> --max-delete 1.
func (c *Conn) DeleteFile(ctx context.Context, rel string) error {
	if err := CheckRetentionFile(rel); err != nil {
		return err
	}
	if err := CheckArgName(rel); err != nil {
		return err
	}
	cmd := command{words: []string{"deletefile"}, args: func(*proc.RunDir) []string {
		return append([]string{c.remote(rel), "--max-delete", "1"}, transferLog...)
	}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// Purge removes a config version directory (§8.3): rclone purge <root>/<rel> --max-delete <n>,
// only when isVersionDir (snapshots.Layout.SplitVersionPath, wired by the caller) calls rel a
// version directory. n is the version's file count plus one (S23).
func (c *Conn) Purge(ctx context.Context, rel string, maxDelete int, isVersionDir func(string) bool) error {
	if err := checkVersionDir(rel, isVersionDir); err != nil {
		return err
	}
	if err := checkMaxDelete(maxDelete); err != nil {
		return err
	}
	cmd := command{words: []string{"purge"}, args: func(*proc.RunDir) []string {
		return append([]string{c.remote(rel), "--max-delete", strconv.Itoa(maxDelete)}, transferLog...)
	}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return err
	}
	if out.status.Code == 3 && !out.status.Stopped() {
		return nil // already gone
	}
	return check(cmd, out)
}

// Rmdirs removes the empty directories under dir (SFTP, after moves and expiry; §7.3 step 6,
// §7.5): rclone rmdirs <root>/<dir> [--leave-root]. A missing dir is not an error.
func (c *Conn) Rmdirs(ctx context.Context, dir string, leaveRoot bool) error {
	if err := CheckArgName(dir); err != nil {
		return err
	}
	cmd := command{words: []string{"rmdirs"}, args: func(*proc.RunDir) []string {
		a := []string{c.remote(dir)}
		if leaveRoot {
			a = append(a, "--leave-root")
		}
		return append(a, jsonLog...)
	}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return err
	}
	if out.status.Code == 3 && !out.status.Stopped() {
		return nil
	}
	return check(cmd, out)
}

// Cat reads the object at rel, at most limit bytes (rclone cat --count). The runner delivers
// output as lines, so the content comes back with "\n" line endings: for small text objects
// (the marker, a manifest's first bytes); byte-exact reads use Download. A directory at rel
// prints the objects under it, one after the other; at most limit bytes of them are read
// (command.stdoutLimit).
func (c *Conn) Cat(ctx context.Context, rel string, limit int64) ([]byte, error) {
	out, cmd, err := c.cat(ctx, rel, limit)
	if err != nil {
		return nil, err
	}
	if err := check(cmd, out); err != nil {
		return nil, err
	}
	return joinLines(out.stdout, limit), nil
}

func (c *Conn) cat(ctx context.Context, rel string, limit int64) (outcome, command, error) {
	if err := CheckArgName(rel); err != nil {
		return outcome{}, command{}, err
	}
	if limit <= 0 {
		return outcome{}, command{}, fmt.Errorf("rclone cat: limit %d", limit)
	}
	cmd := command{words: []string{"cat"}, budget: c.drv.listingBudget(), stdoutLimit: limit, args: func(*proc.RunDir) []string {
		return append([]string{"--count", strconv.FormatInt(limit, 10), c.remote(rel)}, jsonLog...)
	}}
	out, err := c.run(ctx, cmd)
	return out, cmd, err
}

// joinLines rebuilds text from lines ("\n" after each), cut to limit.
func joinLines(lines []string, limit int64) []byte {
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
		if int64(len(b)) >= limit {
			return b[:limit]
		}
	}
	return b
}

// Rcat writes content to rel (rclone rcat --size <n>, content on stdin): only Bunkarr's own
// files in .bunkarr/ outside retention (the marker, links.tsv). An S3 or B2 PUT is atomic; on
// SFTP rclone writes a .partial and renames it (§7.3 step 6).
func (c *Conn) Rcat(ctx context.Context, rel string, content []byte) error {
	if err := checkWritable(rel); err != nil {
		return err
	}
	if content == nil {
		content = []byte{}
	}
	cmd := command{words: []string{"rcat"}, stdin: content, budget: c.drv.listingBudget(), args: func(*proc.RunDir) []string {
		return append([]string{"--size", strconv.Itoa(len(content)), c.remote(rel)}, jsonLog...)
	}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// MinCleanupAge is the smallest max-age BackendCleanup accepts (§7.5: at least 7 days, so an
// upload in progress is never aborted).
const MinCleanupAge = 7 * 24 * time.Hour

// BackendCleanup removes unfinished multipart uploads older than maxAge under the destination's
// own bucket and prefix (§7.5, S3 and B2): rclone backend cleanup BKDEST:<bucket>/<prefix>
// -o max-age=<age>. The top-level rclone cleanup is never used (S23).
func (c *Conn) BackendCleanup(ctx context.Context, maxAge time.Duration) error {
	if c.dest.Kind != engines.S3 && c.dest.Kind != engines.B2 {
		return fmt.Errorf("rclone backend cleanup: not available for kind %s", c.dest.Kind)
	}
	if maxAge < MinCleanupAge {
		return fmt.Errorf("rclone backend cleanup: max-age %s is shorter than %s", maxAge, MinCleanupAge)
	}
	storage := StorageRoot(c.dest)
	cmd := command{words: []string{"backend", "cleanup"}, args: func(*proc.RunDir) []string {
		return append([]string{storage, "-o", "max-age=" + maxAge.Round(time.Second).String()}, jsonLog...)
	}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// LsJSON lists the files under dir ("" is the root), recursively or not, calling fn for each
// (rclone lsjson [-R] --files-only --no-mimetype). A missing directory is an *Error wrapping
// ErrPathNotFound (exit 3; on S3 a missing prefix lists empty). When rclone exits 5 the listing
// runs once more (§10.3), so fn can see an object twice: callers key what they collect by path.
func (c *Conn) LsJSON(ctx context.Context, dir string, recursive bool, fn func(Object) error) error {
	if dir != "" {
		if err := CheckArgName(dir); err != nil {
			return err
		}
	}
	cmd := command{words: []string{"lsjson"}, budget: c.drv.listingBudget(),
		stdout: func(line string) error {
			o, ok, err := ParseLsJSONLine(line)
			if err != nil || !ok {
				return err
			}
			return fn(o)
		},
		args: func(*proc.RunDir) []string {
			var a []string
			if recursive {
				a = append(a, "-R")
			}
			return append(append(a, "--files-only", "--no-mimetype", c.remote(dir)), jsonLog...)
		}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// StatMany lists which of rels (relative to dir; "" is the root) exist as files, with size and
// modification time (§7.3 statMany): rclone lsjson -R --files-only --no-mimetype
// --files-from-raw. -R is needed for paths in subdirectories (make test-engines: without it
// lsjson lists the top level only, filtered by the list); with the list rclone visits only the
// directories on its paths. Every "does it exist" question uses it; lsjson --stat is never
// trusted, because a missing S3 object can stat as a directory. A missing directory means none
// exists.
func (c *Conn) StatMany(ctx context.Context, dir string, rels []string) (map[string]Object, error) {
	found := map[string]Object{}
	if len(rels) == 0 {
		return found, nil
	}
	if dir != "" {
		if err := CheckArgName(dir); err != nil {
			return nil, err
		}
	}
	list, err := fileList(rels)
	if err != nil {
		return nil, err
	}
	cmd := command{words: []string{"lsjson"}, budget: c.drv.listingBudget(), data: map[string][]byte{"files": list},
		stdout: func(line string) error {
			o, ok, err := ParseLsJSONLine(line)
			if ok && !o.IsDir {
				found[o.Path] = o
			}
			return err
		},
		args: func(rd *proc.RunDir) []string {
			return append([]string{"-R", "--files-only", "--no-mimetype", "--files-from-raw", rd.DataPath("files"), c.remote(dir)}, jsonLog...)
		}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if out.status.Code == 3 && !out.status.Stopped() {
		return map[string]Object{}, nil
	}
	if err := check(cmd, out); err != nil {
		return nil, err
	}
	return found, nil
}

// Check marks of rclone check --combined (§7.6). rclone's own source reports a file only in the
// source (missing at the destination) with '+' and one only in the destination with '-'; with
// --one-way the latter does not occur.
const (
	MarkMatch           = '='
	MarkDiffer          = '*'
	MarkMissingOnDest   = '+'
	MarkMissingOnSource = '-'
	MarkError           = '!'
)

// ParseCombined parses a --combined report: one "<mark> <path>" line per file.
func ParseCombined(data []byte) (map[string]rune, error) {
	marks := map[string]rune{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		if len(line) < 3 || line[1] != ' ' || !strings.ContainsRune("=*+-!", rune(line[0])) {
			return nil, fmt.Errorf("rclone check: unexpected combined line %q", line)
		}
		marks[line[2:]] = rune(line[0])
	}
	return marks, nil
}

// CheckInput is one verify sample (§7.6).
type CheckInput struct {
	SourceRoot string
	DestFolder string
	// Files are catalog paths relative to SourceRoot (and to DestFolder).
	Files     []string
	Interrupt <-chan struct{}
}

// Check compares the content of a sample of files with the source (§7.6): rclone check
// <source root> <root>/<destFolder> --one-way --download --files-from-raw <run>/sample
// --combined <run>/combined. Without --download rclone compares sizes only on crypt and on SFTP
// without hashes, so it is never used without it. The result maps each path to its mark; rclone
// exits 1 when it found differences, which is a result, not an error.
func (c *Conn) Check(ctx context.Context, in CheckInput) (map[string]rune, error) {
	if err := checkLocal(in.SourceRoot); err != nil {
		return nil, err
	}
	if err := CheckDestFolder(in.DestFolder); err != nil {
		return nil, err
	}
	list, err := fileList(in.Files)
	if err != nil {
		return nil, err
	}
	// The report outlives the command's own run directory, which the runner removes when the
	// command ends.
	scratch, err := c.dirs.New(c.rt.JobID)
	if err != nil {
		return nil, fmt.Errorf("rclone check: %w", err)
	}
	defer func() { _ = scratch.Remove() }()
	combined := scratch.DataPath("combined")
	t, _ := c.transfers()
	cmd := command{words: []string{"check"}, data: map[string][]byte{"sample": list}, interrupt: in.Interrupt,
		args: func(rd *proc.RunDir) []string {
			return append([]string{in.SourceRoot, c.remote(in.DestFolder), "--one-way", "--download",
				"--files-from-raw", rd.DataPath("sample"), "--combined", combined, "--checkers", t}, jsonLog...)
		}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	data, readErr := os.ReadFile(combined)
	if out.status.Stopped() || (out.status.Code != 0 && out.status.Code != 1) {
		return nil, fail(cmd, out)
	}
	if readErr != nil {
		if out.status.Code != 0 {
			return nil, fail(cmd, out)
		}
		return nil, fmt.Errorf("rclone check: read the combined report: %w", readErr)
	}
	marks, err := ParseCombined(data)
	if err != nil {
		return nil, err
	}
	if out.status.Code == 1 && len(marks) == 0 {
		return nil, fail(cmd, out)
	}
	return marks, nil
}

// About is rclone about --json: the remote's space in bytes (nil when unknown).
type About struct {
	Total   *int64 `json:"total"`
	Used    *int64 `json:"used"`
	Free    *int64 `json:"free"`
	Trashed *int64 `json:"trashed"`
	Other   *int64 `json:"other"`
	Objects *int64 `json:"objects"`
}

// About asks the remote for its space (SFTP servers with the statvfs extension; S3 has none).
func (c *Conn) About(ctx context.Context) (About, error) {
	cmd := command{words: []string{"about"}, budget: c.drv.listingBudget(), args: func(*proc.RunDir) []string {
		return append([]string{"--json", c.root}, jsonLog...)
	}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return About{}, err
	}
	if err := check(cmd, out); err != nil {
		return About{}, err
	}
	var a About
	if err := json.Unmarshal([]byte(strings.Join(out.stdout, "\n")), &a); err != nil {
		return About{}, fmt.Errorf("rclone about: %w", err)
	}
	return a, nil
}

// MaxTestEntries is how many top-level entries Test counts (§4.5).
const MaxTestEntries = 1000

// countTop lists the top level of remote (rclone lsf --max-depth 1) and counts at most
// MaxTestEntries entries.
func (c *Conn) countTop(ctx context.Context, remote string) (int, outcome, command, error) {
	n := 0
	cmd := command{words: []string{"lsf"}, budget: c.drv.listingBudget(),
		stdout: func(line string) error {
			if line != "" && n < MaxTestEntries {
				n++
			}
			return nil
		},
		args: func(*proc.RunDir) []string {
			return append([]string{"--max-depth", "1", remote}, jsonLog...)
		}}
	out, err := c.run(ctx, cmd)
	return n, out, cmd, err
}

// Marker is the content of <root>/.bunkarr/destination.json (the Phase 1 marker, S25).
type Marker struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// Marker states of readMarker.
const (
	markerMissing = iota
	markerPresent
	// markerGarbage: an object is there but does not parse as a marker.
	markerGarbage
)

// readMarker reads the marker through the (crypt) root. A missing object, an S3 "directory"
// with nothing under it (it cats as nothing), and a wrong crypt password (the encrypted name
// differs) are missing. A directory with objects under it prints them all: at most MarkerLimit
// bytes of them are read (cat) and parsed like an object's content.
func (c *Conn) readMarker(ctx context.Context) (Marker, int, error) {
	out, cmd, err := c.cat(ctx, filecopy.MarkerRel, MarkerLimit)
	if err != nil {
		return Marker{}, 0, err
	}
	st := out.status
	switch {
	case st.Stopped() || out.hostKey || out.undecryptable:
		return Marker{}, 0, fail(cmd, out)
	case st.Code == 3 || st.Code == 4:
		return Marker{}, markerMissing, nil
	case st.Code != 0:
		return Marker{}, 0, fail(cmd, out)
	}
	raw := joinLines(out.stdout, MarkerLimit)
	if strings.TrimSpace(string(raw)) == "" {
		return Marker{}, markerMissing, nil
	}
	var m Marker
	if err := json.Unmarshal(raw, &m); err != nil || m.ID == "" {
		return Marker{}, markerGarbage, nil
	}
	return m, markerPresent, nil
}

// CheckMarker is the S25 identity check of a job, test and dry run: the marker is read with
// rclone cat and its parsed id compared with the destination's marker_id (the content is always
// compared: a missing S3 object can cat as nothing with exit 0). A missing or unparseable marker
// (also a wrong crypt password under strict_names) is engines.ErrMarkerMissing, another id
// engines.ErrMarkerMismatch, a host key that is not pinned engines.ErrHostKeyChanged, and a
// pending create engines.ErrPending (nothing runs).
func (c *Conn) CheckMarker(ctx context.Context) (Marker, error) {
	if strings.HasPrefix(c.dest.MarkerID, "pending:") || c.dest.MarkerID == "" {
		return Marker{}, engines.ErrPending
	}
	m, state, err := c.readMarker(ctx)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && errors.Is(e.Err, ErrPathNotFound) {
			return Marker{}, fmt.Errorf("%w: %v", engines.ErrMarkerMissing, err)
		}
		return Marker{}, err
	}
	switch state {
	case markerMissing:
		return Marker{}, fmt.Errorf("%w: %s not found at %s", engines.ErrMarkerMissing, filecopy.MarkerRel, c.dest.Target)
	case markerGarbage:
		return Marker{}, fmt.Errorf("%w: %s is not a Bunkarr marker", engines.ErrMarkerMissing, filecopy.MarkerRel)
	}
	if m.ID != c.dest.MarkerID {
		return m, engines.ErrMarkerMismatch
	}
	return m, nil
}
