package enginerun

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

func intp(v int) *int { return &v }

// dailySyncs runs n syncs one day apart, each with a new file, and returns their snapshots.
func (h *harness) dailySyncs(n int) []string {
	h.t.Helper()
	var out []string
	for i := range n {
		h.writeSrc("day"+string(rune('a'+i))+".mkv", content("d"+string(rune('a'+i)), 500+i), i)
		st, _ := h.mustSync(jobs.Params{})
		if len(st.Snapshots) != 1 {
			h.t.Fatalf("day %d: %d snapshots", i, len(st.Snapshots))
		}
		out = append(out, st.Snapshots[0].SnapshotID)
		h.advance(24 * time.Hour)
	}
	return out
}

func newRetentionHarness(t *testing.T, daily int) *harness {
	return newHarness(t, harnessOptions{kind: engines.Restic, retention: &destinations.Retention{DeletedDays: 30,
		SnapshotDaily: intp(daily), SnapshotWeekly: intp(0), SnapshotMonthly: intp(0), SnapshotYearly: intp(0)}})
}

// callIndex returns the index of the first call of a restic subcommand after from (-1: none).
func (h *harness) callIndex(sub string, from int) int {
	for i, c := range h.runner.Calls() {
		if i >= from && c.Binary == proc.Restic && c.Subcommand() == sub {
			return i
		}
	}
	return -1
}

// TestResticRetentionForget: the forget step forgets exactly KeepMedia's complement and the
// requests FilterForget accepts, by id, after the guarded unlock; snapshots of another
// engine_tag are untouched; then prune and a structure check run (§6.5, S24, D28).
func TestResticRetentionForget(t *testing.T) {
	h := newRetentionHarness(t, 2)
	s := h.dailySyncs(5)
	foreignTags, _ := restic.Tags(restic.TagInput{EngineTag: "0123456789abcdef0123456789abcdef", Kind: engines.VersionMedia, JobID: 1,
		SourceID: h.src.ID, Batch: 1})
	foreign := h.restic.addSnapshot(h.now().Add(-40*24*time.Hour), foreignTags, nil)
	m1tags, _ := restic.Tags(restic.TagInput{EngineTag: h.dest.EngineTag, Kind: engines.VersionManifest, JobID: 90, Version: "20260901T000000Z"})
	m2tags, _ := restic.Tags(restic.TagInput{EngineTag: h.dest.EngineTag, Kind: engines.VersionManifest, JobID: 91, Version: "20260902T000000Z"})
	m1 := h.restic.addSnapshot(h.now().Add(-30*24*time.Hour), m1tags, nil)
	m2 := h.restic.addSnapshot(h.now().Add(-29*24*time.Hour), m2tags, nil)
	if err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		return h.svc.RequestForget(h.ctx, tx, h.dest.ID, m1, engines.VersionManifest, "pruned by the manifest runner")
	}); err != nil {
		t.Fatal(err)
	}
	from := len(h.runner.Calls())
	res, j := h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	st := res.Stats.(RetentionStats)
	want := []string{s[3], s[4], foreign, m2}
	got := h.restic.ids()
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("snapshots left %v, want %v", shorts(got), shorts(want))
	}
	if st.SnapshotsForgotten != 4 || !st.Pruned || st.ForgetRequests != 1 {
		t.Errorf("stats %+v", st)
	}
	unlock, forget, prune := h.callIndex("unlock", from), h.callIndex("forget", from), h.callIndex("prune", from)
	check := h.callIndex("check", prune)
	if !(unlock >= 0 && unlock < forget && forget < prune && prune < check) {
		t.Errorf("order: unlock %d, forget %d, prune %d, check %d", unlock, forget, prune, check)
	}
	for _, c := range h.callsOf(proc.Restic, "forget") {
		for _, a := range c.Args {
			if len(a) > 7 && a[:7] == "--keep-" {
				t.Errorf("forget with a policy: %s", c)
			}
		}
	}
	reqs, _ := h.svc.forgetRequests(h.ctx, h.dest.ID)
	if len(reqs) != 0 {
		t.Errorf("requests left %+v", reqs)
	}
	rows, _ := h.svc.snapshotRows(h.ctx, h.dest.ID)
	if len(rows) != 2 {
		t.Errorf("%d rows left, want 2", len(rows))
	}
	var forgetItems int
	for _, it := range h.items(j.ID) {
		d, _ := parseItem(it)
		if d.Check == checkExpireSnapshot && it.Status == jobs.ItemDone && d.Group != "" && d.Snapshot != "" {
			forgetItems++
		}
	}
	if forgetItems != 4 {
		t.Errorf("%d forget items done", forgetItems)
	}
	// The next day nothing is due: no prune (pruneEveryDays), no unlock without a forget.
	h.advance(24 * time.Hour)
	from = len(h.runner.Calls())
	res, _ = h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	if res.Stats.(RetentionStats).Pruned || h.callIndex("prune", from) >= 0 || h.callIndex("unlock", from) >= 0 {
		t.Error("a prune or unlock ran although nothing was due")
	}
	// params.prune prunes now; after pruneEveryDays it is due again.
	res, _ = h.mustRun(jobs.TypeRetention, false, jobs.Params{Prune: true})
	if !res.Stats.(RetentionStats).Pruned {
		t.Error("params.prune did not prune")
	}
	h.advance(8 * 24 * time.Hour)
	res, _ = h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	if !res.Stats.(RetentionStats).Pruned {
		t.Error("no prune after pruneEveryDays")
	}
}

func shorts(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = short(id)
	}
	return out
}

// TestResticRetentionRecheck: right before the forget the repository is listed again (S24): a
// snapshot that became the newest of its group meanwhile is kept.
func TestResticRetentionRecheck(t *testing.T) {
	h := newRetentionHarness(t, 0)
	s := h.dailySyncs(3)
	base := h.restic.snapshotCalls
	h.restic.onSnapshots = func(n int) {
		if n == base+2 { // the re-listing right before the forget
			h.restic.removeLocked(s[2]) // another process removed the newest: s[1] is the newest now
		}
	}
	_, j := h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	if h.restic.snapshot(s[1]) == nil {
		t.Error("the snapshot that became the newest was forgotten")
	}
	if h.restic.snapshot(s[0]) != nil {
		t.Error("the unprotected snapshot was not forgotten")
	}
	var kept bool
	for _, it := range h.items(j.ID) {
		d, _ := parseItem(it)
		if d.Snapshot == s[1] && it.Status == jobs.ItemSkipped {
			kept = true
		}
	}
	if !kept {
		t.Error("no skipped item for the re-checked snapshot")
	}
}

// TestResticRetentionDryRun: a dry run lists the expire items and the snapshots it would forget,
// and forgets, prunes and unlocks nothing (S9).
func TestResticRetentionDryRun(t *testing.T) {
	h := newRetentionHarness(t, 0)
	s := h.dailySyncs(3)
	from := len(h.runner.Calls())
	_, j := h.mustRun(jobs.TypeRetention, true, jobs.Params{})
	for _, sub := range []string{"forget", "prune", "unlock", "list", "snapshots"} {
		if h.callIndex(sub, from) >= 0 {
			t.Errorf("a dry run ran restic %s", sub)
		}
	}
	var would []string
	for _, it := range h.items(j.ID) {
		d, _ := parseItem(it)
		if d.Check == checkExpireSnapshot {
			would = append(would, d.Snapshot)
		}
	}
	slices.Sort(would)
	want := []string{s[0], s[1]}
	slices.Sort(want)
	if !slices.Equal(would, want) {
		t.Errorf("would forget %v, want %v", shorts(would), shorts(want))
	}
	if len(h.restic.ids()) != 3 {
		t.Error("a dry run forgot snapshots")
	}
}

// TestResticRetentionForeignLock: a lock with this container's host name that is newer than
// this process and not a child's refuses the guarded unlock; nothing is forgotten (§6.7).
func TestResticRetentionForeignLock(t *testing.T) {
	h := newRetentionHarness(t, 0)
	h.dailySyncs(3)
	h.restic.locks["cafebabe00000000000000000000000000000000000000000000000000000000"] = restic.Lock{Time: h.now(), Hostname: "bunkarr-test",
		PID: 999999, Exclusive: false}
	j := h.newJob(jobs.TypeRetention, false, jobs.Params{})
	_, err := h.runJob(j)
	if !errors.Is(err, restic.ErrForeignLock) {
		t.Fatalf("err = %v", err)
	}
	if len(h.restic.ids()) != 3 || len(h.callsOf(proc.Restic, "forget")) != 0 {
		t.Error("snapshots were forgotten")
	}
}

// TestResticRetentionExpiry: expired rows are deleted with the Phase 1-3 holds (the
// irreplaceable flag, the replaced-version hold); nothing in the repository changes by it.
func TestResticRetentionExpiry(t *testing.T) {
	h := newRetentionHarness(t, 7)
	h.firstSnapshot(map[string]string{"del.mkv": content("d", 500), "flag.mkv": content("f", 600), "upd.mkv": content("u", 700),
		"keep.mkv": content("k", 800)})
	h.removeSrc("del.mkv")
	h.removeSrc("flag.mkv")
	h.writeSrc("upd.mkv", content("U", 750), 20)
	h.advance(time1h)
	h.mustSync(jobs.Params{})
	h.exec(`UPDATE destination_files SET state = 'missing' WHERE id = ?`, h.mustLive(h.dp("upd.mkv")).ID)
	h.withRetentionTiers("flag.mkv")
	h.advance(31 * 24 * time.Hour)
	res, _ := h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	st := res.Stats.(RetentionStats)
	if st.FilesExpired != 1 || st.FilesHeld != 2 {
		t.Errorf("stats %+v", st)
	}
	left := map[string]string{}
	for _, r := range h.retained() {
		left[r.SourceRelPath] = r.Reason
	}
	if _, ok := left["del.mkv"]; ok || left["flag.mkv"] != syncer.ReasonDeleted || left["upd.mkv"] != syncer.ReasonReplaced {
		t.Errorf("retained rows left %v", left)
	}
	h.assertResticRefs(t)
	_ = context.Background
}

// TestResticCheckAfterPruneResumes: the restic check after a prune that the window's end
// interrupted runs in the resumed job although no prune is due any more (S24).
func TestResticCheckAfterPruneResumes(t *testing.T) {
	h := newHarness(t, harnessOptions{kind: engines.Restic, bandwidth: nightWindow(false, 0)})
	h.setClock(time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC))
	h.writeSrc("a.mkv", content("a", 1000), 1)
	h.mustSync(jobs.Params{})
	h.restic.hangExclusive["check"] = true
	h.restic.exclusiveHost = "nas-before-restart"
	h.setClock(time.Date(2026, 9, 28, 6, 59, 59, 950_000_000, time.UTC))
	j := h.newJob(jobs.TypeRetention, false, jobs.Params{})
	_, err := h.runJob(j)
	mustDefer(t, err)
	if h.restic.prunes != 1 || len(h.callsOf(proc.Restic, "check")) != 1 {
		t.Fatalf("prunes %d, checks %d", h.restic.prunes, len(h.callsOf(proc.Restic, "check")))
	}
	delete(h.restic.hangExclusive, "check")
	h.setClock(time.Date(2026, 9, 29, 1, 0, 0, 0, time.UTC))
	j.Deferrals = 1
	if _, err := h.runJob(j); err != nil {
		t.Fatal(err)
	}
	h.closeJob(j.ID)
	if h.restic.prunes != 1 || len(h.callsOf(proc.Restic, "check")) != 2 {
		t.Errorf("after the resume: prunes %d, checks %d", h.restic.prunes, len(h.callsOf(proc.Restic, "check")))
	}
	if st, _ := h.svc.State(h.ctx, h.dest.ID); st.LastCheckAt == nil {
		t.Error("no check recorded")
	}
	// The next retention job, with no prune due, runs no check.
	h.advance(time1h)
	h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	if n := len(h.callsOf(proc.Restic, "check")); n != 2 {
		t.Errorf("checks %d", n)
	}
}

// TestResticRetentionDryRunOrphans: a dry run keeps the snapshots of a deleted source (an orphan
// group, source_id NULL) as the real forget does, instead of grouping them with source 1, and
// leaves out a forget request whose snapshot a config version references (S9, S24).
func TestResticRetentionDryRunOrphans(t *testing.T) {
	h := newRetentionHarness(t, 0)
	s := h.dailySyncs(2)
	orphanTags, err := restic.Tags(restic.TagInput{EngineTag: h.dest.EngineTag, Kind: engines.VersionMedia, JobID: 1, SourceID: 99, Batch: 1})
	if err != nil {
		t.Fatal(err)
	}
	orphan := h.restic.addSnapshot(h.now().Add(-72*time.Hour), orphanTags, map[string]fakeObj{})
	h.exec(`INSERT INTO engine_snapshots (destination_id, source_id, job_id, snapshot_id, created_at, batch, complete, files, bytes,
		data_added, summary) VALUES (?, NULL, NULL, ?, ?, 1, 1, 0, 0, 0, '{}')`, h.dest.ID, orphan, db.FormatTime(h.now().Add(-72*time.Hour)))
	referenced := strings.Repeat("cd", 32)
	h.svc.o.ConfigRefs = func(context.Context, int64) ([]string, error) { return []string{referenced}, nil }
	if err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		return h.svc.RequestForget(h.ctx, tx, h.dest.ID, referenced, engines.VersionPlexDB, "pruned")
	}); err != nil {
		t.Fatal(err)
	}
	_, j := h.mustRun(jobs.TypeRetention, true, jobs.Params{})
	var would []string
	for _, it := range h.items(j.ID) {
		if d, _ := parseItem(it); d.Check == checkExpireSnapshot {
			would = append(would, d.Snapshot)
		}
	}
	if !slices.Equal(would, []string{s[0]}) {
		t.Errorf("would forget %v, want %v", shorts(would), shorts([]string{s[0]}))
	}
	h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	if h.restic.snapshot(orphan) == nil || h.restic.snapshot(s[0]) != nil {
		t.Errorf("the real run: orphan kept %v, %s forgotten %v", h.restic.snapshot(orphan) != nil, short(s[0]), h.restic.snapshot(s[0]) == nil)
	}
}
