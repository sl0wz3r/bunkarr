package mediaindex

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
)

// TestUnmappedPagesWithoutLoadingEveryFile: the unmapped listing pages in SQL and checks only the
// files whose recorded location has no live catalog file of the *arr's size against the catalog,
// not every indexed file on every page request.
func TestUnmappedPagesWithoutLoadingEveryFile(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	const extra = 40
	var movies []map[string]any
	for i := range extra {
		dir := fmt.Sprintf("Extra %02d", i)
		p := "/movies/" + dir + "/" + dir + ".mkv"
		size := int64(1000 + i)
		reported := size
		switch {
		case i < 3:
			reported++ // mismatched: the catalog file has another size
		case i == 3:
			p = "/movies/Missing/Missing.mkv" // mismatched: no catalog file
		case i >= extra-2:
			p = "/movies-4k/" + dir + "/" + dir + ".mkv" // unmapped
		}
		if i != 3 && i < extra-2 {
			e.writeFile(p, size)
		}
		movies = append(movies, map[string]any{"id": 100 + i, "title": dir, "year": 2000, "path": path.Dir(p),
			"rootFolderPath": "/movies", "hasFile": true, "movieFileId": 1000 + i, "monitored": true, "qualityProfileId": 6,
			"movieFile": map[string]any{"id": 1000 + i, "path": p, "size": reported}})
	}
	e.editList("movie", "movie.json", func(list []map[string]any) []map[string]any { return append(list, movies...) })
	e.scan()
	e.full()

	s := e.runner.Store()
	type row struct {
		Path   string
		Reason string
		CatSz  int64
	}
	pageOf := func(page int) ([]row, int64) {
		t.Helper()
		p, err := s.Unmapped(context.Background(), e.it.ID, page, 4)
		if err != nil {
			t.Fatal(err)
		}
		out := []row{}
		for _, r := range p.Records {
			cs := int64(-1)
			if r.CatalogSize != nil {
				cs = *r.CatalogSize
			}
			out = append(out, row{r.Path, r.Reason, cs})
		}
		return out, p.TotalRecords
	}
	s.matchChecks.Store(0)
	p1, total := pageOf(1)
	want1 := []row{
		{"/movies-4k/Extra 38/Extra 38.mkv", ReasonUnmapped, -1},
		{"/movies-4k/Extra 39/Extra 39.mkv", ReasonUnmapped, -1},
		{"/movies/Extra 00/Extra 00.mkv", ReasonMismatched, 1000},
		{"/movies/Extra 01/Extra 01.mkv", ReasonMismatched, 1001},
	}
	if total != 6 || !reflect.DeepEqual(p1, want1) {
		t.Fatalf("page 1 = %+v (total %d), want %+v", p1, total, want1)
	}
	// Only the four mismatched files were checked against the catalog, not the 39 located ones.
	if n := s.matchChecks.Load(); n != 4 {
		t.Fatalf("checked %d files against the catalog, want 4", n)
	}
	p2, total := pageOf(2)
	want2 := []row{
		{"/movies/Extra 02/Extra 02.mkv", ReasonMismatched, 1002},
		{"/movies/Missing/Missing.mkv", ReasonMismatched, -1},
	}
	if total != 6 || !reflect.DeepEqual(p2, want2) {
		t.Fatalf("page 2 = %+v (total %d), want %+v", p2, total, want2)
	}
	if p3, total := pageOf(3); total != 6 || len(p3) != 0 {
		t.Fatalf("page 3 = %+v (total %d)", p3, total)
	}
}

// insertLocated inserts n located files (one item each unless paths repeat) of integration e.it
// in source e.src with no catalog file, so all of them are mismatched. pathOf names file i's
// relative path; file i's size is 1000+i.
func insertLocated(t *testing.T, e *testEnv, n int, pathOf func(i int) string) {
	t.Helper()
	ctx := context.Background()
	seen := db.FormatTime(time.Now().UTC())
	err := e.db.Write(ctx, func(tx *sql.Tx) error {
		for i := range n {
			var id int64
			if err := tx.QueryRowContext(ctx, `INSERT INTO arr_items (integration_id, kind, arr_id, title, year, external_ids, path,
				root_folder, quality_profile_id, metadata_profile_id, monitored, tags, genres, added_at, detail, seen_at, deleted_at)
				VALUES (?, 'movie', ?, ?, 2000, '{}', ?, '/movies', 0, 0, 1, '[]', '[]', NULL, '{}', ?, NULL) RETURNING id`,
				e.it.ID, 100000+i, fmt.Sprintf("M%05d", i), fmt.Sprintf("/movies/M%05d", i), seen).Scan(&id); err != nil {
				return err
			}
			rel := pathOf(i)
			if _, err := tx.ExecContext(ctx, `INSERT INTO arr_files (integration_id, item_id, arr_file_id, path, local_path, source_id,
				rel_path, size, quality, date_added, detail, seen_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', NULL, '{}', ?)`,
				e.it.ID, id, 100000+i, "/movies/"+rel, e.src.Path+"/"+rel, e.src.ID, rel, 1000+i, seen); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestUnmappedMismatchedChunksKeepSharedPaths: the mismatched listing reads its candidates in
// keyset chunks over (path, id); files that share a path across a chunk boundary are each listed
// once, in path order, and each is checked against the catalog once.
func TestUnmappedMismatchedChunksKeepSharedPaths(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	const n = 2*mismatchedChunk + 7
	// Three files per path, so chunk boundaries (500, 1000) fall inside a run of equal paths.
	insertLocated(t, e, n, func(i int) string { return fmt.Sprintf("zz/M%04d/M.mkv", i/3) })
	s := e.runner.Store()
	s.matchChecks.Store(0)
	var (
		got   []UnmappedFile
		calls int64
	)
	for page := 1; ; page++ {
		calls++
		p, err := s.Unmapped(context.Background(), e.it.ID, page, 300)
		if err != nil {
			t.Fatal(err)
		}
		if p.TotalRecords != n {
			t.Fatalf("page %d: total = %d, want %d", page, p.TotalRecords, n)
		}
		if len(p.Records) == 0 {
			break
		}
		got = append(got, p.Records...)
	}
	if len(got) != n {
		t.Fatalf("listed %d files, want %d", len(got), n)
	}
	sizes := map[int64]bool{}
	for i, u := range got {
		if u.Reason != ReasonMismatched || sizes[u.Size] {
			t.Fatalf("row %d = %+v (duplicate or wrong reason)", i, u)
		}
		sizes[u.Size] = true
		if i > 0 && u.Path < got[i-1].Path {
			t.Fatalf("row %d %s sorts before row %d %s", i, u.Path, i-1, got[i-1].Path)
		}
	}
	// Every page request checks every candidate once (the total needs them all).
	if c := s.matchChecks.Load(); c != calls*n {
		t.Fatalf("checked %d files against the catalog in %d requests, want %d", c, calls, calls*n)
	}
}

// TestUnmappedWithoutThePathIndex: the mismatched listing does not depend on the index
// arr_files_path existing. It was added to migration 0003 in place, and migrate records versions
// only, so a database that applied the earlier text of 0003 lacks it: the listing must still work
// there (a slower plan), not fail with "no such index".
func TestUnmappedWithoutThePathIndex(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	const n = mismatchedChunk + 3
	insertLocated(t, e, n, func(i int) string { return fmt.Sprintf("zz/M%04d/M.mkv", i) })
	err := e.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `DROP INDEX arr_files_path`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.runner.Store().Unmapped(context.Background(), e.it.ID, 2, mismatchedChunk)
	if err != nil {
		t.Fatal(err)
	}
	if p.TotalRecords != n || len(p.Records) != 3 || p.Records[2].Path != fmt.Sprintf("/movies/zz/M%04d/M.mkv", n-1) {
		t.Fatalf("page = total %d, records %+v", p.TotalRecords, p.Records)
	}
}

// TestUnmappedMismatchedQueryIsARangeScan: each keyset chunk of the mismatched listing is a range
// scan of arr_files_path in (path, id) order, never a scan and sort of every located file (which
// made listing N candidates cost N/500 whole-table sorts).
func TestUnmappedMismatchedQueryIsARangeScan(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	rows, err := e.db.Reader().QueryContext(context.Background(), `EXPLAIN QUERY PLAN `+mismatchedQuery, "/movies/a", 7, e.it.ID, e.it.ID, mismatchedChunk)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "SEARCH f USING INDEX arr_files_path (") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("plan:\n%s", joined)
	}
}
