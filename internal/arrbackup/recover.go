package arrbackup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// recover cleans up after interrupted jobs of this integration (Run step 2), as plexdb does for
// Plex DB versions (phase1.md §5). It returns this job's own version when an earlier attempt had
// written it completely (ownVersion).
func (w *run) recover(ctx context.Context) (*snapshots.Snapshot, error) {
	root := w.h.Root
	folders, err := readDirNames(root, ArrRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", ArrRoot, err)
	}
	if err := snapshots.RealDirs(root, ArrRoot); err != nil {
		return nil, fmt.Errorf("check %s: %w", ArrRoot, err)
	}
	for _, folder := range folders {
		if snapshots.FolderIntegrationID(folder) != w.it.ID {
			continue
		}
		fdir := ArrRoot + "/" + folder
		if fi, err := root.Lstat(fdir); err != nil || !fi.IsDir() {
			continue
		}
		names, err := readDirNames(root, fdir)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", fdir, err)
		}
		for _, name := range names {
			rel := fdir + "/" + name
			if pruned, ok := snapshots.PrunedVersion(name); ok {
				// A version a prune was deleting. While its row exists, the next prune deletes it or
				// restores it; without one, the prune had deleted the row and stopped before the
				// files.
				recorded, err := w.r.store.Recorded(ctx, w.h.Destination.ID, fdir+"/"+pruned)
				if err != nil {
					return nil, err
				}
				if recorded {
					continue
				}
				if err := removeTree(root, rel); err != nil {
					w.warn("The rest of a pruned *arr backup version could not be deleted", "path", rel, "error", err.Error())
					continue
				}
				w.rep.Log(slog.LevelInfo, "Removed the rest of an *arr backup version an interrupted prune was deleting", "path", rel)
				continue
			}
			switch {
			case snapshots.IsPartialName(name):
				// A version an interrupted job of this integration was writing; jobs of one
				// integration never run at the same time, so no job is writing it now.
				if err := removeTree(root, rel); err != nil {
					return nil, err
				}
				w.rep.Log(slog.LevelInfo, "Removed the unfinished version of an interrupted backup", "path", rel)
			case snapshots.IsVersionName(name):
				recorded, err := w.r.store.Recorded(ctx, w.h.Destination.ID, rel)
				if err != nil {
					return nil, err
				}
				if recorded {
					continue
				}
				if fi, err := root.Lstat(rel); err != nil || !fi.IsDir() {
					w.warn("An unrecorded entry in the *arr backup versions was left alone", "path", rel, "reason", "not a directory")
					continue
				}
				if _, err := w.adopt(ctx, rel); err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					w.warn("An unrecorded *arr backup version was left alone", "path", rel, "reason", err.Error())
				}
			}
		}
	}
	return w.ownVersion(ctx)
}

// ownVersion returns the version an earlier attempt of this job wrote completely and that is
// recorded now: its manifest names this job (ownManifest) and its directory exists. It returns nil
// for a job that is not resumed, when there is none, and when a version recorded as failed has a
// manifest that says ok (its files did not match it: the backup starts over).
func (w *run) ownVersion(ctx context.Context) (*snapshots.Snapshot, error) {
	if w.job.Trigger != jobs.TriggerResume && w.job.Attempt <= 1 {
		return nil, nil
	}
	snaps, err := w.r.store.ListFor(ctx, snapshots.KindArr, w.h.Destination.ID, w.it.ID)
	if err != nil {
		return nil, err
	}
	for _, s := range snaps {
		if !ownManifest(s, w.job) {
			continue
		}
		if fi, err := w.h.Root.Lstat(s.Path); err != nil || !fi.IsDir() {
			continue
		}
		var m Manifest
		if s.Integrity != IntegrityOK && (json.Unmarshal(s.Manifest, &m) != nil || m.Integrity.Result() == IntegrityOK) {
			continue
		}
		return &s, nil
	}
	return nil, nil
}

// ownManifest reports whether a recorded version's manifest names job as its writer: its id and
// its queue time.
func ownManifest(s snapshots.Snapshot, job jobs.Job) bool {
	var m struct {
		JobID       int64     `json:"jobId"`
		JobQueuedAt time.Time `json:"jobQueuedAt"`
	}
	return json.Unmarshal(s.Manifest, &m) == nil && m.JobID == job.ID && m.JobQueuedAt.Equal(job.QueuedAt)
}

// manifestZipName returns the zip file a manifest names after checking that it is a plain backup
// name (so a manifest read from the destination can only name a file in its own directory).
func manifestZipName(m Manifest) (string, error) {
	if m.Zip.Name == "" {
		return "", errors.New("the manifest names no zip file")
	}
	if err := arr.ValidateBackup(arr.Backup{Type: arr.BackupManual, Name: m.Zip.Name}); err != nil {
		return "", fmt.Errorf("the manifest names an unexpected file: %w", err)
	}
	return m.Zip.Name, nil
}

// adopt verifies an unrecorded, complete version directory against its manifest and records it:
// integrity ok only when the manifest says ok and the zip has its recorded size and sha256.
//
// Integration ids start over with a new Bunkarr database, so a folder with this integration's id
// may hold the versions of an integration that no longer exists; once recorded, this
// integration's retention would prune them. A version is recorded only when its manifest is of
// this integration's application and it lies in this integration's folder (FolderName), or its
// manifest names a job of this integration in this database (writtenHere: it was written before
// the integration was renamed).
func (w *run) adopt(ctx context.Context, rel string) (snapshots.Snapshot, error) {
	raw, err := readSmallFile(w.h.Root, rel+"/"+ManifestName, maxManifestBytes)
	if err != nil {
		return snapshots.Snapshot{}, fmt.Errorf("no readable manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return snapshots.Snapshot{}, fmt.Errorf("the manifest is not valid: %w", err)
	}
	if m.IntegrationID != w.it.ID {
		return snapshots.Snapshot{}, fmt.Errorf("the manifest names integration %d", m.IntegrationID)
	}
	if m.App != string(w.kind) {
		return snapshots.Snapshot{}, fmt.Errorf("the manifest is of a %q backup, not %s (an integration of an earlier Bunkarr database had this id?)",
			truncate(m.App, 32), w.kind)
	}
	if folder := path.Dir(rel); folder != w.folder {
		here, err := w.writtenHere(ctx, m)
		if err != nil {
			return snapshots.Snapshot{}, err
		}
		if !here {
			return snapshots.Snapshot{}, fmt.Errorf("it is not in %s, the folder of %s %q, and its job is not one of this integration "+
				"(an integration of an earlier Bunkarr database had this id?)", w.folder, w.app, w.it.Name)
		}
	}
	name, err := manifestZipName(m)
	if err != nil {
		return snapshots.Snapshot{}, err
	}
	integrity := m.Integrity.Result()
	if _, err := filecopy.VerifyFile(ctx, w.h.Root, rel+"/"+name, m.Zip.Size, filecopy.HashPrefix+m.Zip.SHA256, nil); err != nil {
		if ctx.Err() != nil {
			return snapshots.Snapshot{}, ctx.Err()
		}
		integrity = IntegrityFailed
		w.rep.Log(slog.LevelWarn, "The zip of an unrecorded *arr backup version does not match its manifest", "path", rel, "error", err.Error())
	}
	method := m.Method
	if method != MethodFolder && method != MethodHTTP {
		method = MethodHTTP
	}
	createdAt := m.CreatedAt
	if createdAt.IsZero() {
		createdAt = w.r.now()
	}
	snap, err := w.r.store.Insert(context.WithoutCancel(ctx), snapshots.Snapshot{DestinationID: w.h.Destination.ID, Kind: snapshots.KindArr,
		IntegrationID: w.it.ID, JobID: w.job.ID, Path: rel, CreatedAt: createdAt, Size: m.Zip.Size, Method: method,
		Integrity: integrity, Manifest: raw})
	if err != nil {
		return snapshots.Snapshot{}, err
	}
	w.stats.Recovered++
	w.rep.Log(slog.LevelInfo, "Recorded an *arr backup version an interrupted backup had written", "path", rel,
		"integrity", integrity, "writtenByJob", m.JobID)
	return snap, nil
}

// writtenHere reports whether manifest m names a job of this Bunkarr database that backed up this
// integration: this very job, or an arr_backup job of this integration with m's id and queue time
// in the jobs table. Job ids start over with a new database, so the queue time tells a job of a
// lost database apart. A job deleted with the job history is not found.
func (w *run) writtenHere(ctx context.Context, m Manifest) (bool, error) {
	switch {
	case m.JobID <= 0 || m.JobQueuedAt.IsZero():
		return false, nil
	case m.JobID == w.job.ID:
		return m.JobQueuedAt.Equal(w.job.QueuedAt), nil
	}
	var queued string
	err := w.r.database.Reader().QueryRowContext(ctx, `SELECT queued_at FROM jobs WHERE id = ? AND type = ? AND integration_id = ?`,
		m.JobID, string(jobs.TypeArrBackup), w.it.ID).Scan(&queued)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up job %d: %w", m.JobID, err)
	}
	t, err := db.ParseTime(queued)
	if err != nil {
		return false, fmt.Errorf("job %d: queued_at: %w", m.JobID, err)
	}
	return t.Equal(m.JobQueuedAt), nil
}

// finishRecovered completes a job whose version an earlier attempt had written completely, as that
// attempt would have: with its warnings; a version that failed verification fails the job with
// ErrIntegrity, an ok one is followed by pruning.
func (w *run) finishRecovered(ctx context.Context, snap snapshots.Snapshot) (jobs.Result, error) {
	w.rep.Log(slog.LevelInfo, "An earlier attempt of this job wrote its *arr backup version; finishing with it", "path", snap.Path,
		"integrity", snap.Integrity)
	var m Manifest
	_ = json.Unmarshal(snap.Manifest, &m)
	for _, msg := range m.Warnings {
		w.warn(msg, "path", snap.Path)
	}
	w.stats.BackupName, w.stats.BackupType = m.Backup.Name, m.Backup.Type
	w.stats.ReusedScheduled = m.Backup.Type == arr.BackupScheduled
	w.stats.Bytes, w.stats.Integrity = snap.Size, snap.Integrity
	w.stats.SnapshotID, w.stats.Path = snap.ID, snap.Path
	if snap.Integrity != IntegrityOK {
		return w.failedVerification([]string{fmt.Sprintf("zip %s, database %s", m.Integrity.Zip, m.Integrity.Database)})
	}
	w.prune(ctx)
	return w.result(w.summary()), nil
}

// prune applies the version retention (arrDaily, arrWeekly) to this integration's versions at the
// destination after a successful backup; rows of versions gone from the destination are removed
// first (dropLost). Problems are warnings: the backup itself succeeded.
func (w *run) prune(ctx context.Context) {
	if err := w.h.Recheck(); err != nil {
		w.warn("Old *arr backup versions were not pruned", "error", err.Error())
		return
	}
	snaps, err := w.r.store.ListFor(ctx, snapshots.KindArr, w.h.Destination.ID, w.it.ID)
	if err != nil {
		w.warn("Old *arr backup versions were not pruned", "error", err.Error())
		return
	}
	cands := make([]snapshots.Version, 0, len(snaps))
	byID := map[int64]snapshots.Snapshot{}
	present := make([]snapshots.Snapshot, 0, len(snaps))
	for _, s := range snaps {
		if w.dropLost(ctx, s) {
			continue
		}
		cands = append(cands, s.Version())
		byID[s.ID] = s
		present = append(present, s)
	}
	remove := snapshots.Prune(cands, w.h.Retention.ArrDaily, w.h.Retention.ArrWeekly, w.r.now(), w.r.loc)
	removeSet := map[int64]bool{}
	for _, id := range remove {
		removeSet[id] = true
	}
	for _, s := range present {
		if removeSet[s.ID] {
			continue
		}
		if restored, err := restoreVersion(w.h.Root, s.Path, s.Integrity, s.Manifest); err != nil {
			w.warn("A kept *arr backup version could not be restored from an interrupted prune", "path", s.Path, "error", err.Error())
		} else if restored {
			w.rep.Log(slog.LevelInfo, "Restored a kept *arr backup version from an interrupted prune", "path", s.Path)
		}
	}
	for _, id := range remove {
		if ctx.Err() != nil {
			return
		}
		s := byID[id]
		// The version leaves its name, then its row, then its files (snapshots.Layout.TrashVersion).
		trash, err := layout.TrashVersion(w.h.Root, s.Path)
		if err != nil {
			w.warn("An old *arr backup version could not be deleted", "path", s.Path, "error", err.Error())
			continue
		}
		faultinject.Point(PointPruneAfterTrash)
		// The version has left its name: drop its row even if the job is being cancelled.
		if err := w.r.store.Remove(context.WithoutCancel(ctx), id); err != nil {
			w.warn("An old *arr backup version was not deleted: its record could not be removed", "path", s.Path, "error", err.Error())
			continue
		}
		faultinject.Point(PointPruneAfterUnrecord)
		w.stats.VersionsPruned++
		w.rep.Log(slog.LevelInfo, "Pruned an old *arr backup version", "path", s.Path, "createdAt", s.CreatedAt, "integrity", s.Integrity)
		if err := snapshots.RemoveTrash(w.h.Root, trash); err != nil {
			w.warn("The files of a pruned *arr backup version could not be deleted; the next backup retries", "path", trash, "error", err.Error())
			continue
		}
		faultinject.Point(PointPruneAfterRemove)
	}
}

// dropLost removes the row of a version that is gone from the destination (versionLost), so it
// takes no retention slot, then what is left of its ".prune-*" directory. It reports whether it
// removed the row.
func (w *run) dropLost(ctx context.Context, s snapshots.Snapshot) bool {
	if lost, err := versionLost(w.h.Root, s.Path, s.Integrity, s.Manifest); err != nil || !lost {
		return false
	}
	if err := w.r.store.Remove(ctx, s.ID); err != nil {
		w.warn("An *arr backup version missing at the destination is still recorded: its record could not be removed", "path", s.Path,
			"error", err.Error())
		return false
	}
	w.warn("A recorded *arr backup version was missing or incomplete at the destination; its record was removed", "path", s.Path,
		"createdAt", s.CreatedAt)
	if err := snapshots.RemoveTrash(w.h.Root, snapshots.TrashPath(s.Path)); err != nil {
		w.warn("The rest of an *arr backup version could not be deleted; the next backup retries", "path", snapshots.TrashPath(s.Path),
			"error", err.Error())
	}
	return true
}

// restoreVersion moves a kept version back from its ".prune-*" name (snapshots.Layout).
func restoreVersion(root *os.Root, rel, integrity string, manifest json.RawMessage) (bool, error) {
	return layout.RestoreVersion(root, rel, completeCheck(root, manifest, integrity))
}

// versionLost reports whether the recorded version rel is gone from the destination
// (snapshots.Layout.VersionLost).
func versionLost(root *os.Root, rel, integrity string, manifest json.RawMessage) (bool, error) {
	return layout.VersionLost(root, rel, completeCheck(root, manifest, integrity))
}

// completeCheck is checkComplete for a recorded version: the zip's size is compared only for a
// version whose integrity is ok.
func completeCheck(root *os.Root, manifest json.RawMessage, integrity string) func(dir string) error {
	return func(dir string) error { return checkComplete(root, dir, manifest, integrity == IntegrityOK) }
}

// checkComplete checks that dir holds manifest.json and the manifest's zip, with its recorded size
// when sizes is set (snapshots.ErrIncomplete otherwise).
func checkComplete(root *os.Root, dir string, manifest json.RawMessage, sizes bool) error {
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return fmt.Errorf("the recorded manifest is not valid: %w", err)
	}
	name, err := manifestZipName(m)
	if err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		size int64
	}{{ManifestName, -1}, {name, m.Zip.Size}} {
		fi, err := root.Lstat(dir + "/" + f.name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("%w: no %s", snapshots.ErrIncomplete, f.name)
		case err != nil:
			return err
		case !fi.Mode().IsRegular() || (sizes && f.size >= 0 && fi.Size() != f.size):
			return fmt.Errorf("%w: %s is not the recorded file", snapshots.ErrIncomplete, f.name)
		}
	}
	return nil
}

// removeTree removes the directory rel inside the *arr tree (every directory on the way must be
// a real directory); a missing rel is not an error.
func removeTree(root *os.Root, rel string) error {
	if !strings.HasPrefix(rel, ArrRoot+"/") {
		return fmt.Errorf("refusing to remove %q: outside %s", rel, ArrRoot)
	}
	fi, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove %s: %w", rel, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("remove %s: not a directory", rel)
	}
	if err := snapshots.RealDirs(root, rel); err != nil {
		return fmt.Errorf("remove %s: %w", rel, err)
	}
	if err := root.RemoveAll(rel); err != nil {
		return fmt.Errorf("remove %s: %w", rel, err)
	}
	return nil
}

// readDirNames lists a directory inside root.
func readDirNames(root *os.Root, rel string) ([]string, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// readSmallFile reads the regular file rel (never through a final symlink), at most limit bytes.
func readSmallFile(root *os.Root, rel string, limit int64) ([]byte, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, filecopy.ErrNotRegular
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return data, nil
}
