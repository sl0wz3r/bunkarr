package syncer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
)

// TestHarnessDatabaseMatchesAFreshMigration: newHarness copies a database migrated once
// (openMigratedDB) instead of migrating one per test. The copy must have the schema and version of
// a database migrated from scratch, and every harness must have its own copy.
func TestHarnessDatabaseMatchesAFreshMigration(t *testing.T) {
	ctx := context.Background()
	fresh, err := db.Open(ctx, filepath.Join(t.TempDir(), "fresh.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	schema := func(d *db.DB) (int, []string) {
		t.Helper()
		v, err := d.SchemaVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := d.Reader().QueryContext(ctx, `SELECT type || ' ' || name || ' ' || COALESCE(sql, '') FROM sqlite_master`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		slices.Sort(out)
		return v, out
	}
	wantV, want := schema(fresh)
	h1, h2 := newHarness(t), newHarness(t)
	if v, got := schema(h1.db); v != wantV || !slices.Equal(got, want) {
		t.Fatalf("harness database: version %d, want %d; schema equal: %v", v, wantV, slices.Equal(got, want))
	}
	// Each harness has its own database.
	other := filepath.Join(h1.base, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := h1.cat.Create(ctx, catalog.SourceInput{Name: "Other", Path: other}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		h    *harness
		want int
	}{{h1, 2}, {h2, 1}} {
		list, err := c.h.cat.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != c.want {
			t.Errorf("%s: %d sources, want %d", c.h.db.Path, len(list), c.want)
		}
	}
}
