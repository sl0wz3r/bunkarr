//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
)

// Acceptance 8 for an upgraded install (docs/design/phase2-3.md, the user's decision that media
// stays backed up in full by default): a database written by the Phase 2 release and opened by this
// binary, with no tier rules, plans exactly what Phase 2 plans for the same state (the same items
// and stats of a dry run), every file is full by the built-in fallback, and a second sync plans
// nothing. Without an *arr integration no job type that Phase 1 did not run is queued.

// phase2Commit is the Phase 2 release ("feat: phase 2 — *arr awareness and Sign in with Plex").
const phase2Commit = "dce143f3af4c898965a3b0d1caf96eee56b24d30"

// previous is the Phase 2 binary, built once on first use; TestMain removes its directory.
var previous struct {
	once sync.Once
	dir  string
	path string
	skip string
	err  error
}

// previousBinary returns the Phase 2 binary: $BUNKARR_E2E_PREVIOUS_BINARY when set, else
// phase2Commit of this repository, exported with `git archive` (the checkout is not touched) and
// built with -tags e2e. The test skips when git or that commit is not available.
func previousBinary(t *testing.T) string {
	t.Helper()
	previous.once.Do(func() {
		if p := os.Getenv("BUNKARR_E2E_PREVIOUS_BINARY"); p != "" {
			previous.path, previous.err = filepath.Abs(p)
			return
		}
		root, err := moduleRoot()
		if err != nil {
			previous.err = err
			return
		}
		if _, err := exec.LookPath("git"); err != nil {
			previous.skip = "git is not installed (set BUNKARR_E2E_PREVIOUS_BINARY to a Phase 2 binary)"
			return
		}
		if err := exec.Command("git", "-C", root, "cat-file", "-e", phase2Commit+"^{commit}").Run(); err != nil {
			previous.skip = fmt.Sprintf("the Phase 2 commit %.12s is not in this checkout (set BUNKARR_E2E_PREVIOUS_BINARY to a Phase 2 binary)", phase2Commit)
			return
		}
		dir, err := os.MkdirTemp("", "bunkarr-e2e-phase2-")
		if err != nil {
			previous.err = err
			return
		}
		previous.dir = dir
		src := filepath.Join(dir, "src")
		if err := os.Mkdir(src, 0o755); err != nil {
			previous.err = err
			return
		}
		tarball := filepath.Join(dir, "phase2.tar")
		if out, err := exec.Command("git", "-C", root, "archive", "--format=tar", "-o", tarball, phase2Commit).CombinedOutput(); err != nil {
			previous.err = fmt.Errorf("git archive %.12s: %w\n%s", phase2Commit, err, out)
			return
		}
		if out, err := exec.Command("tar", "-x", "-f", tarball, "-C", src).CombinedOutput(); err != nil {
			previous.err = fmt.Errorf("tar: %w\n%s", err, out)
			return
		}
		out := filepath.Join(dir, "bunkarr-phase2")
		build := exec.Command("go", "build", "-trimpath", "-tags", "e2e", "-o", out, "./cmd/bunkarr")
		build.Dir = src
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := build.CombinedOutput(); err != nil {
			previous.err = fmt.Errorf("build the Phase 2 binary: %w\n%s", err, b)
			return
		}
		previous.path = out
	})
	if previous.skip != "" {
		t.Skip(previous.skip)
	}
	if previous.err != nil {
		t.Fatal(previous.err)
	}
	return previous.path
}

// copyTree copies the regular files and directories under from to to.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		info, err := in.Stat()
		if err != nil {
			return err
		}
		o, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(o, in); err != nil {
			o.Close()
			return err
		}
		return o.Close()
	})
	if err != nil {
		t.Fatalf("copy %s: %v", from, err)
	}
}

// planOf returns a dry run's items as comparable lines, sorted.
func planOf(c *client, jobID int64) []string {
	c.t.Helper()
	var out []string
	for _, it := range c.items(jobID, "") {
		out = append(out, fmt.Sprintf("%s %s %s %d %q", it.Action, it.Status, it.RelPath, it.Bytes, it.Error))
	}
	slices.Sort(out)
	return out
}

func TestUpgradeFromPhase2(t *testing.T) {
	prev := previousBinary(t)
	t.Run("no arr", func(t *testing.T) { upgradeFromPhase2(t, prev, false) })
	t.Run("radarr", func(t *testing.T) { upgradeFromPhase2(t, prev, true) })
}

func upgradeFromPhase2(t *testing.T, prevBin string, withArr bool) {
	root := resolvedTempDir(t)
	media := filepath.Join(root, "media")
	writeLibrary(t, media)
	target := filepath.Join(root, "nas")
	mkdirAll(t, target)

	// The Phase 2 binary backs the library up (with a fake Radarr for part of it).
	old := newServer(t, root)
	old.bin = prevBin
	old.start()
	old.setup()
	if code, _, err := old.do("GET", "/tiers/rules", nil); err != nil || code != http.StatusNotFound {
		t.Fatalf("the previous binary answers GET /tiers/rules with %d (%v): it is not the Phase 2 release", code, err)
	}
	src := old.createSource("Media", media)
	dest := old.createDestination("NAS", target, src.ID)
	if withArr {
		arrMovies := filepath.Join(media, "Radarr")
		for _, m := range radarrMovies(t) {
			writeFile(t, arrMovies, m.rel, content(m.rel, 1, int(m.size)))
		}
		srv := arrtest.NewServer(t, arr.KindRadarr, hookArrKey)
		id := createIntegration(old.client, map[string]any{"type": "radarr", "name": "Radarr", "url": srv.URL, "apiKey": hookArrKey,
			"settings": map[string]any{"pathMappings": []map[string]string{{"arr": "/movies", "local": arrMovies}}}})
		waitRefreshes(old.client, id)
	}
	requireStatus(t, old.runSync(dest.ID, nil), "completed")
	waitJobsIdle(old.client)

	// The library changes: an update, an addition, a deletion and a rename.
	writeFile(t, media, "Movies/Cube (1997)/Cube (1997).mkv", content("cube v2", 2, 151000))
	writeFile(t, media, "Movies/Brazil (1985)/Brazil (1985).mkv", content("brazil", 1, 99000))
	if err := os.Remove(filepath.Join(media, "Movies/Up (2009)/Up (2009).nfo")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(media, "Movies/Dune (2021)/Dune.mkv"), filepath.Join(media, "Movies/Dune (2021)/Dune (2021).mkv")); err != nil {
		t.Fatal(err)
	}
	old.stop()
	upgraded := filepath.Join(root, "config-upgraded")
	copyTree(t, old.cfg, upgraded)

	// What Phase 2 plans now.
	old.start()
	oldDry := old.runSync(dest.ID, map[string]any{"dryRun": true})
	requireStatus(t, oldDry, "completed")
	oldPlan, oldStats := planOf(old.client, oldDry.ID), decodeStats[syncStats](t, oldDry)
	old.stop()

	// This binary on a copy of the same database: the same plan.
	cur := newServer(t, root)
	cur.cfg, cur.key = upgraded, old.key
	cur.start()
	var rules apiRuleSet
	cur.call(http.StatusOK, "GET", "/tiers/rules", nil, &rules)
	if rules.Revision != 0 || len(rules.Rules) != 0 {
		t.Fatalf("rules after the upgrade: %+v", rules)
	}
	dry := cur.runSync(dest.ID, map[string]any{"dryRun": true})
	requireStatus(t, dry, "completed")
	plan, stats := planOf(cur.client, dry.ID), decodeStats[syncStats](t, dry)
	if !slices.Equal(plan, oldPlan) || stats != oldStats {
		t.Fatalf("the upgraded install plans differently.\nPhase 2 (%+v):\n  %s\nnow (%+v):\n  %s", oldStats, strings.Join(oldPlan, "\n  "),
			stats, strings.Join(plan, "\n  "))
	}
	if stats.FilesCopied != 1 || stats.FilesUpdated != 1 || stats.FilesMoved != 1 || stats.FilesRetained != 1 {
		t.Fatalf("the dry run does not cover the changes: %+v", stats)
	}
	// Every item of the plan is full by the fallback; the preview agrees.
	for _, it := range tierItems(cur.client, dry.ID, "") {
		if it.Detail.Tier != nil && (it.Detail.Tier.Tier != "full" || it.Detail.Tier.RuleID != 0 || it.Detail.Tier.RuleName != "no rule matched") {
			t.Fatalf("dry-run item %+v: decision %+v", it.apiItem, *it.Detail.Tier)
		}
	}
	p, items := tierPreview(cur.client, dest.ID)
	for _, it := range items {
		if d := diffDecision(it.decision(), fallbackDecision()); len(d) > 0 {
			t.Fatalf("preview of %s: %s", it.RelPath, strings.Join(d, "; "))
		}
	}
	if len(p.Destinations) != 1 || p.Destinations[0].Manifest.Files != 0 || p.Destinations[0].Skip.Files != 0 || len(items) == 0 {
		t.Fatalf("preview after the upgrade: %+v (%d items)", p, len(items))
	}

	// The real sync applies it; a second sync plans nothing.
	j := cur.runSync(dest.ID, nil)
	requireStatus(t, j, "completed")
	if st := decodeStats[syncStats](t, j); st.FilesCopied != 1 || st.FilesUpdated != 1 || st.FilesMoved != 1 || st.FilesRetained != 1 {
		t.Fatalf("sync after the upgrade: %+v", st)
	}
	j = cur.runSync(dest.ID, nil)
	requireStatus(t, j, "completed")
	if st := decodeStats[syncStats](t, j); st.FilesPlanned != 0 || st.BytesPlanned != 0 {
		t.Fatalf("second sync after the upgrade: %+v", st)
	}
	verifyMirror(t, media, target, src.DestFolder)
	waitJobsIdle(cur.client)

	if withArr {
		return
	}
	// No *arr integration: only the job types Phase 1 ran, and no schedule of another type.
	time.Sleep(time.Second)
	phase1 := []string{"scan", "sync", "verify", "retention", "plexdb_backup"}
	var page apiPage[apiJob]
	cur.call(http.StatusOK, "GET", "/jobs?pageSize=500", nil, &page)
	for _, j := range page.Records {
		if !slices.Contains(phase1, j.Type) {
			t.Fatalf("job %d of type %s after the upgrade without an *arr integration", j.ID, j.Type)
		}
	}
	var schedules []struct {
		JobType string `json:"jobType"`
	}
	cur.call(http.StatusOK, "GET", "/schedules", nil, &schedules)
	for _, s := range schedules {
		if !slices.Contains(phase1, s.JobType) {
			t.Fatalf("schedule of type %s after the upgrade without an *arr integration", s.JobType)
		}
	}
}
