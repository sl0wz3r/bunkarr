package destinations

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// TestProbeEnforcesModes: a local filesystem stores the permission bits a file is given, so the
// probe reports EnforcesModes (phase2-3.md S17); the result is stored with the destination.
// Capabilities stored by a probe older than the field read false.
func TestProbeEnforcesModes(t *testing.T) {
	target := t.TempDir()
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	caps, _, err := probeRoot(context.Background(), root, time.Now(), filecopy.Lstat)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.EnforcesModes {
		t.Fatalf("a local filesystem does not enforce modes: %+v", caps)
	}
	if es, err := os.ReadDir(filepath.Join(target, filepath.FromSlash(filecopy.ProbeDir))); err != nil || len(es) != 0 {
		t.Fatalf("the probe left %v (%v)", es, err)
	}

	var old Capabilities
	if err := json.Unmarshal([]byte(`{"hardlinks":true,"fsType":"cifs","probeVersion":1}`), &old); err != nil || old.EnforcesModes {
		t.Fatalf("capabilities of an older probe: %+v, %v", old, err)
	}
}

// TestProbeModes: both modes read back on a local filesystem; a probe file that cannot be created
// is an error, never "enforced".
func TestProbeModes(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ok, err := probeModes(root)
	if err != nil || !ok {
		t.Fatalf("probeModes = %v, %v", ok, err)
	}
	if fi, err := root.Lstat("mode"); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("the probe file: %v, %v", fi, err)
	}
	if ok, err := probeModes(root); err == nil || ok {
		t.Fatalf("probeModes with its file already present = %v, %v", ok, err)
	}
}
