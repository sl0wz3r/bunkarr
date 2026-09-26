package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pathsFixture is a scanned source: Movies/A and Movies/B with a file each, a sidecar, and TV.
func pathsFixture(t *testing.T) (*Store, *Scanner, Source, string) {
	t.Helper()
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	sc.estaleWait = 10 * time.Millisecond
	root := tempDir(t)
	writeFiles(t, root, map[string]string{
		"Movies/A/a.mkv": "a", "Movies/A/a.srt": "s", "Movies/B/b.mkv": "b", "TV/x.mkv": "x",
	})
	src := createSource(t, st, "S", root)
	mustScan(t, sc, src.ID)
	return st, sc, src, root
}

func mustScanPaths(t *testing.T, sc *Scanner, id int64, paths ...string) PathsResult {
	t.Helper()
	r, err := sc.ScanPaths(context.Background(), id, paths, nil)
	if err != nil {
		t.Fatalf("ScanPaths(%v): %v", paths, err)
	}
	return r
}

func targetStates(r PathsResult) map[string]TargetState {
	out := map[string]TargetState{}
	for _, tg := range r.Targets {
		out[tg.Path] = tg.State
	}
	return out
}

func TestCanonicalTargets(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
		bad  bool
	}{
		{in: []string{"b", "a", "a", "a/x", "a b/c"}, want: []string{"a", "a b/c", "b"}},
		{in: []string{".hack SIGN (2002)", "Movie..Name/x"}, want: []string{".hack SIGN (2002)", "Movie..Name/x"}},
		{in: []string{"x/../y"}, bad: true},
		{in: []string{"/abs"}, bad: true},
		{in: []string{"."}, bad: true},
		{in: []string{"./x"}, bad: true},
	} {
		got, err := CanonicalTargets(tc.in)
		if tc.bad {
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Errorf("CanonicalTargets(%q) = %v, %v; want a validation error", tc.in, got, err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("CanonicalTargets(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestScanPathsUpdatesOnlyTheTargets(t *testing.T) {
	ctx := context.Background()
	st, sc, src, root := pathsFixture(t)
	before := allRows(t, st, src.ID)
	srcBefore, _ := st.Get(ctx, src.ID)
	var seqBefore int64
	if err := st.db.Reader().QueryRow(`SELECT scan_seq FROM sources WHERE id = ?`, src.ID).Scan(&seqBefore); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond) // a new scan stamp
	writeFiles(t, root, map[string]string{"Movies/A/a.mkv": "a-1080p", "Movies/A/new.mkv": "n", "TV/y.mkv": "y"})
	for _, p := range []string{"Movies/A/a.srt", "Movies/B/b.mkv"} {
		if err := os.Remove(filepath.Join(root, p)); err != nil {
			t.Fatal(err)
		}
	}
	state := treeState(t, root)

	r := mustScanPaths(t, sc, src.ID, "Movies/A")
	if got := targetStates(r); got["Movies/A"] != TargetScanned || len(got) != 1 {
		t.Fatalf("targets = %+v", r.Targets)
	}
	if r.Files != 2 || r.Added != 1 || r.Changed != 1 || r.Deleted != 1 || r.WarningCount != 0 {
		t.Fatalf("result = %+v", r.ScanResult)
	}
	rows := allRows(t, st, src.ID)
	if rows["Movies/A/a.srt"].DeletedAt == nil || rows["Movies/A/a.mkv"].Size != int64(len("a-1080p")) || rows["Movies/A/new.mkv"].DeletedAt != nil {
		t.Fatalf("target rows = %+v", rows)
	}
	// Outside the target nothing was read or changed: B's deleted file and TV's new one are the
	// next full scan's business.
	if _, ok := rows["TV/y.mkv"]; ok {
		t.Fatal("a file outside the target was cataloged")
	}
	for _, rel := range []string{"Movies/B/b.mkv", "TV/x.mkv"} {
		if rows[rel].DeletedAt != nil || !rows[rel].LastSeenAt.Equal(before[rel].LastSeenAt) {
			t.Fatalf("%s changed: %+v -> %+v", rel, before[rel], rows[rel])
		}
	}
	var seq int64
	if err := st.db.Reader().QueryRow(`SELECT scan_seq FROM sources WHERE id = ?`, src.ID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	after, _ := st.Get(ctx, src.ID)
	if seq != seqBefore+1 || after.Stats.Files != 4 || !after.LastScanAt.Equal(*srcBefore.LastScanAt) {
		t.Fatalf("source after = seq %d stats %+v lastScanAt %v (before %v)", seq, after.Stats, after.LastScanAt, srcBefore.LastScanAt)
	}
	// The scan never touched the source (S1).
	if got := treeState(t, root); len(got) != len(state) {
		t.Fatal("the source tree changed")
	}

	// A file target updates exactly that file.
	writeFiles(t, root, map[string]string{"Movies/A/new.mkv": "n2", "Movies/A/other.mkv": "o"})
	r = mustScanPaths(t, sc, src.ID, "Movies/A/new.mkv")
	rows = allRows(t, st, src.ID)
	if r.Files != 1 || rows["Movies/A/new.mkv"].Size != 2 {
		t.Fatalf("file target = %+v rows %+v", r.ScanResult, rows)
	}
	if _, ok := rows["Movies/A/other.mkv"]; ok {
		t.Fatal("a file target cataloged its neighbor")
	}
}

func TestScanPathsGoneTarget(t *testing.T) {
	st, sc, src, root := pathsFixture(t)
	if err := os.RemoveAll(filepath.Join(root, "Movies/A")); err != nil {
		t.Fatal(err)
	}
	r := mustScanPaths(t, sc, src.ID, "Movies/A", "Movies/Z")
	if got := targetStates(r); got["Movies/A"] != TargetGone || got["Movies/Z"] != TargetGone || r.Deleted != 2 {
		t.Fatalf("result = %+v %+v", r.Targets, r.ScanResult)
	}
	live := liveRows(t, st, src.ID)
	if _, ok := live["Movies/A/a.mkv"]; ok || len(live) != 2 {
		t.Fatalf("live = %v", keys(live))
	}
}

func TestScanPathsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, root string) []string // returns the targets
		state  TargetState
		reason string
	}{
		{"missing parent", func(t *testing.T, root string) []string {
			if err := os.RemoveAll(filepath.Join(root, "Movies")); err != nil {
				t.Fatal(err)
			}
			return []string{"Movies/A"}
		}, TargetDropped, "does not exist"},
		{"empty parent", func(t *testing.T, root string) []string {
			// Movies looks like an unmounted share: no entries, while the catalog lists B's file.
			if err := os.RemoveAll(filepath.Join(root, "Movies")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "Movies"), 0o755); err != nil {
				t.Fatal(err)
			}
			return []string{"Movies/A"}
		}, TargetRefused, "is empty while the catalog lists 1 other files"},
		{"parent is a file", func(t *testing.T, root string) []string {
			if err := os.RemoveAll(filepath.Join(root, "Movies")); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, root, map[string]string{"Movies": "x"})
			return []string{"Movies/A"}
		}, TargetDropped, "is not a folder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, sc, src, root := pathsFixture(t)
			before := allRows(t, st, src.ID)
			targets := tc.setup(t, root)
			r := mustScanPaths(t, sc, src.ID, targets...)
			if len(r.Targets) != 1 || r.Targets[0].State != tc.state || !strings.Contains(r.Targets[0].Reason, tc.reason) {
				t.Fatalf("targets = %+v", r.Targets)
			}
			if r.WarningCount != 1 || r.Current() != nil {
				t.Fatalf("result = %+v", r.ScanResult)
			}
			catalogUnchanged(t, st, src.ID, before)
		})
	}

	// An empty parent whose catalog rows are all inside the target: the target is gone.
	st, sc, src, root := pathsFixture(t)
	if err := os.RemoveAll(filepath.Join(root, "TV")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "TV"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{"TV/Show/e1.mkv": "e"})
	mustScan(t, sc, src.ID)
	if err := os.RemoveAll(filepath.Join(root, "TV/Show")); err != nil {
		t.Fatal(err)
	}
	r := mustScanPaths(t, sc, src.ID, "TV/Show")
	if r.Targets[0].State != TargetGone || r.Deleted != 1 {
		t.Fatalf("result = %+v %+v", r.Targets, r.ScanResult)
	}
	if _, ok := liveRows(t, st, src.ID)["TV/Show/e1.mkv"]; ok {
		t.Fatal("row not marked deleted")
	}
}

func TestScanPathsErrorKeepsRows(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	st, sc, src, root := pathsFixture(t)
	writeFiles(t, root, map[string]string{"Movies/A/locked/x.mkv": "x"})
	mustScan(t, sc, src.ID)
	if err := os.Remove(filepath.Join(root, "Movies/A/a.srt")); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, "Movies/A/locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	r := mustScanPaths(t, sc, src.ID, "Movies/A")
	live := liveRows(t, st, src.ID)
	if _, ok := live["Movies/A/locked/x.mkv"]; !ok || r.WarningCount != 1 || r.Deleted != 1 {
		t.Fatalf("result = %+v, live %v", r.ScanResult, keys(live))
	}

	// The target folder itself unreadable: dropped, its rows kept.
	before := allRows(t, st, src.ID)
	if err := os.Chmod(filepath.Join(root, "Movies/A"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "Movies/A"), 0o755) })
	r = mustScanPaths(t, sc, src.ID, "Movies/A")
	if r.Targets[0].State != TargetDropped {
		t.Fatalf("targets = %+v", r.Targets)
	}
	catalogUnchanged(t, st, src.ID, before)
}

// TestScanPathsNeverFollowsWhatAFullScanSkips: a symlinked ancestor, an excluded ancestor and an
// ancestor that is a destination root leave the catalog without rows under them, exactly like a
// full scan; the target itself follows the full scan's rules.
func TestScanPathsNeverFollowsWhatAFullScanSkips(t *testing.T) {
	st := newStore(t, StoreOptions{})
	base := tempDir(t)
	root := filepath.Join(base, "src")
	outside := filepath.Join(base, "outside")
	writeFiles(t, root, map[string]string{"a.mkv": "a", "Extras/X/e.mkv": "e", "backup/media/X/b.mkv": "b"})
	writeFiles(t, outside, map[string]string{"X/secret.mkv": "s"})
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "X"), filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	forbidden := []string{filepath.Join(root, "backup")}
	sc := NewScanner(st, ScannerOptions{ForbiddenRoots: func(context.Context) ([]string, error) { return forbidden, nil }})
	src, err := st.Create(context.Background(), SourceInput{Name: "S", Path: root, Exclude: []string{"/Extras/"}})
	if err != nil {
		t.Fatal(err)
	}
	full := mustScan(t, sc, src.ID)
	want := keys(liveRows(t, st, src.ID))
	slices.Sort(want)
	if !slices.Equal(want, []string{"a.mkv"}) {
		t.Fatalf("full scan cataloged %v (%+v)", want, full)
	}
	for _, tc := range []struct {
		target string
		state  TargetState
		reason string
	}{
		{"link/X", TargetDropped, "symbolic link"},
		{"Extras/X", TargetDropped, "excluded"},
		{"backup/media/X", TargetDropped, "same directory as"},
		// The target itself: a symlink is skipped, like in a full scan.
		{"linkdir", TargetScanned, ""},
	} {
		r := mustScanPaths(t, sc, src.ID, tc.target)
		if r.Targets[0].State != tc.state || !strings.Contains(r.Targets[0].Reason, tc.reason) {
			t.Errorf("%s: targets = %+v", tc.target, r.Targets)
		}
		got := keys(liveRows(t, st, src.ID))
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: catalog = %v, a full scan gives %v", tc.target, got, want)
		}
	}
}

func TestScanPathsESTALERetriedOnce(t *testing.T) {
	st, sc, src, root := pathsFixture(t)
	writeFiles(t, root, map[string]string{"Movies/A/new.mkv": "n"})
	calls := 0
	sc.beforeReadDir = func(rel string) error {
		if rel == "Movies/A" {
			calls++
			if calls == 1 {
				return syscall.ESTALE
			}
		}
		return nil
	}
	rep := &recReporter{}
	r, err := sc.ScanPaths(context.Background(), src.ID, []string{"Movies/A"}, rep)
	if err != nil || calls != 2 || r.Added != 1 || !rep.hasLog("stale file handle") {
		t.Fatalf("ScanPaths = %+v, %v (calls %d)", r.ScanResult, err, calls)
	}
	if _, ok := liveRows(t, st, src.ID)["Movies/A/new.mkv"]; !ok {
		t.Fatal("the retry did not catalog the file")
	}

	// Twice in a row: fatal, and nothing was written.
	before := allRows(t, st, src.ID)
	writeFiles(t, root, map[string]string{"Movies/A/other.mkv": "o"})
	sc.beforeReadDir = func(rel string) error {
		if rel == "Movies/A" {
			return syscall.ESTALE
		}
		return nil
	}
	if _, err := sc.ScanPaths(context.Background(), src.ID, []string{"Movies/A"}, nil); !errors.Is(err, ErrScanRefused) || !errors.Is(err, syscall.ESTALE) {
		t.Fatalf("err = %v, want refused with ESTALE", err)
	}
	catalogUnchanged(t, st, src.ID, before)
}

func TestScanPathsHardlinkPartners(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"tv/S1/e1.mkv": "one", "tv/S1/e2.mkv": "two", "keep.mkv": "k"})
	link := func(from, to string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, to)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(root, from), filepath.Join(root, to)); err != nil {
			t.Fatal(err)
		}
	}
	link("tv/S1/e1.mkv", "downloads/e1.mkv")
	link("tv/S1/e2.mkv", "downloads/e2.mkv")
	src := createSource(t, st, "S", root)
	mustScan(t, sc, src.ID)
	rows := allRows(t, st, src.ID)
	if rows["downloads/e1.mkv"].HardlinkGroup == "" || rows["downloads/e2.mkv"].HardlinkGroup == "" {
		t.Fatalf("full scan groups = %+v", rows)
	}
	// e2's partner was replaced by another file since the last scan: its old numbers nominate it,
	// its current stats reject it.
	if err := os.Remove(filepath.Join(root, "downloads/e2.mkv")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{"downloads/e2.mkv": "two"})
	time.Sleep(5 * time.Millisecond)

	r := mustScanPaths(t, sc, src.ID, "tv/S1")
	if r.Groups != 1 || r.GroupedFiles != 2 {
		t.Fatalf("result = %+v", r.ScanResult)
	}
	rows = allRows(t, st, src.ID)
	var seq int64
	if err := st.db.Reader().QueryRowContext(ctx, `SELECT scan_seq FROM sources WHERE id = ?`, src.ID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	g := rows["tv/S1/e1.mkv"].HardlinkGroup
	if g == "" || g != rows["downloads/e1.mkv"].HardlinkGroup || !strings.HasPrefix(g, strings.Join([]string{itoa(src.ID), itoa(seq)}, ".")+":") {
		t.Fatalf("e1 group %q / %q (seq %d)", g, rows["downloads/e1.mkv"].HardlinkGroup, seq)
	}
	// The partner belongs to this scan: refreshed, never marked deleted.
	if !rows["downloads/e1.mkv"].LastSeenAt.Equal(r.StartedAt) {
		t.Fatalf("partner not refreshed: %+v (scan %v)", rows["downloads/e1.mkv"], r.StartedAt)
	}
	if rows["tv/S1/e2.mkv"].HardlinkGroup != "" {
		t.Fatalf("e2 grouped with a replaced partner: %+v", rows["tv/S1/e2.mkv"])
	}
	if rows["downloads/e2.mkv"].DeletedAt != nil {
		t.Fatal("a rejected partner outside the target was marked deleted")
	}

	// On FUSE the partner must also have the same content.
	sc.identityHook = func(id rootIdentity) rootIdentity { id.fuse = true; return id }
	sc.hashFile = func(root *os.Root, rel string, m Meta) (string, error) {
		if rel == "downloads/e1.mkv" {
			return "different", nil
		}
		return headTailHash(root, rel, m)
	}
	r = mustScanPaths(t, sc, src.ID, "tv/S1")
	if r.Groups != 0 || r.WarningCount != 1 || !strings.Contains(r.Warnings[0], "not their content") {
		t.Fatalf("FUSE result = %+v", r.ScanResult)
	}
	if rows := allRows(t, st, src.ID); rows["tv/S1/e1.mkv"].HardlinkGroup != "" || rows["downloads/e1.mkv"].HardlinkGroup != "" {
		t.Fatalf("FUSE groups = %+v", rows)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestScanPathsDefaultExcludes(t *testing.T) {
	st, sc, src, root := pathsFixture(t)
	writeFiles(t, root, map[string]string{"Movies/A/a.mkv.partial~": "p", "Movies/A/a.mkv.backup~": "b", "Movies/A/c.mkv": "c"})
	r := mustScanPaths(t, sc, src.ID, "Movies/A")
	live := liveRows(t, st, src.ID)
	if r.Excluded != 2 || len(live) != 5 {
		t.Fatalf("result = %+v live %v", r.ScanResult, keys(live))
	}
	for _, rel := range []string{"Movies/A/a.mkv.partial~", "Movies/A/a.mkv.backup~"} {
		if _, ok := live[rel]; ok {
			t.Fatalf("%s was cataloged", rel)
		}
	}
	if full := mustScan(t, sc, src.ID); full.Excluded != 2 {
		t.Fatalf("full scan = %+v", full)
	}
}

func TestScanPathsRootChecksAndLock(t *testing.T) {
	ctx := context.Background()
	st, sc, src, root := pathsFixture(t)
	if _, err := sc.ScanPathsLocked(ctx, src.ID, []string{"Movies/A"}, nil); err == nil || !strings.Contains(err.Error(), "without holding") {
		t.Fatalf("ScanPathsLocked without the lock = %v", err)
	}
	if _, err := sc.ScanPaths(ctx, src.ID, []string{"../x"}, nil); err == nil {
		t.Fatal("an invalid path was accepted")
	}
	if _, err := sc.ScanPaths(ctx, src.ID, nil, nil); err == nil {
		t.Fatal("no paths was accepted")
	}
	before := allRows(t, st, src.ID)
	if err := os.Rename(root, root+".gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.ScanPaths(ctx, src.ID, []string{"Movies/A"}, nil); !errors.Is(err, ErrScanRefused) {
		t.Fatalf("missing root = %v, want refused", err)
	}
	catalogUnchanged(t, st, src.ID, before)
	s, _ := st.Get(ctx, src.ID)
	if s.LastScanStatus == ScanStatusFailed {
		t.Fatal("a refused targeted scan recorded a failed full scan")
	}
}

// finds reports whether dir's filesystem finds the entry spelled have by the spelling want (a
// case- or normalization-insensitive filesystem such as APFS or an SMB share).
func finds(t *testing.T, dir, have, want string) bool {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(probe, have), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := os.Lstat(filepath.Join(probe, want))
	return err == nil
}

// A target spelled differently from the names on disk (an *arr's spelling on a case- or
// normalization-insensitive share) must not add rows under that spelling: they would be a second
// copy of the folder's files for the sync to transfer and later retain.
func TestScanPathsKeysRowsByTheNamesOnDisk(t *testing.T) {
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	const (
		nfc = "Amélie (2001)"  // é precomposed
		nfd = "Amélie (2001)" // e + combining acute
	)
	caseless, normless := finds(t, root, "the matrix (1999)", "The Matrix (1999)"), finds(t, root, nfc, nfd)
	if !caseless && !normless {
		t.Skip("the filesystem of the temporary directory is case- and normalization-sensitive")
	}
	writeFiles(t, root, map[string]string{
		"movies/the matrix (1999)/m.mkv": "m", "movies/" + nfc + "/a.mkv": "a", "movies/Heat (1995)/h.mkv": "h",
	})
	src := createSource(t, st, "S", root)
	mustScan(t, sc, src.ID)
	before := allRows(t, st, src.ID)
	time.Sleep(5 * time.Millisecond) // a new scan stamp

	// Another spelling of the last name is gone under that spelling (no rows are under it);
	// another spelling of an ancestor drops the target.
	var targets []string
	want := map[string]TargetState{}
	if caseless {
		targets = append(targets, "movies/The Matrix (1999)", "Movies/Heat (1995)", "movies/Heat (1995)/H.mkv")
		want["movies/The Matrix (1999)"], want["Movies/Heat (1995)"], want["movies/Heat (1995)/H.mkv"] = TargetGone, TargetDropped, TargetGone
	}
	if normless {
		targets = append(targets, "movies/"+nfd)
		want["movies/"+nfd] = TargetGone
	}
	r := mustScanPaths(t, sc, src.ID, targets...)
	for _, tg := range r.Targets {
		if tg.State != want[tg.Path] || tg.State == TargetDropped && !strings.Contains(tg.Reason, "exact name") {
			t.Errorf("target %q = %s (%s); want %s for its spelling", tg.Path, tg.State, tg.Reason, want[tg.Path])
		}
	}
	if len(r.Targets) != len(targets) || r.Files != 0 || r.Added != 0 || r.Deleted != 0 {
		t.Fatalf("result = %+v %+v", r.Targets, r.ScanResult)
	}
	after := allRows(t, st, src.ID)
	if len(after) != len(before) {
		t.Fatalf("rows = %v; want only %v", keys(after), keys(before))
	}
	for rel, f := range before {
		if a := after[rel]; a.DeletedAt != nil || !a.LastSeenAt.Equal(f.LastSeenAt) {
			t.Errorf("%s changed: %+v -> %+v", rel, f, a)
		}
	}
	if got, err := st.LiveUnder(context.Background(), src.ID, targets); err != nil || len(got) != 0 {
		t.Fatalf("LiveUnder(%q) = %+v, %v; want none", targets, got, err)
	}

	// The spelling on disk is scanned as before.
	r = mustScanPaths(t, sc, src.ID, "movies/the matrix (1999)", "movies/"+nfc)
	if got := targetStates(r); got["movies/the matrix (1999)"] != TargetScanned || got["movies/"+nfc] != TargetScanned || r.Files != 2 {
		t.Fatalf("exact spelling: %+v %+v", r.Targets, r.ScanResult)
	}
}

// A hardlink partner recorded under a spelling the disk no longer lists (a case-only rename on a
// case-insensitive share) is not refreshed or grouped under that old spelling.
func TestScanPathsPartnerUnderAnotherSpelling(t *testing.T) {
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	if !finds(t, root, "downloads", "Downloads") {
		t.Skip("the filesystem of the temporary directory is case-sensitive")
	}
	writeFiles(t, root, map[string]string{"tv/S1/e1.mkv": "one"})
	if err := os.Mkdir(filepath.Join(root, "downloads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "tv/S1/e1.mkv"), filepath.Join(root, "downloads/e1.mkv")); err != nil {
		t.Fatal(err)
	}
	src := createSource(t, st, "S", root)
	mustScan(t, sc, src.ID)
	if rows := allRows(t, st, src.ID); rows["downloads/e1.mkv"].HardlinkGroup == "" {
		t.Fatalf("full scan groups = %+v", rows)
	}
	if err := os.Rename(filepath.Join(root, "downloads"), filepath.Join(root, "Downloads")); err != nil {
		t.Fatal(err)
	}
	before := allRows(t, st, src.ID)["downloads/e1.mkv"]
	time.Sleep(5 * time.Millisecond)

	r := mustScanPaths(t, sc, src.ID, "tv/S1")
	if r.Groups != 0 {
		t.Fatalf("result = %+v", r.ScanResult)
	}
	rows := allRows(t, st, src.ID)
	if p := rows["downloads/e1.mkv"]; !p.LastSeenAt.Equal(before.LastSeenAt) || p.DeletedAt != nil {
		t.Fatalf("old spelling of the partner: %+v (before %+v)", p, before)
	}
	if _, ok := rows["Downloads/e1.mkv"]; ok {
		t.Fatal("the partner was cataloged outside the target")
	}
	if rows["tv/S1/e1.mkv"].HardlinkGroup != "" {
		t.Fatalf("e1 grouped with a partner under an old spelling: %+v", rows["tv/S1/e1.mkv"])
	}
}

// A case-only rename on a case-insensitive source (Radarr renaming "Movie Of X" to "Movie of X"):
// the old spelling still resolves by lookup but is no longer listed. Its rows are marked deleted,
// as the full scan would, so a targeted sync plans both spellings and can pair the move instead of
// copying every file again while the old rows stay live.
func TestScanPathsCaseOnlyRenameMarksTheOldSpellingGone(t *testing.T) {
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	if !finds(t, root, "movie of x (2000)", "Movie Of X (2000)") {
		t.Skip("the filesystem of the temporary directory is case-sensitive")
	}
	const (
		oldDir = "movies/Movie Of X (2000)"
		newDir = "movies/Movie of X (2000)"
	)
	writeFiles(t, root, map[string]string{oldDir + "/m.mkv": "m", "movies/Heat (1995)/h.mkv": "h"})
	src := createSource(t, st, "S", root)
	mustScan(t, sc, src.ID)
	if err := os.Rename(filepath.Join(root, oldDir), filepath.Join(root, newDir)); err != nil {
		t.Fatal(err)
	}
	// A file renamed by case only, targeted by its old spelling.
	if err := os.Rename(filepath.Join(root, "movies/Heat (1995)/h.mkv"), filepath.Join(root, "movies/Heat (1995)/H.mkv")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	r := mustScanPaths(t, sc, src.ID, oldDir, newDir, "movies/Heat (1995)/h.mkv")
	got := targetStates(r)
	if got[oldDir] != TargetGone || got[newDir] != TargetScanned || got["movies/Heat (1995)/h.mkv"] != TargetGone {
		t.Fatalf("targets = %+v", r.Targets)
	}
	if r.Added != 1 || r.Deleted != 2 {
		t.Fatalf("result = %+v", r.ScanResult)
	}
	live := liveRows(t, st, src.ID)
	if _, ok := live[newDir+"/m.mkv"]; !ok || len(live) != 1 {
		t.Fatalf("live = %v; want only %s/m.mkv (H.mkv is not a target)", keys(live), newDir)
	}
	if cur := r.Current(); !slices.Contains(cur, oldDir) || !slices.Contains(cur, newDir) {
		t.Fatalf("Current() = %v; want both spellings planned", cur)
	}
	if left, err := st.LiveUnder(context.Background(), src.ID, []string{oldDir}); err != nil || len(left) != 0 {
		t.Fatalf("LiveUnder(old spelling) = %+v, %v; want none", left, err)
	}
}

// A leaf created after this scan attempt listed its folder (an *arr writing while the targets are
// walked) is found by lookup but missing from the cached listing: the folder is listed again
// before the target is taken for another spelling and its rows marked deleted.
func TestScanPathsRelistsTheParentBeforeAGoneSpelling(t *testing.T) {
	st, sc, src, root := pathsFixture(t)
	ctx := context.Background()
	in, err := st.loadScanSource(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	s := sc.newScan(in, nopReporter{}, time.Now().UTC())
	if err := s.openRoot(); err != nil {
		t.Fatal(err)
	}
	defer s.root.Close()
	if err := s.loadForbidden(ctx); err != nil {
		t.Fatal(err)
	}
	if tg, err := s.walkTarget(ctx, "Movies/A"); err != nil || tg.State != TargetScanned {
		t.Fatalf("walk Movies/A = %+v, %v", tg, err)
	}
	writeFiles(t, root, map[string]string{"Movies/C/c.mkv": "c"})
	if tg, err := s.walkTarget(ctx, "Movies/C"); err != nil || tg.State != TargetScanned {
		t.Fatalf("walk Movies/C after the listing of Movies was cached = %+v, %v; want scanned", tg, err)
	}
}
