package arrbackup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// engineOpener is an engines.VersionOpener over one fake store.
type engineOpener struct {
	mu     sync.Mutex
	store  *enginetest.FakeVersionStore
	opened int
	closed int
}

type closerFunc func() error

func (c closerFunc) Close() error { return c() }

func (o *engineOpener) open(ctx context.Context, destinationID int64, rt engines.Runtime) (engines.VersionStore, io.Closer, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opened++
	return o.store, closerFunc(func() error { o.mu.Lock(); o.closed++; o.mu.Unlock(); return nil }), nil
}

// engineDest inserts a restic or rclone destination row with a confirmed recovery kit.
func (f *fixture) engineDest(t *testing.T, engine string, kind engines.DestKind, enc engines.EncryptionMode) destinations.Destination {
	t.Helper()
	now := db.FormatTime(time.Now())
	var id int64
	err := f.db.Write(f.ctx, func(tx *sql.Tx) error {
		var n int64
		if err := tx.QueryRowContext(f.ctx, `SELECT count(*) FROM destinations`).Scan(&n); err != nil {
			return err
		}
		var origin, confirmed, tag any
		if enc != engines.EncryptionNone {
			origin, confirmed = "generated", now
		}
		if engine == "restic" {
			tag = fmt.Sprintf("%032x", n+1)
		}
		caps, _ := json.Marshal(map[string]any{"enforcesModes": enc != engines.EncryptionNone})
		res, err := tx.ExecContext(f.ctx, `INSERT INTO destinations (name, engine, target, marker_id, fs_type, root_dev, capabilities, kind,
			encryption, secret_origin, kit_confirmed_at, engine_tag, created_at, updated_at) VALUES (?, ?, 's3:minio/bk', ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("Offsite %d", n+1), engine, fmt.Sprintf("marker-%d", n+1), string(kind), string(caps), string(kind), string(enc),
			origin, confirmed, tag, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.dests.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// useEngine rebuilds the runner with a fake version store opener.
func (f *fixture) useEngine(t *testing.T) *engineOpener {
	t.Helper()
	o := &engineOpener{store: enginetest.NewFakeVersionStore()}
	var err error
	if f.runner, err = NewRunner(Options{DB: f.db, Integrations: f.ints, Destinations: f.dests, ConfigDir: f.config,
		Now: f.clock.Now, Location: time.UTC, PollInterval: 5 * time.Millisecond, CommandTimeout: 2 * time.Second,
		FolderRetries: 3, FolderRetryDelay: 20 * time.Millisecond, OpenVersions: o.open}); err != nil {
		t.Fatal(err)
	}
	return o
}

// engineJob is an arr_backup job row to dest.
func (f *fixture) engineJob(t *testing.T, dest destinations.Destination, dry bool) jobs.Job {
	t.Helper()
	job := f.newJob(t, dry)
	job.Params.DestinationID = dest.ID
	raw, _ := json.Marshal(job.Params)
	if err := f.db.Write(f.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, `UPDATE jobs SET params = ?, destination_id = ? WHERE id = ?`, string(raw), dest.ID, job.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return job
}

func (f *fixture) rowsOf(t *testing.T, dest destinations.Destination) []snapshots.Snapshot {
	t.Helper()
	s, err := f.runner.Store().List(f.ctx, dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// checkStored checks a recorded version against the fake store: the zip with its size and
// sha256, manifest.json equal to the row's.
func checkStored(t *testing.T, store *enginetest.FakeVersionStore, s snapshots.Snapshot, ref engines.Ref) {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(s.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	zip, err := store.ReadFile(context.Background(), ref, m.Zip.Name, 1<<30)
	sum := sha256.Sum256(zip)
	if err != nil || int64(len(zip)) != m.Zip.Size || hex.EncodeToString(sum[:]) != m.Zip.SHA256 {
		t.Fatalf("zip of %s: %d bytes, %v", ref, len(zip), err)
	}
	onStore, err := store.ReadFile(context.Background(), ref, ManifestName, 1<<20)
	if err != nil || strings.TrimSpace(string(onStore)) != strings.TrimSpace(string(s.Manifest)) {
		t.Fatalf("manifest.json %s, %v", onStore, err)
	}
	if strings.Contains(string(s.Manifest), zipSecret) {
		t.Fatal("the manifest holds a secret of the zip")
	}
}

func TestEngineBackup(t *testing.T) {
	for _, tc := range []struct {
		engine string
		kind   engines.DestKind
		enc    engines.EncryptionMode
	}{{"restic", engines.B2, engines.EncryptionRestic}, {"rclone", engines.S3, engines.EncryptionCrypt}} {
		t.Run(tc.engine, func(t *testing.T) {
			f := newFixture(t)
			o := f.useEngine(t)
			dest := f.engineDest(t, tc.engine, tc.kind, tc.enc)
			res, err := f.run(t, f.engineJob(t, dest, false))
			if err != nil {
				t.Fatalf("%v\n%s", err, f.rep.text())
			}
			st := res.Stats.(Stats)
			rows, stored := f.rowsOf(t, dest), o.store.Stored()
			if len(rows) != 1 || len(stored) != 1 || st.Engine != tc.engine || rows[0].Integrity != IntegrityOK || rows[0].Path != stored[0].LogicalPath {
				t.Fatalf("stats %+v, rows %+v, stored %+v", st, rows, stored)
			}
			if want := map[bool]string{true: string(stored[0].Ref), false: ""}[tc.engine == "restic"]; rows[0].EngineRef != want {
				t.Fatalf("engine_ref %q, want %q", rows[0].EngineRef, want)
			}
			checkStored(t, o.store, rows[0], stored[0].Ref)
			if o.opened != 1 || o.closed != 1 {
				t.Fatalf("opener %+v", o)
			}
			if st, err := os.ReadDir(filepath.Join(f.config, StagingRoot)); err == nil && len(st) != 0 {
				t.Fatalf("staging left: %v", st)
			}
			if len(f.snapshots(t)) != 0 {
				t.Fatal("the filecopy destination got a version")
			}
		})
	}
}

// TestEngineUnchanged: a scheduled backup already stored is "unchanged" only after its
// manifest.json reads back and the zip lists with its size; a damaged copy is marked failed and
// the backup is stored again.
func TestEngineUnchanged(t *testing.T) {
	f := newFixture(t)
	o := f.useEngine(t)
	dest := f.engineDest(t, "restic", engines.B2, engines.EncryptionRestic)
	scheduled := entry{id: 7, name: "radarr_backup_v6.4.4.10685_2026.09.21_06.00.00.zip", typ: arr.BackupScheduled,
		time: f.clock.Now().Add(-4 * 24 * time.Hour), zip: goodZip(t)}
	f.setEntries(t, f.entries[0], scheduled)
	if _, err := f.run(t, f.engineJob(t, dest, false)); err != nil {
		t.Fatal(err)
	}
	f.clock.Set(f.clock.Now().Add(time.Hour))
	res, err := f.run(t, f.engineJob(t, dest, false))
	if err != nil || !res.Stats.(Stats).Unchanged || len(o.store.Stored()) != 1 || f.commands() != 0 {
		t.Fatalf("second run %+v, %v, stored %d", res, err, len(o.store.Stored()))
	}
	// Damage the stored copy: the zip is shorter than recorded.
	v := o.store.Stored()[0]
	zip, _ := o.store.ReadFile(f.ctx, v.Ref, scheduled.name, 1<<30)
	man, _ := o.store.ReadFile(f.ctx, v.Ref, ManifestName, 1<<20)
	if err := o.store.Remove(f.ctx, nil, v.Ref, engines.VersionArr); err != nil {
		t.Fatal(err)
	}
	o.store.AddLeftover(engines.StoredVersion{Ref: v.Ref, LogicalPath: v.LogicalPath, Time: v.Time, Complete: true, JobID: v.JobID,
		IntegrationID: v.IntegrationID}, map[string][]byte{scheduled.name: zip[:len(zip)-10], ManifestName: man})
	f.clock.Set(f.clock.Now().Add(time.Hour))
	res, err = f.run(t, f.engineJob(t, dest, false))
	if err != nil || res.Stats.(Stats).Unchanged || !f.rep.has("is marked failed and the backup is copied again") {
		t.Fatalf("damaged copy: %+v, %v\n%s", res, err, f.rep.text())
	}
	rows := f.rowsOf(t, dest)
	if len(rows) != 2 || rows[0].Integrity != IntegrityOK || rows[1].Integrity != IntegrityFailed {
		t.Fatalf("rows %+v", rows)
	}
}

// TestEngineCredentialsGate: an *arr backup to an unencrypted remote destination fails before
// any command; acceptInsecureModes of the UNAS target does not cover it.
func TestEngineCredentialsGate(t *testing.T) {
	f := newFixture(t)
	o := f.useEngine(t)
	plain := f.engineDest(t, "rclone", engines.S3, engines.EncryptionNone)
	f.setSettings(t, map[string]any{"backupFolder": f.backups, "backup": map[string]any{"targets": []integrations.BackupTarget{
		{DestinationID: f.dest.ID, AcceptInsecureModes: true}, {DestinationID: plain.ID}}}})
	_, err := f.run(t, f.engineJob(t, plain, false))
	if !errors.Is(err, integrations.ErrUnencrypted) || !strings.Contains(err.Error(), "this backup holds Radarr's credentials; use an encrypted destination") {
		t.Fatalf("err = %v", err)
	}
	if f.commands() != 0 || o.opened != 0 {
		t.Fatalf("%d commands, %d opens before the preflight failed", f.commands(), o.opened)
	}
}

// TestEngineCrashAtVersionPoints: a crash between Put and the row resumes without a lost or a
// duplicate version, and no second Backup command.
func TestEngineCrashAtVersionPoints(t *testing.T) {
	for _, engine := range []string{"restic", "rclone"} {
		for _, point := range []string{snapshots.PointVersionsAfterPut, snapshots.PointVersionsBeforeRecord} {
			t.Run(engine+"/"+point, func(t *testing.T) {
				f := newFixture(t)
				o := f.useEngine(t)
				kind, enc := engines.B2, engines.EncryptionRestic
				if engine == "rclone" {
					kind, enc = engines.S3, engines.EncryptionCrypt
				}
				dest := f.engineDest(t, engine, kind, enc)
				job := f.engineJob(t, dest, false)
				f.crashRun(t, job, point)
				if len(f.rowsOf(t, dest)) != 0 || len(o.store.Stored()) != 1 {
					t.Fatalf("after the crash: rows %+v, stored %+v", f.rowsOf(t, dest), o.store.Stored())
				}
				job.Attempt, job.Trigger = 2, jobs.TriggerResume
				f.clock.Set(f.clock.Now().Add(time.Minute))
				res, err := f.run(t, job)
				if err != nil {
					t.Fatalf("%v\n%s", err, f.rep.text())
				}
				if res.Stats.(Stats).Recovered != 1 || len(f.rowsOf(t, dest)) != 1 || len(o.store.Stored()) != 1 || f.commands() != 1 {
					t.Fatalf("resumed: %+v, rows %d, stored %d, commands %d", res.Stats, len(f.rowsOf(t, dest)), len(o.store.Stored()), f.commands())
				}
			})
		}
	}
}

// TestEnginePrune: the keep math is the filecopy path's; restic removes rows with their forget
// requests in one transaction, rclone purges first.
func TestEnginePrune(t *testing.T) {
	for _, engine := range []string{"restic", "rclone"} {
		t.Run(engine, func(t *testing.T) {
			f := newFixture(t)
			o := f.useEngine(t)
			kind, enc := engines.B2, engines.EncryptionRestic
			if engine == "rclone" {
				kind, enc = engines.S3, engines.EncryptionCrypt
			}
			dest := f.engineDest(t, engine, kind, enc)
			if _, err := f.dests.Update(f.ctx, dest.ID, destinations.Input{Retention: &destinations.Retention{ArrDaily: 2, ArrWeekly: 1}}); err != nil {
				t.Fatal(err)
			}
			for day := range 4 {
				f.clock.Set(cmdQueued.Add(30*time.Minute).AddDate(0, 0, day))
				if _, err := f.run(t, f.engineJob(t, dest, false)); err != nil {
					t.Fatal(err)
				}
			}
			rows := f.rowsOf(t, dest)
			if len(rows) != 2 || len(o.store.Stored()) != 2 || len(o.store.Removed()) != 2 {
				t.Fatalf("rows %+v, stored %d, removed %+v", rows, len(o.store.Stored()), o.store.Removed())
			}
			for _, r := range o.store.Removed() {
				if (r.Tx != nil) != (engine == "restic") || r.Kind != engines.VersionArr {
					t.Fatalf("remove %+v", r)
				}
			}
		})
	}
}
