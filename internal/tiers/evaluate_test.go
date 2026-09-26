package tiers

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

var evalNow = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func daysAgo(n float64) *time.Time {
	t := evalNow.Add(-time.Duration(n * float64(24*time.Hour)))
	return &t
}

// managed is a file claimed by a Radarr movie.
func managed() *Facts {
	added := daysAgo(10)
	return &Facts{FileID: 1, SourceID: 3, RelPath: "Heat (1995)/Heat.mkv", Size: 5000, FirstSeenAt: *daysAgo(2), Flags: []int64{},
		Arr: ArrFacts{State: ArrItem, IntegrationID: 7, DateAdded: added, Item: &ArrItemFacts{IntegrationID: 7, App: "Radarr",
			Tags: []string{"bunkarr-full", "4k"}, QualityProfile: "HD-1080p", QualityProfileID: 4, RootFolder: "/movies/",
			Monitored: true, Genres: []string{"Crime", "Drama"}}},
		Plex:        &PlexFacts{Known: true, IntegrationID: 1, Section: "1:2"},
		Watch:       &WatchFacts{Known: true, IntegrationID: 5, Plays: 3, LastWatched: daysAgo(40)},
		Requests:    &RequestFacts{Requested: True, IntegrationID: 6, Users: []int64{2, 9}},
		Maintainerr: &MaintainerrFacts{Pending: False, IntegrationID: 8},
	}
}

func eval(t *testing.T, c Condition, f *Facts) Reason {
	t.Helper()
	cc, err := compileCondition(c, 0, 0)
	if err != nil {
		t.Fatalf("compile %v: %v", c, err)
	}
	return NewEvaluator(nil, 1, evalNow).evalCondition(cc, f)
}

func TestConditionsEveryFieldAndOp(t *testing.T) {
	unmanaged := func() *Facts { f := managed(); f.Arr = ArrFacts{State: ArrUnmanaged}; return f }
	unknownArr := func() *Facts {
		f := managed()
		f.Arr = ArrFacts{State: ArrUnknown, IntegrationID: 7, Why: "Radarr cache is 31 h old"}
		return f
	}
	lower := func() *Facts {
		f := managed()
		f.Watch = &WatchFacts{Known: true, Plays: 3, LastWatched: daysAgo(5), LowerBound: true}
		return f
	}
	never := func() *Facts { f := managed(); f.Watch = &WatchFacts{Known: true}; return f }
	bare := func() *Facts {
		f := managed()
		f.Plex, f.Watch, f.Requests, f.Maintainerr = nil, nil, nil, nil
		return f
	}
	flagged := func() *Facts { f := managed(); f.Flags = []int64{4}; return f }
	cases := []struct {
		name string
		c    Condition
		f    func() *Facts
		want Result
	}{
		{"managed is true", cnd(FieldArrManaged, OpIs, true), managed, True},
		{"managed is false", cnd(FieldArrManaged, OpIs, false), managed, False},
		{"unmanaged: managed is false", cnd(FieldArrManaged, OpIs, false), unmanaged, True},
		{"unknown: managed", cnd(FieldArrManaged, OpIs, true), unknownArr, Unknown},
		{"tag has (case)", cnd(FieldArrTag, OpHas, "BUNKARR-FULL"), managed, True},
		{"tag has missing", cnd(FieldArrTag, OpHas, "kids"), managed, False},
		{"tag hasNot", cnd(FieldArrTag, OpHasNot, "kids"), managed, True},
		{"unmanaged tag has", cnd(FieldArrTag, OpHas, "bunkarr-full"), unmanaged, False},
		{"unmanaged tag hasNot", cnd(FieldArrTag, OpHasNot, "bunkarr-full"), unmanaged, True},
		{"unknown tag hasNot", cnd(FieldArrTag, OpHasNot, "x"), unknownArr, Unknown},
		{"profile is", cnd(FieldArrQualityProfile, OpIs, "hd-1080P"), managed, True},
		{"profile isNot", cnd(FieldArrQualityProfile, OpIsNot, "HD-1080p"), managed, False},
		{"unmanaged profile isNot", cnd(FieldArrQualityProfile, OpIsNot, "HD-1080p"), unmanaged, True},
		{"root folder is (slashes)", cnd(FieldArrRootFolder, OpIs, "/movies"), managed, True},
		{"root folder isNot", cnd(FieldArrRootFolder, OpIsNot, "/movies-4k/"), managed, True},
		{"monitored is true", cnd(FieldArrMonitored, OpIs, true), managed, True},
		{"monitored is false", cnd(FieldArrMonitored, OpIs, false), managed, False},
		{"unmanaged monitored is false", cnd(FieldArrMonitored, OpIs, false), unmanaged, False},
		{"genre has", cnd(FieldMediaGenre, OpHas, "crime"), managed, True},
		{"genre hasNot", cnd(FieldMediaGenre, OpHasNot, "Crime"), managed, False},
		{"unmanaged genre", cnd(FieldMediaGenre, OpHas, "Crime"), unmanaged, Unknown},
		{"source is", cnd(FieldSource, OpIs, 3), managed, True},
		{"source isNot", cnd(FieldSource, OpIsNot, 3), managed, False},
		{"size gt", cnd(FieldFileSize, OpGt, 4999), managed, True},
		{"size gt equal", cnd(FieldFileSize, OpGt, 5000), managed, False},
		{"size gte", cnd(FieldFileSize, OpGte, 5000), managed, True},
		{"size lt", cnd(FieldFileSize, OpLt, 5000), managed, False},
		{"size lte", cnd(FieldFileSize, OpLte, 5000), managed, True},
		{"age olderThan (arr dateAdded)", cnd(FieldFileAge, OpOlderThan, 9), managed, True},
		{"age newerThan", cnd(FieldFileAge, OpNewerThan, 9), managed, False},
		{"age falls back to first seen", cnd(FieldFileAge, OpNewerThan, 3), func() *Facts { f := managed(); f.Arr.DateAdded = nil; return f }, True},
		{"age plex addedAt before first seen", cnd(FieldFileAge, OpOlderThan, 20), func() *Facts {
			f := managed()
			f.Arr.DateAdded, f.Plex.AddedAt = nil, daysAgo(30)
			return f
		}, True},
		{"section is", cnd(FieldPlexSection, OpIs, "1:2"), managed, True},
		{"section isNot", cnd(FieldPlexSection, OpIsNot, "1:3"), managed, True},
		{"section unknown", cnd(FieldPlexSection, OpIs, "1:2"), bare, Unknown},
		{"section not known", cnd(FieldPlexSection, OpIsNot, "1:2"), func() *Facts { f := managed(); f.Plex = &PlexFacts{Why: "stale"}; return f }, Unknown},
		{"requested is true", cnd(FieldSeerrRequested, OpIs, true), managed, True},
		{"requested is false", cnd(FieldSeerrRequested, OpIs, false), managed, False},
		{"requested unknown", cnd(FieldSeerrRequested, OpIs, false), func() *Facts {
			f := managed()
			f.Requests = &RequestFacts{Requested: Unknown, Why: "Seerr cache is stale"}
			return f
		}, Unknown},
		{"requested no provider", cnd(FieldSeerrRequested, OpIs, true), bare, Unknown},
		{"requestedBy in", cnd(FieldSeerrRequestedBy, OpIn, []int{9, 11}), managed, True},
		{"requestedBy in other", cnd(FieldSeerrRequestedBy, OpIn, []int{11}), managed, False},
		{"requestedBy notIn", cnd(FieldSeerrRequestedBy, OpNotIn, []int{11}), managed, True},
		{"requestedBy not requested notIn", cnd(FieldSeerrRequestedBy, OpNotIn, []int{2}), func() *Facts {
			f := managed()
			f.Requests = &RequestFacts{Requested: False, Users: []int64{}}
			return f
		}, True},
		{"plays gt", cnd(FieldTautulliPlayCount, OpGt, 2), managed, True},
		{"plays eq", cnd(FieldTautulliPlayCount, OpEq, 3), managed, True},
		{"plays lt", cnd(FieldTautulliPlayCount, OpLt, 3), managed, False},
		{"lower bound gt observed", cnd(FieldTautulliPlayCount, OpGt, 2), lower, True},
		{"lower bound gt not observed", cnd(FieldTautulliPlayCount, OpGt, 5), lower, Unknown},
		{"lower bound gte observed", cnd(FieldTautulliPlayCount, OpGte, 3), lower, True},
		{"lower bound lt", cnd(FieldTautulliPlayCount, OpLt, 10), lower, Unknown},
		{"lower bound lte", cnd(FieldTautulliPlayCount, OpLte, 10), lower, Unknown},
		{"lower bound eq", cnd(FieldTautulliPlayCount, OpEq, 3), lower, Unknown},
		{"plays no provider", cnd(FieldTautulliPlayCount, OpGt, 0), bare, Unknown},
		{"plays not fresh", cnd(FieldTautulliPlayCount, OpGt, 0), func() *Facts { f := managed(); f.Watch = &WatchFacts{Why: "stale"}; return f }, Unknown},
		{"watched olderThan", cnd(FieldTautulliLastWatch, OpOlderThan, 30), managed, True},
		{"watched olderThan strict", cnd(FieldTautulliLastWatch, OpOlderThan, 40), managed, False},
		{"watched newerThan", cnd(FieldTautulliLastWatch, OpNewerThan, 50), managed, True},
		{"watched never", cnd(FieldTautulliLastWatch, OpNever, nil), managed, False},
		{"no row: never", cnd(FieldTautulliLastWatch, OpNever, nil), never, True},
		{"no row: olderThan", cnd(FieldTautulliLastWatch, OpOlderThan, 365), never, True},
		{"no row: newerThan", cnd(FieldTautulliLastWatch, OpNewerThan, 365), never, False},
		{"history off: newerThan observed", cnd(FieldTautulliLastWatch, OpNewerThan, 7), lower, True},
		{"history off: newerThan not observed", cnd(FieldTautulliLastWatch, OpNewerThan, 3), lower, Unknown},
		{"history off: olderThan", cnd(FieldTautulliLastWatch, OpOlderThan, 3), lower, Unknown},
		{"history off: never", cnd(FieldTautulliLastWatch, OpNever, nil), lower, Unknown},
		{"pending is false", cnd(FieldMaintainerrPending, OpIs, false), managed, True},
		{"pending is true", cnd(FieldMaintainerrPending, OpIs, true), managed, False},
		{"pending undecided", cnd(FieldMaintainerrPending, OpIs, false), func() *Facts {
			f := managed()
			f.Maintainerr = &MaintainerrFacts{Pending: Unknown, Why: "undecided"}
			return f
		}, Unknown},
		{"pending no provider", cnd(FieldMaintainerrPending, OpIs, true), bare, Unknown},
		{"flag is true", cnd(FieldFlagIrreplaceable, OpIs, true), flagged, True},
		{"flag is false", cnd(FieldFlagIrreplaceable, OpIs, false), managed, True},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := eval(t, tc.c, tc.f())
			if r.Result != tc.want {
				t.Fatalf("result %s, want %s (reason %+v)", r.Result, tc.want, r)
			}
			if r.Result == Unknown && r.Why == "" {
				t.Errorf("an unknown result has no why: %+v", r)
			}
		})
	}
}

func rule(id int64, name string, action Tier, match Match, conds ...Condition) Rule {
	return Rule{ID: id, Priority: int(id), Name: name, Enabled: true, Match: match, Conditions: conds, Action: action}
}

func TestRuleMatchAllAndAny(t *testing.T) {
	yes := cnd(FieldSource, OpIs, 3)
	no := cnd(FieldSource, OpIs, 4)
	unk := cnd(FieldPlexSection, OpIs, "1:2")
	f := managed()
	f.Plex = nil
	cases := []struct {
		match Match
		conds []Condition
		want  Result
	}{
		{MatchAll, nil, True},
		{MatchAny, nil, True},
		{MatchAll, []Condition{yes, yes}, True},
		{MatchAll, []Condition{yes, no}, False},
		{MatchAll, []Condition{yes, unk}, Unknown},
		{MatchAll, []Condition{unk, no}, False},
		{MatchAny, []Condition{no, yes}, True},
		{MatchAny, []Condition{no, no}, False},
		{MatchAny, []Condition{no, unk}, Unknown},
		{MatchAny, []Condition{unk, yes}, True},
	}
	for i, tc := range cases {
		ev := NewEvaluator([]Rule{rule(1, "r", Skip, tc.match, tc.conds...)}, 1, evalNow)
		got, _ := ev.evalRule(&ev.rules[0], f)
		if got != tc.want {
			t.Errorf("case %d (%s %d conditions): %s, want %s", i, tc.match, len(tc.conds), got, tc.want)
		}
	}
}

func TestProtectiveUnknownRule(t *testing.T) {
	f := managed()
	f.Maintainerr = &MaintainerrFacts{Pending: Unknown, IntegrationID: 8, Why: "Maintainerr cache is 30 h old"}
	pending := cnd(FieldMaintainerrPending, OpIs, true)
	t.Run("an earlier unknown full rule beats a later manifest", func(t *testing.T) {
		ev := NewEvaluator([]Rule{rule(1, "keep pending", Full, MatchAll, pending), rule(2, "rest", Manifest, MatchAll)}, 3, evalNow)
		d := ev.Decide(9, f)
		if d.Tier != Full || d.RuleID != 1 || !d.UnknownPromoted || d.Revision != 3 {
			t.Fatalf("decision %+v", d)
		}
		if len(d.Reasons) != 1 || d.Reasons[0].Result != Unknown || len(d.Unknown) != 1 {
			t.Fatalf("reasons %+v unknown %+v", d.Reasons, d.Unknown)
		}
	})
	t.Run("an earlier unknown skip rule does not beat a later manifest", func(t *testing.T) {
		ev := NewEvaluator([]Rule{rule(1, "drop pending", Skip, MatchAll, pending), rule(2, "rest", Manifest, MatchAll)}, 3, evalNow)
		d := ev.Decide(9, f)
		if d.Tier != Manifest || d.RuleID != 2 || d.UnknownPromoted {
			t.Fatalf("decision %+v", d)
		}
		if len(d.Unknown) != 1 || d.Unknown[0].RuleID != 1 || d.Unknown[0].Why != "Maintainerr cache is 30 h old" {
			t.Fatalf("unknown %+v", d.Unknown)
		}
	})
	t.Run("the most protective of several unknown rules decides", func(t *testing.T) {
		ev := NewEvaluator([]Rule{rule(1, "a", Manifest, MatchAll, pending), rule(2, "b", Full, MatchAll, pending),
			rule(3, "c", Skip, MatchAll)}, 3, evalNow)
		d := ev.Decide(9, f)
		if d.Tier != Full || d.RuleID != 2 || !d.UnknownPromoted || len(d.Unknown) != 2 {
			t.Fatalf("decision %+v", d)
		}
	})
	t.Run("a later unknown rule changes nothing", func(t *testing.T) {
		ev := NewEvaluator([]Rule{rule(1, "rest", Manifest, MatchAll), rule(2, "keep pending", Full, MatchAll, pending)}, 3, evalNow)
		d := ev.Decide(9, f)
		if d.Tier != Manifest || d.UnknownPromoted || len(d.Unknown) != 0 {
			t.Fatalf("decision %+v", d)
		}
	})
	t.Run("no rule true: the fallback is full and never promoted", func(t *testing.T) {
		ev := NewEvaluator([]Rule{rule(1, "drop pending", Skip, MatchAll, pending)}, 3, evalNow)
		d := ev.Decide(9, f)
		if d.Tier != Full || d.RuleID != 0 || d.RuleName != FallbackRuleName || d.UnknownPromoted || len(d.Unknown) != 1 {
			t.Fatalf("decision %+v", d)
		}
	})
}

func TestIrreplaceableOverridesRules(t *testing.T) {
	f := managed()
	f.Flags = []int64{12}
	ev := NewEvaluator([]Rule{rule(1, "skip all", Skip, MatchAll)}, 2, evalNow)
	d := ev.Decide(1, f)
	if d.Tier != Full || d.RuleID != 0 || d.RuleName != IrreplaceableRuleName || len(d.Reasons) != 1 {
		t.Fatalf("decision %+v", d)
	}
	if got := d.Reasons[0].Text(); got != "irreplaceable (flag #12)" {
		t.Errorf("text %q", got)
	}
}

func TestDestinationScopes(t *testing.T) {
	f := managed()
	skipAt := func(ids []int64) Rule {
		r := rule(1, "skip", Skip, MatchAll)
		r.DestinationIDs = ids
		return r
	}
	cases := []struct {
		name string
		ids  []int64
		dest int64
		want Tier
	}{
		{"null applies everywhere", nil, 5, Skip},
		{"a list applies there", []int64{5, 6}, 6, Skip},
		{"a list does not apply elsewhere", []int64{5, 6}, 7, Full},
		{"empty applies nowhere", []int64{}, 5, Full},
		{"a deleted destination's id matches no destination", []int64{99}, 5, Full},
	}
	for _, tc := range cases {
		ev := NewEvaluator([]Rule{skipAt(tc.ids)}, 1, evalNow)
		if got := ev.Decide(tc.dest, f).Tier; got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
		if ev.Applies(tc.dest) != (tc.want == Skip) {
			t.Errorf("%s: Applies = %v", tc.name, ev.Applies(tc.dest))
		}
	}
	disabled := skipAt(nil)
	disabled.Enabled = false
	if ev := NewEvaluator([]Rule{disabled}, 1, evalNow); ev.Applies(5) || ev.Decide(5, f).Tier != Full {
		t.Error("a disabled rule was evaluated")
	}
}

func TestDecisionJSON(t *testing.T) {
	f := managed()
	ev := NewEvaluator([]Rule{rule(4, "tagged", Full, MatchAll, cnd(FieldArrTag, OpHas, "bunkarr-full"))}, 7, evalNow)
	d := ev.Decide(1, f)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var back Decision
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Tier != Full || back.RuleID != 4 || back.Revision != 7 || len(back.Reasons) != 1 || back.Reasons[0].Result != True {
		t.Fatalf("round trip %s -> %+v", b, back)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	want := []string{"tier", "ruleId", "ruleName", "reasons", "unknown", "unknownPromoted", "revision"}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("decision JSON has no %s: %s", k, b)
		}
	}
	if got := d.Text(); got != `full (rule "tagged"): *arr tag has "bunkarr-full": true (bunkarr-full, 4k)` {
		t.Errorf("text %q", got)
	}
}

func TestInvalidStoredConditionIsUnknown(t *testing.T) {
	r := rule(1, "future", Skip, MatchAll, Condition{Field: "future.field", Op: "is", Value: raw(true)})
	ev := NewEvaluator([]Rule{r}, 1, evalNow)
	d := ev.Decide(1, managed())
	if d.Tier != Full || len(d.Unknown) != 1 {
		t.Fatalf("decision %+v", d)
	}
	if !reflect.DeepEqual(d.Unknown[0].Result, Unknown) {
		t.Fatal("not unknown")
	}
}

// BenchmarkDecide200k decides 200,000 files with the spec preset (design §8.6: a preview of 100k
// files within 5 s; this benchmark runs outside -short, with -bench).
func BenchmarkDecide200k(b *testing.B) {
	rules := draftRules(Presets()[1].Rules)
	facts := make([]*Facts, 200_000)
	for i := range facts {
		f := managed()
		f.FileID, f.RelPath = int64(i+1), "m/"+string(rune('a'+i%26))+"/"+time.Duration(i).String()
		if i%3 == 0 {
			f.Arr.Item = &ArrItemFacts{Tags: []string{"x"}}
		}
		if i%5 == 0 {
			f.Group = "g" + time.Duration(i/10).String()
		}
		facts[i] = f
	}
	ev := NewEvaluator(rules, 1, evalNow)
	b.ResetTimer()
	for range b.N {
		if got := decideFiles(ev, 1, facts); len(got) != len(facts) {
			b.Fatalf("%d decisions", len(got))
		}
	}
}
