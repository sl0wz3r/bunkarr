package destinations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

type fixture struct {
	ctx   context.Context
	db    *db.DB
	store *Store
	base  string // a scratch directory for targets
}

func newFixture(t *testing.T, o Options) *fixture {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(base, "bunkarr.db"), nil)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if o.Now == nil {
		o.Now = func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) }
	}
	return &fixture{ctx: ctx, db: d, store: New(d, o), base: base}
}

// target creates an empty directory to use as a target.
func (f *fixture) target(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(f.base, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// create makes a destination on a fresh target (local filesystems allowed: test temp dirs are
// local).
func (f *fixture) create(t *testing.T, name string) Destination {
	t.Helper()
	d, err := f.store.Create(f.ctx, Input{Name: name, Target: f.target(t, name)}, CreateOptions{AllowLocal: true})
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	return d
}

func (f *fixture) addSource(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	err := f.db.Write(f.ctx, func(tx *sql.Tx) error {
		now := db.FormatTime(time.Now())
		res, err := tx.ExecContext(f.ctx, `INSERT INTO sources (name, path, dest_folder, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			name, "/media/"+name, name, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("add source: %v", err)
	}
	return id
}

func readMarkerFile(t *testing.T, target string) Marker {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(target, filecopy.MarkerRel))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m Marker
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse marker: %v", err)
	}
	return m
}

func dirEntries(t *testing.T, p string) []string {
	t.Helper()
	entries, err := os.ReadDir(p)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestCreate(t *testing.T) {
	f := newFixture(t, Options{})
	src1, src2 := f.addSource(t, "Movies"), f.addSource(t, "TV")
	target := f.target(t, "unas")
	link := filepath.Join(f.base, "via-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	enabled := false
	d, err := f.store.Create(f.ctx, Input{
		Name: "  UNAS  ", Target: link, Enabled: &enabled, SourceIDs: []int64{src2, src1, src2},
		Settings:  &Settings{Verify: Verify{Mode: VerifyFull}},
		Retention: &Retention{DeletedDays: 7},
	}, CreateOptions{AllowLocal: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if d.Name != "UNAS" || d.Engine != EngineFilecopy || d.Target != target || d.Enabled {
		t.Errorf("created %+v", d)
	}
	if !slices.Equal(d.SourceIDs, []int64{src1, src2}) {
		t.Errorf("SourceIDs = %v", d.SourceIDs)
	}
	if d.Settings.Verify.Mode != VerifyFull || d.Settings.Verify.SamplePercent != DefaultSamplePercent || d.Settings.MaxChangeFiles != DefaultMaxChangeFiles {
		t.Errorf("settings = %+v", d.Settings)
	}
	wantRet := DefaultRetention()
	wantRet.DeletedDays = 7
	if d.Retention != wantRet {
		t.Errorf("retention = %+v", d.Retention)
	}
	m := readMarkerFile(t, target)
	if m.ID == "" || m.ID != d.MarkerID || m.Name != "UNAS" || m.CreatedAt.IsZero() {
		t.Errorf("marker = %+v, row marker id %q", m, d.MarkerID)
	}
	if d.FSType == "" || d.Capabilities.FSType != d.FSType || d.Capabilities.CheckedAt.IsZero() || d.Capabilities.MtimeGranularityNs < 1 {
		t.Errorf("fsType %q, capabilities %+v", d.FSType, d.Capabilities)
	}
	if left := dirEntries(t, filepath.Join(target, filecopy.ProbeDir)); len(left) != 0 {
		t.Errorf("probe left files behind: %v", left)
	}
	if got := dirEntries(t, target); !slices.Equal(got, []string{filecopy.MetaDir}) {
		t.Errorf("target holds %v, want only %s", got, filecopy.MetaDir)
	}
	got, err := f.store.Get(f.ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MarkerID != d.MarkerID || got.RootDev == 0 && got.FSType == "" {
		t.Errorf("Get = %+v", got)
	}
	if ids, _ := f.store.ForSource(f.ctx, src1); !slices.Equal(ids, []int64{d.ID}) {
		t.Errorf("ForSource = %v", ids)
	}
	list, err := f.store.List(f.ctx)
	if err != nil || len(list) != 1 || !slices.Equal(list[0].SourceIDs, []int64{src1, src2}) {
		t.Errorf("List = %+v, %v", list, err)
	}
}

func TestCreateRefusals(t *testing.T) {
	guardErr := errors.New("the target is inside source Movies")
	f := newFixture(t, Options{
		PathGuard: func(_ context.Context, target string) error {
			if filepath.Base(target) == "inside-source" {
				return guardErr
			}
			return nil
		},
	})
	existing := f.create(t, "existing")
	file := filepath.Join(f.base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(f.base, "missing", "deeper")
	nested := filepath.Join(existing.Target, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	var ve ValidationError
	tests := []struct {
		name  string
		in    Input
		check func(error) bool
	}{
		{"missing target", Input{Name: "a", Target: missing}, func(err error) bool { return errors.As(err, &ve) }},
		{"relative target", Input{Name: "a", Target: "relative/dir"}, func(err error) bool { return errors.As(err, &ve) }},
		{"file target", Input{Name: "a", Target: file}, func(err error) bool { return errors.As(err, &ve) }},
		{"root", Input{Name: "a", Target: "/"}, func(err error) bool { return errors.As(err, &ve) }},
		{"no name", Input{Name: " ", Target: f.target(t, "x1")}, func(err error) bool { return errors.As(err, &ve) }},
		{"name taken", Input{Name: "EXISTING", Target: f.target(t, "x2")}, func(err error) bool { return errors.Is(err, ErrNameTaken) }},
		{"other engine", Input{Name: "a", Engine: "restic", Target: f.target(t, "x3")}, func(err error) bool { return errors.As(err, &ve) }},
		{"bad settings", Input{Name: "a", Target: f.target(t, "x4"), Settings: &Settings{MaxChangePercent: 500}}, func(err error) bool { return errors.As(err, &ve) }},
		{"inside another destination", Input{Name: "a", Target: nested}, func(err error) bool { return errors.As(err, &ve) }},
		{"same as another destination", Input{Name: "a", Target: existing.Target}, func(err error) bool { return errors.As(err, &ve) }},
		{"path guard", Input{Name: "a", Target: f.target(t, "inside-source")}, func(err error) bool { return errors.Is(err, guardErr) }},
		{"unknown source", Input{Name: "a", Target: f.target(t, "x5"), SourceIDs: []int64{999}}, func(err error) bool { return errors.As(err, &ve) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.store.Create(f.ctx, tt.in, CreateOptions{AllowLocal: true})
			if err == nil || !tt.check(err) {
				t.Fatalf("Create: err = %v", err)
			}
			// Nothing was left at the target (the unknown-source case wrote a marker and must
			// have removed it with the .bunkarr directory it created).
			if fi, err := os.Stat(tt.in.Target); err == nil && fi.IsDir() && tt.in.Target != existing.Target && tt.in.Target != nested && tt.in.Target != "/" {
				if left := dirEntries(t, tt.in.Target); len(left) != 0 {
					t.Errorf("target holds %v after a refused create", left)
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Create made the missing target's parent (err=%v)", err)
	}
	if list, _ := f.store.List(f.ctx); len(list) != 1 {
		t.Errorf("%d destinations after refused creates, want 1", len(list))
	}
}

func TestCreateConfigDirOverlap(t *testing.T) {
	cfgBase := t.TempDir()
	f := newFixture(t, Options{ConfigDir: filepath.Join(cfgBase, "config")})
	cfg := filepath.Join(cfgBase, "config")
	if err := os.MkdirAll(filepath.Join(cfg, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{cfg, filepath.Join(cfg, "sub"), cfgBase} {
		var ve ValidationError
		if _, err := f.store.Create(f.ctx, Input{Name: "a", Target: target}, CreateOptions{AllowLocal: true}); !errors.As(err, &ve) {
			t.Errorf("Create(%s): err = %v, want a ValidationError", target, err)
		}
	}
}

func TestCreateExistingMarkerAndAttach(t *testing.T) {
	f := newFixture(t, Options{})
	target := f.target(t, "share")
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	foreign := Marker{ID: newUUID(), Name: "from another Bunkarr", CreatedAt: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)}
	if err := writeMarker(root, foreign); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	before, _ := os.ReadFile(filepath.Join(target, filecopy.MarkerRel))

	if _, err := f.store.Create(f.ctx, Input{Name: "a", Target: target}, CreateOptions{AllowLocal: true}); !errors.Is(err, ErrMarkerExists) {
		t.Fatalf("Create over a marker: err = %v, want ErrMarkerExists", err)
	}
	d, err := f.store.Create(f.ctx, Input{Name: "a", Target: target}, CreateOptions{AllowLocal: true, Attach: true})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if d.MarkerID != foreign.ID {
		t.Errorf("attached marker id %q, want %q", d.MarkerID, foreign.ID)
	}
	after, _ := os.ReadFile(filepath.Join(target, filecopy.MarkerRel))
	if string(before) != string(after) {
		t.Errorf("attach rewrote the marker")
	}
	h, err := f.store.Open(f.ctx, d.ID)
	if err != nil {
		t.Fatalf("Open attached: %v", err)
	}
	_ = h.Close()
	// The same share cannot be attached twice (e.g. through another path).
	link := filepath.Join(f.base, "share-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(f.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	d2, err := f.store.Create(f.ctx, Input{Name: "b", Target: link}, CreateOptions{AllowLocal: true, Attach: true})
	if err != nil {
		t.Fatalf("re-attach after delete: %v", err)
	}
	if _, err := f.store.byMarker(f.ctx, d2.MarkerID); err != nil {
		t.Fatal(err)
	}

	// An unreadable marker is never overwritten, attach or not.
	bad := f.target(t, "bad-marker")
	if err := os.MkdirAll(filepath.Join(bad, filecopy.MetaDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, filecopy.MarkerRel), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, attach := range []bool{false, true} {
		if _, err := f.store.Create(f.ctx, Input{Name: "c", Target: bad}, CreateOptions{AllowLocal: true, Attach: attach}); !errors.Is(err, ErrMarkerExists) {
			t.Errorf("Create over an invalid marker (attach=%v): err = %v, want ErrMarkerExists", attach, err)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(bad, filecopy.MarkerRel)); string(raw) != "not json" {
		t.Errorf("invalid marker changed: %q", raw)
	}
}

func TestCreateRefusesLocalFilesystem(t *testing.T) {
	probe := t.TempDir()
	devs, err := DevsOf(probe)
	if err != nil {
		t.Fatal(err)
	}
	// Pretend the temp dir's device is the one holding / (injected, as main does with DevsOf).
	f := newFixture(t, Options{LocalDevs: devs})
	target := f.target(t, "local")
	_, err = f.store.Create(f.ctx, Input{Name: "local", Target: target}, CreateOptions{})
	if !errors.Is(err, ErrLocalFilesystem) {
		t.Fatalf("Create on a local disk: err = %v, want ErrLocalFilesystem", err)
	}
	if left := dirEntries(t, target); len(left) != 0 {
		t.Errorf("refused create wrote %v", left)
	}
	res, err := f.store.Test(f.ctx, target, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Local || !res.OK || len(res.Warnings) == 0 {
		t.Errorf("Test on a local disk = %+v, want local with a warning", res)
	}
	if _, err := f.store.Create(f.ctx, Input{Name: "local", Target: target}, CreateOptions{AllowLocal: true}); err != nil {
		t.Fatalf("Create with allowLocal: %v", err)
	}
}

func TestOpen(t *testing.T) {
	f := newFixture(t, Options{})
	d := f.create(t, "unas")
	h, err := f.store.Open(f.ctx, d.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()
	if h.Destination.ID != d.ID || h.Settings != d.Settings || h.Retention != d.Retention || h.Capabilities != d.Capabilities {
		t.Errorf("handle = %+v", h)
	}
	if err := h.Recheck(); err != nil {
		t.Errorf("Recheck: %v", err)
	}
	if err := h.Root.WriteFile("probe.txt", []byte("x"), 0o644); err != nil {
		t.Errorf("write through the handle: %v", err)
	}
	if _, err := h.Root.Stat("../"); err == nil {
		t.Errorf("the handle's root lets a path escape")
	}
	if _, err := f.store.Open(f.ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open(999): err = %v, want ErrNotFound", err)
	}
}

func TestOpenFailures(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, f *fixture, d Destination)
		want   error
	}{
		{"marker removed", func(t *testing.T, f *fixture, d Destination) {
			if err := os.Remove(filepath.Join(d.Target, filecopy.MarkerRel)); err != nil {
				t.Fatal(err)
			}
		}, ErrNotMounted},
		{"whole .bunkarr removed", func(t *testing.T, f *fixture, d Destination) {
			if err := os.RemoveAll(filepath.Join(d.Target, filecopy.MetaDir)); err != nil {
				t.Fatal(err)
			}
		}, ErrNotMounted},
		{"marker of another destination", func(t *testing.T, f *fixture, d Destination) {
			raw, _ := json.Marshal(Marker{ID: newUUID(), Name: "other", CreatedAt: time.Now()})
			if err := os.WriteFile(filepath.Join(d.Target, filecopy.MarkerRel), raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}, ErrMarkerMismatch},
		{"marker corrupted", func(t *testing.T, f *fixture, d Destination) {
			if err := os.WriteFile(filepath.Join(d.Target, filecopy.MarkerRel), []byte("{"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, ErrMarkerMismatch},
		{"target gone", func(t *testing.T, f *fixture, d Destination) {
			if err := os.Rename(d.Target, d.Target+".moved"); err != nil {
				t.Fatal(err)
			}
		}, ErrNotMounted},
		{"empty mountpoint", func(t *testing.T, f *fixture, d Destination) {
			if err := os.Rename(d.Target, d.Target+".unmounted"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(d.Target, 0o755); err != nil {
				t.Fatal(err)
			}
		}, ErrNotMounted},
		{"filesystem changed", func(t *testing.T, f *fixture, d Destination) {
			f.store.statFS = func(r *os.Root) (filecopy.FSStat, error) {
				st, err := filecopy.StatFS(r)
				st.Type = "tmpfs-" + st.Type
				return st, err
			}
		}, ErrFSChanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Options{})
			d := f.create(t, "unas")
			tt.damage(t, f, d)
			h, err := f.store.Open(f.ctx, d.ID)
			if !errors.Is(err, tt.want) {
				if h != nil {
					_ = h.Close()
				}
				t.Fatalf("Open: err = %v, want %v", err, tt.want)
			}
			if err.Error() == "" {
				t.Errorf("empty error message")
			}
			// Nothing was written: no marker was recreated.
			if tt.want == ErrNotMounted {
				if _, err := os.Stat(filepath.Join(d.Target, filecopy.MarkerRel)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("a marker appeared at %s (err=%v)", d.Target, err)
				}
			}
		})
	}
}

func TestRecheckAfterUnmount(t *testing.T) {
	f := newFixture(t, Options{})
	d := f.create(t, "unas")
	h, err := f.store.Open(f.ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := os.Remove(filepath.Join(d.Target, filecopy.MarkerRel)); err != nil {
		t.Fatal(err)
	}
	if err := h.Recheck(); !errors.Is(err, ErrNotMounted) {
		t.Errorf("Recheck after the marker vanished: err = %v, want ErrNotMounted", err)
	}
}

func TestHandleStaysOnTheCheckedDirectory(t *testing.T) {
	// A share "unmounted" mid-job (here: the directory replaced by an empty one): the handle keeps
	// working on the directory it checked and never writes into the new, empty mountpoint.
	f := newFixture(t, Options{})
	d := f.create(t, "unas")
	h, err := f.store.Open(f.ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := os.Rename(d.Target, d.Target+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(d.Target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.Root.WriteFile("after.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if left := dirEntries(t, d.Target); len(left) != 0 {
		t.Errorf("the handle wrote into the new mountpoint: %v", left)
	}
	if _, err := os.Stat(filepath.Join(d.Target+".old", "after.txt")); err != nil {
		t.Errorf("write did not reach the checked directory: %v", err)
	}
}

func TestUpdate(t *testing.T) {
	f := newFixture(t, Options{})
	s1, s2 := f.addSource(t, "Movies"), f.addSource(t, "TV")
	d := f.create(t, "unas")
	other := f.create(t, "other")
	disabled := false

	got, err := f.store.Update(f.ctx, d.ID, Input{
		Name: "UNAS main", Enabled: &disabled, SourceIDs: []int64{s2, s1},
		Settings:  &Settings{Hardlinks: HardlinksCopy},
		Retention: &Retention{DeletedDays: 0, PlexDBDaily: 3},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "UNAS main" || got.Enabled || !slices.Equal(got.SourceIDs, []int64{s1, s2}) {
		t.Errorf("updated = %+v", got)
	}
	if got.Settings.Hardlinks != HardlinksCopy || got.Settings.Verify.Mode != VerifySample {
		t.Errorf("settings = %+v", got.Settings)
	}
	wantRet := DefaultRetention()
	wantRet.PlexDBDaily = 3
	if got.Retention != wantRet {
		t.Errorf("retention = %+v (zero deletedDays must become 30)", got.Retention)
	}
	// Empty fields keep the stored values; the unchanged target (even through a symlink) is fine.
	link := filepath.Join(f.base, "unas-link")
	if err := os.Symlink(d.Target, link); err != nil {
		t.Fatal(err)
	}
	got2, err := f.store.Update(f.ctx, d.ID, Input{Target: link})
	if err != nil {
		t.Fatalf("Update with the same target: %v", err)
	}
	if got2.Name != got.Name || got2.Settings != got.Settings || !slices.Equal(got2.SourceIDs, got.SourceIDs) {
		t.Errorf("empty update changed the destination: %+v", got2)
	}
	if got2, _ = f.store.Update(f.ctx, d.ID, Input{SourceIDs: []int64{}}); len(got2.SourceIDs) != 0 {
		t.Errorf("sources not cleared: %v", got2.SourceIDs)
	}

	var ve ValidationError
	if _, err := f.store.Update(f.ctx, d.ID, Input{Target: other.Target}); !errors.As(err, &ve) {
		t.Errorf("target change: err = %v, want a ValidationError", err)
	}
	if _, err := f.store.Update(f.ctx, d.ID, Input{Engine: "rclone"}); !errors.As(err, &ve) {
		t.Errorf("engine change: err = %v, want a ValidationError", err)
	}
	if _, err := f.store.Update(f.ctx, d.ID, Input{Name: "OTHER"}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("name conflict: err = %v, want ErrNameTaken", err)
	}
	if _, err := f.store.Update(f.ctx, d.ID, Input{Retention: &Retention{DeletedDays: -3}}); !errors.As(err, &ve) {
		t.Errorf("negative retention: err = %v, want a ValidationError", err)
	}
	if _, err := f.store.Update(f.ctx, 999, Input{Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", err)
	}
	if err := f.store.SetSources(f.ctx, 999, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetSources unknown id: err = %v", err)
	}
	if err := f.store.SetSources(f.ctx, d.ID, []int64{s1}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := f.store.Sources(f.ctx, d.ID); !slices.Equal(ids, []int64{s1}) {
		t.Errorf("Sources = %v", ids)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t, Options{})
	src := f.addSource(t, "Movies")
	d := f.create(t, "unas")
	if err := f.store.SetSources(f.ctx, d.ID, []int64{src}); err != nil {
		t.Fatal(err)
	}
	// File records with hardlink dependencies (destination_files.link_of is ON DELETE RESTRICT):
	// the primary has the lower id, so a plain cascade meets it before its dependents, and one
	// dependent is itself pointed at (a chain).
	err := f.db.Write(f.ctx, func(tx *sql.Tx) error {
		ins := func(rel string, linkOf any, state string) (int64, error) {
			res, err := tx.ExecContext(f.ctx, `INSERT INTO destination_files (destination_id, source_id, rel_path, source_rel_path, size, mtime_ns, link_of, state)
				VALUES (?, ?, ?, ?, 1, 1, ?, ?)`, d.ID, src, rel, rel, linkOf, state)
			if err != nil {
				return 0, err
			}
			return res.LastInsertId()
		}
		primary, err := ins("Movies/a.mkv", nil, "present")
		if err != nil {
			return err
		}
		dep, err := ins("Movies/b.mkv", primary, "linked")
		if err != nil {
			return err
		}
		_, err = ins("Movies/c.mkv", dep, "link_recorded")
		return err
	})
	if err != nil {
		t.Fatalf("insert file records: %v", err)
	}
	if err := f.store.Delete(f.ctx, d.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.store.Get(f.ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: err = %v", err)
	}
	var n int
	if err := f.db.Reader().QueryRowContext(f.ctx, `SELECT count(*) FROM destination_files`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d file records left (err=%v)", n, err)
	}
	if ids, _ := f.store.ForSource(f.ctx, src); len(ids) != 0 {
		t.Errorf("source links left: %v", ids)
	}
	// The data and the marker stay at the target.
	if m := readMarkerFile(t, d.Target); m.ID != d.MarkerID {
		t.Errorf("marker changed by Delete")
	}
	if err := f.store.Delete(f.ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete: err = %v, want ErrNotFound", err)
	}
}

func TestTestNewTarget(t *testing.T) {
	f := newFixture(t, Options{})
	empty := f.target(t, "empty")
	res, err := f.store.Test(f.ctx, empty, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Marker != MarkerMissing || !res.Writable || res.Capabilities != nil || res.TotalBytes == 0 || res.FSType == "" {
		t.Errorf("Test(empty) = %+v", res)
	}
	if left := dirEntries(t, empty); len(left) != 0 {
		t.Errorf("Test wrote %v into a target that is not a destination", left)
	}

	full := f.target(t, "full")
	for _, n := range []string{"Movies", "TV"} {
		if err := os.Mkdir(filepath.Join(full, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	res, _ = f.store.Test(f.ctx, full, 0)
	if !res.OK || res.Entries != 2 || len(res.Warnings) == 0 {
		t.Errorf("Test(non-empty) = %+v", res)
	}

	res, _ = f.store.Test(f.ctx, filepath.Join(f.base, "nope"), 0)
	if res.OK || res.Message == "" {
		t.Errorf("Test(missing) = %+v", res)
	}

	// A marker from elsewhere: foreign (attach possible); our own destination's target: refused
	// (by the overlap check; the marker check catches the same share under another path).
	d := f.create(t, "ours")
	res, _ = f.store.Test(f.ctx, d.Target, 0)
	if res.OK || res.Message == "" {
		t.Errorf("Test(our destination's target) = %+v", res)
	}
	alias := filepath.Join(f.base, "alias")
	if err := os.Mkdir(alias, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(alias, filecopy.MetaDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(d.Target, filecopy.MarkerRel), filepath.Join(alias, filecopy.MarkerRel)); err != nil {
		t.Fatal(err)
	}
	res, _ = f.store.Test(f.ctx, alias, 0)
	if res.OK || res.Marker != MarkerForeign {
		t.Errorf("Test(same marker under another path) = %+v", res)
	}
	foreign := f.target(t, "foreign")
	root, _ := os.OpenRoot(foreign)
	if err := writeMarker(root, Marker{ID: newUUID(), Name: "elsewhere", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	res, _ = f.store.Test(f.ctx, foreign, 0)
	if !res.OK || res.Marker != MarkerForeign || len(res.Warnings) == 0 {
		t.Errorf("Test(foreign marker) = %+v", res)
	}
}

func TestTestExistingDestination(t *testing.T) {
	f := newFixture(t, Options{})
	d := f.create(t, "unas")
	later := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	f.store.now = func() time.Time { return later }

	res, err := f.store.Test(f.ctx, "", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Marker != MarkerOK || !res.Writable || res.Capabilities == nil {
		t.Fatalf("Test(existing) = %+v", res)
	}
	got, _ := f.store.Get(f.ctx, d.ID)
	if !got.Capabilities.CheckedAt.Equal(later) {
		t.Errorf("capabilities not stored: checkedAt %v", got.Capabilities.CheckedAt)
	}
	if left := dirEntries(t, filepath.Join(d.Target, filecopy.ProbeDir)); len(left) != 0 {
		t.Errorf("probe left %v", left)
	}

	var ve ValidationError
	if _, err := f.store.Test(f.ctx, f.target(t, "elsewhere"), d.ID); !errors.As(err, &ve) {
		t.Errorf("Test with another target: err = %v, want a ValidationError", err)
	}
	if _, err := f.store.Test(f.ctx, "", 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Test(999): err = %v", err)
	}

	caps, err := f.store.RefreshCapabilities(f.ctx, d.ID)
	if err != nil || caps.FSType != d.FSType {
		t.Errorf("RefreshCapabilities = %+v, %v", caps, err)
	}

	// Filesystem changed: reported, no probe.
	f.store.statFS = func(r *os.Root) (filecopy.FSStat, error) {
		st, err := filecopy.StatFS(r)
		st.Type = "nfs-" + st.Type
		return st, err
	}
	if res, _ := f.store.Test(f.ctx, "", d.ID); res.OK || res.Capabilities != nil {
		t.Errorf("Test after a filesystem change = %+v", res)
	}
	if _, err := f.store.RefreshCapabilities(f.ctx, d.ID); !errors.Is(err, ErrFSChanged) {
		t.Errorf("RefreshCapabilities after a filesystem change: err = %v", err)
	}
	f.store.statFS = filecopy.StatFS

	// Marker missing: reported, nothing written.
	if err := os.Remove(filepath.Join(d.Target, filecopy.MarkerRel)); err != nil {
		t.Fatal(err)
	}
	res, _ = f.store.Test(f.ctx, "", d.ID)
	if res.OK || res.Marker != MarkerMissing || res.Writable || res.Capabilities != nil {
		t.Errorf("Test with the marker missing = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(d.Target, filecopy.MarkerRel)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Test recreated the marker")
	}
	// Marker of another destination: mismatch.
	raw, _ := json.Marshal(Marker{ID: newUUID(), Name: "other", CreatedAt: time.Now()})
	if err := os.WriteFile(filepath.Join(d.Target, filecopy.MarkerRel), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if res, _ = f.store.Test(f.ctx, "", d.ID); res.OK || res.Marker != MarkerMismatch {
		t.Errorf("Test with another marker = %+v", res)
	}
}

func TestProbe(t *testing.T) {
	dir := t.TempDir()
	caps, err := Probe(context.Background(), dir)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	// Test temp directories are on APFS (macOS) or ext4/overlay/tmpfs (Linux): all store any
	// character, keep trailing dots, support hardlinks, number inodes stably and store nanosecond
	// mtimes.
	if !caps.Hardlinks || caps.UnstableInodes || !caps.InodeIdentity() || caps.InvalidChars != "" || !caps.TrailingDotSpace ||
		caps.MtimeGranularityNs != 1 || caps.FSType == "" {
		t.Errorf("Probe = %+v", caps)
	}
	if left := dirEntries(t, filepath.Join(dir, filecopy.ProbeDir)); len(left) != 0 {
		t.Errorf("probe left %v", left)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Probe(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Probe: err = %v", err)
	}
}

func TestGranularity(t *testing.T) {
	odd := probeMtimes[0].UnixNano()
	even := probeMtimes[1].UnixNano()
	const s = int64(time.Second)
	tests := []struct {
		name      string
		set, got  int64
		want      int64
		wantExact bool
	}{
		{"ns", odd, odd, 1, true},
		{"100ns truncating (NTFS)", odd, odd - 89, 100, true},
		{"100ns rounding", odd, odd + 11, 100, true},
		{"µs", odd, odd - 789, 1000, true},
		{"1s truncating", odd, odd - 123_456_789, s, true},
		{"1s rounding up", odd, odd - 123_456_789 + s, s, true},
		{"2s truncating on an odd second", odd, odd - 123_456_789 - s, 2 * s, true},
		{"2s rounding up on an even second", even, even - 123_456_789 + 2*s, 2 * s, true},
		{"ignored", odd, 42, 2 * s, false},
	}
	for _, tt := range tests {
		g, exact := granularity(tt.set, tt.got)
		if g != tt.want || exact != tt.wantExact {
			t.Errorf("%s: granularity = %d, %v; want %d, %v", tt.name, g, exact, tt.want, tt.wantExact)
		}
	}
}

func TestOverlaps(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"/mnt/unas", "/mnt/unas", true},
		{"/mnt/unas/backup", "/mnt/unas", true},
		{"/mnt/unas", "/mnt/unas/backup", true},
		{"/mnt/unas2", "/mnt/unas", false},
		{"/mnt/a", "/mnt/b", false},
		{"/data", "/", true},
	}
	for _, tt := range tests {
		if got := overlaps(tt.a, tt.b); got != tt.want {
			t.Errorf("overlaps(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestNewUUID(t *testing.T) {
	a, b := newUUID(), newUUID()
	if a == b || len(a) != 36 || a[14] != '4' {
		t.Errorf("newUUID = %q, %q", a, b)
	}
}
