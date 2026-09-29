package enginerun

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// A folder of a source that becomes a symlink out of the source after the scan (S1, S28): rclone
// and restic open a source file by its absolute path, and the kernel follows a symlinked folder on
// it, so without the check through the source's root a planned file of the same name as one
// behind the link (the config directory's bunkarr.key here) would send that file off-site under
// the media file's name. The fakes read the source as the real binaries do (os.Lstat and
// os.ReadFile of the joined path).

// secretKey is the content of the file behind the link; no destination object or snapshot may
// ever hold it.
const secretKey = "SECRET bunkarr.key: every stored credential"

// escapeScenario writes the file behind the link (the config directory's bunkarr.key) and a
// source with a decoy of the same name in z/: once the plan is made, swap replaces z with a
// symlink to the config directory.
func escapeScenario(t *testing.T, kind engines.Kind) *harness {
	t.Helper()
	settings := &destinations.Settings{MaxChangeFiles: 1000, MaxChangePercent: 100}
	if kind == engines.Restic {
		settings.Restic = &destinations.ResticSettings{BatchFiles: 1}
	} else {
		settings.Rclone = &destinations.RcloneSettings{BatchFiles: 1}
	}
	h := newHarness(t, harnessOptions{kind: kind, settings: settings, plain: kind == engines.Rclone})
	if err := os.WriteFile(filepath.Join(h.configDir, "bunkarr.key"), []byte(secretKey), 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

// swap replaces the source folder dir with a symlink to the config directory.
func (h *harness) swap(dir string) {
	h.t.Helper()
	if err := os.RemoveAll(h.srcPath(dir)); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Symlink(h.configDir, h.srcPath(dir)); err != nil {
		h.t.Fatal(err)
	}
}

// assertNoSecret fails when any destination object or snapshot file holds the secret.
func (h *harness) assertNoSecret(t *testing.T) {
	t.Helper()
	if h.rclone != nil {
		for _, p := range h.rclone.paths("") {
			if o, _ := h.rclone.get(p); strings.Contains(string(o.data), secretKey) {
				t.Errorf("the object %s holds the file behind the symlink", p)
			}
		}
	}
	if h.restic != nil {
		for _, id := range h.restic.ids() {
			for p, o := range h.restic.snapshot(id).files {
				if strings.Contains(string(o.data), secretKey) {
					t.Errorf("snapshot %s holds the file behind the symlink as %s", short(id), p)
				}
			}
		}
	}
}

// TestSourceEscapeRcloneBatch: z/ becomes a symlink while an earlier batch runs (the job is long);
// the later batch's file fails its item and is never listed for rclone copy.
func TestSourceEscapeRcloneBatch(t *testing.T) {
	h := escapeScenario(t, engines.Rclone)
	h.writeSrc("a.mkv", content("a", 1000), 1)
	h.writeSrc("z/bunkarr.key", content("decoy", 700), 2)
	var once sync.Once
	h.rclone.beforeCopy = func(files []string) {
		if len(files) == 1 && files[0] == "a.mkv" {
			once.Do(func() { h.swap("z") })
		}
	}
	h.advance(time1h)
	st, j := h.mustSync(jobs.Params{})
	h.assertNoSecret(t)
	for _, c := range h.rclone.copies() {
		for _, f := range c.files {
			if f == "z/bunkarr.key" {
				t.Errorf("rclone copy was given %s: %+v", f, c)
			}
		}
	}
	if s, msg := itemStatus(h.items(j.ID), h.dp("z/bunkarr.key")); s != jobs.ItemFailed || !strings.Contains(msg, "inside its source folder") {
		t.Errorf("z/bunkarr.key: %s %q", s, msg)
	}
	if _, ok := h.live(h.dp("z/bunkarr.key")); ok || st.FilesCopied != 1 {
		t.Errorf("stats %+v, records %+v", st, h.records())
	}
}

// TestSourceEscapeRcloneCopyto: the same for a file uploaded alone with copyto (a repair whose
// damaged object is still at its path): z/ becomes a symlink after the batch's check, while the
// damaged object moves into retention, and the check before copyto fails the item.
func TestSourceEscapeRcloneCopyto(t *testing.T) {
	h := escapeScenario(t, engines.Rclone)
	h.writeSrc("z/bunkarr.key", content("decoy", 700), 2)
	h.mustSync(jobs.Params{})
	rec := h.mustLive(h.dp("z/bunkarr.key"))
	h.exec(`UPDATE destination_files SET state = 'missing' WHERE id = ?`, rec.ID)
	var once sync.Once
	faultinject.SetHook(func(name string) {
		if name == PointRcloneAfterIntent {
			once.Do(func() { h.swap("z") })
		}
	})
	defer faultinject.SetHook(nil)
	h.advance(time1h)
	_, j := h.mustSync(jobs.Params{})
	h.assertNoSecret(t)
	if len(h.callsOf(proc.Rclone, "copyto")) != 0 {
		t.Error("rclone copyto ran")
	}
	if s, msg := itemStatus(h.items(j.ID), h.dp("z/bunkarr.key")); s != jobs.ItemFailed || !strings.Contains(msg, "inside its source folder") {
		t.Errorf("z/bunkarr.key: %s %q\n%s", s, msg, h.rep.dump())
	}
}

// TestSourceEscapeResticBatch: the same on restic, for a file of the batch listed on its own (z/
// holds a file of a later batch, so it is not listed whole).
func TestSourceEscapeResticBatch(t *testing.T) {
	h := escapeScenario(t, engines.Restic)
	h.writeSrc("a.mkv", content("a", 1000), 1)
	h.writeSrc("z/bunkarr.key", content("decoy", 700), 2)
	h.writeSrc("z/other.mkv", content("o", 800), 3)
	var once sync.Once
	h.restic.beforeBackup = func(*enginetest.Call) { once.Do(func() { h.swap("z") }) }
	h.advance(time1h)
	_, j := h.mustSync(jobs.Params{})
	h.assertNoSecret(t)
	if s, msg := itemStatus(h.items(j.ID), h.dp("z/bunkarr.key")); s != jobs.ItemFailed || !strings.Contains(msg, "inside its source folder") {
		t.Errorf("z/bunkarr.key: %s %q", s, msg)
	}
	if _, ok := h.live(h.dp("z/bunkarr.key")); ok {
		t.Error("z/bunkarr.key got a record")
	}
}

// TestSourceEscapeResticIncluded: a file an earlier sync backed up is in every batch's include
// list (listed on its own when its folder also holds a pending item); once its folder is a
// symlink out of the source it is left out of the list, with a warning.
func TestSourceEscapeResticIncluded(t *testing.T) {
	h := escapeScenario(t, engines.Restic)
	h.writeSrc("a.mkv", content("a", 1000), 1)
	h.writeSrc("z/bunkarr.key", content("decoy", 700), 2)
	h.writeSrc("z/other.mkv", content("o", 800), 3)
	h.mustSync(jobs.Params{})
	if r := h.mustLive(h.dp("z/bunkarr.key")); r.State != syncer.StatePresent {
		t.Fatalf("z/bunkarr.key: %+v", r)
	}
	// Two new files (two batches) and an update in z/ (pending until the last batch).
	h.writeSrc("b.mkv", content("b", 900), 10)
	h.writeSrc("c.mkv", content("c", 950), 11)
	h.writeSrc("z/other.mkv", content("O", 820), 12)
	// z/ becomes a symlink once the first batch is recorded (that batch read the decoy).
	var once sync.Once
	faultinject.SetHook(func(name string) {
		if name == PointAfterRecord {
			once.Do(func() { h.swap("z") })
		}
	})
	defer faultinject.SetHook(nil)
	h.advance(time1h)
	h.mustSync(jobs.Params{})
	h.assertNoSecret(t)
	if !h.rep.has("leads outside the source folder") {
		t.Errorf("no warning for the path left out:\n%s", h.rep.dump())
	}
}
