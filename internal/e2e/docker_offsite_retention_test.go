//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// The pinned retention table of docs/design/phase4.md §14.5: 40 complete snapshots, one a day at
// 02:00 UTC from 2026-08-01 to 2026-09-09; retention daily 7, weekly 4, monthly 3, yearly 0,
// deletedDays 30; a deleted file last in 2026-08-12 (its row not expired), a held update whose old
// version is in 2026-08-20, and a retained row of 2026-08-04 that expired (its expire item runs
// first).
var (
	retFirst = time.Date(2026, 8, 1, 2, 0, 0, 0, time.UTC)
	// retKept are the days the table keeps.
	retKept = []string{"2026-09-09", "2026-09-08", "2026-09-07", "2026-09-06", "2026-09-05", "2026-09-04", "2026-09-03",
		"2026-08-31", "2026-08-30", "2026-08-23", "2026-08-20", "2026-08-12"}
)

// TestDockerOffsiteRetention is acceptance 2 (§14.5, §14.6 item 2) in a real repository on
// MinIO: the series is made with `restic backup --time` under the destination row's engine_tag and
// recorded as the table says (engine_snapshots rows, a retained row referencing 2026-08-12 that
// has not expired, a held update's live row referencing 2026-08-20, and a retained row
// referencing 2026-08-04 that expired). Two more snapshots sit in the repository: an unrecorded
// one of 2026-09-09 04:00 (a crashed, cancelled job: the newest, so 09-09 02:00 stays as the base)
// and one of another engine_tag with the same source id (an earlier install: never touched). The
// retention job forgets exactly the 28 snapshots the table lists (2026-08-04 after its row's
// expire item); prune and `restic check` pass. With every keep value 0 only the newest, the base
// and the referenced snapshots stay. The container's clock is the real one, weeks after the
// newest snapshot: every run is also the "long after the last snapshot" variant, and `restic
// snapshots` lists the newest snapshot after every prune.
func TestDockerOffsiteRetention(t *testing.T) {
	o := newOffsite(t)
	o.startMinIO()
	o.writeRandom(map[string]int64{
		"ret/Show/Season 01/Show - S01E01.mkv": 200000,
		"ret/Show/Season 01/Show - S01E02.mkv": 210000,
		"ret/Show/held.mkv":                    50000,
		"ret/Show/deleted.nfo":                 1200,
		"ret/Show/old.nfo":                     1100,
	})
	b := o.startBunkarr("bunkarr", bunkarrOpts{})
	src := b.createSource("Series", "ret")
	d, k := b.createOffsite(b.resticS3("Series off-site", "ret", []int64{src.ID}, map[string]any{
		"retention": map[string]any{"deletedDays": 30, "snapshotDaily": 7, "snapshotWeekly": 4, "snapshotMonthly": 3, "snapshotYearly": 0}}))

	// The series, with restic itself (the tools container reads the same media at the same path).
	var series []retSnap
	var script strings.Builder
	script.WriteString("set -e\n")
	backup := func(at time.Time, tag string, job int) {
		fmt.Fprintf(&script, "id=$(restic backup --json --host bunkarr --time %s --tag %s %s | sed -n 's/.*\"snapshot_id\":\"\\([0-9a-f]*\\)\".*/\\1/p')\necho \"SNAP %s %s $id\"\n",
			shq(at.Format("2006-01-02 15:04:05")),
			shq(fmt.Sprintf("bunkarr,bunkarr-dest:%s,bunkarr-kind:media,bunkarr-job:%d,bunkarr-source:%d,bunkarr-batch:1", tag, job, src.ID)),
			shq(offsiteMedia+"/ret"), tag, at.Format(time.RFC3339))
	}
	for i := 0; i < 40; i++ {
		backup(retFirst.AddDate(0, 0, i), k.EngineTag, 9000+i)
	}
	crashed := time.Date(2026, 9, 9, 4, 0, 0, 0, time.UTC)
	backup(crashed, k.EngineTag, 9999)
	foreignTag := randomHex(16)
	backup(time.Date(2026, 8, 2, 2, 0, 0, 0, time.UTC), foreignTag, 1)
	dir, env := o.kitWorkdir(k)
	out := o.execStdin(o.tools, env, "cd "+shq(dir)+"\n"+script.String(), "sh", "-s")
	ids := map[string]string{} // RFC3339 → id, this row's tag
	var foreignID, crashedID string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 4 || f[0] != "SNAP" || len(f[3]) != 64 {
			continue
		}
		switch {
		case f[1] == foreignTag:
			foreignID = f[3]
		case f[2] == crashed.Format(time.RFC3339):
			crashedID = f[3]
		default:
			ids[f[2]] = f[3]
		}
	}
	if len(ids) != 40 || foreignID == "" || crashedID == "" {
		t.Fatalf("restic backup --time made %d snapshots of the series (want 40), foreign %q, crashed %q:\n%s", len(ids), foreignID, crashedID, out)
	}
	for i := 0; i < 40; i++ {
		at := retFirst.AddDate(0, 0, i)
		series = append(series, retSnap{at, ids[at.Format(time.RFC3339)]})
	}
	byDay := map[string]string{}
	for _, m := range series {
		byDay[m.at.Format(time.DateOnly)] = m.id
	}
	if all := o.resticSnapshots(k, ""); len(all) != 42 {
		t.Fatalf("repository has %d snapshots, want 42", len(all))
	}

	// The records the table describes.
	now := time.Now().UTC()
	b.editDB(func(ctx context.Context, tx *sql.Tx) error {
		for _, m := range series {
			if _, err := tx.ExecContext(ctx, `INSERT INTO engine_snapshots (destination_id, source_id, snapshot_id, created_at, batch, complete, files, bytes)
				VALUES (?, ?, ?, ?, 1, 1, 5, 462300)`, d.ID, src.ID, m.id, db.FormatTime(m.at)); err != nil {
				return err
			}
		}
		row := func(rel, state, reason, ref string, expires *time.Time) error {
			var exp, retained any
			if expires != nil {
				exp, retained = db.FormatTime(*expires), offsiteMedia+"/ret/"+rel
			}
			var r any
			if reason != "" {
				r = reason
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO destination_files (destination_id, source_id, rel_path, source_rel_path, size, mtime_ns,
				state, retained_path, reason, engine_ref, copied_at, retained_at, expires_at)
				VALUES (?, ?, ?, ?, 1000, ?, ?, ?, ?, ?, ?, ?, ?)`, d.ID, src.ID, src.DestFolder+"/"+rel, rel, retFirst.UnixNano(), state, retained, r,
				ref, db.FormatTime(retFirst), retainedAt(expires), exp)
			return err
		}
		inThree, expired := now.AddDate(0, 0, 3), now.Add(-time.Hour)
		if err := row("Show/deleted.nfo", "retained", "deleted", byDay["2026-08-12"], &inThree); err != nil {
			return err
		}
		if err := row("Show/old.nfo", "retained", "deleted", byDay["2026-08-04"], &expired); err != nil {
			return err
		}
		return row("Show/held.mkv", "present", "", byDay["2026-08-20"], nil)
	})

	// The dry run lists what the real run forgets and changes nothing.
	dry := b.retention(d.ID, map[string]any{"dryRun": true})
	requireStatus(t, dry, "completed")
	dryForget := forgotten(b.items(dry.ID))
	if len(dryForget) < 27 || len(o.resticSnapshots(k, "")) != 42 {
		t.Fatalf("retention dry run: %d snapshots to forget (%s); the repository must be unchanged", len(dryForget), dry.Stats)
	}

	// The real run: row B's expire item first, then exactly the 28 snapshots.
	j := b.retention(d.ID, nil)
	requireStatus(t, j, "completed")
	items := b.items(j.ID)
	gone := forgotten(items)
	wantGone := map[string]bool{}
	for _, m := range series {
		if !slices.Contains(retKept, m.at.Format(time.DateOnly)) {
			wantGone[m.id] = true
		}
	}
	if len(wantGone) != 28 {
		t.Fatalf("the table forgets %d snapshots, want 28", len(wantGone))
	}
	if !sameSet(gone, wantGone) {
		t.Fatalf("retention forgot %d snapshots:\n%s\nwant the 28 of the table:\n%s", len(gone), days(gone, series), days(wantGone, series))
	}
	expireB, forget0804 := int64(0), int64(0)
	for _, it := range items {
		switch {
		case it.RelPath == src.DestFolder+"/Show/old.nfo" && it.Action == "expire" && it.Status == "done":
			expireB = it.ID
		case it.RelPath == "snapshots/"+byDay["2026-08-04"]:
			forget0804 = it.ID
		case it.RelPath == src.DestFolder+"/Show/deleted.nfo":
			t.Fatalf("the unexpired retained row got an item: %+v", it)
		}
	}
	if expireB == 0 || forget0804 == 0 || expireB > forget0804 {
		t.Fatalf("the expired row's item (%d) must come before the forget of 2026-08-04 (%d)", expireB, forget0804)
	}
	if st := decodeStats[retentionStatsE2E](t, j); st.SnapshotsForgotten != 28 || st.FilesExpired != 1 {
		t.Fatalf("retention stats: %s", j.Stats)
	}
	wantLeft := map[string]bool{crashedID: true, foreignID: true}
	for _, day := range retKept {
		wantLeft[byDay[day]] = true
	}
	o.requireSnapshots(k, wantLeft, crashedID, series)

	// Prune and check (restic check in the job and again here), then every keep value 0.
	p := b.retention(d.ID, map[string]any{"prune": true})
	requireStatus(t, p, "completed")
	if st := decodeStats[retentionStatsE2E](t, p); !st.Pruned || st.SnapshotsForgotten != 0 {
		t.Fatalf("retention with prune: %s", p.Stats)
	}
	o.resticCheck(k, false)
	o.requireSnapshots(k, wantLeft, crashedID, series)

	zero := 0
	b.api.call(viaKey, 200, "PUT", fmt.Sprintf("/destinations/%d", d.ID), map[string]any{"retention": map[string]any{"deletedDays": 30,
		"snapshotDaily": zero, "snapshotWeekly": zero, "snapshotMonthly": zero, "snapshotYearly": zero}}, nil)
	z := b.retention(d.ID, map[string]any{"prune": true})
	requireStatus(t, z, "completed")
	// Only the newest (the crashed 04:00 one), the base (09-09 02:00), the two referenced ones and
	// the other engine_tag's stay.
	wantLeft = map[string]bool{crashedID: true, foreignID: true, byDay["2026-09-09"]: true, byDay["2026-08-20"]: true, byDay["2026-08-12"]: true}
	o.requireSnapshots(k, wantLeft, crashedID, series)
	o.resticCheck(k, true)
	o.auditAll()
}

// retainedAt is the retained_at of a retained row (the day after the table's first snapshot).
func retainedAt(expires *time.Time) any {
	if expires == nil {
		return nil
	}
	return db.FormatTime(retFirst.AddDate(0, 0, 1))
}

// forgotten returns the snapshot ids of a retention job's forget items that are done.
func forgotten(items []offItem) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		if id, ok := strings.CutPrefix(it.RelPath, "snapshots/"); ok && it.Action == "expire" && (it.Status == "done" || it.Status == "skipped" || it.Status == "pending" || it.Status == "planned") {
			out[id] = true
		}
	}
	return out
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// retSnap is one snapshot of the series.
type retSnap struct {
	at time.Time
	id string
}

// days renders a set of snapshot ids as the series' days.
func days(set map[string]bool, series []retSnap) string {
	var out []string
	for _, m := range series {
		if set[m.id] {
			out = append(out, m.at.Format(time.DateOnly))
		}
	}
	return strings.Join(out, " ")
}

// requireSnapshots requires the repository to hold exactly want, with newest still listed.
func (o *offsite) requireSnapshots(k kitInfo, want map[string]bool, newest string, series []retSnap) {
	o.t.Helper()
	got := map[string]bool{}
	var times []string
	for _, s := range o.resticSnapshots(k, "") {
		got[s.ID] = true
		times = append(times, s.Time.UTC().Format(time.RFC3339))
	}
	if !sameSet(got, want) || !got[newest] {
		o.t.Fatalf("the repository holds %d snapshots %v, want %d: %s plus the newest %s and the other engine_tag's", len(got), times, len(want),
			days(want, series), newest)
	}
}
