package plexdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// The engine path (docs/design/phase4.md §8.4): a Plex DB version at a restic or rclone
// destination. Backup, staging and verification are the filecopy path's; the version is stored
// through the destination's VersionStore (snapshots.EngineVersions) instead of copied under an
// os.Root:
//
//  1. Preflight: the destination is enabled and holds Plex's credentials only encrypted (S17 for
//     engines: a destination whose kind is not local must be encrypted; an encrypted one keeps
//     modes; acceptInsecureModes counts only for this target), its recovery kit custody is
//     confirmed (S21; not for a dry run), and the version store opens (S25). The transfer window
//     does not apply; the bandwidth limits do (the opener's runtime).
//  2. Recovery: rclone leftovers without manifest.json older than 24 h are purged, complete
//     unrecorded versions of this integration adopted after their files list with the manifest's
//     sizes; on restic only this job's own snapshot is adopted. A resumed job whose version is
//     recorded finishes with it.
//  3. Backup and Verify into <config>/staging/plexdb-job<id>/; the files and manifest.json go into
//     its version/ directory, which VersionStore.Put stores (read back complete) under
//     .bunkarr/plex/<folder>/<version>; then the row, with engine_ref on restic.
//  4. Pruning with the unchanged keep math: restic rows are deleted with their forget request in
//     one transaction; rclone versions are purged before their rows.
//
// A dry run runs the preflight (Put nothing) and lists the files it would back up.

// runEngine is Run's engine path for destination destID.
func (w *run) runEngine(ctx context.Context, destID int64) (jobs.Result, error) {
	d, err := w.r.destinations.Get(ctx, destID)
	if err != nil {
		return jobs.Result{}, err
	}
	w.dest = d
	if !d.Enabled {
		return jobs.Result{}, fmt.Errorf("destination %q is disabled", d.Name)
	}
	target, _ := w.ps.Backup.TargetFor(destID)
	if err := integrations.CheckBackupDestination("Plex", d.Kind, d.Encryption.Mode, d.Capabilities.EnforcesModes,
		target.AcceptInsecureModes && target.DestinationID == destID); err != nil {
		return jobs.Result{}, fmt.Errorf("destination %q: %w", d.Name, err)
	}
	if blocked := destinations.Blocked(d); blocked != "" && !w.job.DryRun {
		return jobs.Result{}, fmt.Errorf("destination %q: %s", d.Name, blocked)
	}
	if w.r.openVersions == nil {
		return jobs.Result{}, fmt.Errorf("destination %q is a %s destination, and no engine runtime is available", d.Name, d.Engine)
	}
	vs, closer, err := w.r.openVersions(ctx, d.ID, engines.Runtime{JobID: w.job.ID, DryRun: w.job.DryRun, Reporter: w.rep,
		Now: w.r.now, Location: w.r.loc, Log: w.r.log})
	if err != nil {
		return jobs.Result{}, fmt.Errorf("destination %q: %w", d.Name, err)
	}
	defer func() {
		if cerr := closer.Close(); cerr != nil {
			w.r.log.Warn("Could not close a destination's version store", "destination", d.Name, "error", cerr)
		}
	}()
	w.ev = &snapshots.EngineVersions{DB: w.r.store.DB(), Store: vs, Engine: engines.Kind(d.Engine), Kind: engines.VersionPlexDB,
		Now: w.r.now, Info: func(msg string, args ...any) { w.rep.Log(slog.LevelInfo, msg, args...) }, Warn: w.warn}
	w.stats.Engine = d.Engine
	w.folder = FolderName(w.it.Name, w.it.ID)
	if w.job.DryRun {
		return w.dryRun(ctx)
	}
	return w.backupEngine(ctx)
}

// backupEngine runs a real backup to an engine destination (see runEngine).
func (w *run) backupEngine(ctx context.Context) (res jobs.Result, err error) {
	staging := w.r.StagingDir(w.job.ID)
	defer func() {
		// A failed or cancelled job leaves nothing but what it recorded (a crash leaves its state
		// to the resumed job).
		if err != nil {
			if rerr := os.RemoveAll(staging); rerr != nil {
				w.r.log.Error("Could not remove a Plex DB staging directory", "path", staging, "error", rerr)
			}
		}
	}()
	if err := os.RemoveAll(staging); err != nil {
		return jobs.Result{}, fmt.Errorf("remove the staging directory of an earlier attempt: %w", err)
	}
	w.r.cleanStaleStaging(w.job.ID)
	own, err := w.recoverEngine(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	if own != nil {
		return w.finishRecovered(ctx, *own)
	}
	if err := w.items.DeleteItems(ctx, w.job.ID); err != nil {
		return jobs.Result{}, err
	}
	plexVersion := w.plexInfo(ctx)
	if err := w.checkSpace(); err != nil {
		return jobs.Result{}, err
	}
	w.rep.Progress(jobs.Progress{Phase: "backing-up", CurrentFile: LibraryDB})
	br, err := Backup(ctx, BackupOptions{DataPath: w.ps.DataPath, StagingDir: staging, BusyTimeout: w.r.busyTimeout,
		Attempts: w.r.attempts, Log: w.rep.Log})
	if err != nil {
		return jobs.Result{}, err
	}
	w.warns += len(br.Warnings) // Backup logged them
	w.noted = append(w.noted, br.Warnings...)
	reports, integrity, err := w.verifyStaged(ctx, br)
	if err != nil {
		return jobs.Result{}, err
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
	byRel, err := w.addItems(ctx, br, logical)
	if err != nil {
		return jobs.Result{}, err
	}
	m := Manifest{Format: manifestFormat, CreatedAt: created, Method: br.Method, PlexVersion: plexVersion,
		SQLiteVersion: br.SQLiteVersion, IntegrationID: w.it.ID, IntegrationName: w.it.Name, JobID: w.job.ID,
		JobQueuedAt: w.job.QueuedAt, Result: integrity, Integrity: reports[LibraryDB], Files: []ManifestFile{},
		Warnings: w.noted}
	versionDir := filepath.Join(staging, "version")
	if err := os.Mkdir(versionDir, 0o700); err != nil {
		return jobs.Result{}, fmt.Errorf("create the version directory: %w", err)
	}
	files := map[string]int64{}
	for _, f := range br.Files {
		mf := ManifestFile{Name: f.Name, Size: f.Size, SHA256: f.SHA256, Method: f.Method}
		if rep, ok := reports[f.Name]; ok {
			mf.Integrity = &rep
		}
		m.Files = append(m.Files, mf)
		// Staged 0600 (Preferences.xml holds the PlexOnlineToken); the engine stores them encrypted.
		if err := os.Rename(f.Path, filepath.Join(versionDir, f.Name)); err != nil {
			return jobs.Result{}, fmt.Errorf("stage %s: %w", f.Name, err)
		}
		files[f.Name] = f.Size
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return jobs.Result{}, fmt.Errorf("encode the manifest: %w", err)
	}
	manifest = append(manifest, '\n')
	if err := os.WriteFile(filepath.Join(versionDir, ManifestName), manifest, 0o600); err != nil {
		return jobs.Result{}, fmt.Errorf("write the manifest: %w", err)
	}
	files[ManifestName] = int64(len(manifest))
	w.rep.Progress(jobs.Progress{Phase: "uploading", FilesTotal: int64(len(br.Files)), BytesTotal: br.Size()})
	var snap snapshots.Snapshot
	ref, err := w.ev.Put(ctx, engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: logical, Dir: versionDir, Time: created,
		JobID: w.job.ID, IntegrationID: w.it.ID}, func(ctx context.Context, ref engines.Ref) error {
		var err error
		snap, err = w.record(ctx, snapshots.Snapshot{DestinationID: w.dest.ID, Kind: snapshots.KindPlexDB, IntegrationID: w.it.ID,
			JobID: w.job.ID, Path: logical, CreatedAt: created, Size: br.Size(), Method: br.Method, Integrity: integrity,
			Manifest: manifest[:len(manifest)-1], EngineRef: w.ev.RowRef(ref)})
		return err
	})
	if err != nil {
		return jobs.Result{}, err
	}
	w.listing.Add(engines.StoredVersion{Ref: ref, LogicalPath: logical, Version: path.Base(logical), Time: created, Complete: true,
		Files: files, JobID: w.job.ID, IntegrationID: w.it.ID})
	for _, it := range byRel {
		if it.ID != 0 {
			if err := w.items.Finish(context.WithoutCancel(ctx), it.ID, jobs.ItemDone, it.Bytes, ""); err != nil {
				return jobs.Result{}, err
			}
		}
	}
	if err := os.RemoveAll(staging); err != nil {
		w.r.log.Error("Could not remove a Plex DB staging directory", "path", staging, "error", err)
	}
	w.rep.Log(slog.LevelInfo, "Recorded the Plex DB version", "path", logical, "integrity", integrity, "size", br.Size(), "ref", string(ref))
	lib := reports[LibraryDB]
	w.stats.Files, w.stats.Bytes = int64(len(br.Files)), br.Size()
	w.stats.Method, w.stats.Integrity = br.Method, integrity
	w.stats.SnapshotID, w.stats.Path, w.stats.EngineRef = snap.ID, snap.Path, string(ref)
	w.stats.MetadataItems, w.stats.MediaParts = lib.MetadataItems, lib.MediaParts
	if integrity != IntegrityOK {
		return w.failedVerification(reports)
	}
	w.prune(ctx)
	return w.result(w.summary()), nil
}

// record inserts a version's row; when the integration was deleted during the backup it is
// recorded without the link (as the filecopy path does).
func (w *run) record(ctx context.Context, rec snapshots.Snapshot) (snapshots.Snapshot, error) {
	snap, err := w.r.store.Insert(ctx, rec)
	if err == nil {
		return snap, nil
	}
	if _, gerr := w.r.integrations.Get(ctx, w.it.ID); errors.Is(gerr, integrations.ErrNotFound) {
		rec.IntegrationID = 0
		if snap, err = w.r.store.Insert(ctx, rec); err == nil {
			w.warn("The Plex integration was deleted during the backup; its version was recorded without it", "path", rec.Path)
			return snap, nil
		}
	}
	return snapshots.Snapshot{}, err
}

// engineRow is the EngineVersions view of a row.
func engineRow(s snapshots.Snapshot) snapshots.EngineRow {
	return snapshots.EngineRow{Path: s.Path, EngineRef: s.EngineRef}
}

// recoverEngine settles what interrupted jobs left at the engine destination (runEngine step 2)
// and returns this job's own recorded version when an earlier attempt wrote it completely.
func (w *run) recoverEngine(ctx context.Context) (*snapshots.Snapshot, error) {
	rows, err := w.r.store.List(ctx, w.dest.ID)
	if err != nil {
		return nil, err
	}
	w.listing, err = w.ev.Recover(ctx, snapshots.RecoverInput{
		KindFolder: PlexRoot,
		JobID:      w.job.ID,
		Mine: func(v engines.StoredVersion) bool {
			return folderIntegrationID(snapshots.FolderOf(v.LogicalPath)) == w.it.ID
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
	if w.job.Trigger != jobs.TriggerResume && w.job.Attempt <= 1 {
		return nil, nil
	}
	snaps, err := w.r.store.ListFor(ctx, snapshots.KindPlexDB, w.dest.ID, w.it.ID)
	if err != nil {
		return nil, err
	}
	for _, s := range snaps {
		if !ownManifest(s, w.job) || w.ev.Lost(w.listing, engineRow(s)) {
			continue
		}
		var m Manifest
		if s.Integrity != IntegrityOK && (json.Unmarshal(s.Manifest, &m) != nil || m.Result == IntegrityOK) {
			continue
		}
		return &s, nil
	}
	return nil, nil
}

// adoptEngine records an unrecorded, complete version of this integration after its manifest
// parses (on restic it must name this job: its id and queue time) and its files list with the
// manifest's sizes; a version whose files do not is recorded as failed (it is pruned after
// FailedKeep). Content is checked by the destination's verify job.
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
	if w.ev.Restic() && (m.JobID != w.job.ID || !m.JobQueuedAt.Equal(w.job.QueuedAt)) {
		return fmt.Errorf("the manifest names job %d of %s, not this job", m.JobID, m.JobQueuedAt.UTC().Format("2006-01-02 15:04:05"))
	}
	want := map[string]int64{ManifestName: -1}
	var size int64
	hasLibrary := false
	for _, f := range m.Files {
		if !validFileName(f.Name) {
			return fmt.Errorf("the manifest names an unexpected file %q", f.Name)
		}
		hasLibrary = hasLibrary || f.Name == LibraryDB
		want[f.Name] = f.Size
		size += f.Size
	}
	integrity := m.Result
	if integrity != IntegrityOK || !hasLibrary {
		integrity = IntegrityFailed
	}
	if err := snapshots.CheckSizes(v, want); err != nil {
		integrity = IntegrityFailed
		w.rep.Log(slog.LevelWarn, "An unrecorded Plex DB version does not list the files of its manifest", "path", v.LogicalPath, "error", err.Error())
	}
	createdAt := m.CreatedAt
	if createdAt.IsZero() {
		createdAt = v.Time
	}
	if _, err := w.r.store.Insert(context.WithoutCancel(ctx), snapshots.Snapshot{DestinationID: w.dest.ID, Kind: snapshots.KindPlexDB,
		IntegrationID: w.it.ID, JobID: w.job.ID, Path: v.LogicalPath, CreatedAt: createdAt, Size: size, Method: m.Method,
		Integrity: integrity, Manifest: raw, EngineRef: w.ev.RowRef(v.Ref)}); err != nil {
		return err
	}
	w.stats.Recovered++
	w.rep.Log(slog.LevelInfo, "Recorded a Plex DB version an interrupted backup had written", "path", v.LogicalPath,
		"integrity", integrity, "writtenByJob", m.JobID)
	return nil
}

// pruneEngine applies the version retention to this integration's versions at the engine
// destination: rows whose version is gone are dropped first (Lost), then the versions
// snapshots.Prune selects are removed with their rows (EngineVersions.Remove). Problems are
// warnings: the backup itself succeeded.
func (w *run) pruneEngine(ctx context.Context) {
	snaps, err := w.r.store.ListFor(ctx, snapshots.KindPlexDB, w.dest.ID, w.it.ID)
	if err != nil {
		w.warn("Old Plex DB versions were not pruned", "error", err.Error())
		return
	}
	cands := make([]snapshots.Version, 0, len(snaps))
	byID := map[int64]snapshots.Snapshot{}
	for _, s := range snaps {
		if w.ev.Lost(w.listing, engineRow(s)) {
			if err := w.r.store.Remove(ctx, s.ID); err != nil {
				w.warn("A Plex DB version missing at the destination is still recorded: its record could not be removed", "path", s.Path, "error", err.Error())
				continue
			}
			w.warn("A recorded Plex DB version was missing or incomplete at the destination; its record was removed", "path", s.Path,
				"createdAt", s.CreatedAt)
			continue
		}
		cands = append(cands, s.Version())
		byID[s.ID] = s
	}
	for _, id := range snapshots.Prune(cands, w.dest.Retention.PlexDBDaily, w.dest.Retention.PlexDBWeekly, w.r.now(), w.r.loc) {
		if ctx.Err() != nil {
			return
		}
		s := byID[id]
		err := w.ev.Remove(ctx, w.listing, engineRow(s), func(ctx context.Context, tx *sql.Tx) error { return snapshots.RemoveTx(ctx, tx, id) })
		if err != nil {
			w.warn("An old Plex DB version could not be deleted", "path", s.Path, "error", err.Error())
			continue
		}
		w.stats.Pruned++
		w.rep.Log(slog.LevelInfo, "Pruned an old Plex DB version", "path", s.Path, "createdAt", s.CreatedAt, "integrity", s.Integrity)
	}
}
