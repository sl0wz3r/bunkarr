package manifest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// versionDir is the absolute path of a recorded version.
func (e *testEnv) versionDir(v Version) string {
	return filepath.Join(e.target, filepath.FromSlash(v.Path))
}

func TestRunWritesAVersion(t *testing.T) {
	e := newEnv(t)
	e.recordAll()
	job := e.newJob(false)
	res, rep, err := e.run(job)
	if err != nil {
		t.Fatalf("export: %v\n%s", err, rep)
	}
	st := res.Stats.(Stats)
	vs := e.assertConverged(1)
	v := vs[0]
	if v.Path != Root+"/20260925T120000Z" || v.JobID != job.ID || v.Format != 1 || v.ItemCount != 7 || v.FileCount != 13 ||
		!v.CreatedAt.Equal(e.clock.Now()) || st.ManifestID != v.ID || st.Path != v.Path || st.Unchanged || st.Items != 7 || st.Files != 13 ||
		st.UnlocatedItems != 1 || st.StaleIntegrations != 0 {
		t.Fatalf("version %+v, stats %+v", v, st)
	}
	// The unlocated /movies-4k item is a warning (S20).
	if res.Warnings != 1 || !rep.warned("lie in no source") {
		t.Fatalf("warnings %d:\n%s", res.Warnings, rep)
	}
	names, _ := os.ReadDir(e.versionDir(v))
	var files []string
	for _, n := range names {
		files = append(files, n.Name())
	}
	if strings.Join(files, ",") != "SHA256SUMS,manifest.csv,manifest.json" {
		t.Fatalf("version files %v", files)
	}
	data, _ := os.ReadFile(filepath.Join(e.versionDir(v), JSONName))
	if Checksum(SHA256Hex(data)) != v.Checksum {
		t.Fatalf("checksum %s", v.Checksum)
	}
	m, _ := Parse(bytes.NewReader(data))
	if m.Job == nil || m.Job.ID != job.ID || !m.Job.QueuedAt.Equal(job.QueuedAt) || m.Scope.DestinationID != e.dest.ID {
		t.Fatalf("manifest job %+v scope %+v", m.Job, m.Scope)
	}
	if !strings.Contains(res.Summary, "Wrote the manifest") {
		t.Fatalf("summary %q", res.Summary)
	}
}

func TestRunUnchangedWritesNothing(t *testing.T) {
	e := newEnv(t)
	first, _ := e.runOK()
	e.clock.Advance(time.Hour)
	st, rep := e.runOK()
	if !st.Unchanged || st.ManifestID != first.ManifestID || st.DamagedFound != 0 {
		t.Fatalf("second run %+v\n%s", st, rep)
	}
	e.assertConverged(1)
	// A change of the library is a new version (the retention keeps the newest of the day).
	e.writeFile("movies/new.mkv", 10)
	e.scan()
	e.clock.Advance(time.Hour)
	st, _ = e.runOK()
	if st.Unchanged || st.ManifestID == first.ManifestID || st.VersionsPruned != 1 {
		t.Fatalf("after a change: %+v", st)
	}
	if vs := e.assertConverged(1); vs[0].ID != st.ManifestID {
		t.Fatalf("kept %+v", vs[0])
	}
}

func TestRunReplacesADamagedNewestVersion(t *testing.T) {
	for name, damage := range map[string]func(dir string) error{
		"manifest.json changed": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, JSONName), []byte(`{"format":"bunkarr-manifest"}`), 0o644)
		},
		"manifest.csv changed": func(dir string) error { return os.WriteFile(filepath.Join(dir, CSVName), []byte("x"), 0o644) },
		"SHA256SUMS missing":   func(dir string) error { return os.Remove(filepath.Join(dir, SumsName)) },
		"directory missing":    os.RemoveAll,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			first, _ := e.runOK()
			v := e.versions()[0]
			if err := damage(e.versionDir(v)); err != nil {
				t.Fatal(err)
			}
			e.clock.Advance(time.Hour)
			res, rep, err := e.run(e.newJob(false))
			if err != nil {
				t.Fatalf("export: %v\n%s", err, rep)
			}
			st := res.Stats.(Stats)
			if st.Unchanged || st.DamagedFound != 1 || st.ManifestID == first.ManifestID || !rep.warned("was damaged") {
				t.Fatalf("stats %+v\n%s", st, rep)
			}
			vs := e.versions()
			byID := map[int64]Version{}
			for _, x := range vs {
				byID[x.ID] = x
			}
			if byID[st.ManifestID].Integrity != IntegrityOK {
				t.Fatalf("new version %+v", byID[st.ManifestID])
			}
			old, kept := byID[first.ManifestID]
			switch {
			case name == "directory missing" && kept:
				t.Fatalf("the row of a lost version was kept: %+v", old)
			case name != "directory missing" && (!kept || old.Integrity != IntegrityDamaged):
				t.Fatalf("damaged version %+v (kept %v)", old, kept)
			}
		})
	}
}

func TestRunDryRun(t *testing.T) {
	e := newEnv(t)
	res, rep, err := e.run(e.newJob(true))
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, rep)
	}
	st := res.Stats.(Stats)
	if !st.DryRun || st.Unchanged || st.Items != 7 || st.Files != 13 || st.ManifestID != 0 || len(e.entries()) != 0 || len(e.versions()) != 0 {
		t.Fatalf("dry run %+v, entries %v", st, e.entries())
	}
	if !strings.HasPrefix(res.Summary, "Dry run:") || !strings.Contains(res.Summary, "a new version would be written") {
		t.Fatalf("summary %q", res.Summary)
	}
	e.runOK()
	res, _, _ = e.run(e.newJob(true))
	if st := res.Stats.(Stats); !st.Unchanged {
		t.Fatalf("dry run after an export %+v", st)
	}
	// A damaged newest version: the dry run reports it and marks nothing.
	v := e.versions()[0]
	if err := os.WriteFile(filepath.Join(e.versionDir(v), CSVName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, rep, _ = e.run(e.newJob(true))
	if st := res.Stats.(Stats); st.Unchanged || st.DamagedFound != 1 || !rep.warned("damaged") || e.versions()[0].Integrity != IntegrityOK {
		t.Fatalf("dry run with a damaged version %+v", st)
	}
}

func TestRunWarnsAboutStaleIntegrations(t *testing.T) {
	e := newEnv(t)
	e.runOK()
	e.clock.Advance(25 * time.Hour)
	res, rep, err := e.run(e.newJob(false))
	if err != nil {
		t.Fatal(err)
	}
	st := res.Stats.(Stats)
	// Going stale is a change (status and fresh are in the content hash).
	if st.StaleIntegrations != 2 || st.Unchanged || res.Warnings != 3 || !rep.warned(`index of "Radarr" is not fresh`) {
		t.Fatalf("stats %+v, warnings %d\n%s", st, res.Warnings, rep)
	}
}

func TestRunPreflightFailures(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name  string
		setup func()
		want  func(error) bool
	}{
		{"no destination", func() {}, func(err error) bool { return err != nil && strings.Contains(err.Error(), "destinationId") }},
		{"disabled", func() {
			off := false
			if _, err := e.dests.Update(e.ctx, e.dest.ID, destinations.Input{Enabled: &off}); err != nil {
				t.Fatal(err)
			}
		}, func(err error) bool { return err != nil && strings.Contains(err.Error(), "disabled") }},
		{"not mounted", func() {
			if err := os.Remove(filepath.Join(e.target, filepath.FromSlash(filecopy.MarkerRel))); err != nil {
				t.Fatal(err)
			}
		}, func(err error) bool { return errors.Is(err, destinations.ErrNotMounted) }},
	}
	for i, c := range cases {
		c.setup()
		job := e.newJob(false)
		if i == 0 {
			job.Params.DestinationID = 0
		}
		_, _, err := e.run(job)
		if !c.want(err) {
			t.Fatalf("%s: err %v", c.name, err)
		}
	}
	if len(e.versions()) != 0 {
		t.Fatal("a failed preflight recorded a version")
	}
}

func TestRunRefusesASymlinkedManifestsFolder(t *testing.T) {
	e := newEnv(t)
	elsewhere := filepath.Join(e.base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(e.target, filepath.FromSlash(Root))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run(e.newJob(false)); err == nil {
		t.Fatal("an export through a symlinked .bunkarr/manifests succeeded")
	}
	if es, _ := os.ReadDir(elsewhere); len(es) != 0 {
		t.Fatalf("written through the symlink: %v", es)
	}
}

func TestRunRecordsAnOrphanVersionAndFinishesItsOwn(t *testing.T) {
	// A version an interrupted job wrote completely (renamed, not recorded) is checked and recorded
	// by the next job; a resumed job whose own version is recorded finishes with it.
	e := newEnv(t)
	job := e.newJob(false)
	e.crashRun(job, PointBeforeRecord)
	if len(e.versions()) != 0 || len(e.entries()) != 1 {
		t.Fatalf("after the crash: %v, %v", e.versions(), e.entries())
	}
	// Another job (not the crashed one) records it and finds the content unchanged.
	e.clock.Advance(time.Minute)
	st, rep := e.runOK()
	if st.Recovered != 1 || !st.Unchanged || !strings.Contains(rep.String(), "an interrupted export had written") {
		t.Fatalf("stats %+v\n%s", st, rep)
	}
	e.assertConverged(1)
	// A damaged orphan is recorded as damaged, then pruned in time.
	e.clock.Advance(time.Hour)
	job = e.newJob(false)
	e.writeFile("movies/another.mkv", 5) // a change, so the job writes a version
	e.scan()
	e.crashRun(job, PointAfterRename)
	for _, n := range e.entries() {
		if n != filepath.Base(e.versions()[0].Path) {
			if err := os.WriteFile(filepath.Join(e.target, filepath.FromSlash(Root), n, CSVName), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.clock.Advance(time.Minute)
	st, rep = e.runOK()
	if st.Recovered != 1 || !rep.warned("recorded as damaged") {
		t.Fatalf("stats %+v\n%s", st, rep)
	}
	var integrities []string
	for _, v := range e.versions() {
		integrities = append(integrities, v.Integrity)
	}
	slices.Sort(integrities)
	if strings.Join(integrities, ",") != "damaged,ok" {
		t.Fatalf("integrities %v", integrities)
	}
	e.clock.Advance(8 * 24 * time.Hour)
	e.writeFile("movies/third.mkv", 5)
	e.scan()
	e.refresh(e.rad.ID)
	e.refresh(e.son.ID)
	st, _ = e.runOK()
	for _, v := range e.versions() {
		if v.Integrity != IntegrityOK {
			t.Fatalf("a damaged version older than 7 days was kept: %+v (stats %+v)", v, st)
		}
	}
}

func TestRunLeavesAnUnreadableVersionAlone(t *testing.T) {
	e := newEnv(t)
	junk := filepath.Join(e.target, filepath.FromSlash(Root), "20200101T000000Z")
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatal(err)
	}
	res, rep, err := e.run(e.newJob(false))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.warned("left alone") || res.Warnings != 2 {
		t.Fatalf("warnings %d\n%s", res.Warnings, rep)
	}
	if _, err := os.Stat(junk); err != nil {
		t.Fatalf("the unreadable version was touched: %v", err)
	}
}

func TestRunWithoutJobManager(t *testing.T) {
	e := newEnv(t)
	res, err := e.runner.Run(e.ctx, e.newJob(false), jobs.Env{})
	if err != nil || res.Stats.(Stats).ManifestID == 0 {
		t.Fatalf("run without reporter: %+v, %v", res, err)
	}
}

// gateTiers is AllFull whose Decide (reached once per source while a manifest of a destination
// is built) waits until release is closed; it records how many builds were in it at once.
type gateTiers struct {
	mu           sync.Mutex
	active, peak int
	entered      chan struct{}
	release      chan struct{}
}

func (g *gateTiers) Decide(ctx context.Context, r TierRead, destinationID, sourceID int64) (func(int64) Decision, error) {
	g.mu.Lock()
	g.active++
	g.peak = max(g.peak, g.active)
	g.mu.Unlock()
	g.entered <- struct{}{}
	<-g.release
	g.mu.Lock()
	g.active--
	g.mu.Unlock()
	return AllFull{}.Decide(ctx, r, destinationID, sourceID)
}

func TestBuildsTakeTurns(t *testing.T) {
	// A build holds the library in memory and a read connection: the jobs of two destinations
	// (the job manager runs them in parallel) and an on-the-spot export build one at a time.
	e := newEnv(t)
	target2 := filepath.Join(e.base, "target2")
	if err := os.MkdirAll(target2, 0o755); err != nil {
		t.Fatal(err)
	}
	d2, err := e.dests.Create(e.ctx, destinations.Input{Name: "Offsite", Target: target2, SourceIDs: []int64{e.src.ID}},
		destinations.CreateOptions{AllowLocal: true})
	if err != nil {
		t.Fatal(err)
	}
	gate := &gateTiers{entered: make(chan struct{}, 16), release: make(chan struct{})}
	r, err := NewRunner(Options{DB: e.db, Catalog: e.cat, Integrations: e.ints, Destinations: e.dests, Index: e.index.Store(),
		Tiers: gate, ConfigDir: e.config, Now: e.clock.Now, Location: time.UTC, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 3)
	var wg sync.WaitGroup
	for _, job := range []jobs.Job{e.newJobFor(e.dest.ID, false), e.newJobFor(d2.ID, false)} {
		wg.Go(func() {
			_, err := r.Run(e.ctx, job, jobs.Env{Reporter: &memReporter{}, Items: &memItems{}})
			errs <- err
		})
	}
	wg.Go(func() {
		f, err := r.Export(e.ctx, e.dest.ID, FormatCSV)
		if err == nil {
			_ = f.Close()
		}
		errs <- err
	})
	<-gate.entered
	select {
	case <-gate.entered:
		t.Error("a second manifest was built while one was being built")
	case <-time.After(500 * time.Millisecond):
	}
	close(gate.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if gate.peak != 1 {
		t.Fatalf("%d manifests were built at once", gate.peak)
	}
	if n := len(gate.entered); n != 2 {
		t.Fatalf("%d more builds, want 2", n)
	}
}

func TestBuildWaitEndsWithTheContext(t *testing.T) {
	// A job waiting for its turn to build stops when it is cancelled.
	e := newEnv(t)
	release, err := e.runner.acquireBuild(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(e.ctx, 100*time.Millisecond)
	defer cancel()
	rep := &memReporter{}
	if _, err := e.runner.Run(ctx, e.newJob(false), jobs.Env{Reporter: rep, Items: &memItems{}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(rep.String(), "Waiting for another manifest") {
		t.Fatalf("log:\n%s", rep)
	}
	if _, err := e.runner.Export(ctx, 0, FormatJSON); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("export err %v", err)
	}
}

// episodeManifest is a manifest of n series with eps episodes each and one file each: its JSON
// is large, its CSV small.
func episodeManifest(n, eps int) *Manifest {
	m := goldenManifest()
	tmpl := m.Items[3]
	m.Items, m.OtherFiles = nil, []ExtraFile{}
	for i := range n {
		it := tmpl
		it.ArrID = int64(i + 1)
		it.Detail.Episodes = make([]Episode, eps)
		for j := range eps {
			it.Detail.Episodes[j] = Episode{Season: 1 + j/20, Episode: 1 + j%20, Monitored: j%3 != 0}
		}
		m.Items = append(m.Items, it)
	}
	return m
}

func TestRunWriteStreamsTheManifest(t *testing.T) {
	// A version's files are streamed to the destination: writing them allocates a small part of
	// the manifest's size (a library of a million files makes a manifest of hundreds of MB).
	e := newEnv(t)
	h, err := e.dests.Open(e.ctx, e.dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	job := e.newJob(false)
	w := &run{r: e.runner, job: job, rep: nopReporter{}, h: h}
	m := episodeManifest(300, 300)
	m.Job = w.jobRef()
	hash, err := ContentHash(m)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, wantCSV := referenceJSON(t, m), mustCSV(t, m)
	var v Version
	a := allocated(func() { v, err = w.write(e.ctx, m, hash, Root+"/"+snapshots.PartialName(job.ID)) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckVersion(h.Root, v.Path, v.Checksum); err != nil {
		t.Fatalf("the version does not check: %v", err)
	}
	gotJSON, _ := os.ReadFile(filepath.Join(e.versionDir(v), JSONName))
	gotCSV, _ := os.ReadFile(filepath.Join(e.versionDir(v), CSVName))
	if !bytes.Equal(gotJSON, wantJSON) || !bytes.Equal(gotCSV, wantCSV) {
		t.Fatal("the written files differ from the manifest's encoding")
	}
	if len(wantJSON) < 4<<20 {
		t.Fatalf("the test manifest is only %d bytes", len(wantJSON))
	}
	if a > uint64(len(wantJSON))/4 && !raceEnabled { // allocation counts are meaningless under the race detector
		t.Fatalf("writing a %d-byte manifest allocated %d bytes", len(wantJSON), a)
	}
	left, _ := filepath.Glob(filepath.Join(e.versionDir(v), filecopy.TempPrefix+"*"))
	if len(left) > 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func mustCSV(t *testing.T, m *Manifest) []byte {
	t.Helper()
	b, err := EncodeCSV(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWriteFileStreamFailureLeavesNothing(t *testing.T) {
	// A write that fails half-way leaves neither the file nor its temp file; an existing file is
	// never replaced.
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	boom := errors.New("boom")
	if _, err := writeFileStream(root, "v/manifest.json", 0o644, func(w io.Writer) error {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 200<<10))
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if names, _ := os.ReadDir(filepath.Join(dir, "v")); len(names) != 0 {
		t.Fatalf("left %v", names)
	}
	sum, err := writeFileStream(root, "v/manifest.json", 0o644, func(w io.Writer) error {
		_, err := io.WriteString(w, "{}\n")
		return err
	})
	if err != nil || sum != SHA256Hex([]byte("{}\n")) {
		t.Fatalf("sum %s, %v", sum, err)
	}
	if _, err := writeFileStream(root, "v/manifest.json", 0o644, func(w io.Writer) error {
		_, err := io.WriteString(w, "other")
		return err
	}); !errors.Is(err, filecopy.ErrExists) {
		t.Fatalf("replacing: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "v", "manifest.json")); string(data) != "{}\n" {
		t.Fatalf("content %q", data)
	}
	if names, _ := os.ReadDir(filepath.Join(dir, "v")); len(names) != 1 {
		t.Fatalf("left %v", names)
	}
}
