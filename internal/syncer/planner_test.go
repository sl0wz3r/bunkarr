package syncer

import (
	"database/sql"
	"os"
	"path"
	"slices"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

// The capabilities restic and rclone report to the planner (phase4.md §3.3).
var (
	resticCaps = filecopy.Capabilities{Hardlinks: true, MtimeGranularityNs: 1, TrailingDotSpace: true}
	rcloneCaps = filecopy.Capabilities{Hardlinks: false, MtimeGranularityNs: 1_000_000_000, TrailingDotSpace: true}
)

// engineRecord inserts a live record of the harness's source, as an engine executor records it.
func (h *harness) engineRecord(srcRel string, size int64, mtime time.Time, state State, linkOf int64, headTail, ref string) Record {
	h.t.Helper()
	copied := h.now()
	rec := Record{DestinationID: h.dest.ID, SourceID: h.src.ID, RelPath: path.Join(h.src.DestFolder, srcRel), SourceRelPath: srcRel,
		Size: size, MtimeNs: mtime.UnixNano(), State: state, LinkOf: linkOf, CopiedAt: &copied, HeadTail: headTail, EngineRef: ref}
	err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		id, err := insertRecord(h.ctx, tx, rec)
		rec.ID = id
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return rec
}

// headTail is the head/tail hash of a source file.
func (h *harness) headTail(rel string) string {
	h.t.Helper()
	root, err := os.OpenRoot(h.srcDir)
	if err != nil {
		h.t.Fatal(err)
	}
	defer root.Close()
	ht, err := filecopy.HeadTailHash(root, rel)
	if err != nil {
		h.t.Fatal(err)
	}
	return ht
}

// enginePlan plans a sync of the harness's destination as an engine runner does.
func (h *harness) enginePlan(p *Planner, caps filecopy.Capabilities, fs engines.PlanFS, opts PlanOptions, dryRun bool) (*Plan, []jobs.Item) {
	h.t.Helper()
	j := h.newJob(jobs.TypeSync, dryRun, jobs.Params{DestinationID: h.dest.ID})
	pl, err := p.Plan(h.ctx, PlanInput{Job: j, Env: h.env(), FS: fs, Options: opts,
		Dest: PlanDestination{ID: h.dest.ID, Name: h.dest.Name, SourceIDs: []int64{h.src.ID}, Settings: destinations.DefaultSettings(), Caps: caps}})
	if err != nil {
		h.t.Fatalf("plan: %v\n%s", err, h.rep.dump())
	}
	return pl, h.items(j.ID)
}

func (h *harness) newPlanner(t Tiers) *Planner {
	return NewPlanner(Options{DB: h.db, Store: h.store, Catalog: h.cat, Scanner: h.scanner, Destinations: h.dests, Now: h.now, Tiers: t})
}

// actions renders items as "action path" in plan order.
func actions(items []jobs.Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, string(it.Action)+" "+it.RelPath)
	}
	return out
}

func TestPlannerEngineMoves(t *testing.T) {
	for _, c := range []struct {
		name string
		caps filecopy.Capabilities
	}{{"restic", resticCaps}, {"rclone", rcloneCaps}} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.writeSrc("new/a.mkv", content("a", 3000), 1)
			h.writeSrc("new/b.mkv", content("b", 4000), 2)
			h.writeSrc("same.mkv", content("s", 500), 3)
			fs := &enginetest.FakePlanFS{HeadTails: map[string]string{}, Stats: map[string]filecopy.Stat{}}
			ra := h.engineRecord("old/a.mkv", 3000, baseTime.Add(time.Second), StatePresent, 0, h.headTail("new/a.mkv"), "")
			h.engineRecord("old/b.mkv", 4000, baseTime.Add(2*time.Second), StatePresent, 0, "", "")
			h.engineRecord("same.mkv", 500, baseTime.Add(3*time.Second), StatePresent, 0, h.headTail("same.mkv"), "")
			// The engine's PlanFS answers the record's head_tail, or ErrNoHeadTail when it is NULL.
			fs.HeadTails[ra.RelPath] = ra.HeadTail
			pl, items := h.enginePlan(h.newPlanner(nil), c.caps, fs, PlanOptions{}, false)
			got := actions(items)
			slices.Sort(got)
			want := []string{"copy movies/new/b.mkv", "move movies/new/a.mkv", "retain movies/old/b.mkv"}
			if !slices.Equal(got, want) {
				t.Fatalf("items %v, want %v", got, want)
			}
			if !pl.Planned() || len(pl.Sources()) != 1 || len(pl.Summaries()) != 1 || pl.BytesPlanned() != 4000 {
				t.Errorf("plan: planned %v sources %d summaries %+v bytes %d", pl.Planned(), len(pl.Sources()), pl.Summaries(), pl.BytesPlanned())
			}
			st, _, summary, err := pl.Stats(h.ctx, h.now())
			if err != nil {
				t.Fatal(err)
			}
			if st.FilesPlanned != 3 || st.FilesCopied != 0 || summary == "" {
				t.Errorf("stats %+v %q", st, summary)
			}
			// The move's detail says where it comes from (the executor and flagsAfterSync read it).
			for _, it := range items {
				if it.Action == jobs.ActionMove {
					d, err := ParseDetail(it)
					if err != nil || d.From != "movies/old/a.mkv" || d.FromSource != "old/a.mkv" || d.RecordID != ra.ID {
						t.Errorf("move detail %+v (%v)", d, err)
					}
				}
			}
		})
	}
}

func TestPlannerEngineHardlinks(t *testing.T) {
	for _, c := range []struct {
		name      string
		caps      filecopy.Capabilities
		depState  State
		wantItems []string
	}{
		// restic keeps each name as a linked record: the vanished primary's content moves to the
		// dependent (a record-only promote) and the primary's record is retained.
		{"restic", resticCaps, StateLinked, []string{"promote movies/d.mkv", "retain movies/gone.mkv"}},
		// rclone records the other names only (link_recorded): the promote takes the record over.
		{"rclone", rcloneCaps, StateLinkRecorded, []string{"promote movies/d.mkv"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.writeSrc("d.mkv", content("d", 2000), 1)
			h.writeSrc("x.mkv", content("x", 1000), 2)
			h.linkSrc("x.mkv", "y.mkv")
			p := h.engineRecord("gone.mkv", 2000, baseTime.Add(time.Second), StatePresent, 0, "", "")
			h.engineRecord("d.mkv", 2000, baseTime.Add(time.Second), c.depState, p.ID, "", "")
			fs := &enginetest.FakePlanFS{HeadTails: map[string]string{}, Stats: map[string]filecopy.Stat{}}
			_, items := h.enginePlan(h.newPlanner(nil), c.caps, fs, PlanOptions{}, false)
			got := actions(items)
			want := append(slices.Clone(c.wantItems), "copy movies/x.mkv", "link movies/y.mkv")
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("items %v, want %v", got, want)
			}
		})
	}
}

func TestPlannerUpdateKept(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.writeSrc("kept.mkv", content("k2", 1500), 5)
	h.writeSrc("same.mkv", content("s", 700), 6)
	ft.set("kept.mkv", tiers.Manifest)
	ft.set("same.mkv", tiers.Manifest)
	h.engineRecord("kept.mkv", 1000, baseTime.Add(time.Second), StatePresent, 0, "", "snap1")
	h.engineRecord("same.mkv", 700, baseTime.Add(6*time.Second), StatePresent, 0, "", "")
	fs := &enginetest.FakePlanFS{HeadTails: map[string]string{}, Stats: map[string]filecopy.Stat{}}
	// rclone and filecopy keep a kept file as it is (S15).
	pl, items := h.enginePlan(h.newPlanner(ft), rcloneCaps, fs, PlanOptions{}, false)
	if len(items) != 0 {
		t.Fatalf("kept without UpdateKept: %v", actions(items))
	}
	if st, _, _, _ := pl.Stats(h.ctx, h.now()); st.FilesKept != 2 || st.BytesKept != 2200 {
		t.Errorf("kept stats %+v", st)
	}
	// restic backs a changed kept file up again (D29); its bytes stay kept.
	pl, items = h.enginePlan(h.newPlanner(ft), resticCaps, fs, PlanOptions{UpdateKept: true}, false)
	if got := actions(items); !slices.Equal(got, []string{"update movies/kept.mkv"}) {
		t.Fatalf("UpdateKept items %v", got)
	}
	d, _ := ParseDetail(items[0])
	if d.Tier == nil || d.Tier.Tier != tiers.Manifest || d.OldSize != 1000 || items[0].Bytes != 1500 {
		t.Errorf("update detail %+v bytes %d", d, items[0].Bytes)
	}
	if st, _, _, _ := pl.Stats(h.ctx, h.now()); st.FilesKept != 2 || st.BytesKept != 2200 || st.FilesUpdated != 0 {
		t.Errorf("UpdateKept stats %+v", st)
	}
	// A dry run lists it as the update, not as a kept skip item.
	_, items = h.enginePlan(h.newPlanner(ft), resticCaps, fs, PlanOptions{UpdateKept: true}, true)
	if got := itemsBy(items, jobs.ActionUpdate, ""); len(got) != 1 || len(itemsBy(items, jobs.ActionSkip, "")) != 1 {
		t.Errorf("dry run items %v", actions(items))
	}
}

func TestPlannerResumesCompletePlan(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 100), 1)
	p := h.newPlanner(nil)
	fs := &enginetest.FakePlanFS{HeadTails: map[string]string{}, Stats: map[string]filecopy.Stat{}}
	j := h.newJob(jobs.TypeSync, false, jobs.Params{DestinationID: h.dest.ID})
	in := PlanInput{Job: j, Env: h.env(), FS: fs, Dest: PlanDestination{ID: h.dest.ID, SourceIDs: []int64{h.src.ID},
		Settings: destinations.DefaultSettings(), Caps: resticCaps}}
	if _, err := p.Plan(h.ctx, in); err != nil {
		t.Fatal(err)
	}
	h.writeSrc("b.mkv", content("b", 100), 2)
	pl, err := p.Plan(h.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Planned() || len(h.items(j.ID)) != 1 {
		t.Errorf("resume planned %v items %v", pl.Planned(), actions(h.items(j.ID)))
	}
	// Free space: a known, too small free space fails a real run; unknown skips the check.
	in.Job = h.newJob(jobs.TypeSync, false, jobs.Params{DestinationID: h.dest.ID})
	in.FreeSpace = func() (int64, bool, error) { return 100, true, nil }
	if _, err := p.Plan(h.ctx, in); err == nil {
		t.Error("the free-space check passed with 100 bytes free")
	}
	in.Job = h.newJob(jobs.TypeSync, false, jobs.Params{DestinationID: h.dest.ID})
	in.FreeSpace = func() (int64, bool, error) { return 0, false, nil }
	if _, err := p.Plan(h.ctx, in); err != nil {
		t.Errorf("unknown free space: %v", err)
	}
}
