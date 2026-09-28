//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// Acceptance 8 of Phase 4 (docs/design/phase4.md §14.6 item 8): a database the Phase 3 release
// wrote, with filecopy destinations and their jobs, migrates to schema 0004; the Phase 4 binary on a
// copy plans exactly the dry-run items and stats Phase 3 plans for the same state, and while no
// engine destination exists it queues nothing new: no job and no schedule that was not there.

// phase3Commit is the Phase 3 release ("feat: phase 3 — tiering and prioritization"), and
// phase3PublicCommit its public counterpart (release_test.go).
const (
	phase3Commit       = "7d3477deafa88eafdb417bf8eea0a832ec10f726"
	phase3PublicCommit = "fab6b96be75a0238e5ccaab8234eed4030466ec1"
)

// phase3 is the Phase 3 binary, built once on first use; TestMain removes its directory.
// (BUNKARR_E2E_PREVIOUS_BINARY stays the Phase 2 binary of upgrade_test.go.)
var phase3 = &releaseBuild{
	name:    "Phase 3",
	env:     "BUNKARR_E2E_PHASE3_BINARY",
	commits: []string{phase3Commit, phase3PublicCommit},
	sources: releaseSources,
}

// phase3Binary returns the Phase 3 binary: $BUNKARR_E2E_PHASE3_BINARY when set, else the Phase 3
// release built from its commit (fetched in CI). It skips when that is not available, except in CI,
// where it fails (release_test.go).
func phase3Binary(t *testing.T) string {
	t.Helper()
	return phase3.binary(t)
}

// scheduleRow is a schedule as GET /schedules lists it, for comparison.
type scheduleRow struct {
	ID      int64          `json:"id"`
	JobType string         `json:"jobType"`
	Params  map[string]any `json:"params"`
	Cron    string         `json:"cron"`
	Enabled bool           `json:"enabled"`
}

func (s scheduleRow) String() string {
	return fmt.Sprintf("%d %s %v %q %t", s.ID, s.JobType, s.Params, s.Cron, s.Enabled)
}

// schedulesOf lists a server's schedules as sorted lines.
func schedulesOf(c *client) []string {
	c.t.Helper()
	var list []scheduleRow
	c.call(http.StatusOK, "GET", "/schedules", nil, &list)
	var out []string
	for _, s := range list {
		out = append(out, s.String())
	}
	slices.Sort(out)
	return out
}

func TestUpgradeFromPhase3(t *testing.T) {
	prev := phase3Binary(t)
	root := resolvedTempDir(t)
	media := filepath.Join(root, "media")
	writeLibrary(t, media)
	targets := []string{filepath.Join(root, "nas"), filepath.Join(root, "usb")}
	for _, d := range targets {
		mkdirAll(t, d)
	}

	// The Phase 3 binary backs the library up to two filecopy destinations.
	old := newServer(t, root)
	old.bin = prev
	old.start()
	old.setup()
	if code, _, err := old.do("GET", "/tiers/rules", nil); err != nil || code != http.StatusOK {
		t.Fatalf("the previous binary answers GET /tiers/rules with %d (%v): it is not the Phase 3 release", code, err)
	}
	if code, _, err := old.do("GET", "/settings/engines", nil); err != nil || code != http.StatusNotFound {
		t.Fatalf("the previous binary answers GET /settings/engines with %d (%v): it is not the Phase 3 release", code, err)
	}
	src := old.createSource("Media", media)
	var dests []apiDestination
	for i, target := range targets {
		d := old.createDestination(fmt.Sprintf("Filecopy %d", i+1), target, src.ID)
		dests = append(dests, d)
		requireStatus(t, old.runSync(d.ID, nil), "completed")
		requireVerifies(t, old.client, d.ID)
	}
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
	jobsBefore, schemaBefore := jobIDsOf(t, upgraded)
	if schemaBefore != 3 {
		t.Fatalf("the Phase 3 database has schema version %d, want 3", schemaBefore)
	}

	// What Phase 3 plans now.
	old.start()
	oldSchedules := schedulesOf(old.client)
	type plan struct {
		items []string
		stats syncStats
	}
	oldPlans := map[int64]plan{}
	for _, d := range dests {
		dry := old.runSync(d.ID, map[string]any{"dryRun": true})
		requireStatus(t, dry, "completed")
		oldPlans[d.ID] = plan{planOf(old.client, dry.ID), decodeStats[syncStats](t, dry)}
	}
	old.stop()

	// The Phase 4 binary on the copy: schema 0004, nothing new queued, the same plans.
	cur := newServer(t, root)
	cur.cfg, cur.key = upgraded, old.key
	cur.start()
	if code, _, err := cur.do("GET", "/settings/engines", nil); err != nil || code != http.StatusOK {
		t.Fatalf("GET /settings/engines of the Phase 4 binary: %d (%v)", code, err)
	}
	time.Sleep(2 * time.Second) // start-up recovery and the scheduler have run
	var page apiPage[apiJob]
	cur.call(http.StatusOK, "GET", "/jobs?pageSize=500", nil, &page)
	for _, j := range page.Records {
		if !slices.Contains(jobsBefore, j.ID) {
			t.Fatalf("the upgraded install queued job %d (%s, %s) by itself", j.ID, j.Type, j.Trigger)
		}
	}
	if got := schedulesOf(cur.client); !slices.Equal(got, oldSchedules) {
		t.Fatalf("schedules after the upgrade:\n  %s\nbefore:\n  %s", strings.Join(got, "\n  "), strings.Join(oldSchedules, "\n  "))
	}
	for _, d := range dests {
		var v struct {
			Kind       string `json:"kind"`
			Engine     string `json:"engine"`
			Blocked    string `json:"blockedReason"`
			Encryption struct {
				Mode string `json:"mode"`
			} `json:"encryption"`
			RetentionSchedule any `json:"retentionSchedule"`
		}
		cur.call(http.StatusOK, "GET", fmt.Sprintf("/destinations/%d", d.ID), nil, &v)
		if v.Kind != "local" || v.Engine != "filecopy" || v.Encryption.Mode != "none" || v.Blocked != "" || v.RetentionSchedule != nil {
			t.Fatalf("destination %d after the upgrade: %+v", d.ID, v)
		}
		dry := cur.runSync(d.ID, map[string]any{"dryRun": true})
		requireStatus(t, dry, "completed")
		items, stats := planOf(cur.client, dry.ID), decodeStats[syncStats](t, dry)
		want := oldPlans[d.ID]
		if !slices.Equal(items, want.items) || stats != want.stats {
			t.Fatalf("destination %d plans differently after the upgrade.\nPhase 3 (%+v):\n  %s\nnow (%+v):\n  %s", d.ID, want.stats,
				strings.Join(want.items, "\n  "), stats, strings.Join(items, "\n  "))
		}
		if stats.FilesCopied != 1 || stats.FilesUpdated != 1 || stats.FilesMoved != 1 || stats.FilesRetained != 1 {
			t.Fatalf("the dry run of destination %d does not cover the changes: %+v", d.ID, stats)
		}
	}
	// The real syncs apply the same plans; nothing but the jobs this test started ran.
	for i, d := range dests {
		j := cur.runSync(d.ID, nil)
		requireStatus(t, j, "completed")
		if st := decodeStats[syncStats](t, j); st.FilesCopied != 1 || st.FilesUpdated != 1 || st.FilesMoved != 1 || st.FilesRetained != 1 {
			t.Fatalf("sync of destination %d after the upgrade: %+v", d.ID, st)
		}
		verifyMirror(t, media, targets[i], src.DestFolder)
	}
	waitJobsIdle(cur.client)
	cur.call(http.StatusOK, "GET", "/jobs?pageSize=500", nil, &page)
	for _, j := range page.Records {
		if !slices.Contains(jobsBefore, j.ID) && j.Type != "sync" {
			t.Fatalf("job %d of type %s (%s) after the upgrade; only this test's syncs may run", j.ID, j.Type, j.Trigger)
		}
	}
	cur.stop()
	if _, schema := jobIDsOf(t, upgraded); schema != 4 {
		t.Fatalf("schema version after the upgrade: %d, want 4", schema)
	}
}

// jobIDsOf returns the job ids and the schema version of the database in a (stopped) config
// directory. It opens a copy, so the directory itself is not migrated.
func jobIDsOf(t *testing.T, cfg string) ([]int64, int) {
	t.Helper()
	tmp := t.TempDir()
	copyTree(t, cfg, tmp)
	ctx := context.Background()
	// Read the version before db.Open migrates the copy.
	version := schemaVersionOf(t, filepath.Join(tmp, "bunkarr.db"))
	d, err := db.Open(ctx, filepath.Join(tmp, "bunkarr.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Reader().QueryContext(ctx, `SELECT id FROM jobs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids, version
}

// schemaVersionOf reads the highest applied migration of a database file without migrating it.
func schemaVersionOf(t *testing.T, path string) int {
	t.Helper()
	c, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var v sql.NullInt64
	if err := c.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatalf("schema version of %s: %v", path, err)
	}
	return int(v.Int64)
}
