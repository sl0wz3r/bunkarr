package tiers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

// library is a Radarr over media/movies with three movies, and a Home source outside every *arr
// root folder.
type library struct {
	*env
	radarr       integrations.Integration
	movies, home catalog.Source
	dest         int64
	heatItem     int64
}

const mb = 1 << 20

func newLibrary(t *testing.T, providers ...Provider) *library {
	t.Helper()
	e := newEnv(t, providers...)
	l := &library{env: e}
	l.movies = e.source("Movies", "movies")
	l.home = e.source("Home", "home")
	e.writeFile("movies/Heat (1995)/Heat (1995).mkv", 5*mb)
	e.writeFile("movies/Heat (1995)/Heat (1995).en.srt", 1000)
	e.writeFile("movies/Heat (1995)/Heat (1995).nfo", 900)
	e.writeFile("movies/Heat (1995)/Featurettes/Making of.mkv", 2*mb)
	e.writeFile("movies/Charade (1963)/Charade (1963).mkv", 4*mb)
	e.writeFile("movies/Blob (1958)/Blob (1958).mkv", 3*mb)
	e.writeFile("movies/Stray (2020)/Stray (2020).mkv", 1*mb)
	e.writeFile("home/2019/birthday.mp4", 6*mb)
	l.movies = e.scan(l.movies)
	l.home = e.scan(l.home)
	l.radarr = e.arr(integrations.TypeRadarr, "Radarr", "/movies", "movies")
	e.rootFolder(l.radarr, 1, "/movies", "movies")
	e.meta(l.radarr, "tag", 1, "bunkarr-full", "")
	e.meta(l.radarr, "quality_profile", 4, "HD-1080p", "")
	added := e.clock.Now().Add(-48 * time.Hour)
	l.heatItem = e.item(l.radarr, itemSpec{arrID: 1, title: "Heat", path: "/movies/Heat (1995)", root: "/movies", profile: 4, monitored: true,
		tags: []int64{1}, genres: []string{"Crime"}, ext: mediaindex.ExternalIDs{TMDB: 949}})
	e.arrFile(l.radarr, l.heatItem, 11, "/movies/Heat (1995)/Heat (1995).mkv", "movies/Heat (1995)/Heat (1995).mkv", 5*mb, added)
	charade := e.item(l.radarr, itemSpec{arrID: 2, title: "Charade", path: "/movies/Charade (1963)", root: "/movies", profile: 4, ext: mediaindex.ExternalIDs{TMDB: 4808}})
	e.arrFile(l.radarr, charade, 12, "/movies/Charade (1963)/Charade (1963).mkv", "movies/Charade (1963)/Charade (1963).mkv", 4*mb, added)
	blob := e.item(l.radarr, itemSpec{arrID: 3, title: "The Blob", path: "/movies/Blob (1958)", root: "/movies", profile: 4, deleted: true,
		ext: mediaindex.ExternalIDs{TMDB: 8851}})
	e.arrFile(l.radarr, blob, 13, "/movies/Blob (1958)/Blob (1958).mkv", "movies/Blob (1958)/Blob (1958).mkv", 3*mb, added)
	e.fresh(l.radarr)
	l.dest = e.destination("nas", l.movies.ID, l.home.ID)
	return l
}

// specPreset saves the spec's manifest-by-default rules.
func (l *library) specPreset() {
	l.saveRules(Presets()[1].Rules...)
}

func TestFactsAttribution(t *testing.T) {
	l := newLibrary(t)
	l.specPreset()
	got, sd := l.decisions(l.dest, l.movies)
	check := func(rel string, tier Tier, promoted bool, follows string) Decision {
		t.Helper()
		d, ok := got[rel]
		if !ok {
			t.Fatalf("no decision for %s", rel)
		}
		if d.Tier != tier || d.UnknownPromoted != promoted || d.Follows != follows {
			t.Errorf("%s: %s promoted=%v follows=%q, want %s %v %q (%+v)", rel, d.Tier, d.UnknownPromoted, d.Follows, tier, promoted, follows, d)
		}
		return d
	}
	d := check("Heat (1995)/Heat (1995).mkv", Full, false, "")
	if d.RuleName != "Tagged bunkarr-full" || len(d.Reasons) != 1 || d.Reasons[0].Source.IntegrationID != l.radarr.ID {
		t.Errorf("heat %+v", d)
	}
	check("Heat (1995)/Heat (1995).en.srt", Full, false, "Heat (1995)/Heat (1995).mkv")
	check("Heat (1995)/Heat (1995).nfo", Full, false, "Heat (1995)/Heat (1995).mkv")
	// An extra in the item's folder gets its item-level facts (the tag).
	check("Heat (1995)/Featurettes/Making of.mkv", Full, false, "")
	check("Charade (1963)/Charade (1963).mkv", Manifest, false, "")
	// A deleted item supplies no facts: its file is inside the root folder with no item, so
	// unknown, and the tag rule (full) is more protective than the rest (manifest).
	d = check("Blob (1958)/Blob (1958).mkv", Full, true, "")
	if len(d.Reasons) != 1 || !strings.Contains(d.Reasons[0].Why, "no Radarr item holds this file") {
		t.Errorf("blob %+v", d)
	}
	check("Stray (2020)/Stray (2020).mkv", Full, true, "")
	if sd.Revision != 1 || len(sd.Unknown) != 0 || len(sd.Stale) != 0 {
		t.Errorf("decisions revision %d unknown %v stale %v", sd.Revision, sd.Unknown, sd.Stale)
	}
	if at, ok := sd.ArrDateAdded("Heat (1995)/Heat (1995).mkv"); !ok || at.IsZero() {
		t.Error("no *arr dateAdded for Heat")
	}

	// A file outside every root folder while every *arr cache is fresh is unmanaged: manifest.
	home, _ := l.decisions(l.dest, l.home)
	if d := home["2019/birthday.mp4"]; d.Tier != Manifest || d.UnknownPromoted || d.RuleName != "Everything else" {
		t.Errorf("home video %+v", d)
	}
	// With the Radarr cache stale it is unknown, so full (and unknown-promoted).
	l.clock.Advance(25 * time.Hour)
	home, sd = l.decisions(l.dest, l.home)
	if d := home["2019/birthday.mp4"]; d.Tier != Full || !d.UnknownPromoted || !strings.Contains(d.Reasons[0].Why, "cache is 25 h old") {
		t.Errorf("home video, stale Radarr %+v", d)
	}
	if len(sd.Unknown) != 1 || sd.Unknown[0].IntegrationID != l.radarr.ID {
		t.Errorf("unknown sources %+v", sd.Unknown)
	}
	got, _ = l.decisions(l.dest, l.movies)
	check("Charade (1963)/Charade (1963).mkv", Full, true, "")
}

func TestFactsConflictsAndMismatches(t *testing.T) {
	l := newLibrary(t)
	l.specPreset()
	// A second Radarr claims Charade's file (and its folder): every arr fact is unknown (D12).
	r4k := l.arr(integrations.TypeRadarr, "Radarr 4K", "/movies", "movies")
	l.meta(r4k, "tag", 1, "bunkarr-full", "")
	c4k := l.item(r4k, itemSpec{arrID: 9, title: "Charade", path: "/movies/Charade (1963)", root: "/movies", tags: []int64{1}})
	l.arrFile(r4k, c4k, 91, "/movies/Charade (1963)/Charade (1963).mkv", "movies/Charade (1963)/Charade (1963).mkv", 4*mb, l.clock.Now())
	l.fresh(r4k)
	// Heat's *arr file has another size than the catalog's: mismatched (S18).
	l.exec(`UPDATE arr_files SET size = 7 WHERE arr_file_id = 11`)
	got, _ := l.decisions(l.dest, l.movies)
	c := got["Charade (1963)/Charade (1963).mkv"]
	if c.Tier != Full || !c.UnknownPromoted || !strings.Contains(c.Reasons[0].Why, "two *arr integrations claim this file") {
		t.Errorf("charade claimed twice %+v", c)
	}
	h := got["Heat (1995)/Heat (1995).mkv"]
	if h.Tier != Full || !h.UnknownPromoted || !strings.Contains(h.Reasons[0].Why, "another size") {
		t.Errorf("heat mismatched %+v", h)
	}
	// The source names its *arr integration: only that one's rows count, and the conflict is
	// resolved (the 4K Radarr tags it bunkarr-full).
	l.exec(`UPDATE sources SET arr_integration_id = ? WHERE id = ?`, r4k.ID, l.movies.ID)
	src, err := l.cat.Get(l.ctx, l.movies.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = l.decisions(l.dest, src)
	if c := got["Charade (1963)/Charade (1963).mkv"]; c.Tier != Full || c.UnknownPromoted || c.RuleName != "Tagged bunkarr-full" {
		t.Errorf("charade with the source's integration %+v", c)
	}
}

func TestHardlinkGroupTakesTheMostProtectiveTier(t *testing.T) {
	l := newLibrary(t)
	// A second name of Heat's file in its folder (hardlink groups are per source).
	if err := os.Link(filepath.Join(l.root, "movies/Heat (1995)/Heat (1995).mkv"), filepath.Join(l.root, "movies/Heat (1995)/Heat copy.mkv")); err != nil {
		t.Fatal(err)
	}
	l.movies = l.scan(l.movies)
	// Heat copy.mkv has no *arr file: it is attributed by folder to Heat, so both names are
	// managed and manifest.
	l.saveRules(RuleInput{Name: "managed is manifest", Action: Manifest, Conditions: []Condition{cnd(FieldArrManaged, OpIs, true), cnd(FieldFileSize, OpGt, 0)},
		DestinationIDs: []int64{l.dest}})
	got, _ := l.decisions(l.dest, l.movies)
	if got["Heat (1995)/Heat copy.mkv"].Tier != Manifest || got["Heat (1995)/Heat (1995).mkv"].Tier != Manifest {
		t.Fatalf("both names manifest %+v", got)
	}
	// Flag one name: the group takes its full.
	if _, err := l.eng.AddFlag(l.ctx, FlagInput{Target: FlagTarget{SourceID: l.movies.ID, RelPath: ptr("Heat (1995)/Heat copy.mkv")}}); err != nil {
		t.Fatal(err)
	}
	got, _ = l.decisions(l.dest, l.movies)
	orig := got["Heat (1995)/Heat (1995).mkv"]
	if got["Heat (1995)/Heat copy.mkv"].Tier != Full || orig.Tier != Full || orig.Follows != "Heat (1995)/Heat copy.mkv" {
		t.Fatalf("group %+v / %+v", got["Heat (1995)/Heat copy.mkv"], orig)
	}
	// The sidecars follow Heat's (group) decision.
	if got["Heat (1995)/Heat (1995).en.srt"].Tier != Full {
		t.Errorf("sidecar %+v", got["Heat (1995)/Heat (1995).en.srt"])
	}
}

func ptr[T any](v T) *T { return &v }

func TestNoRulesLoadsNoFacts(t *testing.T) {
	l := newLibrary(t)
	sd, err := l.eng.Decisions(l.ctx, nil, l.dest, l.movies)
	if err != nil {
		t.Fatal(err)
	}
	if !sd.All || sd.byFile != nil {
		t.Fatalf("no rules: %+v", sd)
	}
	d := sd.Decide(l.fileID(l.movies, "Charade (1963)/Charade (1963).mkv"))
	if d.Tier != Full || d.RuleName != FallbackRuleName || d.RuleID != 0 {
		t.Fatalf("decision %+v", d)
	}
	if _, ok := sd.ArrDateAdded("Heat (1995)/Heat (1995).mkv"); !ok {
		t.Error("the same-path replacement check lost the *arr dates")
	}
	// A rule that applies only elsewhere changes nothing here.
	other := l.destination("other", l.movies.ID)
	l.saveRules(RuleInput{Name: "skip there", Action: Skip, DestinationIDs: []int64{other}})
	if sd, _ = l.eng.Decisions(l.ctx, nil, l.dest, l.movies); !sd.All {
		t.Error("a rule of another destination was evaluated here")
	}
	if sd, _ = l.eng.Decisions(l.ctx, nil, other, l.movies); sd.All || sd.Decide(l.fileID(l.movies, "Charade (1963)/Charade (1963).mkv")).Tier != Skip {
		t.Error("the rule does not apply at its destination")
	}
}

func TestFactsForOneFile(t *testing.T) {
	l := newLibrary(t)
	l.specPreset()
	id := l.fileID(l.movies, "Heat (1995)/Heat (1995).en.srt")
	ft, err := l.eng.FactsFor(l.ctx, id, []int64{l.dest})
	if err != nil {
		t.Fatal(err)
	}
	if ft.Facts.Follows != "Heat (1995)/Heat (1995).mkv" || ft.Facts.Arr.Item == nil || ft.Facts.Arr.Item.Title != "Heat" {
		t.Fatalf("facts %+v", ft.Facts)
	}
	if d := ft.Decisions[l.dest]; d.Tier != Full || d.Follows == "" {
		t.Fatalf("decision %+v", d)
	}
	if _, err := l.eng.FactsFor(l.ctx, 99999, nil); err == nil {
		t.Error("an unknown file has facts")
	}
}

func TestStaleReferenceWarnings(t *testing.T) {
	l := newLibrary(t)
	rs, warnings, err := l.eng.SaveRules(l.ctx, 0, []RuleInput{
		{Name: "a", Action: Full, Conditions: []Condition{cnd(FieldArrTag, OpHas, "BUNKARR-FULL"), cnd(FieldArrTag, OpHas, "gone")}},
		{Name: "b", Action: Skip, Conditions: []Condition{cnd(FieldArrQualityProfile, OpIs, "Ultra-HD"), cnd(FieldSource, OpIs, 999),
			cnd(FieldArrRootFolder, OpIs, "/movies/")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rs.Revision != 1 {
		t.Fatalf("revision %d", rs.Revision)
	}
	want := map[[2]int]string{{0, 1}: `tag "gone"`, {1, 0}: `quality profile "Ultra-HD"`, {1, 1}: "source 999"}
	if len(warnings) != len(want) {
		t.Fatalf("warnings %+v", warnings)
	}
	for _, w := range warnings {
		if s, ok := want[[2]int{w.RuleIndex, w.ConditionIndex}]; !ok || !strings.Contains(w.Message, s) {
			t.Errorf("warning %+v", w)
		}
	}
	// A renamed profile: the index no longer knows the old name.
	l.exec(`UPDATE arr_meta SET name = 'HD 1080' WHERE kind = 'quality_profile'`)
	warnings, err = l.eng.Warnings(l.ctx, []Rule{{Name: "x", Conditions: []Condition{cnd(FieldArrQualityProfile, OpIs, "HD-1080p")}}})
	if err != nil || len(warnings) != 1 {
		t.Fatalf("renamed profile: %v %+v", err, warnings)
	}
	// A stale index knows nothing.
	l.clock.Advance(48 * time.Hour)
	warnings, _ = l.eng.Warnings(l.ctx, []Rule{{Name: "x", Conditions: []Condition{cnd(FieldArrTag, OpHas, "bunkarr-full")}}})
	if len(warnings) != 1 {
		t.Errorf("stale index warnings %+v", warnings)
	}
}

func TestFieldsAvailability(t *testing.T) {
	l := newLibrary(t)
	fields, err := l.eng.Fields(l.ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Field{}
	for _, f := range fields {
		by[f.Field] = f
	}
	if len(by) != len(fieldSpecs) {
		t.Fatalf("%d fields", len(by))
	}
	for _, name := range []string{FieldTautulliPlayCount, FieldTautulliLastWatch, FieldSeerrRequested, FieldSeerrRequestedBy, FieldMaintainerrPending} {
		if f := by[name]; f.Available || f.Reason == "" {
			t.Errorf("%s: available %v reason %q", name, f.Available, f.Reason)
		}
	}
	for _, name := range []string{FieldArrTag, FieldArrManaged, FieldSource, FieldFileSize, FieldFlagIrreplaceable, FieldPlexSection, FieldMediaGenre} {
		if !by[name].Available {
			t.Errorf("%s unavailable", name)
		}
	}
	if s := by[FieldArrTag].Suggestions; len(s) != 1 || s[0].Value != "bunkarr-full" || s[0].Label != "bunkarr-full (Radarr)" {
		t.Errorf("tag suggestions %+v", s)
	}
	if s := by[FieldSource].Suggestions; len(s) != 2 {
		t.Errorf("source suggestions %+v", s)
	}
	if s := by[FieldMediaGenre].Suggestions; len(s) != 1 || s[0].Value != "Crime" {
		t.Errorf("genre suggestions %+v", s)
	}
	// A provider makes its fields available.
	l2 := newLibrary(t, &fakeProvider{fields: []string{FieldMaintainerrPending}})
	fields, _ = l2.eng.Fields(l2.ctx)
	for _, f := range fields {
		if f.Field == FieldMaintainerrPending && !f.Available {
			t.Error("a provided field is unavailable")
		}
	}
}
