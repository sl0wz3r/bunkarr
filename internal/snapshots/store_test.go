package snapshots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// fixture opens a migrated database holding destination 2 and integrations 3 (plex) and 4
// (radarr).
func fixture(t *testing.T) (*db.DB, *Store) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "bunkarr.db"), nil)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	const now = "2026-09-25T00:00:00.000000000Z"
	err = d.Write(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`INSERT INTO integrations (id, type, name, url, created_at, updated_at)
				VALUES (3, 'plex', 'Plex', 'http://plex:32400', '` + now + `', '` + now + `'),
				       (4, 'radarr', 'Radarr', 'http://radarr:7878', '` + now + `', '` + now + `')`,
			`INSERT INTO destinations (id, name, engine, target, marker_id, fs_type, root_dev, created_at, updated_at)
				VALUES (2, 'UNAS', 'filecopy', '/backup', 'm-1', 'nfs', 1, '` + now + `', '` + now + `')`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return d, NewStore(d)
}

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, st := fixture(t)
	day := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	plex1, err := st.Insert(ctx, Snapshot{DestinationID: 2, Kind: KindPlexDB, IntegrationID: 3,
		Path: ".bunkarr/plex/plex-3/20260923T120000Z", CreatedAt: day.AddDate(0, 0, -1), Size: 10, Method: "online_backup",
		Integrity: IntegrityOK, Manifest: json.RawMessage(`{"format":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	arr, err := st.Insert(ctx, Snapshot{DestinationID: 2, Kind: KindArr, IntegrationID: 4,
		Path: ".bunkarr/arr/radarr-4/20260924T120000Z", CreatedAt: day, Size: 20, Method: "arr_api_folder",
		Integrity: IntegrityFailed})
	if err != nil {
		t.Fatal(err)
	}
	if plex1.ID == 0 || arr.ID == 0 || string(arr.Manifest) != "{}" {
		t.Fatalf("inserted %+v %+v", plex1, arr)
	}

	// List: every kind, newest first.
	all, err := st.List(ctx, 2)
	if err != nil || len(all) != 2 || all[0].ID != arr.ID || all[1].ID != plex1.ID {
		t.Fatalf("List = %+v, %v", all, err)
	}
	if all[0].Kind != KindArr || all[1].Kind != KindPlexDB || !all[1].CreatedAt.Equal(plex1.CreatedAt) ||
		string(all[1].Manifest) != `{"format":1}` || all[1].IntegrationID != 3 || all[1].JobID != 0 {
		t.Fatalf("List rows %+v", all)
	}
	// ListFor: one kind of one integration.
	if l, err := st.ListFor(ctx, KindPlexDB, 2, 3); err != nil || len(l) != 1 || l[0].ID != plex1.ID {
		t.Fatalf("ListFor(plexdb, 3) = %+v, %v", l, err)
	}
	if l, err := st.ListFor(ctx, KindPlexDB, 2, 4); err != nil || len(l) != 0 {
		t.Fatalf("ListFor(plexdb, 4) = %+v, %v", l, err)
	}
	if l, err := st.ListFor(ctx, KindArr, 2, 4); err != nil || len(l) != 1 || l[0].ID != arr.ID {
		t.Fatalf("ListFor(arr, 4) = %+v, %v", l, err)
	}
	// Get: any kind.
	if g, err := st.Get(ctx, arr.ID); err != nil || g.Path != arr.Path || g.Kind != KindArr {
		t.Fatalf("Get = %+v, %v", g, err)
	}
	if _, err := st.Get(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(999) = %v", err)
	}
	// Recorded.
	if ok, err := st.Recorded(ctx, 2, plex1.Path); err != nil || !ok {
		t.Fatalf("Recorded = %v, %v", ok, err)
	}
	if ok, err := st.Recorded(ctx, 2, ".bunkarr/plex/plex-3/20200101T000000Z"); err != nil || ok {
		t.Fatalf("Recorded(missing) = %v, %v", ok, err)
	}
	// A version directory is recorded at most once per destination.
	if _, err := st.Insert(ctx, Snapshot{DestinationID: 2, Kind: KindPlexDB, IntegrationID: 3, Path: plex1.Path,
		CreatedAt: day, Integrity: IntegrityOK}); err == nil {
		t.Fatal("a second row for one directory was accepted")
	}
	// Remove, twice.
	if err := st.Remove(ctx, plex1.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Remove(ctx, plex1.ID); err != nil {
		t.Fatalf("removing a removed row: %v", err)
	}
	if all, _ := st.List(ctx, 2); len(all) != 1 {
		t.Fatalf("after Remove: %+v", all)
	}
}

func TestStoreInsertValidation(t *testing.T) {
	ctx := context.Background()
	_, st := fixture(t)
	good := Snapshot{DestinationID: 2, Kind: KindPlexDB, IntegrationID: 3, Path: ".bunkarr/plex/plex-3/20260924T120000Z",
		CreatedAt: time.Now(), Integrity: IntegrityOK}
	for name, mut := range map[string]func(s *Snapshot){
		"unknown kind":      func(s *Snapshot) { s.Kind = "restic" },
		"no kind":           func(s *Snapshot) { s.Kind = "" },
		"no path":           func(s *Snapshot) { s.Path = "" },
		"unknown integrity": func(s *Snapshot) { s.Integrity = "maybe" },
		"invalid manifest":  func(s *Snapshot) { s.Manifest = json.RawMessage(`{"a":`) },
		"no destination":    func(s *Snapshot) { s.DestinationID = 99 },
	} {
		s := good
		mut(&s)
		if _, err := st.Insert(ctx, s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if all, _ := st.List(ctx, 2); len(all) != 0 {
		t.Fatalf("a refused insert left rows: %+v", all)
	}
}

func TestStoreKeepsRowsOfDeletedIntegrationsAndJobs(t *testing.T) {
	// The integration and job links are ON DELETE SET NULL: the row stays, with 0.
	ctx := context.Background()
	d, st := fixture(t)
	const now = "2026-09-25T00:00:00.000000000Z"
	err := d.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO jobs (id, type, status, trigger, params, queued_at)
			VALUES (7, 'arr_backup', 'completed', 'manual', '{"integrationId":4}', '`+now+`')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Insert(ctx, Snapshot{DestinationID: 2, Kind: KindArr, IntegrationID: 4, JobID: 7,
		Path: ".bunkarr/arr/radarr-4/20260924T120000Z", CreatedAt: time.Now(), Integrity: IntegrityOK})
	if err != nil {
		t.Fatal(err)
	}
	err = d.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id = 7`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM integrations WHERE id = 4`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	g, err := st.Get(ctx, s.ID)
	if err != nil || g.IntegrationID != 0 || g.JobID != 0 {
		t.Fatalf("after deleting the integration and the job: %+v, %v", g, err)
	}
}

func TestSnapshotJSONShape(t *testing.T) {
	s := Snapshot{ID: 1, DestinationID: 2, Kind: KindArr, IntegrationID: 4, JobID: 7, Path: "p",
		CreatedAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), Size: 3, Method: "arr_api_http", Integrity: IntegrityOK,
		Manifest: json.RawMessage(`{}`)}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "destinationId", "kind", "integrationId", "jobId", "path", "createdAt", "size", "method",
		"integrity", "manifest"} {
		if _, ok := m[k]; !ok {
			t.Errorf("JSON lacks %q: %s", k, b)
		}
	}
	if len(m) != 11 {
		t.Errorf("JSON has %d keys: %s", len(m), b)
	}
}
