package tiers

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

const arrKey = "0123456789abcdef0123456789abcdef"

type nopReporter struct{}

func (nopReporter) Progress(jobs.Progress)         {}
func (nopReporter) Log(slog.Level, string, ...any) {}

// radarrLibrary indexes the recorded Radarr (testdata/arr/radarr) through a real refresh: a
// source over <root>/movies holding the fixtures' files, and a destination.
func radarrLibrary(t *testing.T, providers ...Provider) (*env, integrations.Integration, catalog.Source, int64, map[int64]string) {
	t.Helper()
	e := newEnv(t, providers...)
	srv := arrtest.NewServer(t, arr.KindRadarr, arrKey)
	var movies []struct {
		ID        int64 `json:"id"`
		MovieFile *struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"movieFile"`
	}
	if err := json.Unmarshal(arrtest.Fixture(t, arr.KindRadarr, "movie.json"), &movies); err != nil {
		t.Fatal(err)
	}
	files := map[int64]string{}
	for _, m := range movies {
		if m.MovieFile != nil {
			e.writeFile(strings.TrimPrefix(m.MovieFile.Path, "/"), m.MovieFile.Size)
			files[m.ID] = strings.TrimPrefix(m.MovieFile.Path, "/movies/")
		}
	}
	src := e.scan(e.source("Movies", "movies"))
	settings, _ := json.Marshal(map[string]any{"pathMappings": []map[string]string{{"arr": "/movies", "local": filepath.Join(e.root, "movies")}}})
	it, err := e.ints.Create(e.ctx, integrations.Input{Type: integrations.TypeRadarr, Name: "Radarr", URL: srv.URL, APIKey: arrKey, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := mediaindex.NewRunner(mediaindex.RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	job := jobs.Job{ID: 1, Type: jobs.TypeRefresh, Trigger: jobs.TriggerManual, Attempt: 1, Params: jobs.Params{IntegrationID: it.ID}}
	if _, err := runner.Run(e.ctx, job, jobs.Env{Items: &memItems{}, Reporter: nopReporter{}}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, "movies")); err != nil {
		t.Fatal(err)
	}
	return e, it, src, e.destination("nas", src.ID), files
}

// TestPinnedTable is acceptance 6's pinned table (design §15) with the recorded Radarr: rules R1
// `arr.tag has bunkarr-full` → full and R2 (no conditions) → manifest; then R0
// `maintainerr.pendingDelete is true` → skip first, with movie 3 pending; then with the
// Maintainerr cache stale. The reasons are compared as structured values.
func TestPinnedTable(t *testing.T) {
	mt := &fakeProvider{fields: []string{FieldMaintainerrPending}, integrationID: 99}
	e, radarr, src, dest, files := radarrLibrary(t, mt)
	if len(files) != 3 {
		t.Fatalf("fixture files %v", files)
	}
	mt.pending = map[string]bool{files[3]: true}
	r1 := RuleInput{Name: "R1", Action: Full, Conditions: []Condition{cnd(FieldArrTag, OpHas, "bunkarr-full")}}
	r2 := RuleInput{Name: "R2", Action: Manifest, Conditions: []Condition{}}
	rs := e.saveRules(r1, r2)
	id1, id2 := rs.Rules[0].ID, rs.Rules[1].ID
	arrSrc := ReasonSource{Kind: SourceArr, IntegrationID: radarr.ID}
	tagReason := func(ruleID int64, labels []string, res Result) Reason {
		return Reason{RuleID: ruleID, ConditionIndex: 0, Field: FieldArrTag, Op: OpHas, Value: raw("bunkarr-full"), Actual: labels,
			Result: res, Source: arrSrc}
	}
	check := func(movie int64, tier Tier, ruleID int64, rule string, reasons, unknown []Reason) {
		t.Helper()
		got, _ := e.decisions(dest, src)
		d := got[files[movie]]
		if d.Tier != tier || d.RuleID != ruleID || d.RuleName != rule || d.UnknownPromoted {
			t.Errorf("movie %d: %s by %q (%d), want %s by %q: %+v", movie, d.Tier, d.RuleName, d.RuleID, tier, rule, d)
		}
		norm := func(rs []Reason) string { b, _ := json.Marshal(rs); return string(b) }
		if norm(d.Reasons) != norm(reasons) {
			t.Errorf("movie %d reasons\n got %s\nwant %s", movie, norm(d.Reasons), norm(reasons))
		}
		if norm(d.Unknown) != norm(unknown) {
			t.Errorf("movie %d unknown\n got %s\nwant %s", movie, norm(d.Unknown), norm(unknown))
		}
	}
	check(1, Full, id1, "R1", []Reason{tagReason(id1, []string{"bunkarr-full"}, True)}, []Reason{})
	check(2, Manifest, id2, "R2", []Reason{}, []Reason{})
	check(3, Manifest, id2, "R2", []Reason{}, []Reason{})
	if got, _ := e.decisions(dest, src); len(got) != 3 {
		t.Errorf("movie 4 has no file, yet %d decisions", len(got))
	}

	r0 := RuleInput{Name: "R0", Action: Skip, Conditions: []Condition{cnd(FieldMaintainerrPending, OpIs, true)}}
	rs = e.saveRules(r0, RuleInput{ID: id1, Name: "R1", Action: Full, Conditions: r1.Conditions}, RuleInput{ID: id2, Name: "R2", Action: Manifest})
	id0 := rs.Rules[0].ID
	mtSrc := ReasonSource{Kind: SourceMaintainerr, IntegrationID: 99}
	check(3, Skip, id0, "R0", []Reason{{RuleID: id0, ConditionIndex: 0, Field: FieldMaintainerrPending, Op: OpIs, Value: raw(true), Actual: true,
		Result: True, Source: mtSrc}}, []Reason{})
	check(1, Full, id1, "R1", []Reason{tagReason(id1, []string{"bunkarr-full"}, True)}, []Reason{})

	// The Maintainerr cache is stale: R0 is unknown, but skip is less protective than manifest.
	mt.stale = "Maintainerr cache is 30 h old (stale after 24 h)"
	check(3, Manifest, id2, "R2", []Reason{}, []Reason{{RuleID: id0, ConditionIndex: 0, Field: FieldMaintainerrPending, Op: OpIs,
		Value: raw(true), Result: Unknown, Source: mtSrc, Why: mt.stale}})

	// The dry-run view of the same decisions: the preview's items carry them too.
	p, err := e.eng.Preview(e.ctx, PreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := e.eng.PreviewItems(p.ID, ItemQuery{Tier: Manifest})
	if err != nil || page.TotalRecords != 2 {
		t.Fatalf("preview manifest items %v %+v", err, page)
	}
	if !reflect.DeepEqual(p.UnknownSources, []UnknownSource{{IntegrationID: 99, Name: "Maintainerr", Reason: mt.stale}}) {
		t.Errorf("unknown sources %+v", p.UnknownSources)
	}
}
