package syncer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

// fakeTiers decides tiers by source path (anything else is full by the fallback).
type fakeTiers struct {
	mu       sync.Mutex
	cat      *catalog.Store
	revision int64
	byRel    map[string]tiers.Decision
	arrAdded map[string]time.Time
	covered  map[string]int64
	// onDecisions runs after every Decisions call (a test changes the rules "meanwhile");
	// beforeDecisions runs before it decides (the rules changed before the scan's decisions).
	onDecisions     func()
	beforeDecisions func()
	moveCalls       [][3]string
	afterSync       map[int64][]tiers.Move
	stale           []tiers.Warning
}

func newFakeTiers(h *harness) *fakeTiers {
	return &fakeTiers{cat: h.cat, revision: 1, byRel: map[string]tiers.Decision{}, covered: map[string]int64{}, afterSync: map[int64][]tiers.Move{}}
}

func (f *fakeTiers) set(rel string, tier tiers.Tier) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tier == tiers.Full {
		delete(f.byRel, rel)
		return
	}
	f.byRel[rel] = tiers.Decision{Tier: tier, RuleID: 7, RuleName: "demote", Reasons: []tiers.Reason{}, Unknown: []tiers.Reason{}}
}

func (f *fakeTiers) setUnknown(rel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byRel[rel] = tiers.Decision{Tier: tiers.Full, RuleID: 3, RuleName: "tagged", UnknownPromoted: true,
		Reasons: []tiers.Reason{{RuleID: 3, Field: tiers.FieldArrTag, Result: tiers.Unknown, Why: "Radarr cache is 31 h old"}}, Unknown: []tiers.Reason{}}
}

func (f *fakeTiers) Decisions(ctx context.Context, _ tiers.Queryer, _ int64, src catalog.Source) (*tiers.SourceDecisions, error) {
	if f.beforeDecisions != nil {
		f.beforeDecisions()
	}
	f.mu.Lock()
	byFile := map[int64]tiers.Decision{}
	err := f.cat.Live(ctx, src.ID, func(cf catalog.File) error {
		if d, ok := f.byRel[cf.RelPath]; ok {
			d.Revision = f.revision
			byFile[cf.ID] = d
		}
		return nil
	})
	sd := tiers.NewSourceDecisions(src.ID, f.revision, byFile, f.arrAdded)
	sd.Stale = f.stale
	hook := f.onDecisions
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return sd, err
}

func (f *fakeTiers) Revision(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revision, nil
}

func (f *fakeTiers) MoveFlagsTx(_ context.Context, _ *sql.Tx, sourceID int64, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moveCalls = append(f.moveCalls, [3]string{fmt.Sprint(sourceID), from, to})
	return nil
}

func (f *fakeTiers) FlagsAfterSync(_ context.Context, sourceID int64, moves []tiers.Move) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.afterSync[sourceID] = append(f.afterSync[sourceID], moves...)
	return nil
}

func (f *fakeTiers) Coverage(context.Context) (func(int64, string) (int64, bool), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cov := maps(f.covered)
	return func(_ int64, rel string) (int64, bool) {
		id, ok := cov[rel]
		return id, ok
	}, nil
}

func maps(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// withTiers recreates the runners with a tier engine.
func (h *harness) withTiers(t Tiers) {
	o := Options{DB: h.db, Store: h.store, Catalog: h.cat, Scanner: h.scanner, Destinations: h.dests, Now: h.now, Tiers: t}
	h.sync = NewSyncRunner(o)
	h.sync.planBatch = 3
	h.sync.recheckEvery = 4
	h.verify = NewVerifyRunner(o)
	h.retention = NewRetentionRunner(RetentionOptions{Options: o, PruneHistory: h.jq.PruneHistory})
}

// realEngine is the real tier engine over the harness's database.
func realEngine(t *testing.T, h *harness) *tiers.Engine {
	t.Helper()
	kr, err := config.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return tiers.New(tiers.Options{DB: h.db, Catalog: h.cat, Index: mediaindex.NewStore(h.db, h.cat), Integrations: integrations.NewStore(h.db, kr),
		Now: h.now})
}

// releasePreview runs a release dry run and stores its stats as the job manager does (the
// release checks them).
func (h *harness) releasePreview() (SyncStats, jobs.Job) {
	h.t.Helper()
	_, st, j := h.mustSync(true, jobs.Params{ReleaseDemoted: true})
	b, err := json.Marshal(st)
	if err != nil {
		h.t.Fatal(err)
	}
	h.dbExec(`UPDATE jobs SET stats = ? WHERE id = ?`, string(b), j.ID)
	return st, j
}

// itemList renders a job's items as "action rel [status]".
func (h *harness) itemList(jobID int64) []string {
	var out []string
	for _, it := range h.items(jobID) {
		out = append(out, fmt.Sprintf("%s %s %s", it.Action, it.RelPath, it.Status))
	}
	slices.Sort(out)
	return out
}

func detailOf(t *testing.T, it jobs.Item) Detail {
	t.Helper()
	d, err := parseDetail(it)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestTiersNoRulesPlansLikePhase1 is acceptance 8's planner half: with the real engine and no
// rules, a sync plans exactly what a sync without tiers plans (the same items), and a second sync
// plans nothing.
func TestTiersNoRulesPlansLikePhase1(t *testing.T) {
	build := func(h *harness) {
		h.writeSrc("a.mkv", content("a", 300), 1)
		h.writeSrc("d/b.mkv", content("b", 400), 2)
		h.writeSrc("d/b.en.srt", content("s", 40), 3)
		h.linkSrc("a.mkv", "d/a-link.mkv")
	}
	plain := newHarness(t)
	build(plain)
	_, _, pj := plain.mustSync(true, jobs.Params{})
	tiered := newHarness(t)
	build(tiered)
	tiered.withTiers(realEngine(t, tiered))
	_, st, tj := tiered.mustSync(true, jobs.Params{})
	if a, b := plain.itemList(pj.ID), tiered.itemList(tj.ID); !slices.Equal(a, b) {
		t.Fatalf("plans differ:\nwithout tiers %v\nwith no rules %v", a, b)
	}
	if st.Tiers == nil || st.Tiers.Full.Files != 4 || st.Tiers.Manifest.Files != 0 || st.TierRevision != 0 {
		t.Fatalf("tier stats %+v %+v", st.Tiers, st)
	}
	for _, it := range tiered.items(tj.ID) {
		if d := detailOf(t, it); d.Tier == nil || d.Tier.Tier != tiers.Full || d.Tier.RuleName != tiers.FallbackRuleName {
			t.Errorf("%s: detail tier %+v", it.RelPath, d.Tier)
		}
	}
	tiered.mustSync(false, jobs.Params{})
	_, st, _ = tiered.mustSync(false, jobs.Params{})
	if st.FilesPlanned != 0 {
		t.Fatalf("second sync planned %d", st.FilesPlanned)
	}
	tiered.assertConverged(t)
}

func TestTiersNonFullFilesAreNotCopied(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("full.mkv", content("f", 300), 1)
	h.writeSrc("man.mkv", content("m", 400), 2)
	h.writeSrc("skip.mkv", content("s", 500), 3)
	ft.set("man.mkv", tiers.Manifest)
	ft.set("skip.mkv", tiers.Skip)
	_, st, j := h.mustSync(true, jobs.Params{})
	want := []string{"copy movies/full.mkv pending", "skip movies/man.mkv pending", "skip movies/skip.mkv pending"}
	if got := h.itemList(j.ID); !slices.Equal(got, want) {
		t.Fatalf("dry run items %v", got)
	}
	for _, it := range h.items(j.ID) {
		d := detailOf(t, it)
		if it.Action == jobs.ActionSkip && (d.Reason != reasonNotCopied || d.Tier == nil || d.Tier.Tier == tiers.Full ||
			!strings.HasPrefix(d.Note, "not copied: ")) {
			t.Errorf("skip item %s: %+v", it.RelPath, d)
		}
	}
	if st.FilesPlanned != 1 || st.BytesPlanned != 300 || st.Tiers.Manifest.Files != 1 || st.Tiers.Skip.Bytes != 500 || st.Tiers.Full.Files != 1 {
		t.Fatalf("dry run stats %+v tiers %+v", st, st.Tiers)
	}
	_, st, j = h.mustSync(false, jobs.Params{})
	if got := h.itemList(j.ID); !slices.Equal(got, []string{"copy movies/full.mkv done"}) {
		t.Fatalf("real run items %v (a real run records no skip items)", got)
	}
	if exists(h.dstPath("movies/man.mkv")) || exists(h.dstPath("movies/skip.mkv")) || !exists(h.dstPath("movies/full.mkv")) {
		t.Fatal("the destination holds the wrong files")
	}
	if st.FilesCopied != 1 || st.Tiers.Manifest.Files != 1 {
		t.Fatalf("stats %+v", st)
	}
	if _, st, _ = h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
		t.Fatalf("a second sync planned %d", st.FilesPlanned)
	}
}

// TestTiersKeptFiles: demoting backed-up files never removes or changes them (S15, acceptance 9).
func TestTiersKeptFiles(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("a.mkv", content("a", 300), 1)
	h.writeSrc("b.mkv", content("b", 400), 2)
	h.writeSrc("Old/c.mkv", content("c", 500), 3)
	h.mustSync(false, jobs.Params{})
	before := regularFiles(t, h.dstDir)

	ft.set("a.mkv", tiers.Manifest)
	ft.set("b.mkv", tiers.Skip)
	ft.set("New/c.mkv", tiers.Manifest)
	// The source changes a kept file, verify marks another missing, and a backed-up file is moved
	// into a manifest-tier folder of the same source.
	h.writeSrc("a.mkv", content("A", 350), 5)
	rb, _ := h.liveRecord("movies/b.mkv")
	if err := h.store.markMissing(h.ctx, rb.ID); err != nil {
		t.Fatal(err)
	}
	h.renameSrc("Old/c.mkv", "New/c.mkv")
	_, st, j := h.mustSync(true, jobs.Params{})
	got := h.itemList(j.ID)
	want := []string{"move movies/New/c.mkv pending", "skip movies/a.mkv pending"}
	if !slices.Equal(got, want) {
		t.Fatalf("dry run items %v, want %v (no update, repair or retain of kept files)", got, want)
	}
	for _, it := range h.items(j.ID) {
		if d := detailOf(t, it); it.Action == jobs.ActionSkip && (d.Reason != reasonKept || d.RecordID == 0) {
			t.Errorf("kept item %+v", d)
		}
	}
	if st.FilesKept != 1 || st.BytesKept != 350 {
		t.Errorf("kept stats %d %d", st.FilesKept, st.BytesKept)
	}
	_, st, _ = h.mustSync(false, jobs.Params{})
	if st.FilesMoved != 1 || st.FilesRetained != 0 || st.FilesCopied != 0 || st.FilesUpdated != 0 {
		t.Fatalf("real run %+v", st)
	}
	after := regularFiles(t, h.dstDir)
	if after["movies/a.mkv"] != before["movies/a.mkv"] || after["movies/b.mkv"] != before["movies/b.mkv"] ||
		after["movies/New/c.mkv"] != before["movies/Old/c.mkv"] {
		t.Fatal("a kept file changed at the destination")
	}
	if r, ok := h.liveRecord("movies/New/c.mkv"); !ok || r.State != StatePresent {
		t.Fatalf("moved record %+v", r)
	}
	// Full again: the changed file is updated, its old version retained as replaced; the missing
	// one repaired.
	ft.set("a.mkv", tiers.Full)
	ft.set("b.mkv", tiers.Full)
	_, st, _ = h.mustSync(false, jobs.Params{})
	if st.FilesUpdated != 1 || st.FilesCopied != 1 {
		t.Fatalf("full again %+v", st)
	}
	if rs := h.retainedOf("movies/a.mkv"); len(rs) != 1 || rs[0].Reason != ReasonReplaced {
		t.Fatalf("retained %+v", rs)
	}
}

// TestTiersRelease covers acceptance 9: only a release moves kept files into retention, and only
// the records its dry run listed that are still not full when the item runs.
func TestTiersRelease(t *testing.T) {
	setup := func(t *testing.T) (*harness, *fakeTiers) {
		h := newHarness(t)
		ft := newFakeTiers(h)
		h.withTiers(ft)
		h.writeSrc("a.mkv", content("a", 300), 1)
		h.writeSrc("b.mkv", content("b", 400), 2)
		h.writeSrc("c.mkv", content("c", 500), 3)
		h.mustSync(false, jobs.Params{})
		ft.set("a.mkv", tiers.Manifest)
		ft.set("b.mkv", tiers.Manifest)
		ft.revision = 2
		return h, ft
	}
	t.Run("the confirmed records are released", func(t *testing.T) {
		h, ft := setup(t)
		dry, dj := h.releasePreview()
		if dry.TierRevision != 2 || dry.FilesReleased != 2 || dry.BytesReleased != 700 || dry.FilesRetained != 0 {
			t.Fatalf("dry run stats %+v", dry)
		}
		var releases int
		for _, it := range h.items(dj.ID) {
			if d := detailOf(t, it); it.Action == jobs.ActionRetain && d.Reason == ReasonReleased && d.TierRevision == 2 {
				releases++
			}
		}
		if releases != 2 {
			t.Fatalf("release items %v", h.itemList(dj.ID))
		}
		// Facts change after the preview: b is full again; c becomes manifest (not in the preview).
		ft.set("b.mkv", tiers.Full)
		ft.set("c.mkv", tiers.Manifest)
		_, st, j := h.mustSync(false, jobs.Params{ReleaseDemoted: true, AllowChanges: true, ReleaseOf: dj.ID, ReleaseRevision: 2})
		if st.FilesReleased != 1 || st.BytesReleased != 300 {
			t.Fatalf("real run stats %+v items %v", st, h.itemList(j.ID))
		}
		// Kept counts what stays at the destination: c (manifest, not in the preview), not a.
		if st.FilesKept != 1 || st.BytesKept != 500 {
			t.Errorf("real run kept %d files %d bytes, want 1 and 500 (released files are not kept)", st.FilesKept, st.BytesKept)
		}
		rs := h.retainedOf("movies/a.mkv")
		if len(rs) != 1 || rs[0].Reason != ReasonReleased || rs[0].ExpiresAt == nil || !rs[0].ExpiresAt.After(h.now().Add(29*24*time.Hour)) {
			t.Fatalf("released record %+v", rs)
		}
		if _, ok := h.liveRecord("movies/c.mkv"); !ok {
			t.Fatal("c was released although the preview did not list it")
		}
		if _, ok := h.liveRecord("movies/b.mkv"); !ok {
			t.Fatal("b was released although it is full again")
		}
		if !exists(h.srcPath("a.mkv")) {
			t.Fatal("the source file went away")
		}
		// A further sync plans nothing (a is not copied again: it is manifest).
		if _, st, _ = h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
			t.Fatalf("after the release %d planned", st.FilesPlanned)
		}
	})
	t.Run("rules changed before the real run: nothing is released", func(t *testing.T) {
		h, ft := setup(t)
		_, dj := h.releasePreview()
		ft.revision = 3
		res, st, _ := h.mustSync(false, jobs.Params{ReleaseDemoted: true, AllowChanges: true, ReleaseOf: dj.ID, ReleaseRevision: 2})
		if st.FilesReleased != 0 || res.Warnings == 0 || len(h.retainedOf("movies/a.mkv")) != 0 {
			t.Fatalf("released after a rule change: %+v", st)
		}
	})
	t.Run("a rule reverted after planning: the item re-checks and releases nothing", func(t *testing.T) {
		h, ft := setup(t)
		_, dj := h.releasePreview()
		ft.onDecisions = func() { ft.mu.Lock(); ft.revision = 3; ft.mu.Unlock() }
		_, st, j := h.mustSync(false, jobs.Params{ReleaseDemoted: true, AllowChanges: true, ReleaseOf: dj.ID, ReleaseRevision: 2})
		if st.FilesReleased != 0 || len(h.retainedOf("movies/a.mkv")) != 0 {
			t.Fatalf("released: %+v", st)
		}
		for _, it := range h.items(j.ID) {
			if it.Action == jobs.ActionRetain && (it.Status != jobs.ItemSkipped || !strings.Contains(it.Error, "rules changed since the preview")) {
				t.Errorf("release item %+v", it)
			}
		}
	})
	t.Run("releases are changes for the mass-change guard", func(t *testing.T) {
		h, _ := setup(t)
		h.setSettings(func(s *destinations.Settings) { s.MaxChangeFiles = 1 })
		_, dj := h.releasePreview()
		_, st, _ := h.mustSync(false, jobs.Params{ReleaseDemoted: true, ReleaseOf: dj.ID, ReleaseRevision: 2})
		if st.FilesHeld != 2 || st.FilesReleased != 0 {
			t.Fatalf("guard %+v", st)
		}
		_, st, _ = h.mustSync(false, jobs.Params{ReleaseDemoted: true, AllowChanges: true, ReleaseOf: dj.ID, ReleaseRevision: 2})
		if st.FilesReleased != 2 {
			t.Fatalf("allowChanges %+v", st)
		}
	})
}

// TestTiersRetainDoesNotWaitForNonFull: S6 amended: an upgrade's new version that is not full
// never makes the old one wait.
func TestTiersRetainDoesNotWaitForNonFull(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("X/X-1080p.mkv", content("old", 300), 1)
	h.mustSync(false, jobs.Params{})
	h.removeSrc("X/X-1080p.mkv")
	h.writeSrc("X/X-2160p.mkv", content("new", 900), 2)
	ft.set("X/X-2160p.mkv", tiers.Manifest)
	_, st, _ := h.mustSync(false, jobs.Params{})
	if st.FilesRetained != 1 || st.FilesCopied != 0 || st.FilesFailed != 0 {
		t.Fatalf("stats %+v\n%s", st, h.rep.dump())
	}
	if rs := h.retainedOf("movies/X/X-1080p.mkv"); len(rs) != 1 || rs[0].Reason != ReasonDeleted {
		t.Fatalf("retained %+v", rs)
	}
}

// TestTiersUnknownPromotedCopies: copies that are full only because a fact is unknown count as
// changes (S10) and are held above the limits; when they are what overflows the free space they
// are held and the rest runs.
func TestTiersUnknownPromotedCopies(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("decided.mkv", content("d", 300), 1)
	for i := range 3 {
		rel := fmt.Sprintf("u%d.mkv", i)
		h.writeSrc(rel, content(rel, 1000), i+2)
		ft.setUnknown(rel)
	}
	h.setSettings(func(s *destinations.Settings) { s.MaxChangeFiles = 2 })
	res, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesCopied != 1 || st.FilesHeld != 3 || res.Warnings == 0 {
		t.Fatalf("guard %+v", st)
	}
	for _, it := range h.items(j.ID) {
		if it.Status == jobs.ItemHeld && !strings.Contains(it.Error, "mass-change") {
			t.Errorf("held item %s: %q", it.RelPath, it.Error)
		}
	}
	_, st, _ = h.mustSync(false, jobs.Params{AllowChanges: true})
	if st.FilesCopied != 3 {
		t.Fatalf("allowChanges %+v", st)
	}

	// Free space: the decided copy fits, the unknown ones do not → held, the job completes.
	h2 := newHarness(t)
	ft2 := newFakeTiers(h2)
	h2.withTiers(ft2)
	h2.writeSrc("decided.mkv", content("d", 300), 1)
	h2.writeSrc("u.mkv", content("u", 5000), 2)
	ft2.setUnknown("u.mkv")
	h2.sync.freeSpace = func(*os.Root) (uint64, uint64, error) { return 1<<30 + 1000, 1 << 40, nil }
	res, st, j = h2.mustSync(false, jobs.Params{})
	if st.FilesCopied != 1 || st.FilesHeld != 1 || res.Warnings == 0 {
		t.Fatalf("free space %+v\n%s", st, h2.rep.dump())
	}
	for _, it := range h2.items(j.ID) {
		if it.RelPath == "movies/u.mkv" && (it.Status != jobs.ItemHeld || !strings.Contains(it.Error, "tier unknown because Radarr cache is 31 h old; not enough free space")) {
			t.Errorf("unknown copy %+v", it)
		}
	}
	// The decided copies alone do not fit: the job fails as in Phase 1.
	h2.writeSrc("big.mkv", content("b", 4000), 3)
	j = h2.newJob(jobs.TypeSync, false, jobs.Params{})
	if _, err := h2.run(h2.sync, j); err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("decided overflow: %v", err)
	}
}

// TestTiersSamePathReplacement: an *arr file replaced at the same path with equal size and mtime
// (a new *arr file id added after the copy) is compared by head and tail and updated.
func TestTiersSamePathReplacement(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("m.mkv", content("one", 5000), 1)
	h.mustSync(false, jobs.Params{})
	h.writeSrc("m.mkv", content("two", 5000), 1) // same size, same mtime, other content
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesUpdated != 0 {
		t.Fatalf("updated without an *arr date: %+v", st)
	}
	ft.arrAdded = map[string]time.Time{"m.mkv": h.now().Add(time.Hour)}
	h.advance(2 * time.Hour)
	_, st, _ := h.mustSync(false, jobs.Params{})
	if st.FilesUpdated != 1 {
		t.Fatalf("stats %+v", st)
	}
	if !h.hasContent(t, content("two", 5000)) || !h.hasContent(t, content("one", 5000)) {
		t.Fatalf("the new version is not at the destination, or the old one not in retention: %v %v", regularFiles(t, h.dstDir), h.rep.dump())
	}
	// Same content again with a later *arr date: nothing to do.
	ft.arrAdded = map[string]time.Time{"m.mkv": h.now().Add(time.Hour)}
	if _, st, _ = h.mustSync(false, jobs.Params{}); st.FilesUpdated != 0 {
		t.Fatalf("equal content updated: %+v", st)
	}
}

// TestTiersMovesMoveFlags: an executed move updates exact-file flags in its transaction and hands
// the moves to the folder-flag follow-up.
func TestTiersMovesMoveFlags(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("Old/c.mkv", content("c", 500), 3)
	h.mustSync(false, jobs.Params{})
	h.renameSrc("Old/c.mkv", "New/c.mkv")
	h.mustSync(false, jobs.Params{})
	if len(ft.moveCalls) != 1 || ft.moveCalls[0][1] != "Old/c.mkv" || ft.moveCalls[0][2] != "New/c.mkv" {
		t.Fatalf("move calls %v", ft.moveCalls)
	}
	if m := ft.afterSync[h.src.ID]; len(m) != 1 || m[0] != (tiers.Move{From: "Old/c.mkv", To: "New/c.mkv"}) {
		t.Fatalf("after sync %v", ft.afterSync)
	}
}

func TestTiersStaleReferencesWarnOnce(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	ft.stale = []tiers.Warning{{RuleIndex: 0, ConditionIndex: 1, Message: `no fresh index knows the tag "gone"`}}
	h.withTiers(ft)
	h.writeSrc("a.mkv", content("a", 10), 1)
	res, st, _ := h.mustSync(false, jobs.Params{})
	if st.StaleReferences != 1 || res.Warnings != 1 || !h.rep.has(`tier rule 1, condition 2: no fresh index knows the tag "gone"`) {
		t.Fatalf("stale %+v warnings %d", st, res.Warnings)
	}
}

// TestRetentionHoldsIrreplaceable: the retention job does not expire a retained record whose source
// path an irreplaceable flag covers (§8.7).
func TestRetentionHoldsIrreplaceable(t *testing.T) {
	h := retainedFixture(t)
	ft := newFakeTiers(h)
	ft.covered["d/b.mkv"] = 4
	h.withTiers(ft)
	h.advance(31 * 24 * time.Hour)
	res, st, j, err := h.runRetention()
	if err != nil {
		t.Fatal(err)
	}
	if st.FilesExpired != 2 || st.FilesHeld != 1 || res.Warnings == 0 {
		t.Fatalf("stats %+v", st)
	}
	for _, it := range h.items(j.ID) {
		if it.Status == jobs.ItemHeld && !strings.Contains(it.Error, "irreplaceable (flag #4): not expired; remove the flag to let it expire") {
			t.Errorf("held %+v", it)
		}
	}
	if rs := retainedRecords(h); len(rs) != 1 || rs[0].SourceRelPath != "d/b.mkv" {
		t.Fatalf("retained %+v", rs)
	}
}

// TestTiersMovedToNonFull: backed-up content that reappears in another source under a non-full
// tier cannot be paired as a move; the sync counts it.
func TestTiersMovedToNonFull(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	otherDir := h.base + "/other"
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	other, err := h.cat.Create(h.ctx, sourceInputFor("Other", otherDir, "Other"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.dests.Update(h.ctx, h.dest.ID, destinations.Input{SourceIDs: []int64{h.src.ID, other.ID}}); err != nil {
		t.Fatal(err)
	}
	h.writeSrc("m.mkv", content("m", 700), 1)
	h.writeSrc("stays.mkv", content("s", 70), 2)
	h.mustSync(false, jobs.Params{})
	if err := os.Rename(h.srcPath("m.mkv"), otherDir+"/m.mkv"); err != nil {
		t.Fatal(err)
	}
	ft.set("m.mkv", tiers.Manifest)
	_, st, _ := h.mustSync(true, jobs.Params{})
	if st.MovedToNonFull != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// TestTiersTargetedDefersRetainForNonFullNewVersion: in a targeted sync only a folder that gets a
// copy, update, move or link may lose a name (D14); a new version that is not full gets no item,
// so the old one stays until the next untargeted sync, which retains it without waiting (S6).
func TestTiersTargetedDefersRetainForNonFullNewVersion(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("X/X-1080p.mkv", content("old", 300), 1)
	h.writeSrc("Y/y.mkv", content("y", 100), 2)
	h.mustSync(false, jobs.Params{})
	h.removeSrc("X/X-1080p.mkv")
	h.writeSrc("X/X-2160p.mkv", content("new", 900), 3)
	ft.set("X/X-2160p.mkv", tiers.Manifest)
	_, st, _ := h.mustTargeted("X")
	if st.FilesRetained != 0 || st.RetainsDeferred != 1 || st.FilesCopied != 0 {
		t.Fatalf("targeted %+v", st)
	}
	if _, ok := h.liveRecord("movies/X/X-1080p.mkv"); !ok {
		t.Fatal("the old version left in a targeted sync")
	}
	_, st, _ = h.mustSync(false, jobs.Params{})
	if st.FilesRetained != 1 || st.FilesCopied != 0 {
		t.Fatalf("untargeted %+v", st)
	}
}

// TestTiersReleaseHardlinkGroup: a release of a kept hardlink group retains every name (the
// dependent names go first, so no primary is refused while a name still depends on it).
func TestTiersReleaseHardlinkGroup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  destinations.HardlinkMode
		links bool
	}{{"recreate", destinations.HardlinksRecreate, true}, {"copy", destinations.HardlinksCopy, true}, {"recorded-only", destinations.HardlinksRecreate, false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.setSettings(func(s *destinations.Settings) { s.Hardlinks = tc.mode })
			h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks = tc.links })
			ft := newFakeTiers(h)
			h.withTiers(ft)
			h.writeSrc("a.mkv", content("a", 3000), 1)
			h.linkSrc("a.mkv", "b/a-link.mkv")
			h.mustSync(false, jobs.Params{})
			ft.set("a.mkv", tiers.Manifest)
			ft.set("b/a-link.mkv", tiers.Manifest)
			ft.revision = 2
			_, pj := h.releasePreview()
			res, st, j := h.mustSync(false, jobs.Params{ReleaseDemoted: true, AllowChanges: true, ReleaseOf: pj.ID, ReleaseRevision: 2})
			if st.FilesFailed != 0 || res.Warnings != 0 {
				t.Fatalf("release %+v items %v\n%s", st, h.itemList(j.ID), h.rep.dump())
			}
			for _, r := range h.records() {
				if r.State.Live() {
					t.Errorf("still live: %+v", r)
				}
			}
			if !h.hasContent(t, content("a", 3000)) {
				t.Fatal("the released content is not in retention")
			}
		})
	}
}

// TestTiersReleaseRulesChangedWhilePlanning: the rules change after the real run checked the
// preview's revision but before it decided the tiers: nothing is released (the release items
// carry the preview's revision, not the one the scan saw).
func TestTiersReleaseRulesChangedWhilePlanning(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("a.mkv", content("a", 300), 1)
	h.mustSync(false, jobs.Params{})
	ft.set("a.mkv", tiers.Manifest)
	ft.revision = 2
	_, dj := h.releasePreview()
	ft.beforeDecisions = func() { ft.mu.Lock(); ft.revision = 3; ft.mu.Unlock() }
	res, st, j := h.mustSync(false, jobs.Params{ReleaseDemoted: true, AllowChanges: true, ReleaseOf: dj.ID, ReleaseRevision: 2})
	if st.FilesReleased != 0 || len(h.retainedOf("movies/a.mkv")) != 0 {
		t.Fatalf("released after the rules changed: %+v %v", st, h.itemList(j.ID))
	}
	if _, ok := h.liveRecord("movies/a.mkv"); !ok || res.Warnings == 0 {
		t.Fatalf("kept record gone or no warning: %+v", res)
	}
}

// TestTiersReleaseReusedRecordID: a record id the preview listed is reused by another file's
// record before the release runs: that file is not released.
func TestTiersReleaseReusedRecordID(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("a.mkv", content("a", 300), 1)
	h.writeSrc("b.mkv", content("b", 400), 2)
	h.mustSync(false, jobs.Params{})
	ft.set("a.mkv", tiers.Manifest)
	ft.revision = 2
	_, dj := h.releasePreview()
	ra, _ := h.liveRecord("movies/a.mkv")
	rb, _ := h.liveRecord("movies/b.mkv")
	h.dbExec(`DELETE FROM destination_files WHERE id = ?`, ra.ID)
	h.dbExec(`UPDATE destination_files SET id = ? WHERE id = ?`, ra.ID, rb.ID)
	ft.set("b.mkv", tiers.Manifest)
	_, st, j := h.mustSync(false, jobs.Params{ReleaseDemoted: true, AllowChanges: true, ReleaseOf: dj.ID, ReleaseRevision: 2})
	if st.FilesReleased != 0 || len(h.retainedOf("movies/b.mkv")) != 0 {
		t.Fatalf("b was released although the preview did not list it: %+v %v", st, h.itemList(j.ID))
	}
	if _, ok := h.liveRecord("movies/b.mkv"); !ok {
		t.Fatal("b's record is gone")
	}
}

// TestTiersMissingKeptPrimaryRepairsFullLinks: a kept (not full) primary that verify marked
// missing is never repaired, so a full recorded-only link of it gets its own copy.
func TestTiersMissingKeptPrimaryRepairsFullLinks(t *testing.T) {
	h := newHarness(t)
	crashConfigs[1].apply(h) // hardlinks are recorded, not made
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("dl/x.mkv", content("x", 2000), 1)
	h.linkSrc("dl/x.mkv", "lib/x.mkv")
	h.mustSync(false, jobs.Params{})
	var prim, link Record
	for _, r := range h.records() {
		switch r.State {
		case StatePresent:
			prim = r
		case StateLinkRecorded:
			link = r
		}
	}
	if prim.ID == 0 || link.ID == 0 || link.LinkOf != prim.ID {
		t.Fatalf("records %+v", h.records())
	}
	// The group splits with equal content and mtime; the primary's name becomes manifest.
	h.removeSrc(link.SourceRelPath)
	h.writeSrc(link.SourceRelPath, content("x", 2000), 1)
	ft.set(prim.SourceRelPath, tiers.Manifest)
	// Verify found the primary's file damaged.
	if err := os.Remove(h.dstPath(prim.RelPath)); err != nil {
		t.Fatal(err)
	}
	h.dbExec(`UPDATE destination_files SET state = 'missing' WHERE id = ?`, prim.ID)
	_, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesCopied != 1 {
		t.Fatalf("stats %+v items %v", st, h.itemList(j.ID))
	}
	r, ok := h.liveRecord(link.RelPath)
	if !ok || r.State != StatePresent || !exists(h.dstPath(link.RelPath)) {
		t.Fatalf("the full link has no copy of its own: %+v", r)
	}
	if r, _ := h.liveRecord(prim.RelPath); r.State != StateMissing {
		t.Fatalf("the kept primary changed: %+v", r)
	}
	if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
		t.Fatalf("a further sync planned %v", h.itemList(j.ID))
	}
}

// TestTiersFolderFlagsFollowResumedMoves: moves executed by an attempt that crashed still reach
// the folder-flag follow-up, from the resumed attempt.
func TestTiersFolderFlagsFollowResumedMoves(t *testing.T) {
	want := []tiers.Move{{From: "Old/c.mkv", To: "New/c.mkv"}, {From: "Old/d.mkv", To: "New/d.mkv"}}
	for _, point := range []string{PointRecordAfterDB, "copy.beforeTemp"} {
		t.Run(point, func(t *testing.T) {
			h := newHarness(t)
			ft := newFakeTiers(h)
			h.withTiers(ft)
			h.writeSrc("Old/c.mkv", content("c", 500), 1)
			h.writeSrc("Old/d.mkv", content("d", 600), 2)
			h.mustSync(false, jobs.Params{})
			h.renameSrc("Old/c.mkv", "New/c.mkv")
			h.renameSrc("Old/d.mkv", "New/d.mkv")
			h.writeSrc("z.mkv", content("z", 700), 3)
			j := h.newJob(jobs.TypeSync, false, jobs.Params{})
			if crashed, _, _ := runWithHook(h, j, faultinject.CrashAt(point, 1)); !crashed {
				t.Fatal("no crash")
			}
			ft.mu.Lock()
			ft.afterSync = map[int64][]tiers.Move{} // a real crash (SIGKILL) runs no defer
			ft.mu.Unlock()
			j.Attempt, j.Trigger = 2, jobs.TriggerResume
			if _, err := h.sync.Run(h.ctx, j, h.env()); err != nil {
				t.Fatal(err)
			}
			h.closeJob(j.ID)
			got := slices.Clone(ft.afterSync[h.src.ID])
			slices.SortFunc(got, func(a, b tiers.Move) int { return strings.Compare(a.From, b.From) })
			got = slices.Compact(got)
			if !slices.Equal(got, want) {
				t.Fatalf("the folder flags followed %v, want %v", got, want)
			}
		})
	}
}

// TestTiersFolderFlagsFollowCancelledMoves: a cancelled sync's executed moves reach the
// folder-flag follow-up.
func TestTiersFolderFlagsFollowCancelledMoves(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("Old/c.mkv", content("c", 500), 1)
	h.mustSync(false, jobs.Params{})
	h.renameSrc("Old/c.mkv", "New/c.mkv")
	h.writeSrc("y.mkv", content("y", 700), 3)
	h.writeSrc("z.mkv", content("z", 800), 4)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	faultinject.SetHook(func(name string) {
		if name == "copy.beforeTemp" {
			cancel()
		}
	})
	defer faultinject.SetHook(nil)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if _, err := h.sync.Run(ctx, j, h.env()); err == nil {
		t.Fatal("the sync was not cancelled")
	}
	if m := ft.afterSync[h.src.ID]; len(m) != 1 || m[0] != (tiers.Move{From: "Old/c.mkv", To: "New/c.mkv"}) {
		t.Fatalf("after a cancel the folder flags followed %v", ft.afterSync)
	}
}

// TestTiersSamePathCheckedOnce: a same-path check that finds the backup equal is remembered: the
// next sync does not compare the same *arr file again (a real run only).
func TestTiersSamePathCheckedOnce(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("m.mkv", content("one", 5000), 1)
	h.mustSync(false, jobs.Params{})
	added := h.now().Add(time.Hour).UTC()
	ft.arrAdded = map[string]time.Time{"m.mkv": added}
	h.advance(2 * time.Hour)
	h.mustSync(true, jobs.Params{})
	if r, _ := h.liveRecord("movies/m.mkv"); r.CopiedAt == nil || r.CopiedAt.Equal(added) {
		t.Fatalf("a dry run recorded the check: %+v", r.CopiedAt)
	}
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesUpdated != 0 {
		t.Fatalf("equal content updated: %+v", st)
	}
	if r, _ := h.liveRecord("movies/m.mkv"); r.CopiedAt == nil || !r.CopiedAt.Equal(added) {
		t.Fatalf("the check was not recorded: copied at %v, want %v", r.CopiedAt, added)
	}
	// The same *arr file: not compared again (content that changes without a new *arr file and
	// with equal size and mtime is not detected, as without an *arr).
	h.writeSrc("m.mkv", content("two", 5000), 1)
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesUpdated != 0 {
		t.Fatalf("the same *arr file was compared again: %+v", st)
	}
	// A newer *arr file at the path is compared.
	ft.arrAdded = map[string]time.Time{"m.mkv": h.now().Add(time.Hour)}
	h.advance(2 * time.Hour)
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesUpdated != 1 {
		t.Fatalf("a newer *arr file was not compared: %+v", st)
	}
}

// TestTiersUnknownPromotedRepairsHeld: repairs of files that are full only because a fact is
// unknown are not decided bytes: when they are what does not fit, they are held and the rest runs.
func TestTiersUnknownPromotedRepairsHeld(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("decided.mkv", content("d", 300), 1)
	h.writeSrc("u.mkv", content("u", 5000), 2)
	h.mustSync(false, jobs.Params{})
	ft.setUnknown("u.mkv")
	ru, _ := h.liveRecord("movies/u.mkv")
	if err := os.Remove(h.dstPath(ru.RelPath)); err != nil {
		t.Fatal(err)
	}
	h.dbExec(`UPDATE destination_files SET state = 'missing' WHERE id = ?`, ru.ID)
	h.writeSrc("new.mkv", content("n", 300), 3)
	h.sync.freeSpace = func(*os.Root) (uint64, uint64, error) { return 1<<30 + 1000, 1 << 40, nil }
	res, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesCopied != 1 || st.FilesHeld != 1 || res.Warnings == 0 {
		t.Fatalf("stats %+v items %v", st, h.itemList(j.ID))
	}
	for _, it := range h.items(j.ID) {
		if it.RelPath == "movies/u.mkv" && (it.Status != jobs.ItemHeld || !strings.Contains(it.Error, "tier unknown")) {
			t.Errorf("unknown repair %+v", it)
		}
	}
}

// TestTiersUnknownHoldOnResume: a resumed plan holds the unknown-promoted copies that do not fit
// in the free space (the attempt that planned it may have stopped before holding them).
func TestTiersUnknownHoldOnResume(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("a.mkv", content("a", 300), 1)
	h.writeSrc("u.mkv", content("u", 5000), 2)
	ft.setUnknown("u.mkv")
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if crashed, _, _ := runWithHook(h, j, faultinject.CrashAt("copy.beforeTemp", 1)); !crashed {
		t.Fatal("no crash")
	}
	h.sync.freeSpace = func(*os.Root) (uint64, uint64, error) { return 1<<30 + 1000, 1 << 40, nil }
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	if _, err := h.sync.Run(h.ctx, j, h.env()); err != nil {
		t.Fatal(err)
	}
	h.closeJob(j.ID)
	for _, it := range h.items(j.ID) {
		switch it.RelPath {
		case "movies/u.mkv":
			if it.Status != jobs.ItemHeld {
				t.Errorf("unknown copy %+v", it)
			}
		case "movies/a.mkv":
			if it.Status != jobs.ItemDone {
				t.Errorf("decided copy %+v", it)
			}
		}
	}
}
