package catalog

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "bunkarr.db"), nil)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func newStore(t *testing.T, o StoreOptions) *Store {
	t.Helper()
	return NewStore(openDB(t), o)
}

// tempDir returns a resolved temporary directory (macOS's /var is a symlink to /private/var).
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return d
}

// writeFiles creates files (relative slash paths → content) under root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func createSource(t *testing.T, st *Store, name, path string) Source {
	t.Helper()
	src, err := st.Create(context.Background(), SourceInput{Name: name, Path: path})
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	return src
}

// allRows returns every catalog row of a source (live and deleted) by relative path.
func allRows(t *testing.T, st *Store, sourceID int64) map[string]File {
	t.Helper()
	rows, err := st.db.Reader().QueryContext(context.Background(), `SELECT `+fileColumns+` FROM catalog_files WHERE source_id = ?`, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			t.Fatal(err)
		}
		out[f.RelPath] = f
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func liveRows(t *testing.T, st *Store, sourceID int64) map[string]File {
	t.Helper()
	out := map[string]File{}
	for rel, f := range allRows(t, st, sourceID) {
		if f.DeletedAt == nil {
			out[rel] = f
		}
	}
	return out
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// recReporter records progress and log lines.
type recReporter struct {
	mu       sync.Mutex
	progress []jobs.Progress
	logs     []string
}

func (r *recReporter) Progress(p jobs.Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progress = append(r.progress, p)
}

func (r *recReporter) Log(level slog.Level, msg string, _ ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, level.String()+" "+msg)
}

func (r *recReporter) hasLog(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// treeState snapshots mode, size, mtime and ctime of every entry under root (lstat), to prove a
// scan changed nothing in the source.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		m, _ := MetaOf(fi)
		out[p] = fmt.Sprintf("%v|%d|%d|%d", fi.Mode(), m.Size, m.MtimeNs, m.CtimeNs)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustScan(t *testing.T, sc *Scanner, id int64) ScanResult {
	t.Helper()
	res, err := sc.Scan(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return res
}
