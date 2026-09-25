package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func TestSourcesCRUD(t *testing.T) {
	e := newEnv(t, nil)
	dir := e.mkdir(t, "media/movies")
	link := filepath.Join(e.base, "movies-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	var src catalog.Source
	e.call(t, 201, "POST", "/sources", map[string]any{"name": "Movies HD", "path": link, "exclude": []string{"*.part"}}, &src)
	if src.Path != dir || src.DestFolder != "movies-hd" || !src.Enabled || len(src.Exclude) != 1 {
		t.Fatalf("created (symlink resolved, destFolder derived): %+v", src)
	}
	var list []catalog.Source
	e.call(t, 200, "GET", "/sources", nil, &list)
	if len(list) != 1 || list[0].ID != src.ID {
		t.Fatalf("list: %+v", list)
	}
	path := fmt.Sprintf("/sources/%d", src.ID)
	e.call(t, 200, "PUT", path, map[string]any{"name": "Movies", "path": dir, "enabled": false}, &src)
	if src.Name != "Movies" || src.Enabled || src.DestFolder != "movies-hd" {
		t.Fatalf("updated: %+v", src)
	}
	for _, b := range []map[string]any{
		{"name": "", "path": dir},
		{"name": "X", "path": "relative"},
		{"name": "X", "path": filepath.Join(e.base, "missing")},
		{"name": "X", "path": dir, "destFolder": "../escape"},
		{"name": "X", "path": dir, "destFolder": ".bunkarr"},
		{"name": "X", "path": dir, "extra": true},
		{"name": "X", "path": "/"},
	} {
		if code, msg := e.status(t, "POST", "/sources", b); code != 400 {
			t.Errorf("create %v: %d %q, want 400", b, code, msg)
		}
	}
	// Name and destFolder are unique (409).
	other := e.mkdir(t, "media/tv")
	if code, _ := e.status(t, "POST", "/sources", map[string]any{"name": "movies", "path": other}); code != 409 {
		t.Errorf("duplicate name: %d", code)
	}
	if code, _ := e.status(t, "POST", "/sources", map[string]any{"name": "TV", "path": other, "destFolder": "movies-hd"}); code != 409 {
		t.Errorf("duplicate destFolder: %d", code)
	}
	if code, _ := e.status(t, "GET", "/sources/999", nil); code != 404 {
		t.Errorf("missing source: %d", code)
	}
	e.call(t, 204, "DELETE", path, nil, nil)
	if code, _ := e.status(t, "DELETE", path, nil); code != 404 {
		t.Errorf("second delete: %d", code)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("delete touched the source directory: %v", err)
	}
}

// TestSourceOverlapGuards: a source may not be inside (or contain, or be) a destination target
// or be inside the config directory (safety rule S4).
func TestSourceOverlapGuards(t *testing.T) {
	e := newEnv(t, nil)
	nas := e.mkdir(t, "nas")
	e.mkdir(t, "nas/inside")
	e.createDestination(t, "NAS", nas, nil, nil)
	for _, p := range []string{nas, filepath.Join(nas, "inside"), e.base, e.config, e.mkdir(t, "config/staging")} {
		code, msg := e.status(t, "POST", "/sources", map[string]any{"name": "X", "path": p})
		if code != 400 || !strings.HasPrefix(msg, "path: ") {
			t.Errorf("source %s: %d %q, want 400", p, code, msg)
		}
		var res catalog.TestResult
		e.call(t, 200, "POST", "/sources/test", map[string]any{"path": p}, &res)
		if res.OK {
			t.Errorf("test of %s: %+v", p, res)
		}
	}
}

func TestSourceTestScanAndFiles(t *testing.T) {
	e := newEnv(t, nil)
	dir := e.mkdir(t, "media/movies")
	var res catalog.TestResult
	e.call(t, 200, "POST", "/sources/test", map[string]any{"path": dir}, &res)
	if !res.OK || !res.IsDir || res.Entries != 0 || len(res.Warnings) == 0 {
		t.Fatalf("test of an empty directory: %+v", res)
	}
	mt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := range 7 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("M%d", i), "movie.mkv"), strings.Repeat("x", 10*(i+1)), mt)
	}
	if err := os.Link(filepath.Join(dir, "M0", "movie.mkv"), filepath.Join(dir, "M0", "linked.mkv")); err != nil {
		t.Fatal(err)
	}
	e.call(t, 200, "POST", "/sources/test", map[string]any{"path": dir}, &res)
	if !res.OK || res.Entries != 7 {
		t.Fatalf("test: %+v", res)
	}
	e.call(t, 200, "POST", "/sources/test", map[string]any{"path": "relative"}, &res)
	if res.OK {
		t.Fatalf("test of a relative path: %+v", res)
	}
	id := e.createSource(t, "Movies", dir)
	var j jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/sources/%d/scan", id), nil, &j)
	if j.Type != jobs.TypeScan || len(j.Params.SourceIDs) != 1 || j.Params.SourceIDs[0] != id {
		t.Fatalf("scan job: %+v", j)
	}
	if done := e.waitJob(t, j.ID); done.Status != jobs.StatusCompleted {
		t.Fatalf("scan: %s %s", done.Status, done.Error)
	}
	if code, _ := e.status(t, "POST", "/sources/999/scan", nil); code != 404 {
		t.Fatalf("scan of a missing source: %d", code)
	}

	var page catalog.FilePage
	e.call(t, 200, "GET", fmt.Sprintf("/sources/%d/files?page=2&pageSize=3", id), nil, &page)
	if page.TotalRecords != 8 || page.Page != 2 || page.PageSize != 3 || len(page.Records) != 3 {
		t.Fatalf("page 2: %+v", page)
	}
	e.call(t, 200, "GET", fmt.Sprintf("/sources/%d/files", id), nil, &page)
	if page.PageSize != 50 || len(page.Records) != 8 {
		t.Fatalf("default page: %+v", page)
	}
	e.call(t, 200, "GET", fmt.Sprintf("/sources/%d/files?filter=hardlinked", id), nil, &page)
	if page.TotalRecords != 2 || page.Records[0].HardlinkGroup == "" {
		t.Fatalf("hardlinked: %+v", page)
	}
	e.call(t, 200, "GET", fmt.Sprintf("/sources/%d/files?search=M3%%2F", id), nil, &page)
	if page.TotalRecords != 1 || page.Records[0].RelPath != "M3/movie.mkv" {
		t.Fatalf("search: %+v", page)
	}
	for _, q := range []string{"page=0", "pageSize=0", "pageSize=501", "page=x", "filter=bogus"} {
		if code, _ := e.status(t, "GET", fmt.Sprintf("/sources/%d/files?%s", id, q), nil); code != 400 {
			t.Errorf("files?%s: %d, want 400", q, code)
		}
	}
	if code, _ := e.status(t, "GET", "/sources/999/files", nil); code != 404 {
		t.Errorf("files of a missing source: %d", code)
	}
	var st catalog.GlobalStats
	e.call(t, 200, "GET", "/catalog/stats", nil, &st)
	if st.Sources != 1 || st.Files != 8 || st.HardlinkGroups != 1 || st.UniqueBytes != st.Bytes-10 {
		t.Fatalf("catalog stats: %+v", st)
	}
}

func TestFilesystemBrowser(t *testing.T) {
	e := newEnv(t, nil)
	root := e.mkdir(t, "browse")
	e.mkdir(t, "browse/b-dir")
	e.mkdir(t, "browse/A-dir")
	writeFile(t, filepath.Join(root, "file.txt"), "not listed", time.Now())
	if err := os.Symlink(filepath.Join(root, "b-dir"), filepath.Join(root, "c-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "file.txt"), filepath.Join(root, "d-filelink")); err != nil {
		t.Fatal(err)
	}
	var l dirListing
	e.call(t, 200, "GET", "/filesystem?path="+root+"/", nil, &l)
	var names []string
	for _, d := range l.Directories {
		names = append(names, d.Name)
		if d.Path != filepath.Join(root, d.Name) {
			t.Errorf("path of %s: %s", d.Name, d.Path)
		}
	}
	if l.Path != root || l.Parent != filepath.Dir(root) || strings.Join(names, ",") != "A-dir,b-dir,c-link" || l.Truncated {
		t.Fatalf("listing: %+v", l)
	}
	e.call(t, 200, "GET", "/filesystem", nil, &l)
	if l.Path != "/" || l.Parent != "" || len(l.Directories) == 0 {
		t.Fatalf("root listing: %+v", l)
	}
	for q, want := range map[string]int{
		"?path=relative/dir":                          400,
		"?path=" + filepath.Join(root, "file.txt"):    400,
		"?path=" + filepath.Join(root, "nonexistent"): 404,
	} {
		if code, _ := e.status(t, "GET", "/filesystem"+q, nil); code != want {
			t.Errorf("filesystem%s: %d, want %d", q, code, want)
		}
	}
}

// TestSourceDeleteConflictIs409: while the catalog refuses to delete a source (queued or
// running jobs use it: catalog.StoreOptions.ActiveJobs, or its lock is held), DELETE answers 409
// and the source stays.
func TestSourceDeleteConflictIs409(t *testing.T) {
	e := newEnv(t, nil)
	id := e.createSource(t, "Movies", e.mkdir(t, "media/movies"))
	path := fmt.Sprintf("/sources/%d", id)
	// A server whose catalog reports active jobs for every source.
	app := *e.app
	app.Catalog = catalog.NewStore(e.db, catalog.StoreOptions{ActiveJobs: func(context.Context, int64) (bool, error) { return true, nil }})
	srv := httptest.NewServer(New(Options{Auth: e.auth, DB: e.db, App: &app}).Handler())
	t.Cleanup(srv.Close)
	busy := *e
	busy.srv = srv
	if code, msg := busy.status(t, "DELETE", path, nil); code != 409 || !strings.Contains(msg, "queued or running jobs") {
		t.Fatalf("delete of a source in use: %d %q, want 409", code, msg)
	}
	e.call(t, 200, "GET", path, nil, nil)
	e.call(t, 204, "DELETE", path, nil, nil)
}

// TestSourceLinkedToABusyDestinationIsNotDeleted: the real wiring (App.Catalog's ActiveJobs) also
// counts a job of a destination the source is linked to, since a sync scans and plans the source.
func TestSourceLinkedToABusyDestinationIsNotDeleted(t *testing.T) {
	e := newEnv(t, nil)
	src := e.createSource(t, "Movies", e.mkdir(t, "media/movies"))
	dst := e.createDestination(t, "UNAS", e.mkdir(t, "unas"), []int64{src}, nil)
	ctx := context.Background()
	// A sync of the destination that is running (inserted directly: the manager only picks up
	// queued jobs, so this row stays as it is for the test).
	var jobID int64
	err := e.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO jobs (type, status, trigger, params, destination_id, queued_at, started_at)
			VALUES ('sync', 'running', 'manual', ?, ?, ?, ?)`,
			fmt.Sprintf(`{"destinationId":%d}`, dst), dst, db.FormatTime(time.Now()), db.FormatTime(time.Now()))
		if err != nil {
			return err
		}
		jobID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/sources/%d", src)
	if code, msg := e.status(t, "DELETE", path, nil); code != 409 || !strings.Contains(msg, "queued or running jobs") {
		t.Fatalf("delete of a source whose destination is syncing: %d %q, want 409", code, msg)
	}
	err = e.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'completed', finished_at = ? WHERE id = ?`, db.FormatTime(time.Now()), jobID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.call(t, 204, "DELETE", path, nil, nil)
}
