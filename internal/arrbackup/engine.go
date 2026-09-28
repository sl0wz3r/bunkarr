package arrbackup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// The engine path (docs/design/phase4.md §8.4): an *arr backup version at a restic or rclone
// destination. Choosing, fetching and verifying the backup are the filecopy path's; the version
// (the zip and manifest.json) is stored through the destination's VersionStore
// (snapshots.EngineVersions):
//
//   - Preflight: the destination is enabled; the zip holds the *arr's API key and passwords, so a
//     destination whose kind is not local must be encrypted, an encrypted one keeps modes, and
//     acceptInsecureModes counts only for this target (S17 for engines); the recovery kit custody
//     is confirmed (S21; not for a dry run); the version store opens (S25). No transfer window;
//     the bandwidth limits apply through the opener's runtime.
//   - Recovery: rclone uploads without manifest.json older than 24 h are purged; a complete
//     unrecorded version of this integration is adopted when its manifest is this application's
//     and its files list with the manifest's sizes (on restic only this job's own snapshot).
//   - "Unchanged": a recorded ok copy of the chosen scheduled backup counts only when the
//     destination lists it with manifest.json and the zip at its size, and manifest.json reads
//     back and names the recorded zip (name, size, sha256); otherwise it is marked failed and the
//     backup is copied again. Content is checked by the destination's verify job.
//   - Pruning keeps the keep math: restic rows and forget requests in one transaction, rclone
//     versions purged before their rows.

// openEngine is runJob's preflight of an engine destination (see above). It returns the function
// that closes the version store.
func (w *run) openEngine(ctx context.Context, destID int64, accept bool) (func(), error) {
	d, err := w.r.destinations.Get(ctx, destID)
	if err != nil {
		return nil, err
	}
	w.dest = d
	if !d.Enabled {
		return nil, fmt.Errorf("destination %q is disabled", d.Name)
	}
	if err := integrations.CheckBackupDestination(w.app, d.Kind, d.Encryption.Mode, d.Capabilities.EnforcesModes, accept); err != nil {
		return nil, fmt.Errorf("destination %q: %w", d.Name, err)
	}
	if blocked := destinations.Blocked(d); blocked != "" && !w.job.DryRun {
		return nil, fmt.Errorf("destination %q: %s", d.Name, blocked)
	}
	if w.r.openVersions == nil {
		return nil, fmt.Errorf("destination %q is a %s destination, and no engine runtime is available", d.Name, d.Engine)
	}
	vs, closer, err := w.r.openVersions(ctx, d.ID, engines.Runtime{JobID: w.job.ID, DryRun: w.job.DryRun, Reporter: w.rep,
		Now: w.r.now, Location: w.r.loc, Log: w.r.log})
	if err != nil {
		return nil, fmt.Errorf("destination %q: %w", d.Name, err)
	}
	w.ev = &snapshots.EngineVersions{DB: w.r.database, Store: vs, Engine: engines.Kind(d.Engine), Kind: engines.VersionArr, Now: w.r.now,
		Info: func(msg string, args ...any) { w.rep.Log(slog.LevelInfo, msg, args...) }, Warn: w.warn}
	w.stats.Engine = d.Engine
	return func() {
		if err := closer.Close(); err != nil {
			w.r.log.Warn("Could not close a destination's version store", "destination", d.Name, "error", err)
		}
	}, nil
}

// engineRow is the EngineVersions view of a row.
func engineRow(s snapshots.Snapshot) snapshots.EngineRow {
	return snapshots.EngineRow{Path: s.Path, EngineRef: s.EngineRef}
}

// intactEngine is intactCopy at an engine destination: the version is listed with manifest.json
// and the zip at its recorded size, and manifest.json reads back and names the recorded zip.
func (w *run) intactEngine(ctx context.Context, s snapshots.Snapshot, m Manifest) error {
	v, ok := w.ev.Find(w.listing, engineRow(s))
	if !ok {
		return errNotThere
	}
	name, err := manifestZipName(m)
	if err != nil {
		return err
	}
	if err := snapshots.CheckSizes(v, map[string]int64{ManifestName: -1, name: m.Zip.Size}); err != nil {
		return err
	}
	raw, err := w.ev.ReadSmall(ctx, v.Ref, ManifestName, maxManifestBytes)
	if err != nil {
		return fmt.Errorf("read back %s: %w", ManifestName, err)
	}
	var got Manifest
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("%w: %s does not parse", snapshots.ErrIncomplete, ManifestName)
	}
	if got.Zip != m.Zip || got.IntegrationID != m.IntegrationID || got.App != m.App {
		return fmt.Errorf("%w: %s names another zip than recorded", snapshots.ErrIncomplete, ManifestName)
	}
	return nil
}

// backupEngine is backup at an engine destination (see above and Run).
func (w *run) backupEngine(ctx context.Context) (res jobs.Result, err error) {
	staging := w.r.StagingDir(w.job.ID)
	defer func() {
		if err != nil {
			if rerr := os.RemoveAll(staging); rerr != nil {
				w.r.log.Error("Could not remove an *arr backup staging directory", "path", staging, "error", rerr)
			}
		}
	}()
	if err := os.RemoveAll(staging); err != nil {
		return jobs.Result{}, fmt.Errorf("remove the staging directory of an earlier attempt: %w", err)
	}
	own, err := w.recoverEngine(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	if own != nil {
		return w.finishRecovered(ctx, *own)
	}
	if w.resumeCmd, err = w.interruptedCommand(ctx); err != nil {
		return jobs.Result{}, err
	}
	if err := w.items.DeleteItems(ctx, w.job.ID); err != nil {
		return jobs.Result{}, err
	}
	status, err := w.client.Status(ctx)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("%s %q: %w", w.app, w.it.Name, err)
	}
	c, err := w.choose(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	b := c.b
	w.stats.BackupName, w.stats.BackupType, w.stats.ReusedScheduled = b.Name, b.Type, c.reused
	if c.unchanged != nil {
		return w.finishUnchanged(ctx, b, *c.unchanged)
	}
	if b.Size > MaxBackupBytes {
		return jobs.Result{}, fmt.Errorf("%s's backup %s is %s, more than the %s Bunkarr accepts", w.app, b.Name,
			formatBytes(b.Size), formatBytes(MaxBackupBytes))
	}
	if err := w.prepareStaging(staging, b.Size); err != nil {
		return jobs.Result{}, err
	}
	w.rep.Progress(jobs.Progress{Phase: "fetching", FilesTotal: 1, BytesTotal: b.Size, CurrentFile: b.Name})
	st, err := w.fetchBackup(ctx, b, staging)
	if err != nil {
		return jobs.Result{}, err
	}
	faultinject.Point(PointAfterStage)
	w.rep.Progress(jobs.Progress{Phase: "verifying", FilesTotal: 1, BytesTotal: st.size, CurrentFile: b.Name})
	report, err := VerifyZip(ctx, st.path, string(w.kind), staging)
	if err != nil {
		return jobs.Result{}, err
	}
	integrity := report.Integrity()
	if report.OK() {
		w.rep.Log(slog.LevelInfo, "The backup passed verification", "backup", b.Name, "entries", len(report.Entries))
	} else {
		w.rep.Log(slog.LevelError, "The backup failed verification", "backup", b.Name, "zip", integrity.Zip,
			"database", integrity.Database, "problems", strings.Join(report.Problems, "; "))
	}
	created := w.r.now().UTC()
	rows, err := w.r.store.List(ctx, w.dest.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	recorded := map[string]bool{}
	for _, r := range rows {
		recorded[r.Path] = true
	}
	logical, err := w.listing.FreePath(w.folder, created, w.job.ID, func(p string) bool { return recorded[p] })
	if err != nil {
		return jobs.Result{}, err
	}
	detail := itemDetail{Method: w.fetch, BackupName: b.Name, BackupType: b.Type, BackupTime: b.Time, Size: st.size,
		SHA256: st.sha256, Destination: logical + "/" + b.Name}
	item, err := w.addItem(ctx, jobs.Item{RelPath: relOf(b), Action: jobs.ActionBackup, Status: jobs.ItemPending, Bytes: st.size,
		Detail: detail.json()})
	if err != nil {
		return jobs.Result{}, err
	}
	m := Manifest{Format: ManifestFormat, CreatedAt: created, App: string(w.kind), AppVersion: status.Version,
		IntegrationID: w.it.ID, IntegrationName: w.it.Name, JobID: w.job.ID, JobQueuedAt: w.job.QueuedAt,
		Method: snapshotMethod(w.fetch), Backup: ManifestBackup{Name: b.Name, Type: b.Type, Time: b.Time},
		Zip: ManifestZip{Name: b.Name, Size: st.size, SHA256: st.sha256}, Entries: report.Entries, Integrity: integrity,
		Sensitive: true, Warnings: w.noted}
	if m.Entries == nil {
		m.Entries = []ZipEntry{}
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return jobs.Result{}, fmt.Errorf("encode the manifest: %w", err)
	}
	versionDir := filepath.Join(staging, "version")
	if err := os.Mkdir(versionDir, dirPerm); err != nil {
		return jobs.Result{}, fmt.Errorf("create the version directory: %w", err)
	}
	if err := os.Rename(st.path, filepath.Join(versionDir, b.Name)); err != nil {
		return jobs.Result{}, fmt.Errorf("stage %s: %w", b.Name, err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, ManifestName), append(manifest, '\n'), filePerm); err != nil {
		return jobs.Result{}, fmt.Errorf("write the manifest: %w", err)
	}
	w.rep.Progress(jobs.Progress{Phase: "uploading", FilesTotal: 1, BytesTotal: st.size, CurrentFile: b.Name})
	var snap snapshots.Snapshot
	ref, err := w.ev.Put(ctx, engines.PutVersion{Kind: engines.VersionArr, LogicalPath: logical, Dir: versionDir, Time: created,
		JobID: w.job.ID, IntegrationID: w.it.ID}, func(ctx context.Context, ref engines.Ref) error {
		var err error
		snap, err = w.record(ctx, snapshots.Snapshot{DestinationID: w.dest.ID, Kind: snapshots.KindArr, IntegrationID: w.it.ID,
			JobID: w.job.ID, Path: logical, CreatedAt: created, Size: st.size, Method: m.Method, Integrity: integrity.Result(),
			Manifest: manifest, EngineRef: w.ev.RowRef(ref)})
		return err
	})
	if err != nil {
		return jobs.Result{}, err
	}
	w.listing.Add(engines.StoredVersion{Ref: ref, LogicalPath: logical, Version: path.Base(logical), Time: created, Complete: true,
		Files: map[string]int64{b.Name: st.size, ManifestName: int64(len(manifest) + 1)}, JobID: w.job.ID, IntegrationID: w.it.ID})
	if item.ID != 0 {
		if err := w.items.Finish(context.WithoutCancel(ctx), item.ID, jobs.ItemDone, st.size, ""); err != nil {
			return jobs.Result{}, err
		}
	}
	if err := os.RemoveAll(staging); err != nil {
		w.r.log.Error("Could not remove an *arr backup staging directory", "path", staging, "error", err)
	}
	w.rep.Log(slog.LevelInfo, "Recorded the "+w.app+" backup version", "path", logical, "integrity", snap.Integrity, "size", st.size,
		"ref", string(ref))
	w.stats.Bytes, w.stats.Integrity = st.size, snap.Integrity
	w.stats.SnapshotID, w.stats.Path, w.stats.EngineRef = snap.ID, snap.Path, string(ref)
	if snap.Integrity != IntegrityOK {
		return w.failedVerification(report.Problems)
	}
	w.prune(ctx)
	return w.result(w.summary()), nil
}

// recoverEngine settles what interrupted jobs of this integration left at the engine destination
// and returns this job's own recorded version when an earlier attempt wrote it completely.
func (w *run) recoverEngine(ctx context.Context) (*snapshots.Snapshot, error) {
	rows, err := w.r.store.List(ctx, w.dest.ID)
	if err != nil {
		return nil, err
	}
	w.listing, err = w.ev.Recover(ctx, snapshots.RecoverInput{
		KindFolder: ArrRoot,
		JobID:      w.job.ID,
		Mine: func(v engines.StoredVersion) bool {
			return snapshots.FolderIntegrationID(snapshots.FolderOf(v.LogicalPath)) == w.it.ID
		},
		Recorded: func(v engines.StoredVersion) bool {
			for _, r := range rows {
				if w.ev.Matches(v, engineRow(r)) || r.Path == v.LogicalPath {
					return true
				}
			}
			return false
		},
		Adopt: w.adoptEngine,
	})
	if err != nil {
		return nil, err
	}
	if !w.resumed() {
		return nil, nil
	}
	snaps, err := w.r.store.ListFor(ctx, snapshots.KindArr, w.dest.ID, w.it.ID)
	if err != nil {
		return nil, err
	}
	for _, s := range snaps {
		if !ownManifest(s, w.job) || w.ev.Lost(w.listing, engineRow(s)) {
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

// adoptEngine records an unrecorded, complete version (see adopt): its manifest is of this
// integration and application, in this integration's folder or written by a job of it here (on
// restic: by this job), and its files list with the manifest's sizes; otherwise it is recorded
// as failed.
func (w *run) adoptEngine(ctx context.Context, v engines.StoredVersion) error {
	raw, err := w.ev.ReadSmall(ctx, v.Ref, ManifestName, maxManifestBytes)
	if err != nil {
		return fmt.Errorf("no readable manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("the manifest is not valid: %w", err)
	}
	if m.IntegrationID != w.it.ID {
		return fmt.Errorf("the manifest names integration %d", m.IntegrationID)
	}
	if m.App != string(w.kind) {
		return fmt.Errorf("the manifest is of a %q backup, not %s (an integration of an earlier Bunkarr database had this id?)",
			truncate(m.App, 32), w.kind)
	}
	if w.ev.Restic() && (m.JobID != w.job.ID || !m.JobQueuedAt.Equal(w.job.QueuedAt)) {
		return fmt.Errorf("the manifest names job %d, not this job", m.JobID)
	}
	if path.Dir(v.LogicalPath) != w.folder {
		here, err := w.writtenHere(ctx, m)
		if err != nil {
			return err
		}
		if !here {
			return fmt.Errorf("it is not in %s, the folder of %s %q, and its job is not one of this integration", w.folder, w.app, w.it.Name)
		}
	}
	name, err := manifestZipName(m)
	if err != nil {
		return err
	}
	integrity := m.Integrity.Result()
	if err := snapshots.CheckSizes(v, map[string]int64{ManifestName: -1, name: m.Zip.Size}); err != nil {
		integrity = IntegrityFailed
		w.rep.Log(slog.LevelWarn, "The zip of an unrecorded *arr backup version does not match its manifest", "path", v.LogicalPath, "error", err.Error())
	}
	method := m.Method
	if method != MethodFolder && method != MethodHTTP {
		method = MethodHTTP
	}
	createdAt := m.CreatedAt
	if createdAt.IsZero() {
		createdAt = v.Time
	}
	if _, err := w.r.store.Insert(context.WithoutCancel(ctx), snapshots.Snapshot{DestinationID: w.dest.ID, Kind: snapshots.KindArr,
		IntegrationID: w.it.ID, JobID: w.job.ID, Path: v.LogicalPath, CreatedAt: createdAt, Size: m.Zip.Size, Method: method,
		Integrity: integrity, Manifest: raw, EngineRef: w.ev.RowRef(v.Ref)}); err != nil {
		return err
	}
	w.stats.Recovered++
	w.rep.Log(slog.LevelInfo, "Recorded an *arr backup version an interrupted backup had written", "path", v.LogicalPath,
		"integrity", integrity, "writtenByJob", m.JobID)
	return nil
}

// pruneEngine applies the version retention (arrDaily, arrWeekly) at the engine destination:
// rows whose version is gone are dropped (Lost), then the versions snapshots.Prune selects are
// removed with their rows. Problems are warnings.
func (w *run) pruneEngine(ctx context.Context) {
	snaps, err := w.r.store.ListFor(ctx, snapshots.KindArr, w.dest.ID, w.it.ID)
	if err != nil {
		w.warn("Old *arr backup versions were not pruned", "error", err.Error())
		return
	}
	cands := make([]snapshots.Version, 0, len(snaps))
	byID := map[int64]snapshots.Snapshot{}
	for _, s := range snaps {
		if w.ev.Lost(w.listing, engineRow(s)) {
			if err := w.r.store.Remove(ctx, s.ID); err != nil {
				w.warn("An *arr backup version missing at the destination is still recorded: its record could not be removed", "path", s.Path,
					"error", err.Error())
				continue
			}
			w.warn("A recorded *arr backup version was missing or incomplete at the destination; its record was removed", "path", s.Path,
				"createdAt", s.CreatedAt)
			continue
		}
		cands = append(cands, s.Version())
		byID[s.ID] = s
	}
	for _, id := range snapshots.Prune(cands, w.dest.Retention.ArrDaily, w.dest.Retention.ArrWeekly, w.r.now(), w.r.loc) {
		if ctx.Err() != nil {
			return
		}
		s := byID[id]
		err := w.ev.Remove(ctx, w.listing, engineRow(s), func(ctx context.Context, tx *sql.Tx) error { return snapshots.RemoveTx(ctx, tx, id) })
		if err != nil {
			w.warn("An old *arr backup version could not be deleted", "path", s.Path, "error", err.Error())
			continue
		}
		w.stats.VersionsPruned++
		w.rep.Log(slog.LevelInfo, "Pruned an old *arr backup version", "path", s.Path, "createdAt", s.CreatedAt, "integrity", s.Integrity)
	}
}
