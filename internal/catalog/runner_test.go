package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// runnerFixture has three sources: A and B enabled, C disabled.
func runnerFixture(t *testing.T) (*Store, *ScanRunner, [3]Source, string) {
	t.Helper()
	st := newStore(t, StoreOptions{})
	base := tempDir(t)
	var srcs [3]Source
	for i, name := range []string{"A", "B", "C"} {
		dir := filepath.Join(base, name)
		writeFiles(t, dir, map[string]string{"one.mkv": "1", "two.mkv": "22"})
		in := SourceInput{Name: name, Path: dir}
		if name == "C" {
			in.Enabled = boolPtr(false)
		}
		src, err := st.Create(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		srcs[i] = src
	}
	return st, NewScanRunner(NewScanner(st, ScannerOptions{})), srcs, base
}

func TestScanRunner(t *testing.T) {
	ctx := context.Background()
	st, r, srcs, base := runnerFixture(t)
	rep := &recReporter{}
	env := jobs.Env{Reporter: rep}

	res, err := r.Run(ctx, jobs.Job{ID: 1, Type: jobs.TypeScan}, env)
	if err != nil {
		t.Fatal(err)
	}
	stats, ok := res.Stats.(ScanJobStats)
	if !ok || stats.SourcesScanned != 2 || stats.Files != 4 || stats.Added != 4 || stats.Bytes != 6 || len(stats.Sources) != 2 {
		t.Fatalf("stats = %+v", res.Stats)
	}
	if res.Warnings != 0 || !strings.HasPrefix(res.Summary, "Scanned 2 sources: 4 files (6 B), 4 added") {
		t.Fatalf("result = %+v", res)
	}
	if b, err := json.Marshal(res.Stats); err != nil || !strings.Contains(string(b), `"sourcesScanned":2`) {
		t.Fatalf("stats JSON = %s, %v", b, err)
	}
	if c, _ := st.Get(ctx, srcs[2].ID); c.LastScanAt != nil {
		t.Fatal("disabled source scanned by a scan-all job")
	}
	if !rep.hasLog("scan finished") {
		t.Fatalf("logs = %v", rep.logs)
	}

	// Named sources are scanned even when disabled.
	res, err = r.Run(ctx, jobs.Job{ID: 2, Type: jobs.TypeScan, Params: jobs.Params{SourceIDs: []int64{srcs[2].ID}}}, env)
	if err != nil || res.Stats.(ScanJobStats).SourcesScanned != 1 || !strings.HasPrefix(res.Summary, "Scanned 1 source:") {
		t.Fatalf("named scan = %+v, %v", res, err)
	}

	// A refused source fails the job after the others were scanned.
	if err := os.RemoveAll(filepath.Join(base, "A")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, filepath.Join(base, "B"), map[string]string{"three.mkv": "3"})
	_, err = r.Run(ctx, jobs.Job{ID: 3, Type: jobs.TypeScan}, env)
	if !errors.Is(err, ErrScanRefused) || !strings.Contains(err.Error(), `"A"`) {
		t.Fatalf("err = %v", err)
	}
	if n := len(liveRows(t, st, srcs[1].ID)); n != 3 {
		t.Fatalf("B has %d live rows after the job; the scan of B did not run", n)
	}
	if a, _ := st.Get(ctx, srcs[0].ID); a.LastScanStatus != ScanStatusFailed {
		t.Fatalf("A status = %q", a.LastScanStatus)
	}
	if !rep.hasLog("scan failed") {
		t.Fatalf("logs = %v", rep.logs)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Run(cctx, jobs.Job{ID: 4, Type: jobs.TypeScan, Params: jobs.Params{SourceIDs: []int64{srcs[1].ID}}}, env); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled job = %v", err)
	}
	if _, err := r.Run(ctx, jobs.Job{ID: 5, Type: jobs.TypeScan, Params: jobs.Params{SourceIDs: []int64{999}}}, jobs.Env{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown source = %v", err)
	}
}

func TestScanRunnerNoSources(t *testing.T) {
	r := NewScanRunner(NewScanner(newStore(t, StoreOptions{}), ScannerOptions{}))
	res, err := r.Run(context.Background(), jobs.Job{ID: 1, Type: jobs.TypeScan}, jobs.Env{})
	if err != nil || res.Summary != "No enabled sources to scan" {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 30: "5.0 GiB", 3 << 40: "3.0 TiB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
