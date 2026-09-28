//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// TestDockerOffsiteTwoDestinations is acceptance 1 (docs/design/phase4.md §14.6 item 1): one source
// backed up to a filecopy destination on a volume (the UNAS stand-in: allowLocal, sync 0 2 * * *,
// deletedDays 30) and to a restic destination on MinIO (the B2 stand-in: sync 0 3 * * *, daily 7,
// deletedDays 14). Both sync schedules run (POST /schedules/{id}/run) at the same time, and one sync
// runs while the other is queued or running; each job and its records belong to its own
// destination only (the restic repository holds no plain file, the volume no restic data); a
// retention run of each changes only its own records and snapshots.
func TestDockerOffsiteTwoDestinations(t *testing.T) {
	o := newOffsite(t)
	o.startMinIO()
	o.writeRandom(map[string]int64{
		"two/Movies/Big (2010)/Big (2010).mkv":       24 << 20,
		"two/Movies/Big (2010)/Big (2010).nfo":       2000,
		"two/Movies/Gone (2011)/Gone (2011).mkv":     300000,
		"two/TV/Show/Season 01/Show - S01E01.mkv":    400000,
		"two/TV/Show/Season 01/Show - S01E01.en.srt": 30000,
	})
	unas := o.d.volume("unas")
	h := o.d.helper("unas-owner", o.image, "-v", unas+":/unas")
	o.d.docker("exec", h, "chown", "1000:1000", "/unas")
	o.d.remove(h)
	b := o.startBunkarr("bunkarr", bunkarrOpts{args: []string{"-v", unas + ":/unas"}})
	src := b.createSource("Library", "two")

	var fc offDest
	b.api.call(viaKey, 201, "POST", "/destinations", map[string]any{"name": "UNAS", "target": "/unas", "sourceIds": []int64{src.ID},
		"allowLocal": true, "schedule": map[string]any{"cron": "0 2 * * *", "enabled": true},
		"retention": map[string]any{"deletedDays": 30}, "settings": map[string]any{"verify": map[string]any{"mode": "full"}}}, &fc)
	rs, k := b.createOffsite(b.resticS3("B2 stand-in", "two", []int64{src.ID}, map[string]any{
		"schedule":  map[string]any{"cron": "0 3 * * *", "enabled": true},
		"retention": map[string]any{"deletedDays": 14, "snapshotDaily": 7},
		// Slow enough that the two syncs overlap.
		"bandwidth": map[string]any{"uploadKiBps": 2048}}))

	// Both schedules run at the same time.
	var scheds []struct {
		ID      int64  `json:"id"`
		JobType string `json:"jobType"`
		Cron    string `json:"cron"`
		Enabled bool   `json:"enabled"`
		Params  struct {
			DestinationID int64 `json:"destinationId"`
		} `json:"params"`
	}
	b.api.call(viaKey, 200, "GET", "/schedules", nil, &scheds)
	syncSched := map[int64]int64{}
	for _, s := range scheds {
		if s.JobType == "sync" {
			syncSched[s.Params.DestinationID] = s.ID
			want := map[int64]string{fc.ID: "0 2 * * *", rs.ID: "0 3 * * *"}[s.Params.DestinationID]
			if s.Cron != want || !s.Enabled {
				t.Fatalf("sync schedule of destination %d: %q enabled %t, want %q", s.Params.DestinationID, s.Cron, s.Enabled, want)
			}
		}
	}
	if syncSched[fc.ID] == 0 || syncSched[rs.ID] == 0 {
		t.Fatalf("sync schedules: %+v", scheds)
	}
	before := o.tree("two")
	jobIDs := map[int64]int64{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, dest := range []int64{fc.ID, rs.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body, _, err := b.api.send(viaKey, "POST", fmt.Sprintf("/schedules/%d/run", syncSched[dest]), nil, true)
			var j apiJob
			if err == nil && code == 202 && decodeJSONOK(body, &j) && j.ID != 0 {
				mu.Lock()
				jobIDs[dest] = j.ID
				mu.Unlock()
				return
			}
			t.Errorf("run the sync schedule of destination %d: HTTP %d %v: %s", dest, code, err, body)
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	overlap := false
	deadline := time.Now().Add(15 * time.Minute)
	var jf, jr offJob
	for {
		jf, jr = b.job(jobIDs[fc.ID]), b.job(jobIDs[rs.ID])
		active := func(j offJob) bool { return j.Status == "queued" || j.Status == "running" }
		if (jf.Status == "running" && active(jr)) || (jr.Status == "running" && active(jf)) {
			overlap = true
		}
		if jf.final() && jr.final() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the syncs did not finish: %+v %+v", jf, jr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	requireStatus(t, jf.apiJob, "completed")
	requireStatus(t, jr.apiJob, "completed")
	if !overlap {
		t.Fatalf("no moment where one sync ran while the other was queued or running: filecopy %v-%v, restic %v-%v",
			jf.StartedAt, jf.FinishedAt, jr.StartedAt, jr.FinishedAt)
	}
	if jf.Params.DestinationID != fc.ID || jr.Params.DestinationID != rs.ID || jf.Trigger != "schedule" && jf.Trigger != "manual" {
		t.Fatalf("jobs: %+v / %+v", jf, jr)
	}
	if after := o.tree("two"); !mapsEqual(before, after) {
		t.Fatal("the two syncs changed the source (S1)")
	}
	for _, j := range []offJob{jf, jr} {
		st := decodeStats[engineSync](t, j.apiJob)
		if st.FilesCopied != 5 || st.FilesFailed != 0 {
			t.Fatalf("sync %d: %s", j.ID, j.Stats)
		}
		for _, it := range b.items(j.ID) {
			if !strings.HasPrefix(it.RelPath, src.DestFolder+"/") || it.Status != "done" {
				t.Fatalf("item of job %d: %+v", j.ID, it)
			}
		}
	}
	if decodeStats[engineSync](t, jr.apiJob).Engine != "restic" {
		t.Fatalf("the restic destination's sync ran on another engine: %s", jr.Stats)
	}
	o.requireOwnRecords(b, fc.ID, rs.ID)
	// The volume holds the mirror and no restic data; the bucket holds only restic's repository.
	if out := o.d.docker("exec", b.c, "sh", "-c", "cd /unas && find . -type f | sort"); strings.Contains(out, "/data/") || strings.Contains(out, "/snapshots/") ||
		!strings.Contains(out, "./"+src.DestFolder+"/Movies/Big (2010)/Big (2010).mkv") {
		t.Fatalf("the filecopy volume:\n%s", out)
	}
	for p := range o.objects("bunkarr/two") {
		top, _, _ := strings.Cut(p, "/")
		if !slices.Contains([]string{"config", "data", "index", "keys", "snapshots", "locks"}, top) {
			t.Fatalf("the restic prefix holds %s: not a restic repository file", p)
		}
	}

	// Retention: a deleted file is retained by both; its rows are made to expire; a retention run
	// of each destination changes only its own records and snapshots.
	o.toolsSh(`rm "$1"`, offsiteMedia+"/two/Movies/Gone (2011)/Gone (2011).mkv")
	for _, id := range []int64{fc.ID, rs.ID} {
		j := b.sync(id, nil)
		requireStatus(t, j, "completed")
		if st := decodeStats[engineSync](t, j); st.FilesRetained != 1 {
			t.Fatalf("sync of destination %d after the delete: %s", id, j.Stats)
		}
	}
	b.editDB(func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE destination_files SET expires_at = ? WHERE state = 'retained'`,
			db.FormatTime(time.Now().Add(-time.Hour)))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 2 {
			return fmt.Errorf("%d retained rows, want 2 (one per destination)", n)
		}
		return nil
	})
	snaps := func() []string {
		var ids []string
		for _, s := range o.resticSnapshots(k, "bunkarr-dest:"+k.EngineTag) {
			ids = append(ids, s.ID)
		}
		return ids
	}
	for _, step := range []struct {
		dest, other int64
		name        string
	}{{fc.ID, rs.ID, "filecopy"}, {rs.ID, fc.ID, "restic"}} {
		otherRows, snapsBefore := o.rowsOf(b, step.other), snaps()
		j := b.retention(step.dest, nil)
		requireStatus(t, j, "completed")
		if st := decodeStats[retentionStatsE2E](t, j); st.FilesExpired != 1 {
			t.Fatalf("%s retention %d: %s", step.name, j.ID, j.Stats)
		}
		if got := o.rowsOf(b, step.other); !slices.Equal(got, otherRows) {
			t.Fatalf("the %s retention changed the other destination's records:\nbefore %q\nafter  %q", step.name, otherRows, got)
		}
		if step.dest == fc.ID && !slices.Equal(snaps(), snapsBefore) {
			t.Fatalf("the filecopy retention changed the restic snapshots: %v → %v", snapsBefore, snaps())
		}
		for _, it := range b.items(j.ID) {
			if it.Action != "expire" {
				t.Fatalf("%s retention item %+v", step.name, it)
			}
		}
	}
	if out := o.d.docker("exec", b.c, "sh", "-c", "find /unas/.bunkarr/retention -type f | wc -l"); strings.TrimSpace(out) != "0" {
		t.Fatalf("the expired version is still in the filecopy retention: %s", out)
	}
	requireVerified(t, b.verify(rs.ID))
	o.auditAll()
}

// retentionStatsE2E are a retention job's counters.
type retentionStatsE2E struct {
	FilesExpired       int64 `json:"filesExpired"`
	FilesHeld          int64 `json:"filesHeld"`
	FilesFailed        int64 `json:"filesFailed"`
	SnapshotsForgotten int64 `json:"snapshotsForgotten"`
	SnapshotsKept      int64 `json:"snapshotsKept"`
	Pruned             bool  `json:"pruned"`
}

// requireOwnRecords requires that every record of each destination was written by one of that
// destination's own jobs, and that restic's snapshot rows and references belong to the restic
// destination only.
func (o *offsite) requireOwnRecords(b *bunkarrC, fc, rs int64) {
	t := o.t
	r := b.readDB()
	rows, err := r.Query(`SELECT f.destination_id, f.rel_path, f.state, COALESCE(f.engine_ref, ''), COALESCE(json_extract(j.params, '$.destinationId'), 0)
		FROM destination_files f LEFT JOIN jobs j ON j.id = f.job_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := map[int64]int{}
	for rows.Next() {
		var dest, jobDest int64
		var rel, state, ref string
		if err := rows.Scan(&dest, &rel, &state, &ref, &jobDest); err != nil {
			t.Fatal(err)
		}
		n[dest]++
		if jobDest != dest {
			t.Fatalf("record %s of destination %d was written by a job of destination %d", rel, dest, jobDest)
		}
		if dest == fc && ref != "" {
			t.Fatalf("filecopy record %s has an engine reference %q", rel, ref)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n[fc] != 5 || n[rs] != 5 {
		t.Fatalf("records per destination: %v, want 5 each", n)
	}
	var other int
	if err := r.QueryRow(`SELECT count(*) FROM engine_snapshots WHERE destination_id <> ?`, rs).Scan(&other); err != nil || other != 0 {
		t.Fatalf("engine snapshots of other destinations: %d (%v)", other, err)
	}
}

// rowsOf returns destination id's records as sorted lines.
func (o *offsite) rowsOf(b *bunkarrC, id int64) []string {
	rows, err := b.readDB().Query(`SELECT rel_path, state, COALESCE(reason, ''), COALESCE(engine_ref, ''), COALESCE(expires_at, ''), size
		FROM destination_files WHERE destination_id = ?`, id)
	if err != nil {
		o.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rel, state, reason, ref, exp string
		var size int64
		if err := rows.Scan(&rel, &state, &reason, &ref, &exp, &size); err != nil {
			o.t.Fatal(err)
		}
		out = append(out, strings.Join([]string{rel, state, reason, ref, exp, fmt.Sprint(size)}, "|"))
	}
	slices.Sort(out)
	return out
}

// decodeJSONOK decodes b into v and reports success.
func decodeJSONOK(b []byte, v any) bool { return json.Unmarshal(b, v) == nil }
