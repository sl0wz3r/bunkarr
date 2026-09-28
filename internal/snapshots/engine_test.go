package snapshots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
)

var engineNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// engineFixture is EngineVersions of kind plexdb over a fake store, with the rows of fixture().
type engineFixture struct {
	st    *Store
	fake  *enginetest.FakeVersionStore
	ev    *EngineVersions
	warns []string
}

func newEngineFixture(t *testing.T, engine engines.Kind) *engineFixture {
	t.Helper()
	d, st := fixture(t)
	f := &engineFixture{st: st, fake: enginetest.NewFakeVersionStore()}
	f.ev = &EngineVersions{DB: d, Store: f.fake, Engine: engine, Kind: engines.VersionPlexDB, Now: func() time.Time { return engineNow },
		Warn: func(msg string, args ...any) { f.warns = append(f.warns, msg) }}
	return f
}

// stage writes a version directory with manifest.json naming job and a db file.
func stage(t *testing.T, job int64, queued time.Time) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "version")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(map[string]any{"integrationId": 3, "jobId": job, "jobQueuedAt": queued})
	for name, data := range map[string][]byte{"manifest.json": m, "library.db": []byte("0123456789")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// record inserts the plexdb row of a put version.
func (f *engineFixture) record(t *testing.T, logical string, job int64) func(ctx context.Context, ref engines.Ref) error {
	return func(ctx context.Context, ref engines.Ref) error {
		_, err := f.st.Insert(ctx, Snapshot{DestinationID: 2, Kind: KindPlexDB, IntegrationID: 3, JobID: 0, Path: logical,
			CreatedAt: engineNow, Size: 10, Method: "online_backup", Integrity: IntegrityOK, EngineRef: f.ev.RowRef(ref)})
		return err
	}
}

// recorded reports whether a listed version has a row (by reference on restic, path on rclone).
func (f *engineFixture) recorded(t *testing.T) func(v engines.StoredVersion) bool {
	return func(v engines.StoredVersion) bool {
		rows, err := f.st.List(context.Background(), 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if f.ev.Matches(v, EngineRow{Path: r.Path, EngineRef: r.EngineRef}) {
				return true
			}
		}
		return false
	}
}

func TestEngineVersionsPutRecordsTheReference(t *testing.T) {
	for _, engine := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(engine), func(t *testing.T) {
			f := newEngineFixture(t, engine)
			if engine == engines.Restic {
				f.fake.RefPrefix = "snap"
			}
			const logical = ".bunkarr/plex/plex-3/20260925T120000Z"
			ref, err := f.ev.Put(context.Background(), engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: logical,
				Dir: stage(t, 9, engineNow), Time: engineNow, JobID: 9, IntegrationID: 3}, f.record(t, logical, 9))
			if err != nil {
				t.Fatal(err)
			}
			rows, _ := f.st.List(context.Background(), 2)
			if len(rows) != 1 || rows[0].Path != logical {
				t.Fatalf("rows = %+v", rows)
			}
			wantRef := ""
			if engine == engines.Restic {
				wantRef = string(ref)
			}
			if ref != f.fake.Stored()[0].Ref {
				t.Fatalf("ref = %q", ref)
			}
			if rows[0].EngineRef != wantRef {
				t.Fatalf("engine_ref = %q, want %q", rows[0].EngineRef, wantRef)
			}
		})
	}
}

// TestEngineVersionsCrashBetweenPutAndRecord: a crash at versions.afterPut or
// versions.beforeRecord leaves an unrecorded version that the resumed job's Recover adopts once:
// no version is lost and none is recorded twice. On restic only the job's own snapshot is
// adopted.
func TestEngineVersionsCrashBetweenPutAndRecord(t *testing.T) {
	for _, engine := range []engines.Kind{engines.Restic, engines.Rclone} {
		for _, point := range []string{PointVersionsAfterPut, PointVersionsBeforeRecord} {
			t.Run(string(engine)+"/"+point, func(t *testing.T) {
				f := newEngineFixture(t, engine)
				const logical = ".bunkarr/plex/plex-3/20260925T120000Z"
				v := engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: logical, Dir: stage(t, 9, engineNow), Time: engineNow, JobID: 9,
					IntegrationID: 3}
				func() {
					faultinject.SetHook(faultinject.CrashAt(point, 1))
					defer faultinject.SetHook(nil)
					defer func() {
						if _, ok := recover().(faultinject.Crash); !ok {
							t.Fatal("no crash")
						}
					}()
					_, _ = f.ev.Put(context.Background(), v, f.record(t, logical, 9))
				}()
				if rows, _ := f.st.List(context.Background(), 2); len(rows) != 0 {
					t.Fatalf("recorded before the crash point: %+v", rows)
				}
				// Another job's run adopts nothing on restic; the resumed job adopts it.
				adopted := 0
				adopt := func(ctx context.Context, sv engines.StoredVersion) error {
					adopted++
					return f.record(t, sv.LogicalPath, 9)(ctx, sv.Ref)
				}
				in := RecoverInput{KindFolder: ".bunkarr/plex", JobID: 10, Recorded: f.recorded(t), Adopt: adopt}
				if _, err := f.ev.Recover(context.Background(), in); err != nil {
					t.Fatal(err)
				}
				wantOther := 0
				if engine == engines.Rclone {
					wantOther = 1 // rclone adopts any complete version (the Phase 1 rule)
				}
				if adopted != wantOther {
					t.Fatalf("job 10 adopted %d versions; want %d", adopted, wantOther)
				}
				in.JobID = 9
				l, err := f.ev.Recover(context.Background(), in)
				if err != nil {
					t.Fatal(err)
				}
				if adopted != 1 {
					t.Fatalf("adopted %d times in all; want 1", adopted)
				}
				rows, _ := f.st.List(context.Background(), 2)
				if len(rows) != 1 || len(f.fake.Stored()) != 1 || f.ev.Lost(l, EngineRow{Path: rows[0].Path, EngineRef: rows[0].EngineRef}) {
					t.Fatalf("rows %+v, stored %+v", rows, f.fake.Stored())
				}
			})
		}
	}
}

// TestEngineVersionsRecoverLeftovers: an rclone version without manifest.json and without a row
// is purged once it is LeftoverAge old, a younger one is left, versions of other integrations are
// not touched.
func TestEngineVersionsRecoverLeftovers(t *testing.T) {
	f := newEngineFixture(t, engines.Rclone)
	old := f.fake.AddLeftover(engines.StoredVersion{LogicalPath: ".bunkarr/plex/plex-3/20260923T120000Z", Time: engineNow.Add(-25 * time.Hour)},
		map[string][]byte{"library.db": []byte("x")})
	young := f.fake.AddLeftover(engines.StoredVersion{LogicalPath: ".bunkarr/plex/plex-3/20260925T020000Z", Time: engineNow.Add(-10 * time.Hour)},
		map[string][]byte{"library.db": []byte("x")})
	other := f.fake.AddLeftover(engines.StoredVersion{LogicalPath: ".bunkarr/plex/plex-7/20260923T120000Z", Time: engineNow.Add(-48 * time.Hour)},
		map[string][]byte{"library.db": []byte("x")})
	mine := func(v engines.StoredVersion) bool { return FolderIntegrationID(FolderOf(v.LogicalPath)) == 3 }
	l, err := f.ev.Recover(context.Background(), RecoverInput{KindFolder: ".bunkarr/plex", JobID: 1, Mine: mine, Recorded: f.recorded(t),
		Adopt: func(context.Context, engines.StoredVersion) error {
			t.Fatal("adopted an incomplete version")
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	removed := f.fake.Removed()
	if len(removed) != 1 || removed[0].Ref != old || removed[0].Tx != nil {
		t.Fatalf("removed = %+v", removed)
	}
	var left []engines.Ref
	for _, v := range l.Versions() {
		left = append(left, v.Ref)
	}
	if len(left) != 2 || left[0] != young || left[1] != other {
		t.Fatalf("listing = %v", left)
	}
	// Restic never purges (only forgets through retention).
	f = newEngineFixture(t, engines.Restic)
	f.fake.AddLeftover(engines.StoredVersion{LogicalPath: ".bunkarr/plex/plex-3/20260923T120000Z", Time: engineNow.Add(-48 * time.Hour), JobID: 1},
		map[string][]byte{"library.db": []byte("x")})
	if _, err := f.ev.Recover(context.Background(), RecoverInput{KindFolder: ".bunkarr/plex", JobID: 1, Recorded: f.recorded(t),
		Adopt: func(context.Context, engines.StoredVersion) error { return nil }}); err != nil || len(f.fake.Removed()) != 0 {
		t.Fatalf("restic recover removed %+v, %v", f.fake.Removed(), err)
	}
}

// TestEngineVersionsRemove: restic deletes the row and requests the forget in one transaction (a
// failed request keeps the row); rclone removes the version first, then the row.
func TestEngineVersionsRemove(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(engine), func(t *testing.T) {
			f := newEngineFixture(t, engine)
			const logical = ".bunkarr/plex/plex-3/20260920T120000Z"
			ref, err := f.ev.Put(ctx, engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: logical, Dir: stage(t, 1, engineNow),
				Time: engineNow, JobID: 1, IntegrationID: 3}, f.record(t, logical, 1))
			if err != nil {
				t.Fatal(err)
			}
			rows, _ := f.st.List(ctx, 2)
			row := EngineRow{Path: rows[0].Path, EngineRef: rows[0].EngineRef}
			del := func(ctx context.Context, tx *sql.Tx) error { return RemoveTx(ctx, tx, rows[0].ID) }

			f.fake.FailNext("Remove", errors.New("request refused"))
			if err := f.ev.Remove(ctx, nil, row, del); err == nil {
				t.Fatal("a failed Remove succeeded")
			}
			if got, _ := f.st.List(ctx, 2); len(got) != 1 {
				t.Fatalf("the row went although the version was not removed: %+v", got)
			}
			l := &Listing{}
			l.Add(engines.StoredVersion{Ref: ref, LogicalPath: logical, Complete: true})
			if err := f.ev.Remove(ctx, l, row, del); err != nil {
				t.Fatal(err)
			}
			if got, _ := f.st.List(ctx, 2); len(got) != 0 {
				t.Fatalf("rows left: %+v", got)
			}
			calls := f.fake.Removed()
			if len(calls) != 1 || calls[0].Ref != ref || calls[0].Kind != engines.VersionPlexDB || (calls[0].Tx != nil) != (engine == engines.Restic) {
				t.Fatalf("Remove calls = %+v", calls)
			}
			if len(l.Versions()) != 0 {
				t.Fatal("the listing still holds the removed version")
			}
		})
	}
}

func TestEngineVersionsLostAndFreePath(t *testing.T) {
	f := newEngineFixture(t, engines.Rclone)
	l := &Listing{}
	l.Add(engines.StoredVersion{Ref: ".bunkarr/plex/plex-3/20260925T120000Z", LogicalPath: ".bunkarr/plex/plex-3/20260925T120000Z", Complete: true})
	l.Add(engines.StoredVersion{Ref: ".bunkarr/plex/plex-3/20260924T120000Z", LogicalPath: ".bunkarr/plex/plex-3/20260924T120000Z"})
	for p, lost := range map[string]bool{".bunkarr/plex/plex-3/20260925T120000Z": false, ".bunkarr/plex/plex-3/20260924T120000Z": true,
		".bunkarr/plex/plex-3/20260923T120000Z": true} {
		if got := f.ev.Lost(l, EngineRow{Path: p}); got != lost {
			t.Errorf("Lost(%s) = %v", p, got)
		}
	}
	p, err := l.FreePath(".bunkarr/plex/plex-3", engineNow, 7, nil)
	if err != nil || p != ".bunkarr/plex/plex-3/20260925T120000Z-job7" {
		t.Fatalf("FreePath = %q, %v", p, err)
	}
	if p, err := l.FreePath(".bunkarr/plex/plex-3", engineNow.Add(time.Second), 7, nil); err != nil || p != ".bunkarr/plex/plex-3/20260925T120001Z" {
		t.Fatalf("FreePath = %q, %v", p, err)
	}
	taken := func(p string) bool { return strings.HasSuffix(p, "-job7") }
	if _, err := l.FreePath(".bunkarr/plex/plex-3", engineNow, 7, taken); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("FreePath = %v", err)
	}
}

func TestCheckSizes(t *testing.T) {
	v := engines.StoredVersion{Complete: true, Files: map[string]int64{"manifest.json": 30, "a.zip": 10}}
	if err := CheckSizes(v, map[string]int64{"manifest.json": -1, "a.zip": 10}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]map[string]int64{"size": {"a.zip": 11}, "missing": {"b.zip": 1}} {
		if err := CheckSizes(v, want); !errors.Is(err, ErrIncomplete) {
			t.Errorf("%s: %v", name, err)
		}
	}
	v.Complete = false
	if err := CheckSizes(v, nil); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("incomplete: %v", err)
	}
	if FolderOf(".bunkarr/arr/radarr-4/20260925T120000Z") != "radarr-4" || FolderOf("x") != "" {
		t.Fatal("FolderOf")
	}
}
