package manifest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// engineOpener is an engines.VersionOpener over one fake store.
type engineOpener struct {
	mu     sync.Mutex
	store  *enginetest.FakeVersionStore
	err    error
	opened int
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
	return o.store, closerFunc(func() error { return nil }), nil
}

// useEngine makes e.dest a new engine destination (kit confirmed; plain rclone when enc is none)
// and the runner open fake version stores.
func (e *testEnv) useEngine(engine string, kind engines.DestKind, enc engines.EncryptionMode) *engineOpener {
	e.t.Helper()
	now := db.FormatTime(time.Now())
	var id int64
	err := e.db.Write(e.ctx, func(tx *sql.Tx) error {
		var origin, confirmed, tag any
		if enc != engines.EncryptionNone {
			origin, confirmed = "generated", now
		}
		if engine == "restic" {
			tag = fmt.Sprintf("%032x", 7)
		}
		res, err := tx.ExecContext(e.ctx, `INSERT INTO destinations (name, engine, target, marker_id, fs_type, root_dev, kind, encryption,
			secret_origin, kit_confirmed_at, engine_tag, created_at, updated_at) VALUES ('Offsite', ?, 's3:minio/bk', 'marker-offsite', ?, 0, ?, ?, ?, ?, ?, ?, ?)`,
			engine, string(kind), string(kind), string(enc), origin, confirmed, tag, now, now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.ExecContext(e.ctx, `INSERT INTO destination_sources (destination_id, source_id) VALUES (?, ?)`, id, e.src.ID)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if e.dest, err = e.dests.Get(e.ctx, id); err != nil {
		e.t.Fatal(err)
	}
	o := &engineOpener{store: enginetest.NewFakeVersionStore()}
	r, err := NewRunner(Options{DB: e.db, Catalog: e.cat, Integrations: e.ints, Destinations: e.dests, Index: e.index.Store(),
		Tiers: e.tiers, ConfigDir: e.config, Now: e.clock.Now, Location: time.UTC, Version: "test", OpenVersions: o.open})
	if err != nil {
		e.t.Fatal(err)
	}
	e.runner = r
	return o
}

// checkStored checks a recorded engine version: its three files, SHA256SUMS naming them, the
// row's checksum that of manifest.json.
func checkStored(t *testing.T, store *enginetest.FakeVersionStore, v Version, ref engines.Ref) {
	t.Helper()
	sums, err := store.ReadFile(context.Background(), ref, SumsName, maxSumsBytes)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSums(sums)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{JSONName, CSVName} {
		b, err := store.ReadFile(context.Background(), ref, name, 1<<30)
		sum := sha256.Sum256(b)
		if err != nil || hex.EncodeToString(sum[:]) != parsed[name] {
			t.Fatalf("%s of %s does not match SHA256SUMS (%v)", name, ref, err)
		}
	}
	if Checksum(parsed[JSONName]) != v.Checksum {
		t.Fatalf("row checksum %s, SHA256SUMS %s", v.Checksum, parsed[JSONName])
	}
}

func TestEngineExport(t *testing.T) {
	for _, tc := range []struct {
		engine string
		kind   engines.DestKind
		enc    engines.EncryptionMode
	}{{"restic", engines.B2, engines.EncryptionRestic}, {"rclone", engines.S3, engines.EncryptionNone}} {
		t.Run(tc.engine, func(t *testing.T) {
			e := newEnv(t)
			o := e.useEngine(tc.engine, tc.kind, tc.enc)
			st, rep := e.runOK()
			vs := e.versions()
			stored := o.store.Stored()
			if len(vs) != 1 || len(stored) != 1 || st.Engine != tc.engine || st.ManifestID != vs[0].ID || stored[0].LogicalPath != vs[0].Path ||
				!strings.HasPrefix(vs[0].Path, Root+"/") {
				t.Fatalf("stats %+v, versions %+v, stored %+v\n%s", st, vs, stored, rep)
			}
			wantRef := ""
			if tc.engine == "restic" {
				wantRef = string(stored[0].Ref)
			}
			if vs[0].EngineRef != wantRef || st.EngineRef != string(stored[0].Ref) {
				t.Fatalf("engine_ref %q (want %q), stats %q", vs[0].EngineRef, wantRef, st.EngineRef)
			}
			checkStored(t, o.store, vs[0], stored[0].Ref)
			if es, _ := os.ReadDir(e.config + "/staging"); len(es) != 0 {
				t.Fatalf("staging left: %v", es)
			}
			// Unchanged: SHA256SUMS reads back with the recorded checksum; nothing is stored.
			e.clock.Advance(time.Hour)
			st, _ = e.runOK()
			if !st.Unchanged || len(o.store.Stored()) != 1 {
				t.Fatalf("second export %+v, stored %d", st, len(o.store.Stored()))
			}
		})
	}
}

// TestEngineUnchangedDamaged: a newest version whose SHA256SUMS no longer names the recorded
// checksum, or that is gone, is not "unchanged": it is marked damaged (or its row dropped) and a
// new version is stored.
func TestEngineUnchangedDamaged(t *testing.T) {
	for _, name := range []string{"sums changed", "version gone"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			o := e.useEngine("rclone", engines.S3, engines.EncryptionCrypt)
			first, _ := e.runOK()
			v := o.store.Stored()[0]
			if err := o.store.Remove(e.ctx, nil, v.Ref, engines.VersionManifest); err != nil {
				t.Fatal(err)
			}
			if name == "sums changed" {
				files := map[string][]byte{}
				for _, n := range []string{JSONName, CSVName} {
					files[n] = []byte("x")
				}
				files[SumsName] = FormatSums(strings.Repeat("a", 64), strings.Repeat("b", 64))
				o.store.AddLeftover(engines.StoredVersion{Ref: v.Ref, LogicalPath: v.LogicalPath, Time: v.Time, Complete: true}, files)
			}
			e.clock.Advance(time.Hour)
			res, rep, err := e.run(e.newJob(false))
			if err != nil {
				t.Fatalf("%v\n%s", err, rep)
			}
			st := res.Stats.(Stats)
			if st.Unchanged || st.ManifestID == first.ManifestID {
				t.Fatalf("stats %+v\n%s", st, rep)
			}
			byID := map[int64]Version{}
			for _, x := range e.versions() {
				byID[x.ID] = x
			}
			old, kept := byID[first.ManifestID]
			switch {
			case name == "version gone" && kept:
				t.Fatalf("the row of a lost version was kept: %+v", old)
			case name == "sums changed" && (!kept || old.Integrity != IntegrityDamaged || st.DamagedFound != 1):
				t.Fatalf("damaged version %+v (kept %v), stats %+v", old, kept, st)
			}
		})
	}
}

// TestEngineDownload: a download fetches the file into staging and verifies it against the
// recorded checksum and SHA256SUMS; a damaged version is refused (and marked); a destination that
// does not open is ErrUnreachable.
func TestEngineDownload(t *testing.T) {
	e := newEnv(t)
	o := e.useEngine("restic", engines.B2, engines.EncryptionRestic)
	st, _ := e.runOK()
	ref := o.store.Stored()[0].Ref
	for _, format := range []string{FormatJSON, FormatCSV} {
		f, err := e.runner.Download(e.ctx, st.ManifestID, format)
		if err != nil {
			t.Fatal(err)
		}
		name := JSONName
		if format == FormatCSV {
			name = CSVName
		}
		want, _ := o.store.ReadFile(e.ctx, ref, name, 1<<30)
		rd, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rd)
		_ = rd.Close()
		sum := sha256.Sum256(want)
		if string(got) != string(want) || f.Size != int64(len(want)) || f.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("%s download differs", format)
		}
		if fi, err := os.Stat(f.path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("staged file %v, %v", fi, err)
		}
		if fi, err := os.Stat(f.dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("staging directory %v, %v", fi, err)
		}
		_ = f.Close()
	}
	o.err = engines.ErrWrongPassword
	if _, err := e.runner.Download(e.ctx, st.ManifestID, FormatJSON); !errors.Is(err, ErrUnreachable) || !errors.Is(err, engines.ErrWrongPassword) {
		t.Fatalf("unreachable: %v", err)
	}
	o.err = nil
	// Damage manifest.json: the download is refused and the version marked damaged.
	v := o.store.Stored()[0]
	sums, _ := o.store.ReadFile(e.ctx, ref, SumsName, maxSumsBytes)
	csv, _ := o.store.ReadFile(e.ctx, ref, CSVName, 1<<30)
	_ = o.store.Remove(e.ctx, nil, ref, engines.VersionManifest)
	o.store.AddLeftover(engines.StoredVersion{Ref: ref, LogicalPath: v.LogicalPath, Time: v.Time, Complete: true},
		map[string][]byte{JSONName: []byte(`{"format":"bunkarr-manifest"}`), CSVName: csv, SumsName: sums})
	if _, err := e.runner.Download(e.ctx, st.ManifestID, FormatJSON); !errors.Is(err, ErrDamaged) {
		t.Fatalf("damaged: %v", err)
	}
	if got, _ := e.runner.Store().Get(e.ctx, st.ManifestID); got.Integrity != IntegrityDamaged {
		t.Fatalf("version %+v", got)
	}
	if es, _ := os.ReadDir(e.config + "/staging"); len(es) != 0 {
		t.Fatalf("staging left: %v", es)
	}
}

// TestEngineCrashAtVersionPoints: a crash between Put and the row resumes without a lost or a
// duplicate version; on restic another job does not adopt the snapshot.
func TestEngineCrashAtVersionPoints(t *testing.T) {
	for _, engine := range []string{"restic", "rclone"} {
		for _, point := range []string{snapshots.PointVersionsAfterPut, snapshots.PointVersionsBeforeRecord} {
			t.Run(engine+"/"+point, func(t *testing.T) {
				e := newEnv(t)
				kind, enc := engines.B2, engines.EncryptionRestic
				if engine == "rclone" {
					kind, enc = engines.S3, engines.EncryptionCrypt
				}
				o := e.useEngine(engine, kind, enc)
				job := e.newJob(false)
				e.crashRun(job, point)
				if len(e.versions()) != 0 || len(o.store.Stored()) != 1 {
					t.Fatalf("after the crash: rows %+v, stored %+v", e.versions(), o.store.Stored())
				}
				job.Attempt, job.Trigger = 2, jobs.TriggerResume
				e.clock.Advance(time.Minute)
				res, rep, err := e.run(job)
				if err != nil {
					t.Fatalf("%v\n%s", err, rep)
				}
				vs := e.versions()
				if res.Stats.(Stats).Recovered != 1 || len(vs) != 1 || len(o.store.Stored()) != 1 || vs[0].JobID != job.ID ||
					res.Stats.(Stats).ManifestID != vs[0].ID {
					t.Fatalf("resumed: %+v, rows %+v, stored %d\n%s", res.Stats, vs, len(o.store.Stored()), rep)
				}
			})
		}
	}
}

// TestEnginePrune: restic removes a pruned version's row with its forget request in one
// transaction; rclone purges first.
func TestEnginePrune(t *testing.T) {
	for _, engine := range []string{"restic", "rclone"} {
		t.Run(engine, func(t *testing.T) {
			e := newEnv(t)
			kind, enc := engines.B2, engines.EncryptionRestic
			if engine == "rclone" {
				kind, enc = engines.S3, engines.EncryptionCrypt
			}
			o := e.useEngine(engine, kind, enc)
			e.setRetention(1, 1)
			e.runOK()
			for i := range 2 {
				e.clock.Advance(24 * time.Hour)
				e.refresh(e.rad.ID)
				e.refresh(e.son.ID)
				e.writeFile(fmt.Sprintf("movies/new-%d.mkv", i), 10)
				e.scan()
				e.runOK()
			}
			if len(e.versions()) != 1 || len(o.store.Stored()) != 1 || len(o.store.Removed()) != 2 {
				t.Fatalf("versions %+v, stored %d, removed %+v", e.versions(), len(o.store.Stored()), o.store.Removed())
			}
			for _, r := range o.store.Removed() {
				if (r.Tx != nil) != (engine == "restic") || r.Kind != engines.VersionManifest {
					t.Fatalf("remove %+v", r)
				}
			}
		})
	}
}

// TestEngineManifestOnPlainRclone: manifests hold no secrets (S20), so an unencrypted rclone
// destination takes them; a destination whose kit is not confirmed runs only dry runs.
func TestEngineManifestOnPlainRclone(t *testing.T) {
	e := newEnv(t)
	o := e.useEngine("rclone", engines.S3, engines.EncryptionNone)
	if st, _ := e.runOK(); st.ManifestID == 0 || len(o.store.Stored()) != 1 {
		t.Fatalf("stats %+v", st)
	}
	e2 := newEnv(t)
	o2 := e2.useEngine("restic", engines.B2, engines.EncryptionRestic)
	if err := e2.db.Write(e2.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(e2.ctx, `UPDATE destinations SET kit_confirmed_at = NULL WHERE id = ?`, e2.dest.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e2.run(e2.newJob(false)); err == nil || !strings.Contains(err.Error(), destinations.BlockedKit) {
		t.Fatalf("unconfirmed kit: %v", err)
	}
	res, _, err := e2.run(e2.newJob(true))
	if err != nil || !res.Stats.(Stats).DryRun || len(o2.store.Stored()) != 0 {
		t.Fatalf("dry run %+v, %v", res, err)
	}
}
