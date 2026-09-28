package restic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// Engine-level operations of the restic driver: Test and Create of §4.5 (Create writes only the
// repository, init; both run within TestBudget) and the capabilities the planner assumes (§3.3).

// Capabilities is what the planner assumes of a restic destination (§3.3): hardlinks in recreate
// mode (other names of a group are linked records; restic stores each name and restores
// hardlinks within one restore), case-sensitive names, no invalid characters, nanosecond
// modification times, stable identities (no two destination names are ever compared), and
// modes enforced (the repository is always encrypted, §8.4 step 6).
func Capabilities() filecopy.Capabilities {
	return filecopy.Capabilities{Hardlinks: true, TrailingDotSpace: true, MtimeGranularityNs: 1, EnforcesModes: true}
}

// AttachWindow is how recent another Bunkarr row's snapshot must be for attach to warn (§4.5).
const AttachWindow = 7 * 24 * time.Hour

// testRepo binds dest for a Test or Create: every command within TestBudget and TestRetryBudget.
func (d *Driver) testRepo(dest engines.Destination, s engines.Secrets) (*Repo, error) {
	r, err := d.Connect(dest, s, engines.Runtime{RetryBudget: TestRetryBudget})
	if err != nil {
		return nil, err
	}
	r.test = true
	return r, nil
}

func noHostKeys(dest engines.Destination) bool {
	return dest.Kind == engines.SFTP && (dest.Remote.SFTP == nil || len(dest.Remote.SFTP.HostKeys) == 0)
}

// Test probes a destination's location and writes nothing (§4.5): reachability (rclone lsf
// --max-depth 1 of the storage root for remote kinds, a stat of the directory for local), then
// restic cat config --json --no-lock: missing (exit 10), exists with the id, wrong-password
// (exit 12) or locked (exit 11). A stored destination (dest.MarkerID set) whose repository has
// another id is reported. An SFTP destination without pinned host keys runs no command.
func (d *Driver) Test(ctx context.Context, dest engines.Destination, s engines.Secrets) (engines.TestResult, error) {
	res := engines.TestResult{EngineVersion: d.Version}
	if noHostKeys(dest) {
		res.Message = "confirm the SFTP server's host keys first"
		return res, nil
	}
	ctx, cancel := context.WithTimeout(ctx, TestBudget)
	defer cancel()
	if dest.Kind.Remote() {
		if d.Rclone == nil {
			return res, errors.New("restic test: no rclone driver for the storage probe")
		}
		p, err := d.Rclone.ProbeStorage(ctx, dest, s)
		if err != nil {
			return res, err
		}
		if !p.Reachable {
			res.Message = p.Message
			return res, nil
		}
		res.Reachable, res.Entries = true, p.Entries
	} else {
		n, free, msg := probeLocal(dest.Target)
		if msg != "" {
			res.Message = msg
			return res, nil
		}
		res.Reachable, res.Entries, res.FreeBytes = true, n, free
	}
	r, err := d.testRepo(dest, s)
	if err != nil {
		return res, err
	}
	id, err := r.CatConfig(ctx)
	switch {
	case err == nil:
		res.Repository, res.ID = engines.RepositoryExists, id
		if dest.MarkerID != "" && !strings.HasPrefix(dest.MarkerID, "pending:") && MarkerID(id) != dest.MarkerID {
			res.Message = engines.ErrAnotherRepository.Error()
			return res, nil
		}
	case errors.Is(err, engines.ErrRepositoryMissing):
		res.Repository = engines.RepositoryMissing
	case errors.Is(err, engines.ErrWrongPassword):
		res.Repository, res.Message = engines.RepositoryWrongPassword, engines.ErrWrongPassword.Error()
		return res, nil
	case errors.Is(err, engines.ErrLocked):
		res.Repository, res.Message = engines.RepositoryLocked, err.Error()
	case ctx.Err() != nil:
		return res, err
	default:
		res.Message = err.Error()
		return res, nil
	}
	res.OK = true
	return res, nil
}

// probeLocal stats a local repository's directory: the first MaxEntries entries and the free
// space, or why it cannot be used.
func probeLocal(target string) (int, *int64, string) {
	fi, err := os.Stat(target)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return 0, nil, "path not found (share not mounted?)"
	case err != nil:
		return 0, nil, err.Error()
	case !fi.IsDir():
		return 0, nil, "not a directory"
	}
	f, err := os.Open(target)
	if err != nil {
		return 0, nil, err.Error()
	}
	names, _ := f.Readdirnames(MaxEntries)
	_ = f.Close()
	var free *int64
	if root, err := os.OpenRoot(target); err == nil {
		if avail, _, err := filecopy.FreeSpace(root); err == nil {
			v := int64(min(avail, 1<<62))
			free = &v
		}
		_ = root.Close()
	}
	return len(names), free, ""
}

// MaxEntries is how many top-level entries Test counts (§4.5).
const MaxEntries = 1000

// Create initializes a new repository or attaches an existing one (§4.5):
//   - without attach the repository must be missing: restic init --json --repository-version 2,
//     then cat config reads its id back (CreateResult.Initialized is set as soon as init
//     succeeded, also when a later step fails: the caller keeps the pending row, whose secret now
//     opens a repository);
//   - with attach it must exist; snapshots of other Bunkarr rows (another bunkarr-dest tag)
//     younger than AttachWindow give the warning "another Bunkarr may be writing to this
//     repository".
//
// MarkerID is "restic:<id>". It runs within TestBudget.
func (d *Driver) Create(ctx context.Context, dest engines.Destination, s engines.Secrets, attach bool) (engines.CreateResult, error) {
	var res engines.CreateResult
	if noHostKeys(dest) {
		return res, errors.New("restic create: the SFTP server's host keys are not pinned")
	}
	ctx, cancel := context.WithTimeout(ctx, TestBudget)
	defer cancel()
	r, err := d.testRepo(dest, s)
	if err != nil {
		return res, err
	}
	id, err := r.CatConfig(ctx)
	switch {
	case err == nil && !attach:
		return res, errors.New("a restic repository already exists at this location: attach it instead")
	case err == nil:
		res.MarkerID = MarkerID(id)
		snaps, err := r.AllSnapshots(ctx)
		if err != nil {
			return res, fmt.Errorf("restic attach: list the repository's snapshots: %w", err)
		}
		if n := recentForeign(snaps, dest.EngineTag, d.now()); n > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"another Bunkarr may be writing to this repository (%d snapshots of other Bunkarr destinations in the last 7 days)", n))
		}
		return res, nil
	case errors.Is(err, engines.ErrRepositoryMissing):
		if attach {
			return res, fmt.Errorf("attach needs an existing repository: %w", err)
		}
	default:
		return res, err
	}
	newID, err := r.Init(ctx)
	if err != nil {
		return res, err
	}
	res.Initialized = true
	id, err = r.CatConfig(ctx)
	if err != nil {
		return res, fmt.Errorf("restic create: read the new repository back: %w", err)
	}
	if id != newID {
		return res, fmt.Errorf("restic create: the new repository reads back as %s, not %s", shortID(id), shortID(newID))
	}
	res.MarkerID = MarkerID(id)
	return res, nil
}

// recentForeign counts the snapshots with a bunkarr-dest tag other than engineTag that are
// younger than AttachWindow at now.
func recentForeign(snaps []Snapshot, engineTag string, now time.Time) int {
	n := 0
	for _, s := range snaps {
		if now.Sub(s.Time) > AttachWindow {
			continue
		}
		for _, t := range s.Tags {
			if strings.HasPrefix(t, TagDestPrefix) && t != DestTag(engineTag) {
				n++
				break
			}
		}
	}
	return n
}
