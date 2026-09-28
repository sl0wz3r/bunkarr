package plexdb

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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// engineOpener is an engines.VersionOpener over one fake store, counting the opens and closes.
type engineOpener struct {
	mu     sync.Mutex
	store  *enginetest.FakeVersionStore
	err    error
	opened int
	closed int
	rt     engines.Runtime
}

type closerFunc func() error

func (c closerFunc) Close() error { return c() }

func (o *engineOpener) open(ctx context.Context, destinationID int64, rt engines.Runtime) (engines.VersionStore, io.Closer, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return nil, nil, o.err
	}
	o.opened++
	o.rt = rt
	return o.store, closerFunc(func() error { o.mu.Lock(); o.closed++; o.mu.Unlock(); return nil }), nil
}

// engineDest inserts a restic or rclone destination row with a confirmed recovery kit (as
// destinations.Create and the kit confirmation leave it) and returns it.
func engineDest(t *testing.T, d *db.DB, dests *destinations.Store, engine string, kind engines.DestKind, enc engines.EncryptionMode, modes bool) destinations.Destination {
	t.Helper()
	ctx := context.Background()
	now := db.FormatTime(time.Now())
	var id int64
	err := d.Write(ctx, func(tx *sql.Tx) error {
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM destinations`).Scan(&n); err != nil {
			return err
		}
		remote := "{}"
		if kind == engines.S3 {
			remote = `{"provider":"Other","endpoint":"https://minio.local:9000","region":"us-east-1","bucket":"bk","prefix":"p"}`
		}
		var origin, confirmed, tag any
		if enc != engines.EncryptionNone {
			origin, confirmed = "generated", now
		}
		if engine == "restic" {
			tag = fmt.Sprintf("%032x", n+1)
		}
		caps, _ := json.Marshal(map[string]any{"enforcesModes": modes, "mtimeGranularityNs": 1})
		res, err := tx.ExecContext(ctx, `INSERT INTO destinations (name, engine, target, marker_id, fs_type, root_dev, capabilities, kind, remote,
			encryption, secret_origin, kit_confirmed_at, engine_tag, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("Offsite %d", n+1), engine, "s3:minio.local/bk/p", fmt.Sprintf("restic:%064x", n+1), string(kind), string(caps),
			string(kind), remote, string(enc), origin, confirmed, tag, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := dests.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return dest
}

// useEngine makes the fixture's runner open fake version stores and returns the opener.
func (f *fixture) useEngine(t *testing.T) *engineOpener {
	t.Helper()
	o := &engineOpener{store: enginetest.NewFakeVersionStore()}
	var err error
	if f.runner, err = NewRunner(Options{DB: f.db, Integrations: f.ints, Destinations: f.dests, ConfigDir: f.config,
		Now: f.clock.Now, Location: time.UTC, OpenVersions: o.open}); err != nil {
		t.Fatal(err)
	}
	return o
}

// engineJob is a plexdb_backup job row to destination dest.
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

// setTargets stores the integration's backup targets.
func (f *fixture) setTargets(t *testing.T, targets ...integrations.BackupTarget) {
	t.Helper()
	settings, _ := json.Marshal(map[string]any{"dataPath": f.data, "backup": map[string]any{"targets": targets}})
	it, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Name: f.integ.Name, URL: f.integ.URL, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	f.integ = it
}

// engineSnaps returns the rows of dest.
func (f *fixture) engineSnaps(t *testing.T, dest destinations.Destination) []snapshots.Snapshot {
	t.Helper()
	s, err := f.runner.Store().List(f.ctx, dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// checkEngineVersion checks a recorded version against the fake store: every manifest file with
// its size and sha256, manifest.json as recorded.
func checkEngineVersion(t *testing.T, store *enginetest.FakeVersionStore, s snapshots.Snapshot, ref engines.Ref) {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(s.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, mf := range m.Files {
		names = append(names, mf.Name)
		b, err := store.ReadFile(context.Background(), ref, mf.Name, 1<<30)
		sum := sha256.Sum256(b)
		if err != nil || int64(len(b)) != mf.Size || hex.EncodeToString(sum[:]) != mf.SHA256 {
			t.Fatalf("%s of %s: %d bytes, %v", mf.Name, ref, len(b), err)
		}
	}
	if !slices.Equal(names, []string{LibraryDB, BlobsDB, PreferencesXML}) {
		t.Fatalf("files %v", names)
	}
	stored, err := store.ReadFile(context.Background(), ref, ManifestName, 1<<20)
	if err != nil || strings.TrimSpace(string(stored)) != string(s.Manifest) {
		t.Fatalf("manifest.json %s, %v", stored, err)
	}
}

func TestEngineBackupPutsAndRecords(t *testing.T) {
	for _, tc := range []struct {
		engine string
		kind   engines.DestKind
		enc    engines.EncryptionMode
	}{{"restic", engines.B2, engines.EncryptionRestic}, {"rclone", engines.S3, engines.EncryptionCrypt}, {"restic", engines.Local, engines.EncryptionRestic}} {
		t.Run(tc.engine+"/"+string(tc.kind), func(t *testing.T) {
			f := newFixture(t)
			o := f.useEngine(t)
			dest := engineDest(t, f.db, f.dests, tc.engine, tc.kind, tc.enc, tc.enc != engines.EncryptionNone)
			res, err := f.run(t, f.engineJob(t, dest, false))
			if err != nil {
				t.Fatal(err)
			}
			st := res.Stats.(Stats)
			snaps := f.engineSnaps(t, dest)
			stored := o.store.Stored()
			if len(snaps) != 1 || len(stored) != 1 || st.Engine != tc.engine || st.Files != 3 || snaps[0].Integrity != IntegrityOK {
				t.Fatalf("stats %+v, rows %+v, stored %+v", st, snaps, stored)
			}
			s := snaps[0]
			if s.Path != FolderName(f.integ.Name, f.integ.ID)+"/20260924T120000Z" || stored[0].LogicalPath != s.Path || stored[0].JobID != f.lastJobID(t) {
				t.Fatalf("row %+v, stored %+v", s, stored[0])
			}
			wantRef := ""
			if tc.engine == "restic" {
				wantRef = string(stored[0].Ref)
			}
			if s.EngineRef != wantRef || st.EngineRef != string(stored[0].Ref) {
				t.Fatalf("engine_ref %q, stats %q, stored %q", s.EngineRef, st.EngineRef, stored[0].Ref)
			}
			checkEngineVersion(t, o.store, s, stored[0].Ref)
			if o.opened != 1 || o.closed != 1 || o.rt.JobID == 0 || o.rt.DryRun {
				t.Fatalf("opener: %+v", o)
			}
			if st, err := os.ReadDir(filepath.Join(f.config, StagingRoot)); err == nil && len(st) != 0 {
				t.Fatalf("staging left: %v", st)
			}
			if _, err := os.Stat(filepath.Join(f.target, ".bunkarr", "plex")); !os.IsNotExist(err) {
				t.Fatalf("the filecopy destination was written: %v", err)
			}
		})
	}
}

// lastJobID is the newest job's id.
func (f *fixture) lastJobID(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := f.db.Reader().QueryRow(`SELECT max(id) FROM jobs`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestEngineCredentialsGate: S17 for engines. A Plex version on an unencrypted remote
// destination fails the job before anything is stored; acceptInsecureModes of the UNAS target
// does not cover a second target on plain rclone; a local plain rclone destination needs its own
// target's flag; the recovery kit must be confirmed (not for a dry run).
func TestEngineCredentialsGate(t *testing.T) {
	f := newFixture(t)
	o := f.useEngine(t)
	plainS3 := engineDest(t, f.db, f.dests, "rclone", engines.S3, engines.EncryptionNone, false)
	plainLocal := engineDest(t, f.db, f.dests, "rclone", engines.Local, engines.EncryptionNone, false)
	f.setTargets(t, integrations.BackupTarget{DestinationID: f.dest.ID, AcceptInsecureModes: true}, integrations.BackupTarget{DestinationID: plainS3.ID},
		integrations.BackupTarget{DestinationID: plainLocal.ID})
	_, err := f.run(t, f.engineJob(t, plainS3, false))
	if !errors.Is(err, integrations.ErrUnencrypted) || !strings.Contains(err.Error(), "this backup holds Plex's credentials; use an encrypted destination") {
		t.Fatalf("plain s3: %v", err)
	}
	_, err = f.run(t, f.engineJob(t, plainLocal, false))
	if !errors.Is(err, integrations.ErrModes) {
		t.Fatalf("plain local without its flag: %v", err)
	}
	if o.opened != 0 || len(o.store.Stored()) != 0 {
		t.Fatalf("the store was used: opened %d, stored %+v", o.opened, o.store.Stored())
	}
	f.setTargets(t, integrations.BackupTarget{DestinationID: f.dest.ID}, integrations.BackupTarget{DestinationID: plainLocal.ID, AcceptInsecureModes: true})
	if _, err := f.run(t, f.engineJob(t, plainLocal, false)); err != nil {
		t.Fatalf("plain local with its flag: %v", err)
	}
	// An encrypted engine destination counts as keeping modes, whatever its probe says.
	encLocal := engineDest(t, f.db, f.dests, "restic", engines.Local, engines.EncryptionRestic, false)
	if _, err := f.run(t, f.engineJob(t, encLocal, false)); err != nil {
		t.Fatalf("encrypted local restic without the flag: %v", err)
	}
	// Custody (S21): a destination whose kit is not confirmed runs no backup; a dry run is allowed.
	enc := engineDest(t, f.db, f.dests, "restic", engines.B2, engines.EncryptionRestic, true)
	if err := f.db.Write(f.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, `UPDATE destinations SET kit_confirmed_at = NULL WHERE id = ?`, enc.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, f.engineJob(t, enc, false)); err == nil || !strings.Contains(err.Error(), destinations.BlockedKit) {
		t.Fatalf("unconfirmed kit: %v", err)
	}
	res, err := f.run(t, f.engineJob(t, enc, true))
	if err != nil || !res.Stats.(Stats).DryRun || len(f.engineSnaps(t, enc)) != 0 {
		t.Fatalf("dry run: %+v, %v", res, err)
	}
	// The opener's S25 errors fail the job.
	o.err = engines.ErrAnotherRepository
	restic := engineDest(t, f.db, f.dests, "restic", engines.B2, engines.EncryptionRestic, true)
	if _, err := f.run(t, f.engineJob(t, restic, false)); !errors.Is(err, engines.ErrAnotherRepository) {
		t.Fatalf("S25: %v", err)
	}
}

// TestPlexModesGateOnFilecopy: Preferences.xml on a local destination that does not keep modes
// needs the target's acceptInsecureModes (phase4.md §8.4 step 6).
func TestPlexModesGateOnFilecopy(t *testing.T) {
	f := newFixture(t)
	caps := f.dest.Capabilities
	caps.EnforcesModes = false
	raw, _ := json.Marshal(caps)
	if err := f.db.Write(f.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, `UPDATE destinations SET capabilities = ? WHERE id = ?`, string(raw), f.dest.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, f.newJob(t, false)); !errors.Is(err, integrations.ErrModes) || !strings.Contains(err.Error(), "acceptInsecureModes") {
		t.Fatalf("err = %v", err)
	}
	f.assertConverged(t, 0)
	f.setTargets(t, integrations.BackupTarget{DestinationID: f.dest.ID, AcceptInsecureModes: true})
	if _, err := f.run(t, f.newJob(t, false)); err != nil {
		t.Fatal(err)
	}
	f.assertConverged(t, 1)
}

func TestEngineDryRun(t *testing.T) {
	f := newFixture(t)
	o := f.useEngine(t)
	dest := engineDest(t, f.db, f.dests, "rclone", engines.S3, engines.EncryptionCrypt, true)
	job := f.engineJob(t, dest, true)
	res, err := f.run(t, job)
	if err != nil {
		t.Fatal(err)
	}
	st := res.Stats.(Stats)
	if !st.DryRun || st.Files != 3 || len(o.store.Stored()) != 0 || !o.rt.DryRun || !strings.Contains(res.Summary, dest.Name) {
		t.Fatalf("dry run %+v / %+v", res, o.store.Stored())
	}
	counts, _ := f.jobs.Counts(f.ctx, job.ID)
	if len(counts) != 1 || counts[0].Status != jobs.ItemSkipped || counts[0].Files != 3 {
		t.Fatalf("items %+v", counts)
	}
}

// TestEnginePrune: the keep math is unchanged; on restic each pruned version's row and forget
// request are one transaction, on rclone the version is removed first.
func TestEnginePrune(t *testing.T) {
	for _, tc := range []struct {
		engine string
		kind   engines.DestKind
		enc    engines.EncryptionMode
	}{{"restic", engines.B2, engines.EncryptionRestic}, {"rclone", engines.S3, engines.EncryptionCrypt}} {
		t.Run(tc.engine, func(t *testing.T) {
			f := newFixture(t)
			o := f.useEngine(t)
			dest := engineDest(t, f.db, f.dests, tc.engine, tc.kind, tc.enc, true)
			if _, err := f.dests.Update(f.ctx, dest.ID, destinations.Input{Retention: &destinations.Retention{PlexDBDaily: 2, PlexDBWeekly: 1}}); err != nil {
				t.Fatal(err)
			}
			for i := range 4 {
				f.clock.Set(time.Date(2026, 9, 24+i, 12, 0, 0, 0, time.UTC))
				if _, err := f.run(t, f.engineJob(t, dest, false)); err != nil {
					t.Fatal(err)
				}
			}
			snaps := f.engineSnaps(t, dest)
			if len(snaps) != 2 || !strings.HasSuffix(snaps[0].Path, "20260927T120000Z") || !strings.HasSuffix(snaps[1].Path, "20260926T120000Z") {
				t.Fatalf("rows %+v", snaps)
			}
			removed := o.store.Removed()
			if len(removed) != 2 {
				t.Fatalf("removed %+v", removed)
			}
			for _, r := range removed {
				if (r.Tx != nil) != (tc.engine == "restic") || r.Kind != engines.VersionPlexDB {
					t.Fatalf("remove %+v", r)
				}
			}
			if len(o.store.Stored()) != 2 {
				t.Fatalf("stored %+v", o.store.Stored())
			}
			// A restic forget request that fails keeps the row (one transaction).
			if tc.engine == "restic" {
				o.store.FailAlways("Remove", errors.New("no forget request"))
				f.clock.Set(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
				res, err := f.run(t, f.engineJob(t, dest, false))
				if err != nil || res.Warnings == 0 || len(f.engineSnaps(t, dest)) != 3 {
					t.Fatalf("failed request: %+v, %v, rows %d", res, err, len(f.engineSnaps(t, dest)))
				}
			}
		})
	}
}

// TestEngineRecovery: an rclone upload without manifest.json older than 24 h is purged, a younger
// one stays; a recorded version gone from the destination loses its row; a version of another
// integration is left alone.
func TestEngineRecovery(t *testing.T) {
	f := newFixture(t)
	o := f.useEngine(t)
	dest := engineDest(t, f.db, f.dests, "rclone", engines.S3, engines.EncryptionCrypt, true)
	folder := FolderName(f.integ.Name, f.integ.ID)
	old := o.store.AddLeftover(engines.StoredVersion{LogicalPath: folder + "/20260920T120000Z", Time: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)},
		map[string][]byte{LibraryDB: []byte("partial")})
	young := o.store.AddLeftover(engines.StoredVersion{LogicalPath: folder + "/20260924T020000Z", Time: time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)},
		map[string][]byte{LibraryDB: []byte("partial")})
	other := o.store.AddLeftover(engines.StoredVersion{LogicalPath: ".bunkarr/plex/other-99/20260920T120000Z", Time: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)},
		map[string][]byte{LibraryDB: []byte("partial")})
	if _, err := f.runner.Store().Insert(f.ctx, snapshots.Snapshot{DestinationID: dest.ID, Kind: snapshots.KindPlexDB, IntegrationID: f.integ.ID,
		Path: folder + "/20260922T120000Z", CreatedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), Size: 1, Method: MethodOnlineBackup,
		Integrity: IntegrityOK}); err != nil {
		t.Fatal(err)
	}
	res, err := f.run(t, f.engineJob(t, dest, false))
	if err != nil {
		t.Fatal(err)
	}
	var removed []engines.Ref
	for _, r := range o.store.Removed() {
		removed = append(removed, r.Ref)
	}
	if !slices.Equal(removed, []engines.Ref{old}) {
		t.Fatalf("removed %v (young %v, other %v)", removed, young, other)
	}
	snaps := f.engineSnaps(t, dest)
	if len(snaps) != 1 || res.Warnings == 0 || !f.rep.has("was missing or incomplete at the destination") {
		t.Fatalf("rows %+v, warnings %d\n%s", snaps, res.Warnings, f.rep.text())
	}
}

// TestEngineCrashAtVersionPoints: a crash at versions.afterPut or versions.beforeRecord resumes
// without a lost or a duplicate version: the resumed job adopts its own version (restic: only its
// own job's snapshot) and finishes with it.
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
				dest := engineDest(t, f.db, f.dests, engine, kind, enc, true)
				job := f.engineJob(t, dest, false)
				f.crashRun(t, job, point)
				if len(f.engineSnaps(t, dest)) != 0 || len(o.store.Stored()) != 1 {
					t.Fatalf("after the crash: rows %+v, stored %+v", f.engineSnaps(t, dest), o.store.Stored())
				}
				// Another job's run (restic) leaves the unrecorded snapshot alone.
				if engine == "restic" {
					f.clock.Set(f.clock.Now().Add(time.Minute))
					other := f.engineJob(t, dest, false)
					if _, err := f.run(t, other); err != nil {
						t.Fatal(err)
					}
					if n := len(f.engineSnaps(t, dest)); n != 1 {
						t.Fatalf("another job adopted this job's snapshot: %d rows", n)
					}
				}
				job.Attempt, job.Trigger = 2, jobs.TriggerResume
				f.clock.Set(f.clock.Now().Add(time.Minute))
				res, err := f.run(t, job)
				if err != nil {
					t.Fatal(err)
				}
				st := res.Stats.(Stats)
				if st.Recovered != 1 || !f.rep.has("An earlier attempt of this job wrote its Plex DB version") {
					t.Fatalf("stats %+v\n%s", st, f.rep.text())
				}
				want := 1
				if engine == "restic" {
					want = 2 // the other job's version
				}
				if rows, stored := f.engineSnaps(t, dest), o.store.Stored(); len(rows) != want || len(stored) != want {
					t.Fatalf("rows %+v, stored %+v", rows, stored)
				}
			})
		}
	}
}
