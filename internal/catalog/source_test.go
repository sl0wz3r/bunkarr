package catalog

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

func boolPtr(b bool) *bool { return &b }

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	dir := tempDir(t)
	file := filepath.Join(dir, "file")
	writeFiles(t, dir, map[string]string{"file": "x"})
	media := filepath.Join(dir, "media")
	if err := os.Mkdir(media, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		in    SourceInput
		field string // "" = valid
	}{
		{"valid", SourceInput{Name: "Movies", Path: media}, ""},
		{"empty name", SourceInput{Name: "  ", Path: media}, "name"},
		{"long name", SourceInput{Name: strings.Repeat("n", 65), Path: media}, "name"},
		{"64 characters", SourceInput{Name: strings.Repeat("é", 64), Path: media, DestFolder: "e"}, ""},
		{"control character", SourceInput{Name: "a\tb", Path: media}, "name"},
		{"empty path", SourceInput{Name: "M", Path: ""}, "path"},
		{"relative path", SourceInput{Name: "M", Path: "media"}, "path"},
		{"missing path", SourceInput{Name: "M", Path: filepath.Join(dir, "nope")}, "path"},
		{"file path", SourceInput{Name: "M", Path: file}, "path"},
		{"filesystem root", SourceInput{Name: "M", Path: "/"}, "path"},
		{"name without slug", SourceInput{Name: "日本", Path: media}, "destFolder"},
		{"parent segment", SourceInput{Name: "M", Path: media, DestFolder: "../x"}, "destFolder"},
		{"unclean", SourceInput{Name: "M", Path: media, DestFolder: "a//b"}, "destFolder"},
		{"trailing slash", SourceInput{Name: "M", Path: media, DestFolder: "a/"}, "destFolder"},
		{"absolute", SourceInput{Name: "M", Path: media, DestFolder: "/a"}, "destFolder"},
		{"dot folder", SourceInput{Name: "M", Path: media, DestFolder: ".hidden"}, "destFolder"},
		{"nested .bunkarr", SourceInput{Name: "M", Path: media, DestFolder: "a/.bunkarr"}, "destFolder"},
		{"smb character", SourceInput{Name: "M", Path: media, DestFolder: "a:b"}, "destFolder"},
		{"trailing dot", SourceInput{Name: "M", Path: media, DestFolder: "movies."}, "destFolder"},
		{"leading space", SourceInput{Name: "M", Path: media, DestFolder: "a/ b"}, "destFolder"},
		{"bad glob", SourceInput{Name: "M", Path: media, Exclude: []string{"[a"}}, "exclude"},
		{"empty glob", SourceInput{Name: "M", Path: media, Exclude: []string{" "}}, "exclude"},
		{"slash-only glob", SourceInput{Name: "M", Path: media, Exclude: []string{"/"}}, "exclude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, err := st.Create(ctx, tc.in)
			if tc.field == "" {
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				if err := st.Delete(ctx, src.ID); err != nil {
					t.Fatal(err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Field != tc.field {
				t.Fatalf("err = %v, want a ValidationError on %s", err, tc.field)
			}
		})
	}
}

func TestCreateDefaults(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	dir := tempDir(t)
	src, err := st.Create(ctx, SourceInput{Name: " My Movies (4K)! ", Path: dir + "/.", Exclude: []string{" *.nfo ", "Extras/"}})
	if err != nil {
		t.Fatal(err)
	}
	if src.Name != "My Movies (4K)!" || src.DestFolder != "my-movies-4k" || !src.Enabled || src.Path != dir ||
		len(src.Exclude) != 2 || src.Exclude[0] != "*.nfo" || src.PlexIntegrationID != nil || src.LastScanAt != nil {
		t.Fatalf("created = %+v", src)
	}
	list, err := st.List(ctx)
	if err != nil || len(list) != 1 || list[0].ID != src.ID {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if _, err := st.Get(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Movies":             "movies",
		"TV Shows":           "tv-shows",
		"  Kids   Films ":    "kids-films",
		"Filme für Kinder":   "filme-fr-kinder",
		".hidden":            "hidden",
		"4K_UHD.remux":       "4k_uhd.remux",
		"a - b":              "a-b",
		"trailing.":          "trailing",
		"日本":                 "",
		"Movies/Collections": "moviescollections",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUniqueness(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	a, b := tempDir(t), tempDir(t)
	createSource(t, st, "Movies", a)
	for _, tc := range []struct {
		name string
		in   SourceInput
	}{
		{"same name, other case", SourceInput{Name: "MOVIES", Path: b, DestFolder: "other"}},
		{"same folder, other case", SourceInput{Name: "Films", Path: b, DestFolder: "Movies"}},
		{"folder inside", SourceInput{Name: "Films", Path: b, DestFolder: "movies/4k"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.Create(ctx, tc.in); !errors.Is(err, ErrConflict) {
				t.Fatalf("err = %v, want ErrConflict", err)
			}
		})
	}
	nested := createSource(t, st, "Movies 4K", b) // slug "movies-4k" does not overlap "movies"
	if nested.DestFolder != "movies-4k" {
		t.Fatalf("destFolder = %q", nested.DestFolder)
	}
	if _, err := st.Update(ctx, nested.ID, SourceInput{Name: "Movies 4K", Path: b, DestFolder: "movies/4k"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("update into another source's folder = %v", err)
	}
}

func TestPathGuardAndIntegrations(t *testing.T) {
	ctx := context.Background()
	guardErr := errors.New("path is inside destination Backup")
	var seen []string
	st := newStore(t, StoreOptions{PathGuard: func(_ context.Context, p string) error {
		seen = append(seen, p)
		if strings.HasSuffix(p, "forbidden") {
			return guardErr
		}
		return nil
	}})
	base := tempDir(t)
	ok := filepath.Join(base, "ok")
	bad := filepath.Join(base, "forbidden")
	for _, d := range []string{ok, bad} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(tempDir(t), "alias")
	if err := os.Symlink(ok, link); err != nil {
		t.Fatal(err)
	}
	src, err := st.Create(ctx, SourceInput{Name: "A", Path: link})
	if err != nil {
		t.Fatal(err)
	}
	if src.Path != ok || len(seen) != 1 || seen[0] != ok {
		t.Fatalf("stored %q, guard saw %v; want the resolved path %q", src.Path, seen, ok)
	}
	_, err = st.Create(ctx, SourceInput{Name: "B", Path: bad})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "path" || !errors.Is(err, guardErr) {
		t.Fatalf("err = %v, want the guard's error as a path ValidationError", err)
	}
	if res := st.TestSource(ctx, bad); res.OK || res.Message != guardErr.Error() {
		t.Fatalf("TestSource = %+v", res)
	}
	id := int64(12345)
	if _, err := st.Create(ctx, SourceInput{Name: "C", Path: ok, DestFolder: "c", PlexIntegrationID: &id}); !errors.As(err, &ve) {
		t.Fatalf("unknown integration = %v", err)
	}
}

func TestUpdate(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"a.mkv": "a"})
	src := createSource(t, st, "Movies", root)
	mustScan(t, NewScanner(st, ScannerOptions{}), src.ID)
	rec, err := st.identity(ctx, src.ID)
	if err != nil || !rec.fsType.Valid || !rec.rootDev.Valid {
		t.Fatalf("identity after the scan = %+v, %v", rec, err)
	}

	up, err := st.Update(ctx, src.ID, SourceInput{Name: "Films", Path: root, Exclude: []string{"*.nfo"}, PlexSectionID: "3", PlexPath: "/data/movies"})
	if err != nil {
		t.Fatal(err)
	}
	if up.Name != "Films" || up.DestFolder != "movies" || !up.Enabled || up.PlexSectionID != "3" || up.PlexPath != "/data/movies" ||
		up.FSType != rec.fsType.String || !up.UpdatedAt.After(src.UpdatedAt) || up.Stats.Files != 1 {
		t.Fatalf("updated = %+v", up)
	}
	var ident identity
	if ident, err = st.identity(ctx, src.ID); err != nil || ident != rec {
		t.Fatalf("identity after save = %+v, %v; want the recorded %+v kept", ident, err, rec)
	}
	// Unchanged path: editable while the share is unmounted.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	up, err = st.Update(ctx, src.ID, SourceInput{Name: "Films", Path: root, Enabled: boolPtr(false)})
	if err != nil || up.Enabled || len(up.Exclude) != 0 {
		t.Fatalf("update of an unmounted source = %+v, %v", up, err)
	}
	if _, err := st.Update(ctx, 999, SourceInput{Name: "X", Path: tempDir(t)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing = %v", err)
	}
}

func TestUpdateWithBackups(t *testing.T) {
	ctx := context.Background()
	has := true
	calls := 0
	st := newStore(t, StoreOptions{HasBackups: func(context.Context, int64) (bool, error) { calls++; return has, nil }})
	base := tempDir(t)
	root := filepath.Join(base, "media")
	writeFiles(t, root, map[string]string{"a.mkv": "a"})
	other := filepath.Join(base, "other")
	writeFiles(t, other, map[string]string{"b.mkv": "b"})
	src := createSource(t, st, "Movies", root)
	mustScan(t, NewScanner(st, ScannerOptions{}), src.ID)

	// Unrelated edits do not ask.
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Movies", Path: root, Enabled: boolPtr(false)}); err != nil || calls != 0 {
		t.Fatalf("plain edit = %v, HasBackups calls %d", err, calls)
	}
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Movies", Path: root, DestFolder: "films"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("destFolder change = %v, want ErrConflict", err)
	}
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Movies", Path: other}); !errors.Is(err, ErrConflict) {
		t.Fatalf("path change to another directory = %v, want ErrConflict", err)
	}

	// The same directory under a new path (a renamed mount point) is accepted: the identity the
	// last scan recorded is compared (the edit above kept it).
	moved := filepath.Join(base, "renamed")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	up, err := st.Update(ctx, src.ID, SourceInput{Name: "Movies", Path: moved})
	if err != nil || up.Path != moved {
		t.Fatalf("path change to the same directory = %+v, %v", up, err)
	}

	// Without the recorded identity (a plain re-save cleared it) the current path is compared.
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Movies", Path: moved}); err != nil {
		t.Fatal(err)
	}
	if ident, err := st.identity(ctx, src.ID); err != nil || ident.rootDev.Valid {
		t.Fatalf("identity after a plain re-save = %+v, %v; want cleared", ident, err)
	}
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Movies", Path: other}); !errors.Is(err, ErrConflict) {
		t.Fatalf("path change without identity = %v, want ErrConflict", err)
	}

	// No backups: both may change.
	has = false
	up, err = st.Update(ctx, src.ID, SourceInput{Name: "Movies", Path: other, DestFolder: "films"})
	if err != nil || up.Path != other || up.DestFolder != "films" {
		t.Fatalf("free change = %+v, %v", up, err)
	}

	// A path change needs the source's lock.
	unlock, err := st.LockSource(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Movies", Path: moved}); !errors.Is(err, ErrConflict) {
		t.Fatalf("path change during a scan = %v, want ErrConflict", err)
	}
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Movies", Path: other, DestFolder: "movies-new"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("destFolder change during a scan = %v, want ErrConflict", err)
	}
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Renamed", Path: other}); err != nil {
		t.Fatalf("plain edit during a scan = %v", err)
	}
	unlock()

	failing := newStore(t, StoreOptions{HasBackups: func(context.Context, int64) (bool, error) { return false, errors.New("boom") }})
	s2 := createSource(t, failing, "X", other)
	if _, err := failing.Update(ctx, s2.ID, SourceInput{Name: "X", Path: other, DestFolder: "y"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("HasBackups error = %v", err)
	}
}

// A save keeps the filesystem identity the last scan recorded (S10a): the scan guard still applies
// after an edit, and a renamed mount point can still be followed after an edit made while the old
// path is gone. A plain re-save (no setting changed) while the path exists clears it, which is how
// a filesystem change a scan refused is accepted; so does a path change while no destination
// holds backups, since the new path may then be any directory.
func TestUpdateKeepsIdentity(t *testing.T) {
	ctx := context.Background()
	has := true
	st := newStore(t, StoreOptions{HasBackups: func(context.Context, int64) (bool, error) { return has, nil }})
	sc := NewScanner(st, ScannerOptions{})
	base := tempDir(t)
	root := filepath.Join(base, "media")
	writeFiles(t, root, map[string]string{"a.mkv": "a", "b.mkv": "b"})
	src := createSource(t, st, "Movies", root)
	mustScan(t, sc, src.ID)
	rec, err := st.identity(ctx, src.ID)
	if err != nil || !rec.fsType.Valid || !rec.rootDev.Valid || !rec.rootIno.Valid {
		t.Fatalf("identity = %+v, %v", rec, err)
	}
	identityIs := func(what string, want identity) {
		t.Helper()
		if got, err := st.identity(ctx, src.ID); err != nil || got != want {
			t.Fatalf("identity after %s = %+v, %v; want %+v", what, got, err, want)
		}
	}
	save := func(what string, in SourceInput) {
		t.Helper()
		in.Name = "Films"
		if _, err := st.Update(ctx, src.ID, in); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	save("an edit", SourceInput{Path: root, Exclude: []string{"*.nfo"}})
	identityIs("an edit", rec)
	sc.identityHook = func(id rootIdentity) rootIdentity { id.fsType = "tmpfs"; return id }
	if _, err := sc.Scan(ctx, src.ID, nil); !errors.Is(err, ErrScanRefused) {
		t.Fatalf("scan of another filesystem after an edit = %v, want refused", err)
	}
	sc.identityHook = nil

	// The mount point is renamed and the failing source is disabled; a re-save while the path is
	// gone has nothing to accept. The path can still change to the renamed directory.
	moved := filepath.Join(base, "renamed")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	save("disabling", SourceInput{Path: root, Exclude: []string{"*.nfo"}, Enabled: boolPtr(false)})
	identityIs("disabling", rec)
	save("a re-save while the path is gone", SourceInput{Path: root, Exclude: []string{"*.nfo"}})
	identityIs("a re-save while the path is gone", rec)
	save("the path change", SourceInput{Path: moved, Exclude: []string{"*.nfo"}, Enabled: boolPtr(true)})
	identityIs("a path change to the same directory", rec)
	if r, err := sc.Scan(ctx, src.ID, nil); err != nil || r.Deleted != 0 || r.Files != 2 {
		t.Fatalf("scan after the path change = %+v, %v", r, err)
	}

	save("a plain re-save", SourceInput{Path: moved, Exclude: []string{"*.nfo"}})
	identityIs("a plain re-save", identity{})

	mustScan(t, sc, src.ID)
	identityIs("a scan", rec)
	has = false
	save("a destFolder change without backups", SourceInput{Path: moved, Exclude: []string{"*.nfo"}, DestFolder: "films"})
	identityIs("a destFolder change without backups", rec)
	sc.identityHook = func(id rootIdentity) rootIdentity { id.fsType = "tmpfs"; return id }
	if _, err := sc.Scan(ctx, src.ID, nil); !errors.Is(err, ErrScanRefused) {
		t.Fatalf("scan of another filesystem after a destFolder change = %v, want refused", err)
	}
	sc.identityHook = nil
	other := filepath.Join(base, "other")
	writeFiles(t, other, map[string]string{"c.mkv": "c"})
	save("a path change without backups", SourceInput{Path: other, Exclude: []string{"*.nfo"}})
	identityIs("a path change without backups", identity{})
}

// Deleting a Plex integration unlinks its sources (ON DELETE SET NULL) but keeps their Plex section
// and path, which the web form does not send for an unlinked source. Saving such a source from the
// form without touching anything is still the plain re-save a refused scan asks for.
func TestPlainResaveAfterPlexUnlink(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"a.mkv": "a"})
	var plexID int64
	if err := st.db.Write(ctx, func(tx *sql.Tx) error {
		now := db.FormatTime(time.Now())
		res, err := tx.ExecContext(ctx, `INSERT INTO integrations (type, name, url, created_at, updated_at)
			VALUES ('plex', 'Plex', 'http://plex:32400', ?, ?)`, now, now)
		if err != nil {
			return err
		}
		plexID, err = res.LastInsertId()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	src, err := st.Create(ctx, SourceInput{Name: "Movies", Path: root, PlexIntegrationID: &plexID, PlexSectionID: "1", PlexPath: "/data/movies"})
	if err != nil {
		t.Fatal(err)
	}
	mustScan(t, sc, src.ID)
	if err := st.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM integrations WHERE id = ?`, plexID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cur, err := st.Get(ctx, src.ID)
	if err != nil || cur.PlexIntegrationID != nil || cur.PlexSectionID != "1" {
		t.Fatalf("source after the integration was deleted = %+v, %v", cur, err)
	}
	sc.identityHook = func(id rootIdentity) rootIdentity { id.fsType = "tmpfs"; return id }
	if _, err := sc.Scan(ctx, src.ID, nil); !errors.Is(err, ErrScanRefused) {
		t.Fatalf("scan of another filesystem = %v, want refused", err)
	}
	// What the form sends for this source when Save is clicked without touching anything.
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: cur.Name, Path: cur.Path, DestFolder: cur.DestFolder,
		Exclude: cur.Exclude, Enabled: boolPtr(cur.Enabled)}); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Scan(ctx, src.ID, nil); err != nil {
		t.Fatalf("scan after the plain re-save = %v, want the change accepted", err)
	}
}

// A plain re-save is one that changes no setting at all.
func TestUnchanged(t *testing.T) {
	one, two := int64(1), int64(2)
	text := func(s string) *string { return &s }
	cur := Source{Name: "Movies", Path: "/media", DestFolder: "movies", Exclude: []string{"*.nfo"}, Enabled: true,
		PlexIntegrationID: &one, PlexSectionID: "3", PlexPath: "/data", ArrIntegrationID: &two}
	base := func() validSource {
		return validSource{name: "Movies", path: "/media", destFolder: "movies", exclude: []string{"*.nfo"}, enabled: true,
			plexIntegrationID: &one, plexSectionID: text("3"), plexPath: text("/data"), arrIntegrationID: &two}
	}
	if v := base(); !v.unchanged(cur) {
		t.Fatalf("the same settings: unchanged = false")
	}
	bare := Source{Name: "Movies", Path: "/media", DestFolder: "movies", Exclude: []string{}}
	if v := (validSource{name: "Movies", path: "/media", destFolder: "movies"}); !v.unchanged(bare) {
		t.Fatalf("the same settings without links: unchanged = false")
	}
	// The Plex section and path left behind by a deleted Plex integration are not settings.
	orphan := bare
	orphan.PlexSectionID, orphan.PlexPath = "3", "/data"
	if v := (validSource{name: "Movies", path: "/media", destFolder: "movies"}); !v.unchanged(orphan) {
		t.Fatalf("dropping an unlinked Plex section and path: unchanged = false")
	}
	if v := (validSource{name: "Movies", path: "/media", destFolder: "movies", plexIntegrationID: &one}); v.unchanged(orphan) {
		t.Fatalf("linking Plex again: unchanged = true")
	}
	for name, edit := range map[string]func(*validSource){
		"name":              func(v *validSource) { v.name = "Films" },
		"path":              func(v *validSource) { v.path = "/other" },
		"destFolder":        func(v *validSource) { v.destFolder = "films" },
		"exclude":           func(v *validSource) { v.exclude = nil },
		"enabled":           func(v *validSource) { v.enabled = false },
		"plexIntegrationId": func(v *validSource) { v.plexIntegrationID = &two },
		"no plex link":      func(v *validSource) { v.plexIntegrationID = nil },
		"plexSectionId":     func(v *validSource) { v.plexSectionID = nil },
		"plexPath":          func(v *validSource) { v.plexPath = text("/other") },
		"arrIntegrationId":  func(v *validSource) { v.arrIntegrationID = nil },
	} {
		v := base()
		edit(&v)
		if v.unchanged(cur) {
			t.Errorf("%s changed: unchanged = true", name)
		}
	}
}

// A renamed mount point after a reboot or remount: the kernel gave the anonymous device (FUSE, NFS,
// CIFS, btrfs, ZFS) another number and the old path is gone, so the source cannot be scanned there
// first. Where inode numbers survive a remount the device and inode must still match, since a
// snapshot, clone or replica has the same inodes on another device; with run-time inode numbers
// (FUSE, CIFS) the directory is recognised by its filesystem type and the catalogued files it
// holds, but only while the old path no longer holds the source.
func TestUpdatePathAfterRemount(t *testing.T) {
	ctx := context.Background()
	runTimeIno := func(id rootIdentity, _ uint64) rootIdentity {
		id.dev += 7
		id.ino += 5
		id.anonDev, id.stableIno = true, false
		return id
	}
	for _, tc := range []struct {
		name   string
		change func(id rootIdentity, recIno uint64) rootIdentity
		prep   func(t *testing.T, moved, other string)
		other  bool // the new path is the other directory
		// keepOld leaves the source at its path (moved is then the old path); oldRemounted also
		// gives the recorded root another device number, as after a remount.
		keepOld, oldRemounted bool
		// fsType, when set, is the recorded filesystem type and the new path's instead of the temp
		// directory's (whose type the old path keeps).
		fsType string
		ok     bool
		msg    string // in the refusal
	}{
		{name: "anonymous device with stable inodes", msg: "scan the source at", change: func(id rootIdentity, _ uint64) rootIdentity {
			id.dev += 7
			id.anonDev, id.stableIno = true, true
			return id
		}},
		{name: "run-time inodes (FUSE, CIFS)", ok: true, change: runTimeIno},
		{name: "a catalogued file deleted since the scan", ok: true, change: runTimeIno, prep: func(t *testing.T, moved, _ string) {
			if err := os.Remove(filepath.Join(moved, "b.mkv")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "disk", change: func(id rootIdentity, _ uint64) rootIdentity {
			id.dev += 7
			id.anonDev = false
			return id
		}},
		{name: "root inode changed", change: func(id rootIdentity, _ uint64) rootIdentity {
			id.dev += 7
			id.ino++
			id.anonDev, id.stableIno = true, true
			return id
		}},
		{name: "filesystem type changed", change: func(id rootIdentity, recIno uint64) rootIdentity {
			id = runTimeIno(id, recIno)
			id.fsType = "fuse-other"
			return id
		}},
		{name: "catalogued files changed", change: runTimeIno, prep: func(t *testing.T, moved, _ string) {
			writeFiles(t, moved, map[string]string{"a.mkv": "other a", "c/d.mkv": "other d"})
		}},
		// Two btrfs subvolumes or ZFS datasets have the same root inode.
		{name: "another filesystem with the same root inode", other: true, change: func(id rootIdentity, recIno uint64) rootIdentity {
			id.dev += 7
			id.ino = recIno
			id.anonDev, id.stableIno = true, true
			return id
		}},
		{name: "another directory with run-time inodes", other: true, change: func(id rootIdentity, _ uint64) rootIdentity {
			id.dev += 7
			id.anonDev, id.stableIno = true, false
			return id
		}},
		// Where inodes survive a remount, a copy with the same sizes and mtimes is another directory;
		// with run-time inodes it cannot be told apart, and holds the same files.
		{name: "a copy", other: true, prep: copyTree, change: func(id rootIdentity, recIno uint64) rootIdentity {
			id.dev += 7
			id.ino = recIno
			id.anonDev, id.stableIno = true, true
			return id
		}},
		{name: "a copy with run-time inodes", other: true, ok: true, prep: copyTree, change: runTimeIno},
		// A btrfs or ZFS snapshot, clone or replica: the same root and file inodes, sizes and mtimes
		// on another device.
		{name: "a snapshot", other: true, prep: linkTree, msg: "scan the source at", change: func(id rootIdentity, recIno uint64) rootIdentity {
			id.dev += 7
			id.ino = recIno
			id.anonDev, id.stableIno = true, true
			return id
		}},
		// Not remounted (the recorded device number): another directory of the same filesystem.
		{name: "a copy with run-time inodes on the same device", other: true, prep: copyTree, change: func(id rootIdentity, _ uint64) rootIdentity {
			id.anonDev, id.stableIno = true, false
			return id
		}},
		// The source is still at its path, so the other directory is a copy (an rsync -a mirror).
		{name: "a copy with run-time inodes while the source is at its path", other: true, keepOld: true, prep: copyTree,
			msg: "still holds", change: runTimeIno},
		{name: "a copy with run-time inodes while the remounted source is at its path", other: true, keepOld: true, oldRemounted: true,
			prep: copyTree, msg: "still holds", change: runTimeIno},
		{name: "a stale copy with run-time inodes while the changed source is at its path", other: true, keepOld: true,
			prep: staleCopy, msg: "still holds", change: runTimeIno},
		// Remounted, so neither the recorded device nor inode is at the old path, and its files were
		// renamed or replaced since the last scan (a Radarr rename): the old path still being on a
		// filesystem of the recorded type is what tells that the source is there.
		{name: "a stale copy with run-time inodes while the changed remounted source is at its path", other: true, keepOld: true,
			oldRemounted: true, prep: staleCopy, msg: "the filesystem type of its last scan", change: runTimeIno},
		{name: "a stale copy on the remounted source's device while the changed source is at its path", other: true, keepOld: true,
			oldRemounted: true, prep: staleCopy, msg: "the filesystem type of its last scan", change: func(id rootIdentity, _ uint64) rootIdentity {
				id.ino += 5
				id.anonDev, id.stableIno = true, false
				return id
			}},
		// A renamed mount point whose old mount point directory is left behind, empty.
		{name: "run-time inodes, the old mount point left on another filesystem", fsType: "fuse.shfs", ok: true,
			prep: emptyOldPath, change: runTimeIno},
		{name: "run-time inodes, the old mount point left on a filesystem of the recorded type", msg: "the filesystem type of its last scan",
			prep: emptyOldPath, change: runTimeIno},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, StoreOptions{HasBackups: func(context.Context, int64) (bool, error) { return true, nil }})
			base := tempDir(t)
			root := filepath.Join(base, "media")
			writeFiles(t, root, map[string]string{"a.mkv": "a", "b.mkv": "b", "c/d.mkv": "d"})
			other := filepath.Join(base, "other")
			writeFiles(t, other, map[string]string{"a.mkv": "x", "b.mkv": "y", "c/d.mkv": "z"})
			src := createSource(t, st, "Movies", root)
			mustScan(t, NewScanner(st, ScannerOptions{}), src.ID)
			rec, err := st.identity(ctx, src.ID)
			if err != nil || !rec.rootIno.Valid {
				t.Fatalf("identity = %+v, %v", rec, err)
			}
			if tc.oldRemounted {
				if err := st.db.Write(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE sources SET root_dev = root_dev + 3 WHERE id = ?`, src.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.fsType != "" {
				if err := st.db.Write(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE sources SET fs_type = ? WHERE id = ?`, tc.fsType, src.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			moved := filepath.Join(base, "renamed")
			if tc.keepOld {
				moved = root
			} else if err := os.Rename(root, moved); err != nil {
				t.Fatal(err)
			}
			if tc.prep != nil {
				tc.prep(t, moved, other)
			}
			st.identityHook = func(id rootIdentity) rootIdentity {
				id = tc.change(id, uint64(rec.rootIno.Int64))
				if tc.fsType != "" {
					id.fsType = tc.fsType
				}
				return id
			}
			target := moved
			if tc.other {
				target = other
			}
			up, err := st.Update(ctx, src.ID, SourceInput{Name: "Movies", Path: target})
			if !tc.ok {
				if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "path cannot change") || !strings.Contains(err.Error(), tc.msg) {
					t.Fatalf("path change = %+v, %v; want ErrConflict", up, err)
				}
				if got, _ := st.Get(ctx, src.ID); got.Path != root {
					t.Fatalf("path = %s after a refused change", got.Path)
				}
				return
			}
			if err != nil || up.Path != target {
				t.Fatalf("path change to the remounted directory = %+v, %v", up, err)
			}
		})
	}
}

// staleCopy makes other a copy of old, then changes every file of old.
func staleCopy(t *testing.T, old, other string) {
	t.Helper()
	copyTree(t, old, other)
	writeFiles(t, old, map[string]string{"a.mkv": "new a", "b.mkv": "new b", "c/d.mkv": "new d"})
}

// emptyOldPath leaves an empty directory at the old path of the renamed directory moved.
func emptyOldPath(t *testing.T, moved, _ string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(filepath.Dir(moved), "media"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// linkTree replaces the files of dst with hard links to src's: the same inodes, sizes and mtimes,
// as in a btrfs or ZFS snapshot.
func linkTree(t *testing.T, src, dst string) {
	t.Helper()
	for _, rel := range []string{"a.mkv", "b.mkv", "c/d.mkv"} {
		to := filepath.Join(dst, rel)
		if err := os.Remove(to); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(src, rel), to); err != nil {
			t.Fatal(err)
		}
	}
}

// copyTree replaces the files of dst with copies of src's, with the same mtimes.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	for _, rel := range []string{"a.mkv", "b.mkv", "c/d.mkv"} {
		from, to := filepath.Join(src, rel), filepath.Join(dst, rel)
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(to, fi.ModTime(), fi.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
}

// A sync executes its plan without the source's lock, so a path or destFolder change also asks
// StoreOptions.ActiveJobs (while holding the lock) and refuses while the source has queued or
// running jobs, even before a sync's first backup makes HasBackups true.
func TestUpdateRefusedWhileJobsActive(t *testing.T) {
	ctx := context.Background()
	var (
		st      *Store
		active  = true
		hookErr error
		asked   int
	)
	st = newStore(t, StoreOptions{ActiveJobs: func(_ context.Context, id int64) (bool, error) {
		asked++
		if !st.locks.held(id) {
			t.Errorf("ActiveJobs(%d) called without the source's lock", id)
		}
		return active, hookErr
	}})
	base := tempDir(t)
	root := filepath.Join(base, "media")
	other := filepath.Join(base, "other")
	writeFiles(t, root, map[string]string{"a.mkv": "a"})
	writeFiles(t, other, map[string]string{"b.mkv": "b"})
	src := createSource(t, st, "Movies", root)
	unchanged := func(what string) {
		t.Helper()
		got, err := st.Get(ctx, src.ID)
		if err != nil || got.Path != root || got.DestFolder != "movies" {
			t.Fatalf("%s: source = %+v, %v", what, got, err)
		}
		if st.locks.held(src.ID) {
			t.Fatalf("%s: the source's lock is still held", what)
		}
	}

	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Films", Path: root}); err != nil || asked != 0 {
		t.Fatalf("plain edit = %v, ActiveJobs calls %d", err, asked)
	}
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Films", Path: other}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "jobs") {
		t.Fatalf("path change with an active job = %v, want ErrConflict", err)
	}
	unchanged("path change")
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Films", Path: root, DestFolder: "films"}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "jobs") {
		t.Fatalf("destFolder change with an active job = %v, want ErrConflict", err)
	}
	unchanged("destFolder change")
	active, hookErr = false, errors.New("boom")
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "Films", Path: other}); err == nil || !strings.Contains(err.Error(), "boom") || errors.Is(err, ErrConflict) {
		t.Fatalf("path change with a failing ActiveJobs = %v", err)
	}
	unchanged("ActiveJobs error")
	hookErr = nil
	up, err := st.Update(ctx, src.ID, SourceInput{Name: "Films", Path: other, DestFolder: "films"})
	if err != nil || up.Path != other || up.DestFolder != "films" || asked != 4 {
		t.Fatalf("change without jobs = %+v, %v (ActiveJobs calls %d)", up, err, asked)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"a.mkv": "a"})
	src := createSource(t, st, "S", root)
	mustScan(t, NewScanner(st, ScannerOptions{}), src.ID)

	unlock, err := st.LockSource(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, src.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete while locked = %v", err)
	}
	unlock()
	if err := st.Delete(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, src.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v", err)
	}
	if n := len(allRows(t, st, src.ID)); n != 0 {
		t.Fatalf("%d catalog rows left", n)
	}
	if err := st.Delete(ctx, src.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	// Ids are never reused (queued jobs and hardlink group ids name sources by id).
	if again := createSource(t, st, "S", root); again.ID == src.ID {
		t.Fatalf("new source reused id %d", src.ID)
	}
}

// A sync executes its plan without the source's lock, so Delete also asks StoreOptions.ActiveJobs
// (while holding the lock) and refuses while the source has queued or running jobs.
func TestDeleteRefusedWhileJobsActive(t *testing.T) {
	ctx := context.Background()
	var (
		st      *Store
		active  = true
		hookErr error
		asked   []int64
	)
	st = newStore(t, StoreOptions{ActiveJobs: func(_ context.Context, id int64) (bool, error) {
		asked = append(asked, id)
		if !st.locks.held(id) {
			t.Errorf("ActiveJobs(%d) called without the source's lock", id)
		}
		return active, hookErr
	}})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"a.mkv": "a"})
	src := createSource(t, st, "S", root)
	mustScan(t, NewScanner(st, ScannerOptions{}), src.ID)
	kept := func(what string) {
		t.Helper()
		if _, err := st.Get(ctx, src.ID); err != nil {
			t.Fatalf("%s: source gone: %v", what, err)
		}
		if n := len(liveRows(t, st, src.ID)); n != 1 {
			t.Fatalf("%s: %d catalog rows left, want 1", what, n)
		}
		if st.locks.held(src.ID) {
			t.Fatalf("%s: the source's lock is still held", what)
		}
	}

	if err := st.Delete(ctx, src.ID); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "jobs") {
		t.Fatalf("delete with an active job = %v, want ErrConflict", err)
	}
	kept("active job")

	active, hookErr = false, errors.New("boom")
	if err := st.Delete(ctx, src.ID); err == nil || !strings.Contains(err.Error(), "boom") || errors.Is(err, ErrConflict) {
		t.Fatalf("delete with a failing ActiveJobs = %v", err)
	}
	kept("ActiveJobs error")

	// While a scan holds the lock, Delete refuses without asking.
	unlock, err := st.LockSource(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, src.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete while locked = %v", err)
	}
	unlock()

	hookErr = nil
	if err := st.Delete(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, src.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v", err)
	}
	if len(asked) != 3 || asked[0] != src.ID || asked[1] != src.ID || asked[2] != src.ID {
		t.Fatalf("ActiveJobs asked for %v, want source %d three times", asked, src.ID)
	}
}
