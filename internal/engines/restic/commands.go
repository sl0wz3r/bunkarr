package restic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// The commands of the restic engine (docs/design/phase4.md §4.5, §6, §8.2). Read-only ones run
// with --no-lock (S9); forget, prune and check take restic's exclusive lock and run with
// --retry-lock 30m after GuardedUnlock (§6.7); nothing ever passes --keep-*, --unsafe-*,
// --repack-*, --insecure-no-password or --password-command (S24, S22).

var snapshotIDRe = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

func retryLock() string { return RetryLock.String() }

// discard consumes the stdout of a long command whose lines are parsed through onLine or not
// needed (status lines of a backup, prune's text), so they are not kept in memory.
func discard(string) error { return nil }

func joinOut(lines []string) []byte { return []byte(strings.Join(lines, "\n")) }

// CatConfig reads the repository's id (restic cat config --json --no-lock). A missing repository
// is engines.ErrRepositoryMissing (exit 10), a wrong password engines.ErrWrongPassword (exit 12),
// a lock engines.ErrLocked (exit 11). Callers compare "restic:<id>" with the marker (S25).
func (r *Repo) CatConfig(ctx context.Context) (string, error) {
	cmd := command{words: []string{"cat", "config"}, budget: r.drv.listingBudget(), args: func(*proc.RunDir) []string {
		return []string{"--json", "--no-lock"}
	}}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if err := check(cmd, out); err != nil {
		return "", err
	}
	var c RepoConfig
	if err := json.Unmarshal(joinOut(out.stdout), &c); err != nil || c.ID == "" {
		return "", fmt.Errorf("restic cat config: no repository id in %q", strings.Join(out.stdout, " "))
	}
	return c.ID, nil
}

// MarkerID is the marker_id of a repository with id (S25).
func MarkerID(repositoryID string) string { return "restic:" + repositoryID }

// CheckIdentity is the S25 check every job, test and dry run makes first, before any command that
// writes (unlock included): a pending create is engines.ErrPending (nothing runs), a local
// repository on another filesystem type than recorded is engines.ErrRepositoryMissing (the share
// is not mounted), and the repository's id must be the marker's (engines.ErrAnotherRepository).
func (r *Repo) CheckIdentity(ctx context.Context) error {
	if r.dest.MarkerID == "" || strings.HasPrefix(r.dest.MarkerID, "pending:") {
		return engines.ErrPending
	}
	if r.dest.Kind == engines.Local && r.dest.FSType != "" {
		root, err := os.OpenRoot(r.dest.Target)
		if err != nil {
			return fmt.Errorf("%w: %v", engines.ErrRepositoryMissing, err)
		}
		fsType, err := filecopy.FSType(root)
		_ = root.Close()
		if err != nil {
			return fmt.Errorf("%w: %v", engines.ErrRepositoryMissing, err)
		}
		if fsType != r.dest.FSType {
			return fmt.Errorf("%w: %s is on %s, not %s", engines.ErrRepositoryMissing, r.dest.Target, fsType, r.dest.FSType)
		}
	}
	id, err := r.CatConfig(ctx)
	if err != nil {
		return err
	}
	if MarkerID(id) != r.dest.MarkerID {
		return engines.ErrAnotherRepository
	}
	return nil
}

// Init creates the repository (restic init --json --repository-version 2) and returns its id.
// The pack size is not a repository setting: backup and prune pass --pack-size (§6.1).
func (r *Repo) Init(ctx context.Context) (string, error) {
	cmd := command{words: []string{"init"}, args: func(*proc.RunDir) []string {
		return []string{"--json", "--repository-version", "2"}
	}}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if err := check(cmd, out); err != nil {
		return "", err
	}
	for _, l := range out.stdout {
		if p, ok := ParseLine(l); ok && p.Init != "" {
			return p.Init, nil
		}
	}
	return "", errors.New("restic init: no repository id in its output")
}

// Snapshots lists this destination's snapshots (restic snapshots --json --no-lock): the tag
// filter is always bunkarr-dest:<engine_tag>, joined with tags (all must match), so a job or
// source id of another row or install never matches (§6.1). ids narrows the listing.
func (r *Repo) Snapshots(ctx context.Context, tags []string, ids []string) ([]Snapshot, error) {
	if !ValidEngineTag(r.dest.EngineTag) {
		return nil, fmt.Errorf("restic snapshots: destination %d has no engine tag", r.dest.ID)
	}
	for _, t := range tags {
		if t == "" || strings.Contains(t, ",") {
			return nil, fmt.Errorf("restic snapshots: tag %q", t)
		}
	}
	return r.snapshots(ctx, append([]string{DestTag(r.dest.EngineTag)}, tags...), ids)
}

// AllSnapshots lists every snapshot of the repository, whatever its tags: only for Create's
// attach warning, which looks for other Bunkarr rows' recent snapshots (§4.5).
func (r *Repo) AllSnapshots(ctx context.Context) ([]Snapshot, error) {
	return r.snapshots(ctx, nil, nil)
}

// snapshots lists the snapshots carrying every tag of tags (none: all), narrowed to ids. restic
// prints the whole listing as one JSON line, and every snapshot holds its whole include list
// (paths), so the line can reach the exec layer's cap (proc.MaxLineBytes): a multi-batch initial
// backup of a large library, or a library whose later snapshots list every sibling folder of a
// held update. The listing is then read in parts (snapshotsInParts) instead of failing, because a
// failing listing would stop every sync and the retention job that could shrink it.
func (r *Repo) snapshots(ctx context.Context, tags []string, ids []string) ([]Snapshot, error) {
	for _, id := range ids {
		if !snapshotIDRe.MatchString(id) {
			return nil, fmt.Errorf("restic snapshots: %q is not a snapshot id", id)
		}
	}
	snaps, err := r.snapshotsOnce(ctx, tags, ids)
	if errors.Is(err, ErrListingTooLarge) {
		return r.snapshotsInParts(ctx, tags, ids)
	}
	return snaps, err
}

// snapshotsOnce runs one restic snapshots --json --no-lock [--tag <tags>] [<id>…]. A line the
// exec layer cut is ErrListingTooLarge.
func (r *Repo) snapshotsOnce(ctx context.Context, tags []string, ids []string) ([]Snapshot, error) {
	cmd := command{words: []string{"snapshots"}, budget: r.drv.listingBudget(), args: func(*proc.RunDir) []string {
		args := []string{"--json", "--no-lock"}
		if len(tags) > 0 {
			args = append(args, "--tag", strings.Join(tags, ","))
		}
		return append(args, ids...)
	}}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if err := check(cmd, out); err != nil {
		return nil, err
	}
	for _, l := range out.stdout {
		if len(l) >= proc.MaxLineBytes {
			return nil, ErrListingTooLarge
		}
	}
	return ParseSnapshots(joinOut(out.stdout))
}

// SnapshotsPart is how many snapshots one restic snapshots --json names when a listing is read
// in parts; a part whose line is still too long is halved, and a single snapshot too long for
// one line is read with restic cat snapshot, which prints indented JSON (one path per line).
const SnapshotsPart = 32

// snapshotsInParts reads a listing too large for one line: the ids (restic list snapshots
// --no-lock, one per line; narrowed to ids by prefix), then the snapshots in parts of at most
// SnapshotsPart ids, each part with the same --tag filter; the result is filtered by tags as
// well, because the ids are every snapshot of the repository and restic may ignore --tag next to
// explicit ids. A snapshot forgotten in between is not listed (restic snapshots skips an id it
// cannot find).
func (r *Repo) snapshotsInParts(ctx context.Context, tags []string, want []string) ([]Snapshot, error) {
	all, err := r.listSnapshotIDs(ctx)
	if err != nil {
		return nil, err
	}
	ids := all
	if len(want) > 0 {
		ids = nil
		for _, id := range all {
			if slices.ContainsFunc(want, func(w string) bool { return strings.HasPrefix(id, w) }) {
				ids = append(ids, id)
			}
		}
	}
	var out []Snapshot
	size := SnapshotsPart
	for len(ids) > 0 {
		part := ids[:min(size, len(ids))]
		snaps, err := r.snapshotsOnce(ctx, tags, part)
		switch {
		case errors.Is(err, ErrListingTooLarge) && len(part) > 1:
			size = len(part) / 2
			continue
		case errors.Is(err, ErrListingTooLarge):
			var s Snapshot
			if s, err = r.catSnapshot(ctx, part[0]); err != nil {
				return nil, err
			}
			snaps = []Snapshot{s}
		case err != nil:
			return nil, err
		}
		out = append(out, slices.DeleteFunc(snaps, func(s Snapshot) bool { return !s.HasTags(tags...) })...)
		ids = ids[len(part):]
	}
	slices.SortStableFunc(out, func(a, b Snapshot) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// listSnapshotIDs lists the ids of every snapshot of the repository (restic list snapshots
// --no-lock).
func (r *Repo) listSnapshotIDs(ctx context.Context) ([]string, error) {
	cmd := command{words: []string{"list"}, budget: r.drv.listingBudget(), args: func(*proc.RunDir) []string {
		return []string{"snapshots", "--no-lock"}
	}}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if err := check(cmd, out); err != nil {
		return nil, err
	}
	var ids []string
	for _, l := range out.stdout {
		if id := strings.TrimSpace(l); fullIDRe.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

var fullIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// catSnapshot reads one snapshot (restic cat snapshot <id> --no-lock): indented JSON without the
// id, which is id.
func (r *Repo) catSnapshot(ctx context.Context, id string) (Snapshot, error) {
	if !fullIDRe.MatchString(id) {
		return Snapshot{}, fmt.Errorf("restic cat snapshot: %q is not a full snapshot id", id)
	}
	cmd := command{words: []string{"cat", "snapshot"}, budget: r.drv.listingBudget(), args: func(*proc.RunDir) []string {
		return []string{id, "--no-lock"}
	}}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return Snapshot{}, err
	}
	if err := check(cmd, out); err != nil {
		return Snapshot{}, err
	}
	for _, l := range out.stdout {
		if len(l) >= proc.MaxLineBytes {
			return Snapshot{}, fmt.Errorf("restic cat snapshot %s: %w", shortID(id), ErrListingTooLarge)
		}
	}
	var s Snapshot
	if err := json.Unmarshal(joinOut(out.stdout), &s); err != nil {
		return Snapshot{}, fmt.Errorf("restic cat snapshot %s: %w", shortID(id), err)
	}
	s.ID, s.ShortID = id, id[:8]
	return s, nil
}

// ErrListingTooLarge means restic printed a line longer than the exec layer keeps
// (proc.MaxLineBytes): snapshots whose path lists are very long (an include list that tier rules
// scattered over thousands of directories), which restic snapshots --json prints as one line.
// Snapshots and AllSnapshots read such a listing in parts; it is returned only when a single
// path is longer than that.
var ErrListingTooLarge = fmt.Errorf("restic printed a line longer than %d bytes (snapshots with very long path lists)", proc.MaxLineBytes)

// BackupArgs is one restic backup (§6.2 step 4, §8.2).
type BackupArgs struct {
	// FilesFrom is the include list (CompressIncludes), written NUL-separated to the run
	// directory and passed as --files-from-raw. Paths is the alternative for a config version:
	// the one directory to back up. Exactly one of them is set.
	FilesFrom []string
	Paths     []string
	// Excludes and IExcludes are the lines of TranslateExcludes (case-sensitive and not).
	Excludes, IExcludes []string
	// ExcludeIfPresent is --exclude-if-present (default KeyFile, S28).
	ExcludeIfPresent string
	// Parent is the source's base (--parent); "" passes none (§6.2: the base is missing).
	Parent string
	// IgnoreInode is set for sources whose inode numbers are unstable.
	IgnoreInode bool
	Tags        []string
	// Host is --host (default Host).
	Host string
	// Time is --time (config versions: the version's time).
	Time *time.Time
	// LimitUpKiB and LimitDownKiB are --limit-upload and --limit-download for a local repository
	// (the value in force when the batch starts, §9.1); remote repositories are limited through
	// RCLONE_BWLIMIT instead, and these are ignored there.
	LimitUpKiB, LimitDownKiB int64
	// RetryLock adds --retry-lock 30m (config version backups, §8.2).
	RetryLock bool
	// OnStatus receives each status line (progress, §10.4).
	OnStatus func(Status)
	// Interrupt, when closed, interrupts the backup (the window's end plus grace, §9.2).
	Interrupt <-chan struct{}
}

// BackupResult is a backup's outcome: exit 0, or exit 3 (some files could not be read; the
// snapshot is saved, §10.3). What the snapshot holds is decided by the read-back (Ls), never by
// these fields (D31).
type BackupResult struct {
	SnapshotID string
	Summary    Summary
	Errors     []ErrorLine
	Exit       int
	// Interrupted: Bunkarr interrupted the backup; its items stay pending (§9.2).
	Interrupted bool
}

// Backup runs restic backup (§6.2 step 4):
//
//	restic backup --json --host bunkarr --tag … --files-from-raw <run>/files
//	  --exclude-file <run>/excludes --iexclude-file <run>/iexcludes
//	  --exclude-if-present bunkarr.key [--parent <base>] [--ignore-inode]
//	  [--limit-upload <KiB/s> --limit-download <KiB/s>]
//
// plus --pack-size, --retry-lock and --time when they apply, and -o rclone.* for remote kinds.
// Exit 0 and 3 are results; Bunkarr's interrupt is a result with Interrupted; anything else is an
// *Error (the batch's items stay pending, and the resumed job runs it again).
func (r *Repo) Backup(ctx context.Context, a BackupArgs) (BackupResult, error) {
	var res BackupResult
	if (len(a.FilesFrom) == 0) == (len(a.Paths) == 0) || len(a.Paths) > 1 {
		return res, errors.New("restic backup: give either an include list or one path")
	}
	if a.Parent != "" && !snapshotIDRe.MatchString(a.Parent) {
		return res, fmt.Errorf("restic backup: parent %q is not a snapshot id", a.Parent)
	}
	if len(a.Tags) == 0 {
		return res, errors.New("restic backup: no tags")
	}
	data := map[string][]byte{}
	if len(a.FilesFrom) > 0 {
		var b []byte
		for _, p := range a.FilesFrom {
			if !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
				return res, fmt.Errorf("restic backup: include path %q is not absolute", p)
			}
			b = append(append(b, p...), 0)
		}
		data["files"] = b
	}
	for name, lines := range map[string][]string{"excludes": a.Excludes, "iexcludes": a.IExcludes} {
		if len(lines) == 0 {
			continue
		}
		for _, l := range lines {
			if strings.ContainsAny(l, "\n\r\x00") {
				return res, fmt.Errorf("restic backup: exclude line %q", l)
			}
		}
		data[name] = []byte(strings.Join(lines, "\n") + "\n")
	}
	host := a.Host
	if host == "" {
		host = Host
	}
	marker := a.ExcludeIfPresent
	if marker == "" {
		marker = KeyFile
	}
	cmd := command{words: []string{"backup"}, data: data, interrupt: a.Interrupt, stdout: discard,
		onLine: func(l Line) {
			if l.Status != nil && a.OnStatus != nil {
				a.OnStatus(*l.Status)
			}
		},
		args: func(rd *proc.RunDir) []string {
			args := []string{"--json", "--host", host}
			for _, t := range a.Tags {
				args = append(args, "--tag", t)
			}
			if len(a.FilesFrom) > 0 {
				args = append(args, "--files-from-raw", rd.DataPath("files"))
			}
			if len(a.Excludes) > 0 {
				args = append(args, "--exclude-file", rd.DataPath("excludes"))
			}
			if len(a.IExcludes) > 0 {
				args = append(args, "--iexclude-file", rd.DataPath("iexcludes"))
			}
			args = append(args, "--exclude-if-present", marker)
			if a.Parent != "" {
				args = append(args, "--parent", a.Parent)
			}
			if a.IgnoreInode {
				args = append(args, "--ignore-inode")
			}
			if r.dest.Kind == engines.Local {
				if a.LimitUpKiB > 0 {
					args = append(args, "--limit-upload", strconv.FormatInt(a.LimitUpKiB, 10))
				}
				if a.LimitDownKiB > 0 {
					args = append(args, "--limit-download", strconv.FormatInt(a.LimitDownKiB, 10))
				}
			}
			if r.dest.PackSizeMiB > 0 {
				args = append(args, "--pack-size", strconv.Itoa(r.dest.PackSizeMiB))
			}
			if a.RetryLock {
				args = append(args, "--retry-lock", retryLock())
			}
			if a.Time != nil {
				args = append(args, "--time", a.Time.Local().Format("2006-01-02 15:04:05"))
			}
			return append(args, a.Paths...)
		}}
	out, err := r.run(ctx, cmd)
	res.Exit, res.Errors = out.status.Code, out.errors
	if out.summary != nil {
		res.Summary, res.SnapshotID = *out.summary, out.summary.SnapshotID
	}
	if err != nil {
		return res, err
	}
	switch st := out.status; {
	case st.Interrupted:
		res.Interrupted = true
		return res, nil
	case st.Stopped():
		return res, fail(cmd, out)
	case st.Code == 0 || st.Code == 3:
		if res.SnapshotID == "" || !snapshotIDRe.MatchString(res.SnapshotID) {
			return res, &Error{Command: cmd.name(), Code: st.Code, Message: out.message(), Err: ErrNoSummary}
		}
		return res, nil
	}
	return res, fail(cmd, out)
}

// Ls streams a snapshot's content (restic ls --json --no-lock <id>): the read-back of §6.2
// step 5. fn is called for every node; an error from fn stops the listing and is returned.
func (r *Repo) Ls(ctx context.Context, snapshotID string, fn func(Node) error) error {
	if !snapshotIDRe.MatchString(snapshotID) {
		return fmt.Errorf("restic ls: %q is not a snapshot id", snapshotID)
	}
	cmd := command{words: []string{"ls"}, budget: r.drv.listingBudget(),
		stdout: func(line string) error {
			n, ok, err := ParseLsLine(line)
			if err != nil && len(line) >= proc.MaxLineBytes {
				// The snapshot line with a long path list, cut by the exec layer: nodes are short.
				return nil
			}
			if err != nil || !ok {
				return err
			}
			return fn(n)
		},
		args: func(*proc.RunDir) []string { return []string{"--json", "--no-lock", snapshotID} }}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// Forget forgets snapshots by id (restic forget --json --retry-lock 30m <id>…), at most
// ForgetChunk per command, never with a --keep-* policy (S24). It returns the ids of the chunks
// that succeeded, also with an error.
func (r *Repo) Forget(ctx context.Context, ids []string) ([]string, error) {
	var done []string
	for _, id := range ids {
		if !snapshotIDRe.MatchString(id) {
			return nil, fmt.Errorf("restic forget: %q is not a snapshot id", id)
		}
	}
	for chunk := range slices.Chunk(ids, ForgetChunk) {
		cmd := command{words: []string{"forget"}, stdout: discard, args: func(*proc.RunDir) []string {
			return append([]string{"--json", "--retry-lock", retryLock()}, chunk...)
		}}
		out, err := r.run(ctx, cmd)
		if err != nil {
			return done, err
		}
		if err := check(cmd, out); err != nil {
			return done, err
		}
		done = append(done, chunk...)
	}
	return done, nil
}

// PruneArgs is one prune (§6.5 step 4).
type PruneArgs struct {
	// MaxUnused is --max-unused ("10%", settings.restic.pruneMaxUnused).
	MaxUnused string
	// LimitUpKiB and LimitDownKiB: a local repository's limits.
	LimitUpKiB, LimitDownKiB int64
	Interrupt                <-chan struct{}
}

// Prune runs restic prune --max-unused <x> --retry-lock 30m, never with --unsafe-* or
// --repack-* (S24). prune prints text even with --json, so only its exit code counts. An
// interrupted prune is safe to run again.
func (r *Repo) Prune(ctx context.Context, a PruneArgs) error {
	if a.MaxUnused == "" {
		return errors.New("restic prune: no --max-unused")
	}
	cmd := command{words: []string{"prune"}, interrupt: a.Interrupt, stdout: discard, args: func(*proc.RunDir) []string {
		args := []string{"--max-unused", a.MaxUnused, "--retry-lock", retryLock()}
		if r.dest.PackSizeMiB > 0 {
			args = append(args, "--pack-size", strconv.Itoa(r.dest.PackSizeMiB))
		}
		if r.dest.Kind == engines.Local {
			if a.LimitUpKiB > 0 {
				args = append(args, "--limit-upload", strconv.FormatInt(a.LimitUpKiB, 10))
			}
			if a.LimitDownKiB > 0 {
				args = append(args, "--limit-download", strconv.FormatInt(a.LimitDownKiB, 10))
			}
		}
		return args
	}}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// CheckArgs is one repository check (§6.6).
type CheckArgs struct {
	// ReadData reads every pack (--read-data); Subset reads one subset ("n/t",
	// --read-data-subset); neither checks the structure only.
	ReadData  bool
	Subset    string
	Interrupt <-chan struct{}
}

var subsetRe = regexp.MustCompile(`^[1-9][0-9]*/[1-9][0-9]*$`)

// Check runs restic check --json --retry-lock 30m [--read-data | --read-data-subset n/t] and
// returns its summary (num_errors, spike check.json). restic exits 1 when it found errors, which
// is a result when the summary was printed. check takes an exclusive lock (spike:
// check-read-data-subset.txt), so GuardedUnlock runs before it.
func (r *Repo) Check(ctx context.Context, a CheckArgs) (CheckResult, error) {
	if a.ReadData && a.Subset != "" {
		return CheckResult{}, errors.New("restic check: read-data and a subset")
	}
	if a.Subset != "" && !subsetRe.MatchString(a.Subset) {
		return CheckResult{}, fmt.Errorf("restic check: subset %q is not n/t", a.Subset)
	}
	var sum *CheckResult
	cmd := command{words: []string{"check"}, interrupt: a.Interrupt, stdout: discard,
		onLine: func(l Line) {
			if l.Type == "summary" {
				var c CheckResult
				if json.Unmarshal([]byte(strings.TrimSpace(l.Raw)), &c) == nil {
					sum = &c
				}
			}
		},
		args: func(*proc.RunDir) []string {
			args := []string{"--json", "--retry-lock", retryLock()}
			switch {
			case a.ReadData:
				args = append(args, "--read-data")
			case a.Subset != "":
				args = append(args, "--read-data-subset", a.Subset)
			}
			return args
		}}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return CheckResult{}, err
	}
	if sum != nil && !out.status.Stopped() && (out.status.Code == 0 || out.status.Code == 1) {
		return *sum, nil
	}
	if err := check(cmd, out); err != nil {
		return CheckResult{}, err
	}
	return CheckResult{}, fmt.Errorf("restic check: no summary")
}

// RestoreArgs is one restore into staging (§6.6 verify samples, §8.2 config version files).
type RestoreArgs struct {
	Snapshot string
	// Subpath is the absolute directory of the snapshot to restore (its content lands directly
	// in Target).
	Subpath string
	// Target is a staging directory; never a source (S1: the caller makes sure).
	Target string
	// Include lists paths relative to Subpath; only they are restored (none: everything).
	Include []string
}

// Restore runs restic restore <snapshot>:<subpath> --target <dir> --no-lock [--include-file
// <run>/include], the include lines anchored and escaped like the exclude files.
func (r *Repo) Restore(ctx context.Context, a RestoreArgs) error {
	if !snapshotIDRe.MatchString(a.Snapshot) {
		return fmt.Errorf("restic restore: %q is not a snapshot id", a.Snapshot)
	}
	for _, p := range []string{a.Subpath, a.Target} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("restic restore: %q is not a clean absolute path", p)
		}
	}
	data := map[string][]byte{}
	if len(a.Include) > 0 {
		var lines []string
		for _, p := range a.Include {
			if p == "" || strings.HasPrefix(p, "/") || filepath.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
				return fmt.Errorf("restic restore: include %q is not a clean relative path", p)
			}
			esc, err := escapeLiteral(p, false)
			if err == nil {
				esc, err = finish("/" + esc)
			}
			if err != nil {
				return fmt.Errorf("restic restore: include %q: %w", p, err)
			}
			lines = append(lines, esc)
		}
		data["include"] = []byte(strings.Join(lines, "\n") + "\n")
	}
	cmd := command{words: []string{"restore"}, data: data, stdout: discard, args: func(rd *proc.RunDir) []string {
		args := []string{a.Snapshot + ":" + a.Subpath, "--target", a.Target, "--no-lock"}
		if len(a.Include) > 0 {
			args = append(args, "--include-file", rd.DataPath("include"))
		}
		return args
	}}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// Dump writes a text file of a snapshot to w, at most limit bytes (restic dump --no-lock <id>
// <path>). The exec layer delivers output as lines, so the file comes back with "\n" after every
// line: exact for a text file with "\n" line endings, not for binary content or "\r\n" (use
// Restore for those, as ReadFile and Fetch do). A line of proc.MaxLineBytes or more fails.
func (r *Repo) Dump(ctx context.Context, snapshotID, path string, w io.Writer, limit int64) error {
	if !snapshotIDRe.MatchString(snapshotID) {
		return fmt.Errorf("restic dump: %q is not a snapshot id", snapshotID)
	}
	var written int64
	cmd := command{words: []string{"dump"}, budget: r.drv.listingBudget(),
		stdout: func(line string) error {
			if len(line) >= proc.MaxLineBytes {
				return errors.New("restic dump: a line too long for a text dump")
			}
			if written >= limit {
				return nil
			}
			b := []byte(line + "\n")
			if int64(len(b)) > limit-written {
				b = b[:limit-written]
			}
			n, err := w.Write(b)
			written += int64(n)
			return err
		},
		args: func(*proc.RunDir) []string { return []string{"--no-lock", snapshotID, path} }}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// ListLocks lists the repository's lock ids (restic list locks --no-lock).
func (r *Repo) ListLocks(ctx context.Context) ([]string, error) {
	cmd := command{words: []string{"list"}, budget: r.drv.listingBudget(), args: func(*proc.RunDir) []string {
		return []string{"locks", "--no-lock"}
	}}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if err := check(cmd, out); err != nil {
		return nil, err
	}
	var ids []string
	for _, l := range out.stdout {
		if id := strings.TrimSpace(l); snapshotIDRe.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// CatLock reads one lock (restic cat lock <id> --no-lock; spike lock.json).
func (r *Repo) CatLock(ctx context.Context, id string) (Lock, error) {
	if !snapshotIDRe.MatchString(id) {
		return Lock{}, fmt.Errorf("restic cat lock: %q is not a lock id", id)
	}
	cmd := command{words: []string{"cat", "lock"}, budget: r.drv.listingBudget(), args: func(*proc.RunDir) []string {
		return []string{id, "--no-lock"}
	}}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return Lock{}, err
	}
	if err := check(cmd, out); err != nil {
		return Lock{}, err
	}
	var l Lock
	if err := json.Unmarshal(joinOut(out.stdout), &l); err != nil {
		return Lock{}, fmt.Errorf("restic cat lock: %w", err)
	}
	return l, nil
}

// Unlock runs restic unlock (stale locks by restic's rules), or restic unlock --remove-all.
// Callers use GuardedUnlock before exclusive operations; --remove-all only for the user's
// explicit request (§6.7).
func (r *Repo) Unlock(ctx context.Context, removeAll bool) error {
	cmd := command{words: []string{"unlock"}, budget: r.drv.listingBudget(), args: func(*proc.RunDir) []string {
		if removeAll {
			return []string{"--remove-all"}
		}
		return nil
	}}
	out, err := r.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// ErrForeignLock is GuardedUnlock's refusal: a lock that may belong to a live process in another
// container with this container's host name (§6.7).
var ErrForeignLock = errors.New("another restic process with the same host name is using this repository")

// LiveLockAge is how old a lock may be and still belong to a live restic process: restic
// refreshes its locks every 5 minutes, ends an operation whose lock it could not refresh for 22.5
// minutes, and calls a lock of another host stale after 30 minutes without a refresh
// (restic.StaleLockTimeout). A live lock of another container with this host name can therefore
// be older than this process's start (it was refreshed shortly before this process started).
const LiveLockAge = 30 * time.Minute

// GuardedUnlock runs restic unlock right before an exclusive operation, and only when no lock
// can belong to a live process (§6.7): every lock is listed and read (restic list locks, cat
// lock, --no-lock). A lock with hostName whose PID is not one of this process's live children
// (isLiveChild; nil: IsLiveChild), that one of its restic children did not leave behind (its
// PID, within its run; children.go: an interrupt stops the rclone backend with restic, so restic
// cannot remove its lock) and that no restic child of an earlier Bunkarr process on this config
// directory left behind (the children file: a kill or crash during a restic command; that process
// is gone, so its lock is stale at once) may come from another container with the same host
// name, which restic would call stale (same host, a PID it does not know):
//
//   - when its time is later than processStart, the unlock is refused with "another restic
//     process with host name <X> is using this repository";
//   - when it is older than processStart but younger than LiveLockAge, it may still be a live
//     process's lock that was refreshed shortly before this process started, or one a previous
//     run left that the children file does not name (lost, or a child found too late): a job (Runtime.JobID set) waits until it is LiveLockAge old and
//     inspects the locks again (a live process refreshes its lock meanwhile, which then is newer
//     than processStart and refuses the unlock); outside a job (the unlock endpoint) the unlock
//     is refused with the time from which it counts as stale.
//
// Locks of other host names are left to restic's own staleness rule. A lock that vanished while
// it was read is skipped; one that cannot be read refuses the unlock. It never runs unlock
// --remove-all.
func (r *Repo) GuardedUnlock(ctx context.Context, hostName string, processStart time.Time, isLiveChild func(pid int) bool) error {
	if isLiveChild == nil {
		isLiveChild = IsLiveChild
	}
	for {
		recent, err := r.inspectLocks(ctx, hostName, processStart, isLiveChild)
		if err != nil {
			return err
		}
		if recent == nil {
			return r.Unlock(ctx, false)
		}
		stale := recent.lock.Time.Add(LiveLockAge)
		if r.rt.JobID == 0 {
			return fmt.Errorf("%w: a lock with host name %s (lock %s, pid %d, refreshed %s) may belong to a live restic process in "+
				"another container; it counts as stale from %s", ErrForeignLock, hostName, shortID(recent.id), recent.lock.PID,
				recent.lock.Time.UTC().Format(time.RFC3339), stale.UTC().Format(time.RFC3339))
		}
		wait := stale.Sub(r.rt.Clock()) + time.Second
		if r.rt.Reporter != nil {
			r.rt.Reporter.Log(slog.LevelWarn, "waiting for a restic lock with this host name to become stale before unlocking",
				"lock", shortID(recent.id), "pid", recent.lock.PID, "refreshed", recent.lock.Time.UTC().Format(time.RFC3339),
				"wait", wait.Round(time.Second).String())
		}
		if err := r.drv.sleepCtx(ctx, min(wait, LiveLockAge+time.Second)); err != nil {
			return err
		}
	}
}

// recentLock is a lock with this host name that may still belong to a live process.
type recentLock struct {
	id   string
	lock Lock
}

// inspectLocks reads every lock for GuardedUnlock: an error refuses the unlock; a recentLock
// (the one that becomes stale last) makes it wait; neither lets it unlock.
func (r *Repo) inspectLocks(ctx context.Context, hostName string, processStart time.Time, isLiveChild func(pid int) bool) (*recentLock, error) {
	ids, err := r.ListLocks(ctx)
	if err != nil {
		return nil, err
	}
	now := r.rt.Clock()
	var recent *recentLock
	unreadable := map[string]error{}
	for _, id := range ids {
		l, err := r.CatLock(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			unreadable[id] = err
			continue
		}
		if l.Hostname != hostName || isLiveChild(l.PID) || leftByOwnChild(l.PID, l.Time) ||
			leftByEarlierProcess(r.drv.childrenPath(), l.PID, l.Time, processStart) {
			continue
		}
		if l.Time.After(processStart) {
			return nil, fmt.Errorf("%w: another restic process with host name %s is using this repository (lock %s, pid %d, created %s)",
				ErrForeignLock, hostName, shortID(id), l.PID, l.Time.UTC().Format(time.RFC3339))
		}
		if now.Sub(l.Time) < LiveLockAge && (recent == nil || l.Time.After(recent.lock.Time)) {
			recent = &recentLock{id: id, lock: l}
		}
	}
	if len(unreadable) > 0 {
		// A lock that could not be read is skipped only when it is gone by now (its process
		// removed it); one still there may be a live process's, so nothing is unlocked.
		again, err := r.ListLocks(ctx)
		if err != nil {
			return nil, err
		}
		for _, id := range again {
			if err := unreadable[id]; err != nil {
				return nil, fmt.Errorf("restic unlock: lock %s cannot be read, so it may belong to a live process: %w", shortID(id), err)
			}
		}
	}
	return recent, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// IsLiveChild reports whether pid is a live child process of this process (Linux: its
// /proc/<pid>/stat names this process as parent). Elsewhere it reports false.
func IsLiveChild(pid int) bool {
	if pid <= 0 {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// "pid (comm) state ppid …": comm may hold spaces and parentheses; the last ")" ends it.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return false
	}
	ppid, err := strconv.Atoi(f[1])
	return err == nil && ppid == os.Getpid() && f[0] != "Z"
}
