package tiers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

// env is a database with the real stores, a clock and a media root on disk.
type env struct {
	t       *testing.T
	ctx     context.Context
	db      *db.DB
	ints    *integrations.Store
	cat     *catalog.Store
	scanner *catalog.Scanner
	idx     *mediaindex.Store
	dests   *destinations.Store
	eng     *Engine
	clock   *clock
	root    string
	// records are the live records the preview sees, per destination.
	records map[int64][]RecordRef
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newEnv(t *testing.T, providers ...Provider) *env {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), filepath.Join(root, "bunkarr.db"), nil)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	kr, err := config.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: context.Background(), db: d, root: filepath.Join(root, "media"), records: map[int64][]RecordRef{},
		clock: &clock{now: time.Now().UTC().Truncate(time.Second)}}
	if err := os.MkdirAll(e.root, 0o755); err != nil {
		t.Fatal(err)
	}
	e.ints = integrations.NewStore(d, kr)
	e.cat = catalog.NewStore(d, catalog.StoreOptions{})
	e.scanner = catalog.NewScanner(e.cat, catalog.ScannerOptions{})
	e.idx = mediaindex.NewStore(d, e.cat)
	e.dests = destinations.New(d, destinations.Options{})
	e.eng = New(Options{DB: d, Catalog: e.cat, Index: e.idx, Integrations: e.ints, Providers: providers, Now: e.clock.Now,
		Destinations: func(ctx context.Context) ([]DestinationRef, error) {
			list, err := e.dests.List(ctx)
			if err != nil {
				return nil, err
			}
			var out []DestinationRef
			for _, d := range list {
				out = append(out, DestinationRef{ID: d.ID, Name: d.Name, Enabled: d.Enabled, SourceIDs: d.SourceIDs})
			}
			return out, nil
		},
		Records: func(_ context.Context, id int64) ([]RecordRef, error) { return e.records[id], nil },
	})
	return e
}

// writeFile creates a file of size bytes (sparse) under the media root.
func (e *env) writeFile(rel string, size int64) {
	e.t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		e.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		e.t.Fatal(err)
	}
}

// source creates a source over a folder of the media root.
func (e *env) source(name, folder string) catalog.Source {
	e.t.Helper()
	p := filepath.Join(e.root, folder)
	if err := os.MkdirAll(p, 0o755); err != nil {
		e.t.Fatal(err)
	}
	s, err := e.cat.Create(e.ctx, catalog.SourceInput{Name: name, Path: p})
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) scan(s catalog.Source) catalog.Source {
	e.t.Helper()
	if _, err := e.scanner.Scan(e.ctx, s.ID, nil); err != nil {
		e.t.Fatalf("scan: %v", err)
	}
	s, err := e.cat.Get(e.ctx, s.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// destination creates a destination linked to sources.
func (e *env) destination(name string, sources ...int64) int64 {
	e.t.Helper()
	dir := filepath.Join(filepath.Dir(e.root), "dest-"+name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	d, err := e.dests.Create(e.ctx, destinations.Input{Name: name, Target: dir, SourceIDs: sources}, destinations.CreateOptions{AllowLocal: true})
	if err != nil {
		e.t.Fatal(err)
	}
	return d.ID
}

// arr creates an *arr integration whose /<app root> maps to the media root.
func (e *env) arr(typ integrations.Type, name, arrRoot, local string) integrations.Integration {
	e.t.Helper()
	settings, _ := json.Marshal(map[string]any{"pathMappings": []map[string]string{{"arr": arrRoot, "local": filepath.Join(e.root, local)}}})
	it, err := e.ints.Create(e.ctx, integrations.Input{Type: typ, Name: name, URL: "http://127.0.0.1:1/" + strings.ReplaceAll(strings.ToLower(name), " ", "-"),
		APIKey: "0123456789abcdef0123456789abcdef", Settings: settings})
	if err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *env) exec(query string, args ...any) {
	e.t.Helper()
	err := e.db.Write(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(e.ctx, query, args...)
		return err
	})
	if err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
}

// fresh marks an integration's cache refreshed at the clock's time for its current URL.
func (e *env) fresh(it integrations.Integration) {
	e.t.Helper()
	e.refreshedAt(it, e.clock.Now())
}

func (e *env) refreshedAt(it integrations.Integration, at time.Time) {
	e.t.Helper()
	e.exec(`INSERT INTO index_state (integration_id, status, refreshed_at, attempted_at, instance_id) VALUES (?, 'ok', ?, ?, ?)
		ON CONFLICT (integration_id) DO UPDATE SET refreshed_at = excluded.refreshed_at, instance_id = excluded.instance_id`,
		it.ID, db.FormatTime(at), db.FormatTime(at), it.URL)
}

// meta adds an arr_meta row (kind quality_profile, root_folder or tag).
func (e *env) meta(it integrations.Integration, kind string, id int64, name string, detail string) {
	e.t.Helper()
	if detail == "" {
		detail = "{}"
	}
	e.exec(`INSERT INTO arr_meta (integration_id, kind, arr_id, name, detail) VALUES (?, ?, ?, ?, ?)`, it.ID, kind, id, name, detail)
}

// rootFolder adds a root folder whose local path is <media root>/<local>.
func (e *env) rootFolder(it integrations.Integration, id int64, arrPath, local string) {
	e.t.Helper()
	d, _ := json.Marshal(map[string]any{"accessible": true, "localPath": filepath.Join(e.root, local)})
	e.meta(it, "root_folder", id, arrPath, string(d))
}

// itemSpec is an arr_items row.
type itemSpec struct {
	kind      string
	arrID     int64
	title     string
	path      string
	root      string
	profile   int64
	monitored bool
	tags      []int64
	genres    []string
	ext       mediaindex.ExternalIDs
	deleted   bool
}

// item adds an arr_items row and returns its id.
func (e *env) item(it integrations.Integration, s itemSpec) int64 {
	e.t.Helper()
	if s.kind == "" {
		s.kind = mediaindex.KindMovie
	}
	tags, _ := json.Marshal(append([]int64{}, s.tags...))
	genres, _ := json.Marshal(append([]string{}, s.genres...))
	ext, _ := json.Marshal(s.ext)
	var deleted any
	if s.deleted {
		deleted = db.FormatTime(e.clock.Now())
	}
	var id int64
	err := e.db.Write(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(e.ctx, `INSERT INTO arr_items (integration_id, kind, arr_id, title, external_ids, path, root_folder,
			quality_profile_id, monitored, tags, genres, seen_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			it.ID, s.kind, s.arrID, s.title, string(ext), s.path, s.root, s.profile, s.monitored, string(tags), string(genres),
			db.FormatTime(e.clock.Now()), deleted)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// arrFile adds an arr_files row at <media root>/<rel> (as the *arr sees it: arrPath).
func (e *env) arrFile(it integrations.Integration, itemID, arrFileID int64, arrPath, rel string, size int64, added time.Time) {
	e.t.Helper()
	local := filepath.Join(e.root, rel)
	locs, err := e.cat.Locate(e.ctx, local)
	if err != nil {
		e.t.Fatal(err)
	}
	var src, relPath any
	if len(locs) > 0 {
		src, relPath = locs[0].SourceID, locs[0].Rel
	}
	e.exec(`INSERT INTO arr_files (integration_id, item_id, arr_file_id, path, local_path, source_id, rel_path, size, quality,
		date_added, seen_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'Bluray-1080p', ?, ?)`,
		it.ID, itemID, arrFileID, arrPath, local, src, relPath, size, db.FormatTime(added), db.FormatTime(e.clock.Now()))
}

// fileID returns a live catalog file's id.
func (e *env) fileID(s catalog.Source, rel string) int64 {
	e.t.Helper()
	var id int64
	err := e.db.Reader().QueryRowContext(e.ctx, `SELECT id FROM catalog_files WHERE source_id = ? AND rel_path = ? AND deleted_at IS NULL`,
		s.ID, rel).Scan(&id)
	if err != nil {
		e.t.Fatalf("catalog file %s: %v", rel, err)
	}
	return id
}

// saveRules replaces the rules at the current revision.
func (e *env) saveRules(rules ...RuleInput) RuleSet {
	e.t.Helper()
	rev, err := e.eng.Revision(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	rs, _, err := e.eng.SaveRules(e.ctx, rev, rules)
	if err != nil {
		e.t.Fatalf("save rules: %v", err)
	}
	return rs
}

// decisions decides a source at a destination and returns the decisions by path.
func (e *env) decisions(destID int64, s catalog.Source) (map[string]Decision, *SourceDecisions) {
	e.t.Helper()
	sd, err := e.eng.Decisions(e.ctx, nil, destID, s)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]Decision{}
	rows, err := e.db.Reader().QueryContext(e.ctx, `SELECT id, rel_path FROM catalog_files WHERE source_id = ? AND deleted_at IS NULL`, s.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id  int64
			rel string
		)
		if err := rows.Scan(&id, &rel); err != nil {
			e.t.Fatal(err)
		}
		out[rel] = sd.Decide(id)
	}
	return out, sd
}

func raw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func cnd(field, op string, v any) Condition {
	c := Condition{Field: field, Op: op}
	if v != nil {
		c.Value = raw(v)
	}
	return c
}

func boolp(b bool) *bool { return &b }

// memItems is an in-memory jobs.ItemStore (the refresh runner of the fixture tests).
type memItems struct {
	mu    sync.Mutex
	items []jobs.Item
	next  int64
}

func (m *memItems) Planned(context.Context, int64) (bool, error) { return false, nil }

func (m *memItems) AddItems(_ context.Context, jobID int64, items []jobs.Item, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range items {
		m.next++
		it.ID, it.JobID = m.next, jobID
		if it.Status == "" {
			it.Status = jobs.ItemPending
		}
		m.items = append(m.items, it)
	}
	return nil
}

func (m *memItems) DeleteItems(context.Context, int64) error { return nil }

func (m *memItems) Pending(_ context.Context, _ int64, after int64, limit int) ([]jobs.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []jobs.Item
	for _, it := range m.items {
		if it.ID > after && it.Status == jobs.ItemPending && len(out) < limit {
			out = append(out, it)
		}
	}
	return out, nil
}

func (m *memItems) SetDetail(_ context.Context, id int64, d json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.items {
		if m.items[i].ID == id {
			m.items[i].Detail = d
		}
	}
	return nil
}

func (m *memItems) Finish(_ context.Context, id int64, st jobs.ItemStatus, bytes int64, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.items {
		if m.items[i].ID == id {
			m.items[i].Status, m.items[i].Bytes, m.items[i].Error = st, bytes, msg
		}
	}
	return nil
}

func (m *memItems) Counts(context.Context, int64) ([]jobs.ItemCount, error) { return nil, nil }
