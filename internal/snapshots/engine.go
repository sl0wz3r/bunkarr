package snapshots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
)

// Config versions at engine destinations (docs/design/phase4.md §8, D24). A Plex DB, *arr or
// manifest version at a restic or rclone destination is stored through the destination's
// engines.VersionStore instead of a directory under an os.Root: one restic snapshot per version,
// or an rclone version directory written with manifest.json last. The runners keep their
// staging, verification and retention math; EngineVersions is what they share on that path
// (plexdb and arrbackup with the snapshots table, internal/manifest with its own):
//
//   - Put stores a staged version and records its row, with the fault points versions.afterPut
//     and versions.beforeRecord between the two;
//   - Recover lists the kind folder at the start of a run and settles versions without a row: an
//     rclone leftover without manifest.json older than LeftoverAge is purged, a complete one is
//     adopted when its files list with the manifest's sizes; on restic only a snapshot of the
//     running job is adopted (the listing holds only this destination row's engine_tag, and the
//     manifest must name this integration, application and job), because another unrecorded
//     snapshot may be a pruned version whose forget request is still pending;
//   - Remove deletes a pruned version: on restic the row delete and the engine_forget request in
//     one transaction (D28), on rclone the version first and then the row, so a crash in between
//     leaves a row whose version is gone, which Listing.Lost reports at the next run.

// Fault points of EngineVersions.Put (the crash matrix, phase4.md §14.3).
const (
	// PointVersionsAfterPut: the version reads back complete at the destination, not recorded.
	PointVersionsAfterPut = "versions.afterPut"
	// PointVersionsBeforeRecord: its row is about to be inserted.
	PointVersionsBeforeRecord = "versions.beforeRecord"
)

// LeftoverAge is how old an rclone version directory without manifest.json and without a row
// must be before Recover purges it: it can then only be the upload of a crashed job (§8.3).
const LeftoverAge = 24 * time.Hour

// ErrNameTaken means no free version name was found for a new version.
var ErrNameTaken = errors.New("the version name is taken")

// EngineRow is a recorded version as EngineVersions sees it: its logical path
// (".bunkarr/<kind>/<folder>/<version>", or ".bunkarr/manifests/<version>") and its engine_ref
// (the restic snapshot id; "" on rclone, where the path is the reference).
type EngineRow struct {
	Path      string
	EngineRef string
}

// EngineVersions keeps the config versions of one kind at one engine destination through its
// VersionStore (see the package section above). Its fields are set by the runner for one job.
type EngineVersions struct {
	// DB holds the rows (the restic prune's transaction).
	DB *db.DB
	// Store is the destination's version store for this job (engines.VersionOpener).
	Store engines.VersionStore
	// Engine is engines.Restic or engines.Rclone.
	Engine engines.Kind
	// Kind is engines.VersionPlexDB, VersionArr or VersionManifest.
	Kind string
	// Now is the clock (the leftover age); nil is time.Now.
	Now func() time.Time
	// Info logs to the job; Warn logs a job warning (the runner counts it). nil discards.
	Info func(msg string, args ...any)
	Warn func(msg string, args ...any)
}

func (e *EngineVersions) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *EngineVersions) info(msg string, args ...any) {
	if e.Info != nil {
		e.Info(msg, args...)
	}
}

func (e *EngineVersions) warn(msg string, args ...any) {
	if e.Warn != nil {
		e.Warn(msg, args...)
	}
}

// Restic reports whether the destination is a restic repository.
func (e *EngineVersions) Restic() bool { return e.Engine == engines.Restic }

// RowRef returns the engine_ref a row stores for the reference Put returned: the restic snapshot
// id, or "" on rclone (the reference is the version's path, which the row has).
func (e *EngineVersions) RowRef(ref engines.Ref) string {
	if e.Restic() {
		return string(ref)
	}
	return ""
}

// Ref returns the store reference of a recorded version: its engine_ref on restic, its path on
// rclone. RefIn prefers the reference a listing holds.
func (e *EngineVersions) Ref(r EngineRow) engines.Ref {
	if e.Restic() {
		return engines.Ref(r.EngineRef)
	}
	return engines.Ref(r.Path)
}

// Matches reports whether listed version v is the recorded version r: the same restic snapshot,
// or the same rclone version path.
func (e *EngineVersions) Matches(v engines.StoredVersion, r EngineRow) bool {
	if e.Restic() {
		return r.EngineRef != "" && string(v.Ref) == r.EngineRef
	}
	return v.LogicalPath == r.Path
}

// RefIn returns the reference of recorded version r as listing l holds it (Ref when l does not
// list it).
func (e *EngineVersions) RefIn(l *Listing, r EngineRow) engines.Ref {
	if l != nil {
		if v, ok := e.Find(l, r); ok {
			return v.Ref
		}
	}
	return e.Ref(r)
}

// Put stores the staged version v (v.Dir holds manifest.json and the version's files) and then
// records it with record, which gets the reference Put returned (RowRef says what its row stores).
// record runs even while ctx is being cancelled (the version is complete at the destination); a
// crash between the two leaves an unrecorded version that the resumed job's Recover adopts. It
// returns the reference.
func (e *EngineVersions) Put(ctx context.Context, v engines.PutVersion, record func(ctx context.Context, ref engines.Ref) error) (engines.Ref, error) {
	ref, err := e.Store.Put(ctx, v)
	if err != nil {
		return "", fmt.Errorf("store the version %s: %w", v.LogicalPath, err)
	}
	faultinject.Point(PointVersionsAfterPut)
	faultinject.Point(PointVersionsBeforeRecord)
	if err := record(context.WithoutCancel(ctx), ref); err != nil {
		return "", err
	}
	return ref, nil
}

// Listing is the versions Recover found under a kind folder.
type Listing struct {
	versions []engines.StoredVersion
}

// Versions returns the listed versions.
func (l *Listing) Versions() []engines.StoredVersion { return slices.Clone(l.versions) }

// Add records a version stored after the listing (the job's own Put).
func (l *Listing) Add(v engines.StoredVersion) { l.versions = append(l.versions, v) }

// Find returns the listed version of a recorded one (Matches).
func (e *EngineVersions) Find(l *Listing, r EngineRow) (engines.StoredVersion, bool) {
	for _, v := range l.versions {
		if e.Matches(v, r) {
			return v, true
		}
	}
	return engines.StoredVersion{}, false
}

// Lost reports whether the recorded version r is gone from the destination: not listed (a restic
// snapshot forgotten or removed outside Bunkarr; an rclone version whose purge ran before its row
// was deleted) or listed without manifest.json (a purge that stopped). Its row then takes no
// retention slot; the runner removes it with a warning (the dropLost of Phases 1-2).
func (e *EngineVersions) Lost(l *Listing, r EngineRow) bool {
	v, ok := e.Find(l, r)
	return !ok || !v.Complete
}

// Taken reports whether a logical path is listed or recorded (recorded may be nil).
func (l *Listing) Taken(p string, recorded func(string) bool) bool {
	for _, v := range l.versions {
		if v.LogicalPath == p {
			return true
		}
	}
	return recorded != nil && recorded(p)
}

// FreePath returns the logical path of a new version made at t by job jobID in folder (the kind
// folder of an integration, or the manifests' root): folder/<VersionName(t)>, or
// folder/<VersionName(t)>-job<id> when that is taken (listed or recorded: two versions in one
// second). ErrNameTaken when both are.
func (l *Listing) FreePath(folder string, t time.Time, jobID int64, recorded func(string) bool) (string, error) {
	base := folder + "/" + VersionName(t)
	for _, p := range []string{base, base + "-job" + strconv.FormatInt(jobID, 10)} {
		if !l.Taken(p, recorded) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNameTaken, base)
}

// RecoverInput configures Recover.
type RecoverInput struct {
	// KindFolder is listed: ".bunkarr/plex", ".bunkarr/arr" or ".bunkarr/manifests".
	KindFolder string
	// JobID is the running job: on restic only its own unrecorded snapshots are adopted.
	JobID int64
	// Mine selects the listed versions this run settles (e.g. the folders of its integration);
	// nil selects all.
	Mine func(v engines.StoredVersion) bool
	// Recorded reports whether a listed version has a row (by its path or reference).
	Recorded func(v engines.StoredVersion) bool
	// Adopt checks an unrecorded complete version against its manifest (ReadFile; on restic it
	// must name this integration, application and job) and records it. An error leaves the
	// version alone, with a warning.
	Adopt func(ctx context.Context, v engines.StoredVersion) error
}

// Recover lists in.KindFolder and settles the versions without a row (§8.2, §8.3): an rclone
// version without manifest.json older than LeftoverAge is purged (younger ones are left: another
// run may still write them), a complete rclone version is adopted; a restic snapshot is adopted
// only when the running job made it (its bunkarr-job tag). It returns the listing, for the
// "unchanged" check, Lost and FreePath.
func (e *EngineVersions) Recover(ctx context.Context, in RecoverInput) (*Listing, error) {
	list, err := e.Store.List(ctx, in.KindFolder)
	if err != nil {
		return nil, fmt.Errorf("list the versions under %s: %w", in.KindFolder, err)
	}
	l := &Listing{}
	for _, v := range list {
		l.versions = append(l.versions, v)
		if (in.Mine != nil && !in.Mine(v)) || in.Recorded(v) {
			continue
		}
		switch {
		case e.Restic() && v.JobID != in.JobID:
			// Another job's snapshot without a row: a pruned version waiting for its forget, or
			// one of a job that did not resume. Left alone (retention decides).
			continue
		case !v.Complete && e.Restic():
			continue
		case !v.Complete:
			if age := e.now().Sub(v.Time); v.Time.IsZero() || age < LeftoverAge {
				e.info("An unfinished version is left alone: it may still be written", "path", v.LogicalPath)
				continue
			}
			if err := e.Store.Remove(ctx, nil, v.Ref, e.Kind); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				e.warn("The upload of an interrupted backup could not be removed", "path", v.LogicalPath, "error", err.Error())
				continue
			}
			l.drop(v.Ref)
			e.info("Removed the upload of an interrupted backup", "path", v.LogicalPath, "files", len(v.Files))
		default:
			if err := in.Adopt(ctx, v); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				e.warn("An unrecorded version was left alone", "path", v.LogicalPath, "reason", err.Error())
			}
		}
	}
	return l, nil
}

// List lists kindFolder without settling anything (a dry run writes nothing, S9).
func (e *EngineVersions) List(ctx context.Context, kindFolder string) (*Listing, error) {
	list, err := e.Store.List(ctx, kindFolder)
	if err != nil {
		return nil, fmt.Errorf("list the versions under %s: %w", kindFolder, err)
	}
	return &Listing{versions: list}, nil
}

// drop removes a version from the listing.
func (l *Listing) drop(ref engines.Ref) {
	l.versions = slices.DeleteFunc(l.versions, func(v engines.StoredVersion) bool { return v.Ref == ref })
}

// Remove deletes the recorded version r (pruned by the runner's retention math) and its row,
// deleted by deleteRow: on restic the row and the engine_forget request of its snapshot in one
// transaction (D28: the destination's retention job forgets it); on rclone the version first
// (VersionStore.Remove, fenced to version directories) and then the row, so a crash in between
// leaves a row whose version is gone (Lost). The row is deleted even while ctx is being cancelled
// once the version is gone.
func (e *EngineVersions) Remove(ctx context.Context, l *Listing, r EngineRow, deleteRow func(ctx context.Context, tx *sql.Tx) error) error {
	ref := e.RefIn(l, r)
	if ref == "" {
		return fmt.Errorf("remove the version %s: no reference recorded", r.Path)
	}
	if e.Restic() {
		err := e.DB.Write(ctx, func(tx *sql.Tx) error {
			if err := deleteRow(ctx, tx); err != nil {
				return err
			}
			return e.Store.Remove(ctx, tx, ref, e.Kind)
		})
		if err != nil {
			return fmt.Errorf("remove the version %s: %w", r.Path, err)
		}
	} else {
		if err := e.Store.Remove(ctx, nil, ref, e.Kind); err != nil {
			return fmt.Errorf("remove the version %s: %w", r.Path, err)
		}
		bg := context.WithoutCancel(ctx)
		if err := e.DB.Write(bg, func(tx *sql.Tx) error { return deleteRow(bg, tx) }); err != nil {
			return fmt.Errorf("remove the record of %s: %w", r.Path, err)
		}
	}
	if l != nil {
		l.drop(ref)
	}
	return nil
}

// ReadSmall reads one small file of a version (manifest.json, SHA256SUMS), at most limit bytes;
// a file longer than limit is an error.
func (e *EngineVersions) ReadSmall(ctx context.Context, ref engines.Ref, name string, limit int64) ([]byte, error) {
	b, err := e.Store.ReadFile(ctx, ref, name, limit+1)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return b, nil
}

// CheckSizes checks that a listed version is complete and holds every file of want with its size
// (the Phase 1 adopt rule and the engine "unchanged" check, §8.4 step 5; content is checked by the
// destination's verify job). It wraps ErrIncomplete.
func CheckSizes(v engines.StoredVersion, want map[string]int64) error {
	if !v.Complete {
		return fmt.Errorf("%w: no manifest.json", ErrIncomplete)
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		size, ok := v.Files[name]
		switch {
		case !ok:
			return fmt.Errorf("%w: no %s", ErrIncomplete, name)
		case want[name] >= 0 && size != want[name]:
			return fmt.Errorf("%w: %s has %d bytes, %d recorded", ErrIncomplete, name, size, want[name])
		}
	}
	return nil
}

// FolderOf returns the folder of a version's logical path ("<root>/<folder>/<version>" →
// "<folder>"), or "" when it has none.
func FolderOf(logicalPath string) string {
	dir := path.Dir(logicalPath)
	if dir == "." || dir == "/" {
		return ""
	}
	return path.Base(dir)
}
