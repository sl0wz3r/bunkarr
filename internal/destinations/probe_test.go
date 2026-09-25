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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// perLookup returns an lstat that numbers inodes per lookup, as a CIFS client mounted with
// noserverino does (Unraid's Unassigned Devices mounts shares so): every call returns a new inode
// number; with nlink1 the link count is 1 (attributes from a directory listing).
func perLookup(nlink1 bool) lstatFunc {
	var next atomic.Uint64
	next.Store(1 << 40)
	return func(root *os.Root, rel string) (filecopy.Stat, error) {
		st, err := filecopy.Lstat(root, rel)
		if err == nil {
			st.Ino = next.Add(1)
			if nlink1 {
				st.Nlink = 1
			}
		}
		return st, err
	}
}

// TestProbeInodeIdentity: link(2) works on all of these filesystems and the second name shares
// the content, but only where every name of the file stats with the same inode number and a link
// count of at least 2 do inode numbers identify files. Elsewhere the probe reports UnstableInodes,
// and the capabilities do not claim InodeIdentity (a sync used to take the old version it had
// hardlinked into retention for an unmanaged file there, and displace it).
func TestProbeInodeIdentity(t *testing.T) {
	tests := []struct {
		name     string
		lstat    lstatFunc
		unstable bool
	}{
		{"stable", filecopy.Lstat, false},
		{"numbered per lookup", perLookup(false), true},
		{"numbered per lookup, link count 1", perLookup(true), true},
		{"one number, link count 1", func(root *os.Root, rel string) (filecopy.Stat, error) {
			st, err := filecopy.Lstat(root, rel)
			st.Nlink = 1
			return st, err
		}, true},
	}
	now := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			caps, _, err := probeRoot(context.Background(), root, now, tt.lstat)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if !caps.Hardlinks || caps.UnstableInodes != tt.unstable || caps.ProbeVersion != filecopy.ProbeVersion ||
				!caps.Current() || caps.InodeIdentity() == tt.unstable {
				t.Errorf("probe = %+v, want hardlinks, unstableInodes %v", caps, tt.unstable)
			}
			if left := dirEntries(t, filepath.Join(dir, filecopy.ProbeDir)); len(left) != 0 {
				t.Errorf("probe left %v", left)
			}
		})
	}
}

// TestProbeCaseSpellings: on a case-insensitive destination "A" and "a" are one file, so they
// must stat with the same inode number; when they do not, inode numbers are unstable.
func TestProbeCaseSpellings(t *testing.T) {
	// insensitive makes a lookup that finds nothing retry in upper case (a case-insensitive
	// destination, also where the test's temp directory is case-sensitive).
	insensitive := func(lstat lstatFunc) lstatFunc {
		return func(root *os.Root, rel string) (filecopy.Stat, error) {
			st, err := lstat(root, rel)
			if errors.Is(err, fs.ErrNotExist) {
				st, err = lstat(root, strings.ToUpper(rel))
			}
			return st, err
		}
	}
	tests := []struct {
		name                  string
		lstat                 lstatFunc
		insensitive, unstable bool
	}{
		{"stable", insensitive(filecopy.Lstat), true, false},
		{"numbered per lookup", insensitive(perLookup(false)), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			ins, unstable, err := probeCase(root, tt.lstat)
			if err != nil || ins != tt.insensitive || unstable != tt.unstable {
				t.Errorf("probeCase = %v, %v, %v; want %v, %v", ins, unstable, err, tt.insensitive, tt.unstable)
			}
		})
	}
	// Case-sensitive: nothing to compare.
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	sensitive := func(root *os.Root, rel string) (filecopy.Stat, error) {
		if rel == "a" {
			return filecopy.Stat{}, &fs.PathError{Op: "lstat", Path: rel, Err: fs.ErrNotExist}
		}
		return perLookup(false)(root, rel)
	}
	if ins, unstable, err := probeCase(root, sensitive); err != nil || ins || unstable {
		t.Errorf("probeCase(case-sensitive) = %v, %v, %v", ins, unstable, err)
	}
}

// TestRefreshStale: a destination whose capabilities come from an older probe (no probeVersion:
// it may claim hardlinks on a share whose inode numbers change per lookup) is probed again by
// Handle.RefreshStale, which stores the result; current capabilities are left alone. Handle.Lstat
// stats through Options.Lstat.
func TestRefreshStale(t *testing.T) {
	later := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	f := newFixture(t, Options{Lstat: perLookup(true)})
	d := f.create(t, "unas")
	if !d.Capabilities.UnstableInodes || !d.Capabilities.Current() || !d.Capabilities.Hardlinks {
		t.Fatalf("created with %+v", d.Capabilities)
	}
	// What an older version stored: no probeVersion, no unstableInodes.
	old := d.Capabilities
	old.UnstableInodes, old.ProbeVersion = false, 0
	raw, _ := json.Marshal(old)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	delete(m, "unstableInodes")
	delete(m, "probeVersion")
	raw, _ = json.Marshal(m)
	err := f.db.Write(f.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, `UPDATE destinations SET capabilities = ? WHERE id = ?`, string(raw), d.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.store.now = func() time.Time { return later }

	h, err := f.store.Open(f.ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Capabilities.Current() || h.Capabilities.InodeIdentity() {
		t.Fatalf("stale capabilities %+v are current", h.Capabilities)
	}
	refreshed, _, err := h.RefreshStale(f.ctx)
	if err != nil || !refreshed {
		t.Fatalf("RefreshStale = %v, %v", refreshed, err)
	}
	got, _ := f.store.Get(f.ctx, d.ID)
	for _, c := range []Capabilities{h.Capabilities, got.Capabilities} {
		if !c.Current() || !c.UnstableInodes || c.InodeIdentity() || !c.CheckedAt.Equal(later) {
			t.Errorf("after RefreshStale: %+v", c)
		}
	}
	if refreshed, _, err := h.RefreshStale(f.ctx); err != nil || refreshed {
		t.Errorf("RefreshStale of current capabilities = %v, %v", refreshed, err)
	}

	if err := h.Root.WriteFile("x", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	a, errA := h.Lstat("x")
	b, errB := h.Lstat("x")
	if errA != nil || errB != nil || a.Ino == b.Ino {
		t.Errorf("Handle.Lstat does not stat through Options.Lstat: %+v %v, %+v %v", a, errA, b, errB)
	}

	res, err := f.store.Test(f.ctx, "", d.ID)
	if err != nil || !res.OK {
		t.Fatalf("Test = %+v, %v", res, err)
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "inode numbers change") }) {
		t.Errorf("Test warnings %q do not explain the unstable inode numbers", res.Warnings)
	}
}

// TestCheckIdentity: every job that writes checks the destination's inode numbers again
// (Handle.CheckIdentity), because what the probe found does not last (a share remounted with
// noserverino, or a CIFS client that turns server inode numbers off at run time). A check that
// finds what is stored stores nothing; a changed finding is stored (only UnstableInodes changes)
// and the handle's capabilities take it; a check that fails leaves the stored capabilities alone
// and makes the handle distrust inode numbers for its job. Capabilities of an older probe are
// probed again as a whole.
func TestCheckIdentity(t *testing.T) {
	later := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	f := newFixture(t, Options{})
	d := f.create(t, "unas")
	if !d.Capabilities.InodeIdentity() || !d.Capabilities.Hardlinks {
		t.Fatalf("created with %+v", d.Capabilities)
	}
	f.store.now = func() time.Time { return later }
	check := func(lstat lstatFunc) (IdentityCheck, Capabilities, error) {
		t.Helper()
		f.store.lstat = lstat
		h, err := f.store.Open(f.ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		c, err := h.CheckIdentity(f.ctx)
		return c, h.Capabilities, err
	}
	stored := func() Destination {
		t.Helper()
		got, err := f.store.Get(f.ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	probeLeft := func() {
		t.Helper()
		if left := dirEntries(t, filepath.Join(d.Target, filecopy.ProbeDir)); len(left) != 0 {
			t.Errorf("the check left %v", left)
		}
	}

	// Unchanged: nothing stored.
	c, caps, err := check(filecopy.Lstat)
	if err != nil || c.Probed || c.Changed || caps != d.Capabilities {
		t.Fatalf("stable: %+v, %+v, %v", c, caps, err)
	}
	if got := stored(); got.Capabilities != d.Capabilities || !got.UpdatedAt.Equal(d.UpdatedAt) {
		t.Fatalf("an unchanged check stored %+v at %v", got.Capabilities, got.UpdatedAt)
	}
	probeLeft()

	// Numbered per lookup now (hardlinked names show other numbers): stored.
	for _, nlink1 := range []bool{false, true} {
		f.store.lstat = filecopy.Lstat
		if err := f.store.setUnstableInodes(f.ctx, d.ID, false); err != nil {
			t.Fatal(err)
		}
		c, caps, err = check(perLookup(nlink1))
		want := d.Capabilities
		want.UnstableInodes = true
		if err != nil || c.Probed || !c.Changed || c.Stored != d.Capabilities || caps != want || caps.InodeIdentity() {
			t.Fatalf("per lookup (link count 1: %v): %+v, %+v, %v", nlink1, c, caps, err)
		}
		if got := stored(); got.Capabilities != want || !got.UpdatedAt.Equal(later) {
			t.Fatalf("per lookup (link count 1: %v): stored %+v at %v", nlink1, got.Capabilities, got.UpdatedAt)
		}
		probeLeft()
	}

	// Stable again (remounted with serverino): stored.
	c, caps, err = check(filecopy.Lstat)
	if err != nil || !c.Changed || caps != d.Capabilities || !caps.InodeIdentity() || stored().Capabilities != d.Capabilities {
		t.Fatalf("stable again: %+v, %+v, %v; stored %+v", c, caps, err, stored().Capabilities)
	}

	// The check cannot run: the job distrusts inode numbers, nothing is stored.
	probe := filepath.Join(d.Target, filecopy.ProbeDir)
	if err := os.RemoveAll(probe); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	before := stored()
	c, caps, err = check(perLookup(false))
	if err == nil || c.Changed || !caps.UnstableInodes || caps.InodeIdentity() {
		t.Fatalf("blocked: %+v, %+v, %v", c, caps, err)
	}
	if got := stored(); got.Capabilities != before.Capabilities || !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("a failed check stored %+v", got.Capabilities)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}

	// Capabilities of an older probe: the whole probe runs.
	old := d.Capabilities
	old.ProbeVersion, old.CheckedAt = 0, d.Capabilities.CheckedAt.Add(-time.Hour)
	raw, _ := json.Marshal(old)
	err = f.db.Write(f.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, `UPDATE destinations SET capabilities = ? WHERE id = ?`, string(raw), d.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	c, caps, err = check(perLookup(false))
	if err != nil || !c.Probed || !c.Changed || !caps.Current() || !caps.UnstableInodes || !caps.CheckedAt.Equal(later) {
		t.Fatalf("stale: %+v, %+v, %v", c, caps, err)
	}
	if got := stored(); got.Capabilities != caps {
		t.Fatalf("stale: stored %+v, want %+v", got.Capabilities, caps)
	}
	probeLeft()
}

// TestCheckIdentityFirst: a check deferred with CheckIdentityFirst runs once, the first time the
// job asks InodeIdentity, and hands its outcome to the job; until then nothing is written. A
// cancelled check answers with the error and leaves the job distrusting inode numbers.
func TestCheckIdentityFirst(t *testing.T) {
	f := newFixture(t, Options{})
	d := f.create(t, "unas")
	probe := filepath.Join(d.Target, filecopy.ProbeDir)
	f.store.lstat = perLookup(false)
	h, err := f.store.Open(f.ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var reports []IdentityCheck
	h.CheckIdentityFirst(func(c IdentityCheck, err error) {
		if err != nil {
			t.Errorf("check: %v", err)
		}
		reports = append(reports, c)
	})
	before, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(probe); len(reports) != 0 || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the check ran before it was needed: %+v", reports)
	}
	for range 2 {
		if id, err := h.InodeIdentity(f.ctx); err != nil || id {
			t.Fatalf("InodeIdentity = %v, %v", id, err)
		}
	}
	if len(reports) != 1 || !reports[0].Changed || !h.Capabilities.UnstableInodes {
		t.Fatalf("reports %+v, capabilities %+v", reports, h.Capabilities)
	}

	f.store.lstat = filecopy.Lstat
	h2, err := f.store.Open(f.ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	h2.CheckIdentityFirst(func(IdentityCheck, error) { t.Error("a cancelled check was reported") })
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := h2.InodeIdentity(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled InodeIdentity: %v", err)
	}
	if id, err := h2.InodeIdentity(f.ctx); err != nil || id {
		t.Fatalf("after a cancelled check: InodeIdentity = %v, %v (want distrust)", id, err)
	}
	if got, _ := f.store.Get(f.ctx, d.ID); !got.Capabilities.UnstableInodes {
		t.Fatalf("the cancelled check stored %+v", got.Capabilities)
	}
}
