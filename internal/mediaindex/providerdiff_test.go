package mediaindex

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr/maintainerrtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
)

// writerChanges returns total_changes() of the writer connection (MaxOpenConns 1): how many rows
// every write so far inserted, updated or deleted.
func writerChanges(t *testing.T, e *provEnv) int64 {
	t.Helper()
	var n int64
	if err := e.db.Write(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT total_changes()`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// dumpTable returns an integration's rows of a table, rendered and sorted.
func dumpTable(t *testing.T, e *provEnv, table string, id int64) []string {
	t.Helper()
	rows, err := e.db.Reader().Query(`SELECT * FROM `+table+` WHERE integration_id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%#v", vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// TestProviderRefreshWritesOnlyChanges: a refresh that finds the cache unchanged rewrites no
// cache row, only its index_state row. (A delete and re-insert of every row held SQLite's only
// writer connection for seconds on a large library, every night, stalling webhook intake.)
func TestProviderRefreshWritesOnlyChanges(t *testing.T) {
	for _, kind := range []string{"plex", "tautulli", "seerr", "maintainerr"} {
		t.Run(kind, func(t *testing.T) {
			e := newProvEnv(t)
			var it integrations.Integration
			tables := []string{}
			switch kind {
			case "plex":
				it, tables = e.plexIt, []string{"plex_sections", "plex_items", "plex_files"}
			case "tautulli":
				e.mustRun(e.plexIt)
				it, tables = e.addTautulli(), []string{"watch_stats"}
			case "seerr":
				it, tables = e.addSeerr(true), []string{"seerr_requests"}
			case "maintainerr":
				e.mustRun(e.plexIt)
				it, tables = e.addMaintainerr(maintainerrtest.V341), []string{"maintainerr_items"}
			}
			e.mustRun(it)
			var rows int64
			before := map[string][]string{}
			for _, tb := range tables {
				rows += e.count(tb, it.ID)
				before[tb] = dumpTable(t, e, tb, it.ID)
			}
			if rows == 0 {
				t.Fatal("the fixture cached no rows")
			}
			refreshed := *e.state(it.ID).RefreshedAt
			e.clock.Advance(time.Hour)
			n := writerChanges(t, e)
			e.mustRun(it)
			if d := writerChanges(t, e) - n; d != 1 {
				t.Fatalf("an unchanged refresh changed %d rows, want 1 (index_state; the cache holds %d rows)", d, rows)
			}
			for _, tb := range tables {
				if got := dumpTable(t, e, tb, it.ID); !slices.Equal(got, before[tb]) {
					t.Fatalf("%s changed:\n%v\nwant\n%v", tb, got, before[tb])
				}
			}
			if st := e.state(it.ID); !st.RefreshedAt.After(refreshed) || !e.fresh(it).Fresh {
				t.Fatalf("the refresh was not recorded: %+v", st)
			}
		})
	}
}

// TestPlexIndexDiffMatchesRebuild: a refresh that finds removed, changed and new items writes only
// those rows, and leaves the same rows as building the index from nothing.
func TestPlexIndexDiffMatchesRebuild(t *testing.T) {
	e := newProvEnv(t)
	movies := e.lib.Rows("1", plex.TypeMovie)
	e.lib.SetListing("1", plex.TypeMovie, movies[2:])
	e.mustRun(e.plexIt)

	// Two movies come back, one is gone, and one is renamed.
	var renamed map[string]any
	dec := json.NewDecoder(bytes.NewReader(movies[3]))
	dec.UseNumber()
	if err := dec.Decode(&renamed); err != nil {
		t.Fatal(err)
	}
	renamed["title"] = "Renamed"
	b, err := json.Marshal(renamed)
	if err != nil {
		t.Fatal(err)
	}
	next := append([]json.RawMessage{movies[0], movies[1], movies[2], b}, movies[5:]...)
	e.lib.SetListing("1", plex.TypeMovie, next)
	n := writerChanges(t, e)
	stats, _ := e.mustRun(e.plexIt)
	changed := writerChanges(t, e) - n
	// 2 items + 2 files back, 1 item + 1 file gone, 1 item renamed, and index_state.
	if changed != 2+2+1+1+1+1 {
		t.Fatalf("the refresh changed %d rows", changed)
	}
	if stats.Items != 31 || e.count("plex_items", e.plexIt.ID) != 31 || e.count("plex_files", e.plexIt.ID) != 21 {
		t.Fatalf("stats %+v, %d items, %d files", stats, e.count("plex_items", e.plexIt.ID), e.count("plex_files", e.plexIt.ID))
	}
	tables := []string{"plex_sections", "plex_items", "plex_files"}
	diffed := map[string][]string{}
	for _, tb := range tables {
		diffed[tb] = dumpTable(t, e, tb, e.plexIt.ID)
	}
	if err := e.db.Write(context.Background(), func(tx *sql.Tx) error {
		for _, tb := range append(tables, "index_state") {
			if _, err := tx.Exec(`DELETE FROM `+tb+` WHERE integration_id = ?`, e.plexIt.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.mustRun(e.plexIt)
	for _, tb := range tables {
		if got := dumpTable(t, e, tb, e.plexIt.ID); !slices.Equal(got, diffed[tb]) {
			t.Fatalf("%s after the diff:\n%v\nafter a rebuild:\n%v", tb, diffed[tb], got)
		}
	}
}

// TestDiffTableFirstRowWins: a repeated key is written once, with the first row's values, as
// INSERT … ON CONFLICT DO NOTHING did; a changed value is rewritten, an unchanged one is not.
func TestDiffTableFirstRowWins(t *testing.T) {
	e := newProvEnv(t)
	s := e.addSeerr(false)
	ctx := context.Background()
	tb := func(rows ...[]any) *cacheTable {
		return &cacheTable{name: "seerr_requests", keys: []string{"request_id"}, vals: []string{"status", "media_type", "is_4k", "seasons", "user_id", "tmdb_id"},
			n: len(rows), row: func(dst []any, i int) []any { return append(dst, rows[i]...) }}
	}
	first := tb(
		[]any{int64(1), 1, "movie", false, "[]", int64(7), nil},
		[]any{int64(1), 2, "tv", true, "[1]", int64(8), int64(3)},
		[]any{int64(2), 5, "tv", true, "[1,2]", int64(9), int64(4)})
	d, err := diffTable(ctx, e.db.Reader(), s.ID, first)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.inserts, []int{0, 2}) || len(d.updates) != 0 || len(d.deletes) != 0 {
		t.Fatalf("diff of an empty cache = %+v", d)
	}
	if err := e.db.Write(ctx, func(tx *sql.Tx) error { return d.apply(ctx, tx, s.ID) }); err != nil {
		t.Fatal(err)
	}
	rows := seerrRows(t, e, s.ID)
	if r := rows[1]; len(rows) != 2 || r.Status != 1 || r.MediaType != "movie" || r.Is4K || r.UserID != 7 || r.TMDBID != 0 {
		t.Fatalf("rows %+v", rows)
	}
	second := tb(
		[]any{int64(2), 5, "tv", true, "[1,2]", int64(9), int64(4)},
		[]any{int64(3), 1, "movie", false, "[]", int64(7), nil},
		[]any{int64(1), 1, "movie", false, "[]", int64(7), int64(5)})
	d, err = diffTable(ctx, e.db.Reader(), s.ID, second)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.inserts, []int{1}) || !slices.Equal(d.updates, []int{2}) || len(d.deletes) != 0 {
		t.Fatalf("diff = %+v", d)
	}
	third := tb([]any{int64(2), 5, "tv", true, "[1,2]", int64(9), int64(4)})
	d, err = diffTable(ctx, e.db.Reader(), s.ID, third)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.inserts)+len(d.updates) != 0 || d.all || len(d.deletes) != 1 || d.deletes[0][0] != int64(1) {
		t.Fatalf("diff = %+v", d)
	}
}

// TestDiffTableReplacesAllWhenMostRowsGone: when more than half of the cached rows are gone (every
// rating_key changes when a Plex library is added again or the integration points at another
// server), the diff deletes the integration's rows with one statement and inserts every row, as
// the full replacement did, instead of deleting the gone rows one at a time by key (which held the
// writer about twice as long); a repeated key is still written once, with the first row's values.
// With half of the rows gone or fewer it still deletes by key and writes only what changed.
func TestDiffTableReplacesAllWhenMostRowsGone(t *testing.T) {
	e := newProvEnv(t)
	s := e.addSeerr(false)
	ctx := context.Background()
	tb := func(rows ...[]any) *cacheTable {
		return &cacheTable{name: "seerr_requests", keys: []string{"request_id"}, vals: []string{"status", "media_type", "is_4k", "seasons", "user_id", "tmdb_id"},
			n: len(rows), row: func(dst []any, i int) []any { return append(dst, rows[i]...) }}
	}
	write := func(tb *cacheTable) tableDiff {
		t.Helper()
		d, err := diffTable(ctx, e.db.Reader(), s.ID, tb)
		if err != nil {
			t.Fatal(err)
		}
		n := writerChanges(t, e)
		if err := e.db.Write(ctx, func(tx *sql.Tx) error { return d.apply(ctx, tx, s.ID) }); err != nil {
			t.Fatal(err)
		}
		if got := writerChanges(t, e) - n; got != int64(d.rowsChanged()) {
			t.Fatalf("the diff changed %d rows, rowsChanged = %d (%+v)", got, d.rowsChanged(), d)
		}
		return d
	}
	r := func(id int64, status int) []any { return []any{id, status, "movie", false, "[]", int64(7), nil} }
	write(tb(r(1, 1), r(2, 1), r(3, 1), r(4, 1)))

	// Half of the rows gone (3 and 4), 2 changed, 5 new: by key.
	if d := write(tb(r(1, 1), r(2, 2), r(5, 1))); d.all || len(d.deletes) != 2 || !slices.Equal(d.updates, []int{1}) || !slices.Equal(d.inserts, []int{2}) {
		t.Fatalf("diff with half of the rows gone = %+v", d)
	}
	// 2 of the 3 cached rows (2 and 5) gone, 1 unchanged, 6 repeated: the whole table, first row wins.
	d := write(tb(r(1, 1), r(6, 3), r(6, 4), r(7, 1), r(8, 2)))
	if !d.all || d.cached != 3 || len(d.deletes)+len(d.updates) != 0 || !slices.Equal(d.inserts, []int{0, 1, 3, 4}) || d.rowsChanged() != 3+4 {
		t.Fatalf("diff with most rows gone = %+v", d)
	}
	rows := seerrRows(t, e, s.ID)
	if len(rows) != 4 || rows[1].Status != 1 || rows[6].Status != 3 || rows[7].Status != 1 || rows[8].Status != 2 {
		t.Fatalf("rows %+v", rows)
	}
	// Unchanged: nothing to write.
	if d := write(tb(r(1, 1), r(6, 3), r(7, 1), r(8, 2))); d.all || d.rowsChanged() != 0 {
		t.Fatalf("diff of an unchanged cache = %+v", d)
	}
}

// TestPlexIndexRekeyedMatchesRebuild: a refresh where every rating_key changed (the libraries
// were added again) replaces the items and files wholesale and leaves the same rows as building
// the index from nothing.
func TestPlexIndexRekeyedMatchesRebuild(t *testing.T) {
	e := newProvEnv(t)
	e.mustRun(e.plexIt)
	items, files := e.count("plex_items", e.plexIt.ID), e.count("plex_files", e.plexIt.ID)
	for _, l := range []struct {
		section string
		typ     int
	}{{"1", plex.TypeMovie}, {"2", 2}, {"2", 3}, {"2", 4}, {"3", 8}, {"3", 9}, {"3", 10}} {
		rows := e.lib.Rows(l.section, l.typ)
		if len(rows) == 0 {
			t.Fatalf("section %s type %d has no rows", l.section, l.typ)
		}
		for i, raw := range rows {
			var m map[string]any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err := dec.Decode(&m); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"ratingKey", "parentRatingKey", "grandparentRatingKey"} {
				if v, ok := m[k].(string); ok {
					m[k] = "9000" + v
				}
			}
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			rows[i] = b
		}
		e.lib.SetListing(l.section, l.typ, rows)
	}
	n := writerChanges(t, e)
	stats, _ := e.mustRun(e.plexIt)
	changed := writerChanges(t, e) - n
	// Every item and file deleted and inserted again (sections unchanged), and index_state.
	if changed != 2*items+2*files+1 || int64(stats.Items) != items || e.count("plex_files", e.plexIt.ID) != files {
		t.Fatalf("the refresh changed %d rows (%d items, %d files), stats %+v", changed, items, files, stats)
	}
	tables := []string{"plex_sections", "plex_items", "plex_files"}
	diffed := map[string][]string{}
	for _, tb := range tables {
		diffed[tb] = dumpTable(t, e, tb, e.plexIt.ID)
	}
	if err := e.db.Write(context.Background(), func(tx *sql.Tx) error {
		for _, tb := range append(tables, "index_state") {
			if _, err := tx.Exec(`DELETE FROM `+tb+` WHERE integration_id = ?`, e.plexIt.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.mustRun(e.plexIt)
	for _, tb := range tables {
		if got := dumpTable(t, e, tb, e.plexIt.ID); !slices.Equal(got, diffed[tb]) {
			t.Fatalf("%s after the diff:\n%v\nafter a rebuild:\n%v", tb, diffed[tb], got)
		}
	}
}
