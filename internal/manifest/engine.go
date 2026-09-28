package manifest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// The engine path (docs/design/phase4.md §8.4, §8.6): a manifest version at a restic or rclone
// destination. The build, the "unchanged" rule and the retention are the filecopy path's; the
// version (manifest.json, manifest.csv, SHA256SUMS) is written into a 0700 staging directory under
// <config>/staging and stored through the destination's VersionStore (snapshots.EngineVersions):
//
//   - Preflight: the destination is enabled, its recovery kit custody is confirmed (S21; not for
//     a dry run) and the version store opens (S25). Manifests hold no secrets (S20): any engine
//     destination may take them, also plain rclone. No transfer window; the limits apply.
//   - Recovery: rclone uploads without manifest.json older than 24 h are purged; an unrecorded
//     complete version is fetched, checked against its SHA256SUMS and recorded (damaged when it
//     does not match); on restic only this job's own snapshot, after its manifest names this job.
//   - "Unchanged": the newest ok version holds the same content hash, its SHA256SUMS reads back
//     and names the recorded checksum of manifest.json, and its three files are listed. The
//     content itself is checked by the destination's verify job.
//   - Pruning: restic rows and forget requests in one transaction; rclone purges first.
//   - Downloads (Runner.Download): the file is fetched into a 0700 staging directory and verified
//     against the recorded checksum and SHA256SUMS before the first byte is served. A version
//     store that does not open is ErrUnreachable; a version whose files are missing or do not
//     match is ErrDamaged (and marked damaged).

// ErrUnreachable means a manifest of an engine destination could not be read because the
// destination did not open (S25: a missing repository, a wrong password, another repository, a
// missing marker) or did not answer. The API answers 409 "destination not reachable".
var ErrUnreachable = errors.New("destination not reachable")

// fetchPrefix names the staging directory of an engine version being read back.
const fetchPrefix = "manifest-fetch-"

// runEngine is Run's engine path (see above).
func (w *run) runEngine(ctx context.Context) (jobs.Result, error) {
	d, err := w.r.destinations.Get(ctx, w.job.Params.DestinationID)
	if err != nil {
		return jobs.Result{}, err
	}
	w.dest = d
	if !d.Enabled {
		return jobs.Result{}, fmt.Errorf("destination %q is disabled", d.Name)
	}
	if blocked := destinations.Blocked(d); blocked != "" && !w.job.DryRun {
		return jobs.Result{}, fmt.Errorf("destination %q: %s", d.Name, blocked)
	}
	vs, closer, err := w.r.openStore(ctx, d, engines.Runtime{JobID: w.job.ID, DryRun: w.job.DryRun, Reporter: w.rep, Now: w.r.now,
		Location: w.r.loc, Log: w.r.log})
	if err != nil {
		return jobs.Result{}, err
	}
	defer func() {
		if err := closer.Close(); err != nil {
			w.r.log.Warn("Could not close a destination's version store", "destination", d.Name, "error", err)
		}
	}()
	w.ev = w.r.engineVersions(d, vs, w.rep, w.warn)
	w.stats.Engine = d.Engine
	if w.job.DryRun {
		// The "unchanged" check reads the listing; a dry run settles nothing (S9).
		if w.listing, err = w.ev.List(ctx, Root); err != nil {
			return jobs.Result{}, err
		}
		return w.dryRun(ctx)
	}
	return w.export(ctx)
}

// openStore opens destination d's version store (S25 errors wrap ErrUnreachable).
func (r *Runner) openStore(ctx context.Context, d destinations.Destination, rt engines.Runtime) (engines.VersionStore, io.Closer, error) {
	if r.openVersions == nil {
		return nil, nil, fmt.Errorf("destination %q is a %s destination, and no engine runtime is available", d.Name, d.Engine)
	}
	vs, closer, err := r.openVersions(ctx, d.ID, rt)
	if err != nil {
		return nil, nil, fmt.Errorf("destination %q: %w: %w", d.Name, ErrUnreachable, err)
	}
	return vs, closer, nil
}

// engineVersions returns the EngineVersions of d's manifests.
func (r *Runner) engineVersions(d destinations.Destination, vs engines.VersionStore, rep jobs.Reporter, warn func(string, ...any)) *snapshots.EngineVersions {
	ev := &snapshots.EngineVersions{DB: r.db, Store: vs, Engine: engines.Kind(d.Engine), Kind: engines.VersionManifest, Now: r.now, Warn: warn}
	if rep != nil {
		ev.Info = func(msg string, args ...any) { rep.Log(slog.LevelInfo, msg, args...) }
	}
	return ev
}

// engineRow is the EngineVersions view of a row.
func engineRow(v Version) snapshots.EngineRow {
	return snapshots.EngineRow{Path: v.Path, EngineRef: v.EngineRef}
}

// checkEngineVersion is CheckVersion at an engine destination: the version is listed with its
// three files and its SHA256SUMS reads back naming the recorded checksum of manifest.json. A
// missing or mismatching version wraps ErrDamaged.
func (w *run) checkEngineVersion(ctx context.Context, v Version) error {
	sv, ok := w.ev.Find(w.listing, engineRow(v))
	if !ok {
		return fmt.Errorf("%w: the version is not at the destination", ErrDamaged)
	}
	if err := snapshots.CheckSizes(sv, map[string]int64{JSONName: -1, CSVName: -1, SumsName: -1}); err != nil {
		return fmt.Errorf("%w: %v", ErrDamaged, err)
	}
	raw, err := w.ev.ReadSmall(ctx, sv.Ref, SumsName, maxSumsBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: no %s", ErrDamaged, SumsName)
		}
		return err
	}
	sums, err := ParseSums(raw)
	if err != nil {
		return err
	}
	if Checksum(sums[JSONName]) != v.Checksum {
		return fmt.Errorf("%w: %s does not match the recorded checksum", ErrDamaged, SumsName)
	}
	return nil
}

// exportEngine is export at an engine destination (see Run).
func (w *run) exportEngine(ctx context.Context) (jobs.Result, error) {
	own, err := w.recoverEngine(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	if own != nil {
		return w.finishOwn(ctx, own)
	}
	release, err := w.acquireBuild(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	defer release()
	m, hash, err := w.build(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	unchanged, err := w.unchanged(ctx, hash, false)
	if err != nil {
		return jobs.Result{}, err
	}
	if unchanged {
		release()
		w.rep.Log(slog.LevelInfo, "The manifest is unchanged since the newest version, which reads back at the destination", "path", w.stats.Path)
		w.prune(ctx)
		return w.result(fmt.Sprintf("The manifest of %q is unchanged: %d items, %d files (%s)", w.dest.Name,
			w.stats.Items, w.stats.Files, formatBytes(w.stats.Bytes))), nil
	}
	v, err := w.writeEngine(ctx, m, hash)
	if err != nil {
		return jobs.Result{}, err
	}
	release()
	w.stats.ManifestID, w.stats.Path, w.stats.EngineRef = v.ID, v.Path, string(w.ev.RefIn(w.listing, engineRow(v)))
	w.rep.Log(slog.LevelInfo, "Recorded the manifest version", "path", v.Path, "items", v.ItemCount, "files", v.FileCount, "checksum", v.Checksum)
	w.prune(ctx)
	return w.result(w.summary()), nil
}

// stagingBase is <config>/staging.
func (r *Runner) stagingBase() string { return filepath.Join(r.configDir, stagingDirName) }

// writeLocal streams fill into a new 0600 file at p and returns the hex sha256 and size of what
// it wrote.
func writeLocal(p string, fill func(io.Writer) error) (string, int64, error) {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	bw := bufio.NewWriterSize(cw, 64<<10)
	err = fill(bw)
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, fmt.Errorf("write %s: %w", filepath.Base(p), err)
	}
	return hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

// writeEngine stages a version (manifest.json, manifest.csv, SHA256SUMS) in a 0700 directory of
// <config>/staging, stores it (VersionStore.Put, read back complete) and records it.
func (w *run) writeEngine(ctx context.Context, m *Manifest, hash string) (Version, error) {
	w.rep.Progress(jobs.Progress{Phase: "writing"})
	writeJSON := func(out io.Writer) error { return WriteJSON(out, m) }
	writeCSV := func(out io.Writer) error { return WriteCSV(out, m) }
	jsonSize, err := encodedSize(writeJSON)
	if err != nil {
		return Version{}, err
	}
	csvSize, err := encodedSize(writeCSV)
	if err != nil {
		return Version{}, err
	}
	base := w.r.stagingBase()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return Version{}, fmt.Errorf("create the staging directory: %w", err)
	}
	if root, err := os.OpenRoot(base); err == nil {
		free, _, ferr := filecopy.FreeSpace(root)
		_ = root.Close()
		if need := uint64(jsonSize + csvSize + spaceMargin); ferr == nil && free < need {
			return Version{}, fmt.Errorf("not enough free space in the config directory for staging the manifest: it needs %s, %s is free",
				formatBytes(int64(need)), formatBytes(int64(free)))
		}
	}
	staging := filepath.Join(base, "manifest-job"+strconv.FormatInt(w.job.ID, 10))
	if err := os.RemoveAll(staging); err != nil {
		return Version{}, fmt.Errorf("remove the staging directory of an earlier attempt: %w", err)
	}
	dir := filepath.Join(staging, "version")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Version{}, fmt.Errorf("create the staging directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(staging); err != nil {
			w.r.log.Error("Could not remove a manifest staging directory", "path", staging, "error", err)
		}
	}()
	jsonHex, _, err := writeLocal(filepath.Join(dir, JSONName), writeJSON)
	if err != nil {
		return Version{}, err
	}
	csvHex, _, err := writeLocal(filepath.Join(dir, CSVName), writeCSV)
	if err != nil {
		return Version{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, SumsName), FormatSums(jsonHex, csvHex), 0o600); err != nil {
		return Version{}, fmt.Errorf("write %s: %w", SumsName, err)
	}
	rows, err := w.r.store.List(ctx, w.dest.ID)
	if err != nil {
		return Version{}, err
	}
	recorded := map[string]bool{}
	for _, r := range rows {
		recorded[r.Path] = true
	}
	logical, err := w.listing.FreePath(Root, m.CreatedAt, w.job.ID, func(p string) bool { return recorded[p] })
	if err != nil {
		return Version{}, err
	}
	w.rep.Progress(jobs.Progress{Phase: "uploading", FilesTotal: 3, BytesTotal: jsonSize + csvSize})
	var v Version
	ref, err := w.ev.Put(ctx, engines.PutVersion{Kind: engines.VersionManifest, LogicalPath: logical, Dir: dir, Time: m.CreatedAt,
		JobID: w.job.ID}, func(ctx context.Context, ref engines.Ref) error {
		var err error
		v, err = w.r.store.Insert(ctx, Version{DestinationID: w.dest.ID, JobID: w.job.ID, CreatedAt: m.CreatedAt, Path: logical,
			Format: FormatVersion, ItemCount: m.Summary.Items, FileCount: m.Summary.Files, Bytes: m.Summary.Bytes,
			Checksum: Checksum(jsonHex), ContentHash: hash, Integrity: IntegrityOK, EngineRef: w.ev.RowRef(ref)})
		return err
	})
	if err != nil {
		return Version{}, err
	}
	w.listing.Add(engines.StoredVersion{Ref: ref, LogicalPath: logical, Version: path.Base(logical), Time: m.CreatedAt, Complete: true,
		Files: map[string]int64{JSONName: jsonSize, CSVName: csvSize, SumsName: int64(len(FormatSums(jsonHex, csvHex)))}, JobID: w.job.ID})
	return v, nil
}

// fetchVersion fetches the three files of a stored version into a new 0700 directory of
// <config>/staging and reads it back (ReadVersion: the files against SHA256SUMS and, when checksum
// is not "", manifest.json against it). The caller removes the directory.
func (r *Runner) fetchVersion(ctx context.Context, vs engines.VersionStore, ref engines.Ref, checksum string) (string, *Manifest, VersionCheck, error) {
	base := r.stagingBase()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, VersionCheck{}, fmt.Errorf("create the staging directory: %w", err)
	}
	dir, err := os.MkdirTemp(base, fetchPrefix)
	if err != nil {
		return "", nil, VersionCheck{}, fmt.Errorf("create a staging directory: %w", err)
	}
	for _, name := range []string{SumsName, JSONName, CSVName} {
		if err := vs.Fetch(ctx, ref, name, dir); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return dir, nil, VersionCheck{}, fmt.Errorf("%w: no %s", ErrDamaged, name)
			}
			return dir, nil, VersionCheck{}, fmt.Errorf("fetch %s: %w", name, err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return dir, nil, VersionCheck{}, err
	}
	defer root.Close()
	m, c, err := ReadVersion(root, ".", checksum)
	return dir, m, c, err
}

// recoverEngine settles what interrupted exports left at the engine destination and returns
// this job's own recorded version when an earlier attempt wrote it.
func (w *run) recoverEngine(ctx context.Context) (*ownVersion, error) {
	rows, err := w.r.store.List(ctx, w.dest.ID)
	if err != nil {
		return nil, err
	}
	w.listing, err = w.ev.Recover(ctx, snapshots.RecoverInput{
		KindFolder: Root,
		JobID:      w.job.ID,
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
	list, err := w.r.store.List(ctx, w.dest.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range list {
		if v.JobID != w.job.ID || v.Integrity != IntegrityOK || w.ev.Lost(w.listing, engineRow(v)) {
			continue
		}
		dir, m, _, err := w.r.fetchVersion(ctx, w.ev.Store, w.ev.RefIn(w.listing, engineRow(v)), v.Checksum)
		if dir != "" {
			defer os.RemoveAll(dir)
		}
		switch {
		case errors.Is(err, ErrDamaged):
			if merr := w.r.store.MarkDamaged(context.WithoutCancel(ctx), v.ID); merr != nil {
				return nil, merr
			}
			w.stats.DamagedFound++
			w.warn("A manifest version of this job's earlier attempt is damaged; it is marked damaged", "path", v.Path, "reason", err.Error())
			continue
		case err != nil:
			return nil, err
		}
		if m.Job != nil && m.Job.ID == w.job.ID && m.Job.QueuedAt.Equal(w.job.QueuedAt) {
			return &ownVersion{v: v, m: m}, nil
		}
	}
	return nil, nil
}

// adoptEngine records an unrecorded, complete version: fetched and checked against its
// SHA256SUMS (damaged when it does not match); on restic only when its manifest names this job.
func (w *run) adoptEngine(ctx context.Context, sv engines.StoredVersion) error {
	dir, m, c, err := w.r.fetchVersion(ctx, w.ev.Store, sv.Ref, "")
	if dir != "" {
		defer os.RemoveAll(dir)
	}
	if m == nil {
		if err == nil {
			err = errors.New("no manifest")
		}
		return err
	}
	if w.ev.Restic() && (m.Job == nil || m.Job.ID != w.job.ID || !m.Job.QueuedAt.Equal(w.job.QueuedAt)) {
		return errors.New("the manifest does not name this job")
	}
	integrity := IntegrityOK
	if err != nil {
		integrity = IntegrityDamaged
		w.warn("An unrecorded manifest version does not match its SHA256SUMS; it is recorded as damaged", "path", sv.LogicalPath, "reason", err.Error())
	}
	hash, herr := ContentHash(m)
	if herr != nil {
		return herr
	}
	created := m.CreatedAt
	if created.IsZero() {
		created = sv.Time
	}
	if _, err := w.r.store.Insert(context.WithoutCancel(ctx), Version{DestinationID: w.dest.ID, JobID: w.job.ID, CreatedAt: created,
		Path: sv.LogicalPath, Format: m.FormatVersion, ItemCount: m.Summary.Items, FileCount: m.Summary.Files, Bytes: m.Summary.Bytes,
		Checksum: Checksum(c.JSON), ContentHash: hash, Integrity: integrity, EngineRef: w.ev.RowRef(sv.Ref)}); err != nil {
		return err
	}
	w.stats.Recovered++
	writer := int64(0)
	if m.Job != nil {
		writer = m.Job.ID
	}
	w.rep.Log(slog.LevelInfo, "Recorded a manifest version an interrupted export had written", "path", sv.LogicalPath, "integrity", integrity,
		"writtenByJob", writer)
	return nil
}

// pruneEngine applies the retention (manifestDays, manifestWeeks) at the engine destination:
// rows whose version is gone are dropped (Lost), then the versions PruneSet selects are removed
// with their rows. Problems are warnings.
func (w *run) pruneEngine(ctx context.Context) {
	keep, err := keepOf(ctx, w.r.db.Reader(), w.dest.ID)
	if err != nil {
		w.warn("Old manifest versions were not pruned", "error", err.Error())
		return
	}
	list, err := w.r.store.List(ctx, w.dest.ID)
	if err != nil {
		w.warn("Old manifest versions were not pruned", "error", err.Error())
		return
	}
	var cands []Retained
	byID := map[int64]Version{}
	for _, v := range list {
		if w.ev.Lost(w.listing, engineRow(v)) {
			if err := w.r.store.Remove(ctx, v.ID); err != nil {
				w.warn("A manifest version missing at the destination is still recorded: its record could not be removed", "path", v.Path, "error", err.Error())
				continue
			}
			w.warn("A recorded manifest version was missing at the destination; its record was removed", "path", v.Path, "createdAt", v.CreatedAt)
			continue
		}
		cands = append(cands, Retained{ID: v.ID, CreatedAt: v.CreatedAt, OK: v.Integrity == IntegrityOK})
		byID[v.ID] = v
	}
	for _, id := range PruneSet(cands, keep.Days, keep.Weeks, w.r.now(), w.r.loc) {
		if ctx.Err() != nil {
			return
		}
		v := byID[id]
		err := w.ev.Remove(ctx, w.listing, engineRow(v), func(ctx context.Context, tx *sql.Tx) error { return removeTx(ctx, tx, id) })
		if err != nil {
			w.warn("An old manifest version could not be deleted", "path", v.Path, "error", err.Error())
			continue
		}
		w.stats.VersionsPruned++
		w.rep.Log(slog.LevelInfo, "Pruned an old manifest version", "path", v.Path, "createdAt", v.CreatedAt, "integrity", v.Integrity)
	}
}

// downloadEngine is Download at an engine destination (§8.6): the version's files are fetched
// into a 0700 staging directory and verified against the recorded checksum and SHA256SUMS before
// the StagedFile is handed out. ErrUnreachable when the version store does not open or a fetch
// fails; ErrDamaged (the version is marked damaged) when a file is missing or does not match.
func (r *Runner) downloadEngine(ctx context.Context, v Version, format string) (*StagedFile, error) {
	d, err := r.destinations.Get(ctx, v.DestinationID)
	if err != nil {
		return nil, err
	}
	vs, closer, err := r.openStore(ctx, d, engines.Runtime{DryRun: true, Now: r.now, Location: r.loc, Log: r.log})
	if err != nil {
		return nil, err
	}
	defer func() { _ = closer.Close() }()
	ev := r.engineVersions(d, vs, nil, nil)
	base := r.stagingBase()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("create the staging directory: %w", err)
	}
	r.cleanStaleStaging(base)
	dir, err := os.MkdirTemp(base, downloadPrefix)
	if err != nil {
		return nil, fmt.Errorf("create a staging directory: %w", err)
	}
	f, err := r.fetchVerified(ctx, ev, d, v, format, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		if errors.Is(err, ErrDamaged) {
			if merr := r.store.MarkDamaged(context.WithoutCancel(ctx), v.ID); merr != nil {
				r.log.Error("Could not mark a damaged manifest version", "id", v.ID, "error", merr)
			}
			r.log.Warn("A manifest version is damaged at its destination", "id", v.ID, "path", v.Path, "reason", err.Error())
			return nil, ErrDamaged
		}
		return nil, err
	}
	return f, nil
}

// fetchVerified fetches SHA256SUMS and the requested file of v into dir and verifies them.
func (r *Runner) fetchVerified(ctx context.Context, ev *snapshots.EngineVersions, d destinations.Destination, v Version, format, dir string) (*StagedFile, error) {
	ref := ev.Ref(engineRow(v))
	raw, err := ev.ReadSmall(ctx, ref, SumsName, maxSumsBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: no %s", ErrDamaged, SumsName)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrUnreachable, SumsName, err)
	}
	sums, err := ParseSums(raw)
	if err != nil {
		return nil, err
	}
	if Checksum(sums[JSONName]) != v.Checksum {
		return nil, fmt.Errorf("%w: %s does not match the recorded checksum", ErrDamaged, SumsName)
	}
	file := JSONName
	if format == FormatCSV {
		file = CSVName
	}
	if err := ev.Store.Fetch(ctx, ref, file, dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: no %s", ErrDamaged, file)
		}
		return nil, fmt.Errorf("%w: fetch %s: %w", ErrUnreachable, file, err)
	}
	fetched := filepath.Join(dir, file)
	in, err := os.Open(fetched)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(h, in)
	_ = in.Close()
	if err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if sum != sums[file] {
		return nil, fmt.Errorf("%w: %s", ErrDamaged, file)
	}
	p := filepath.Join(dir, "manifest."+format)
	if err := os.Rename(fetched, p); err != nil {
		return nil, err
	}
	if err := os.Chmod(p, 0o600); err != nil {
		return nil, err
	}
	label := snapshots.Slug(d.Name)
	if label == "" {
		label = fmt.Sprintf("destination-%d", d.ID)
	}
	ct := "application/json"
	if format == FormatCSV {
		ct = "text/csv; charset=utf-8"
	}
	return &StagedFile{Name: fmt.Sprintf("bunkarr-manifest-%s-%s.%s", label, path.Base(v.Path), format), ContentType: ct, Size: n,
		SHA256: sum, path: p, dir: dir}, nil
}
