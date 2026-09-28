//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// bwEngine is one engine of the bandwidth and window tests.
type bwEngine struct {
	name string
	body func(b *bunkarrC, name, prefix string, sources []int64, extra map[string]any) map[string]any
}

var bwEngines = []bwEngine{{"restic", (*bunkarrC).resticS3}, {"rclone", (*bunkarrC).cryptS3}}

// allDays are the days of a timetable entry or window that applies every day.
var allDays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// TestDockerOffsiteBandwidth is acceptance 5 (docs/design/phase4.md §9, S27, §14.6 item 5) on both
// engines (restic reaches MinIO through its rclone backend, so rclone's timetable applies to both):
//   - 40 MiB at 2 MiB/s takes at least 18 s (90 % of the expected time);
//   - a timetable that switches from 1 MiB/s to 8 MiB/s about 10 s into a transfer shows both
//     limits (Progress.limitBytesPerSec) while the job runs;
//   - a transfer window (BUNKARR_TEST_WINDOW) that closes 15 s into a 100 MiB transfer at 2 MiB/s
//     defers the job: it is queued with notBefore at the reopening, no item failed; it resumes then
//     and completes, and the destination verifies;
//   - a file larger than the whole window at the rate in force fails its item with the warning,
//     and the job completes.
func TestDockerOffsiteBandwidth(t *testing.T) {
	o := newOffsite(t)
	o.startMinIO()
	b := o.startBunkarr("bunkarr", bunkarrOpts{})

	// 1. The limit holds.
	for _, e := range bwEngines {
		dir := "bw-" + e.name + "/rate"
		o.writeRandom(map[string]int64{dir + "/Rate (2020)/Rate (2020).mkv": 40 << 20})
		src := b.createSource("Rate "+e.name, dir)
		d, _ := b.createOffsite(e.body(b, "Rate "+e.name, "bw-rate-"+e.name, []int64{src.ID},
			map[string]any{"bandwidth": map[string]any{"uploadKiBps": 2048}}))
		j, limits := o.watchLimits(b, []int64{d.ID})
		r := j[0]
		requireStatus(t, r.apiJob, "completed")
		took := r.FinishedAt.Sub(*r.StartedAt)
		if took < 18*time.Second {
			t.Fatalf("%s: 40 MiB at 2 MiB/s took %s, want at least 18 s: %s", e.name, took, r.Stats)
		}
		if !slices.Contains(limits[r.ID], 2<<20) {
			t.Fatalf("%s: progress limits %v, want 2 MiB/s (2097152)", e.name, limits[r.ID])
		}
		t.Logf("%s: 40 MiB at 2 MiB/s took %s", e.name, took.Round(time.Second))
	}

	// 2. A timetable change point during a transfer: 1 MiB/s until the next minute, then 8 MiB/s.
	// Both engines at once (two upload slots), started about 12 s before the change point.
	now := time.Now().UTC()
	change := now.Truncate(time.Minute).Add(time.Minute)
	if change.Sub(now) < 25*time.Second {
		change = change.Add(time.Minute)
	}
	var ids []int64
	for _, e := range bwEngines {
		dir := "bw-" + e.name + "/timetable"
		o.writeRandom(map[string]int64{dir + "/Timetable (2021)/Timetable (2021).mkv": 48 << 20})
		src := b.createSource("Timetable "+e.name, dir)
		d, _ := b.createOffsite(e.body(b, "Timetable "+e.name, "bw-tt-"+e.name, []int64{src.ID}, map[string]any{"bandwidth": map[string]any{
			"uploadKiBps": 1024,
			"timetable": []map[string]any{{"days": allDays, "from": change.Format("15:04"), "to": change.Add(20 * time.Minute).Format("15:04"),
				"uploadKiBps": 8192}},
		}}))
		ids = append(ids, d.ID)
	}
	time.Sleep(time.Until(change.Add(-12 * time.Second)))
	jobs, limits := o.watchLimits(b, ids)
	for i, j := range jobs {
		requireStatus(t, j.apiJob, "completed")
		if !slices.Contains(limits[j.ID], 1<<20) || !slices.Contains(limits[j.ID], 8<<20) {
			t.Fatalf("%s: progress limits %v over a change point at %s (job %v-%v), want 1048576 and 8388608", bwEngines[i].name,
				limits[j.ID], change.Format(time.TimeOnly), j.StartedAt, j.FinishedAt)
		}
		t.Logf("%s: limits seen during the transfer: %v", bwEngines[i].name, limits[j.ID])
	}

	// 3. The window closes 15 s into a 100 MiB transfer at 2 MiB/s.
	for _, e := range bwEngines {
		dir := "bw-" + e.name + "/window"
		files := map[string]int64{}
		for i := 1; i <= 10; i++ {
			files[fmt.Sprintf("%s/Season 01/Show - S01E%02d.mkv", dir, i)] = 10 << 20
		}
		o.writeRandom(files)
		src := b.createSource("Window "+e.name, dir)
		d, _ := b.createOffsite(e.body(b, "Window "+e.name, "bw-win-"+e.name, []int64{src.ID},
			map[string]any{"bandwidth": map[string]any{"uploadKiBps": 2048}}))
		start := time.Now().UTC().Truncate(time.Second)
		closeAt, reopen := start.Add(15*time.Second), start.Add(40*time.Second)
		b.setWindow(d.ID, start.Add(-10*time.Minute), closeAt, reopen)
		j := b.startJob(fmt.Sprintf("/destinations/%d/sync", d.ID), nil)
		deferred := false
		deadline := time.Now().Add(10 * time.Minute)
		for {
			cur := b.job(j.ID)
			if cur.Status == "queued" && cur.NotBefore != nil && !deferred {
				deferred = true
				if d := cur.NotBefore.Sub(reopen); d < -2*time.Second || d > 2*time.Second || cur.Deferrals < 1 {
					t.Fatalf("%s: deferred job %d: notBefore %v, deferrals %d; want the reopening %v", e.name, j.ID, cur.NotBefore, cur.Deferrals, reopen)
				}
				for _, it := range b.items(j.ID) {
					if it.Status == "failed" {
						t.Fatalf("%s: item %s failed at the window's end: %s", e.name, it.RelPath, it.Error)
					}
				}
				t.Logf("%s: job %d waits for the window until %s (%d deferral)", e.name, j.ID, cur.NotBefore.Format(time.TimeOnly), cur.Deferrals)
			}
			if cur.final() {
				requireStatus(t, cur.apiJob, "completed")
				if !deferred {
					t.Fatalf("%s: job %d completed without waiting for the window (window %s-%s): %s", e.name, j.ID, start, closeAt, cur.Stats)
				}
				if cur.FinishedAt == nil || cur.FinishedAt.Before(reopen) {
					t.Fatalf("%s: job %d finished at %v, before the window reopened at %v", e.name, j.ID, cur.FinishedAt, reopen)
				}
				if st := decodeStats[engineSync](t, cur.apiJob); st.FilesFailed != 0 || st.FilesCopied != 10 || st.Deferrals < 1 {
					t.Fatalf("%s: the deferred sync: %s", e.name, cur.Stats)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: job %d still %s after 10 minutes", e.name, j.ID, cur.Status)
			}
			time.Sleep(500 * time.Millisecond)
		}
		if logs := strings.Join(b.jobLogs(j.ID), "\n"); !strings.Contains(logs, "transfer window") {
			t.Fatalf("%s: the job log does not mention the transfer window:\n%s", e.name, logs)
		}
		requireVerified(t, b.verify(d.ID))
	}

	// 4. A file larger than the whole window at the rate in force: 100 MiB at 2 MiB/s does not fit
	// a 41 s window. Its item fails with the warning; the rest is backed up and the job completes.
	for _, e := range bwEngines {
		dir := "bw-" + e.name + "/toolarge"
		o.writeRandom(map[string]int64{dir + "/Remux (2022)/Remux (2022).mkv": 100 << 20, dir + "/Remux (2022)/Remux (2022).nfo": 4000})
		src := b.createSource("Too large "+e.name, dir)
		d, _ := b.createOffsite(e.body(b, "Too large "+e.name, "bw-big-"+e.name, []int64{src.ID},
			map[string]any{"bandwidth": map[string]any{"uploadKiBps": 2048}}))
		now := time.Now().UTC()
		b.setWindow(d.ID, now.Add(-time.Second), now.Add(40*time.Second), now.Add(time.Hour))
		j := b.sync(d.ID, nil)
		requireStatus(t, j, "completed_with_warnings")
		st := decodeStats[engineSync](t, j)
		if st.FilesFailed != 1 || st.FilesCopied != 1 || j.Warnings == 0 {
			t.Fatalf("%s: sync with a file larger than the window: %s (warnings %d)", e.name, j.Stats, j.Warnings)
		}
		found := false
		for _, it := range b.items(j.ID) {
			if it.Status == "failed" {
				found = strings.HasSuffix(it.RelPath, ".mkv") && strings.Contains(it.Error, "larger than the transfer window")
				if !found {
					t.Fatalf("%s: failed item %s: %q", e.name, it.RelPath, it.Error)
				}
			}
		}
		if !found {
			t.Fatalf("%s: no failed item for the file larger than the window", e.name)
		}
	}
	b.setWindow(0, time.Unix(0, 0), time.Unix(1, 0), time.Unix(2, 0))
	o.auditAll()
}

// watchLimits starts a sync of every destination at once and polls the jobs until they end,
// collecting the distinct limitBytesPerSec values their progress showed.
func (o *offsite) watchLimits(b *bunkarrC, dests []int64) ([]offJob, map[int64][]int64) {
	o.t.Helper()
	var ids []int64
	for _, d := range dests {
		ids = append(ids, b.startJob(fmt.Sprintf("/destinations/%d/sync", d), nil).ID)
	}
	limits := map[int64][]int64{}
	deadline := time.Now().Add(15 * time.Minute)
	for {
		var out []offJob
		done := true
		for _, id := range ids {
			j := b.job(id)
			if l := j.Progress.LimitBytesPerSec; l > 0 && !slices.Contains(limits[id], l) {
				limits[id] = append(limits[id], l)
			}
			out = append(out, j)
			done = done && j.final()
		}
		if done {
			return out, limits
		}
		if time.Now().After(deadline) {
			o.t.Fatalf("jobs %v did not end within 15 minutes", ids)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
