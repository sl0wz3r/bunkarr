package tiers

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestValidateErrorsNameRuleAndCondition(t *testing.T) {
	ok := RuleInput{Name: "ok", Action: Full, Conditions: []Condition{cnd(FieldSource, OpIs, 1)}}
	many := make([]Condition, MaxConditions+1)
	for i := range many {
		many[i] = cnd(FieldSource, OpIs, 1)
	}
	cases := []struct {
		name       string
		in         RuleInput
		rule, cond int
		msg        string
	}{
		{"unknown field", RuleInput{Name: "x", Action: Full, Conditions: []Condition{cnd(FieldSource, OpIs, 1), cnd("arr.colour", OpIs, "x")}}, 1, 1, "unknown field"},
		{"unknown op", RuleInput{Name: "x", Action: Full, Conditions: []Condition{cnd(FieldArrTag, OpGt, "x")}}, 1, 0, "does not support"},
		{"wrong type", RuleInput{Name: "x", Action: Full, Conditions: []Condition{cnd(FieldFileSize, OpGt, "big")}}, 1, 0, "must be a whole number"},
		{"negative", RuleInput{Name: "x", Action: Full, Conditions: []Condition{cnd(FieldFileSize, OpGt, -1)}}, 1, 0, "at least 0"},
		{"fraction", RuleInput{Name: "x", Action: Full, Conditions: []Condition{cnd(FieldFileAge, OpOlderThan, 1.5)}}, 1, 0, "whole number"},
		{"bool as string", RuleInput{Name: "x", Action: Full, Conditions: []Condition{cnd(FieldArrMonitored, OpIs, "yes")}}, 1, 0, "true or false"},
		{"missing value", RuleInput{Name: "x", Action: Full, Conditions: []Condition{{Field: FieldArrTag, Op: OpHas}}}, 1, 0, "needs a value"},
		{"never with a value", RuleInput{Name: "x", Action: Full, Conditions: []Condition{cnd(FieldTautulliLastWatch, OpNever, 3)}}, 1, 0, "takes no value"},
		{"bad section", RuleInput{Name: "x", Action: Full, Conditions: []Condition{cnd(FieldPlexSection, OpIs, "movies")}}, 1, 0, "plexIntegrationId"},
		{"empty user list", RuleInput{Name: "x", Action: Full, Conditions: []Condition{cnd(FieldSeerrRequestedBy, OpIn, []int{})}}, 1, 0, "list"},
		{"no name", RuleInput{Name: "  ", Action: Full}, 1, -1, "needs a name"},
		{"bad action", RuleInput{Name: "x", Action: "delete"}, 1, -1, "action"},
		{"bad match", RuleInput{Name: "x", Action: Full, Match: "most"}, 1, -1, "match"},
		{"too many conditions", RuleInput{Name: "x", Action: Full, Conditions: many}, 1, -1, "at most 32"},
		{"bad destination", RuleInput{Name: "x", Action: Full, DestinationIDs: []int64{0}}, 1, -1, "destination id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Validate([]RuleInput{ok, tc.in})
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err %v, want a ValidationError", err)
			}
			if ve.RuleIndex != tc.rule || ve.ConditionIndex != tc.cond || !strings.Contains(ve.Msg, tc.msg) {
				t.Fatalf("got rule %d condition %d %q, want %d %d %q", ve.RuleIndex, ve.ConditionIndex, ve.Msg, tc.rule, tc.cond, tc.msg)
			}
		})
	}
	tooMany := make([]RuleInput, MaxRules+1)
	for i := range tooMany {
		tooMany[i] = ok
	}
	if _, err := Validate(tooMany); err == nil {
		t.Error("101 rules accepted")
	}
	if _, err := Validate([]RuleInput{{ID: 3, Name: "a", Action: Full}, {ID: 3, Name: "b", Action: Full}}); err == nil {
		t.Error("a repeated id accepted")
	}
}

func TestValidateNormalizes(t *testing.T) {
	out, err := Validate([]RuleInput{{Name: "  Keep  ", Action: Skip, DestinationIDs: []int64{3, 1, 3},
		Conditions: []Condition{cnd(FieldArrRootFolder, OpIs, " /movies/ "), cnd(FieldSeerrRequestedBy, OpIn, []int{5, 2, 5}),
			cnd(FieldPlexSection, OpIsNot, "01:2"), cnd(FieldTautulliLastWatch, OpNever, nil)}}})
	if err == nil {
		t.Fatal("a section with a leading zero was accepted")
	}
	out, err = Validate([]RuleInput{{Name: "  Keep  ", Action: Skip, DestinationIDs: []int64{3, 1, 3},
		Conditions: []Condition{cnd(FieldArrRootFolder, OpIs, " /movies/ "), cnd(FieldSeerrRequestedBy, OpIn, []int{5, 2, 5}),
			cnd(FieldTautulliLastWatch, OpNever, nil)}}})
	if err != nil {
		t.Fatal(err)
	}
	r := out[0]
	if r.Name != "Keep" || r.Match != MatchAll || !reflect.DeepEqual(r.DestinationIDs, []int64{1, 3}) {
		t.Fatalf("normalized %+v", r)
	}
	if string(r.Conditions[0].Value) != `"/movies"` || string(r.Conditions[1].Value) != `[2,5]` || r.Conditions[2].Value != nil {
		t.Fatalf("values %s %s %s", r.Conditions[0].Value, r.Conditions[1].Value, r.Conditions[2].Value)
	}
}

func TestReplaceRules(t *testing.T) {
	e := newEnv(t)
	d1 := e.destination("one")
	d2 := e.destination("two")
	rs := e.saveRules(
		RuleInput{Name: "tagged", Action: Full, Conditions: []Condition{cnd(FieldArrTag, OpHas, "bunkarr-full")}},
		RuleInput{Name: "rest", Action: Manifest, DestinationIDs: []int64{d1}},
	)
	if rs.Revision != 1 || len(rs.Rules) != 2 || rs.Rules[0].Priority != 1 || rs.Rules[1].Priority != 2 {
		t.Fatalf("first save %+v", rs)
	}
	if rs.Rules[0].DestinationIDs != nil || !reflect.DeepEqual(rs.Rules[1].DestinationIDs, []int64{d1}) {
		t.Fatalf("destinations %v %v", rs.Rules[0].DestinationIDs, rs.Rules[1].DestinationIDs)
	}
	tagged, rest := rs.Rules[0], rs.Rules[1]

	// A stale revision is refused and changes nothing.
	if _, err := e.eng.Store().Replace(e.ctx, 0, nil); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("stale save: %v", err)
	}
	// Reorder, keep ids, drop none; a new rule gets a new id.
	rs2, err := e.eng.Store().Replace(e.ctx, 1, []RuleInput{
		{ID: rest.ID, Name: "rest", Action: Manifest, DestinationIDs: []int64{d1, d2}},
		{Name: "new", Action: Skip, Enabled: boolp(false), DestinationIDs: []int64{}},
		{ID: tagged.ID, Name: "tagged", Action: Full, Conditions: tagged.Conditions},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rs2.Revision != 2 || rs2.Rules[0].ID != rest.ID || rs2.Rules[2].ID != tagged.ID || rs2.Rules[1].ID <= tagged.ID {
		t.Fatalf("second save %+v", rs2)
	}
	if rs2.Rules[1].Enabled || rs2.Rules[1].DestinationIDs == nil || len(rs2.Rules[1].DestinationIDs) != 0 {
		t.Fatalf("new rule %+v", rs2.Rules[1])
	}
	if !rs2.Rules[2].CreatedAt.Equal(tagged.CreatedAt) {
		t.Error("a kept rule lost its createdAt")
	}
	// A dropped rule is deleted; its id is never reused.
	dropped := rs2.Rules[1].ID
	rs3, err := e.eng.Store().Replace(e.ctx, 2, []RuleInput{{ID: rest.ID, Name: "rest", Action: Manifest}, {Name: "again", Action: Skip}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs3.Rules) != 2 || rs3.Rules[1].ID == dropped || rs3.Rules[1].ID == tagged.ID {
		t.Fatalf("third save %+v", rs3)
	}
	// An id that is not stored, and a destination that does not exist, are refused.
	if _, err := e.eng.Store().Replace(e.ctx, 3, []RuleInput{{ID: dropped, Name: "x", Action: Skip}}); err == nil {
		t.Error("a deleted rule's id was accepted")
	}
	var ve *ValidationError
	if _, err := e.eng.Store().Replace(e.ctx, 3, []RuleInput{{Name: "x", Action: Skip, DestinationIDs: []int64{999}}}); !errors.As(err, &ve) {
		t.Errorf("an unknown destination: %v", err)
	}
	if rev, _ := e.eng.Revision(e.ctx); rev != 3 {
		t.Errorf("revision %d after refused saves", rev)
	}
}

func TestRemoveDestinationLeavesEmptyArray(t *testing.T) {
	e := newEnv(t)
	d1 := e.destination("one")
	d2 := e.destination("two")
	e.saveRules(RuleInput{Name: "only one", Action: Skip, DestinationIDs: []int64{d1}},
		RuleInput{Name: "both", Action: Manifest, DestinationIDs: []int64{d1, d2}},
		RuleInput{Name: "all", Action: Manifest})
	if err := e.eng.Store().RemoveDestination(e.ctx, d1); err != nil {
		t.Fatal(err)
	}
	rs, err := e.eng.Store().Rules(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Rules[0].DestinationIDs == nil || len(rs.Rules[0].DestinationIDs) != 0 {
		t.Errorf("only one: %v (want [] = nowhere, never null = everywhere)", rs.Rules[0].DestinationIDs)
	}
	if !reflect.DeepEqual(rs.Rules[1].DestinationIDs, []int64{d2}) || rs.Rules[2].DestinationIDs != nil {
		t.Errorf("both %v all %v", rs.Rules[1].DestinationIDs, rs.Rules[2].DestinationIDs)
	}
	if rs.Revision != 1 {
		t.Errorf("revision %d", rs.Revision)
	}
	ev := NewEvaluator(rs.Rules, rs.Revision, evalNow)
	if ev.Decide(d1, managed()).Tier != Manifest {
		t.Error("the rule for all destinations stopped applying")
	}
}

func TestPresets(t *testing.T) {
	ps := Presets()
	if len(ps) != 3 || ps[0].ID != PresetEverything || len(ps[0].Rules) != 0 {
		t.Fatalf("presets %+v", ps)
	}
	for _, p := range ps {
		if _, err := Validate(p.Rules); err != nil {
			t.Errorf("preset %s: %v", p.ID, err)
		}
	}
	spec := draftRules(ps[1].Rules)
	ev := NewEvaluator(spec, 1, evalNow)
	tagged := managed()
	untagged := managed()
	untagged.Arr.Item.Tags = []string{}
	stale := managed()
	stale.Arr = ArrFacts{State: ArrUnknown, Why: "Radarr cache is 31 h old"}
	home := managed()
	home.Arr = ArrFacts{State: ArrUnmanaged}
	for _, tc := range []struct {
		name     string
		f        *Facts
		want     Tier
		promoted bool
	}{{"tagged", tagged, Full, false}, {"untagged", untagged, Manifest, false}, {"stale", stale, Full, true}, {"outside the root folders", home, Manifest, false}} {
		d := ev.Decide(1, tc.f)
		if d.Tier != tc.want || d.UnknownPromoted != tc.promoted {
			t.Errorf("spec preset, %s: %+v", tc.name, d)
		}
	}
	if !strings.Contains(ps[2].Description, "no authentication") || ps[2].Rules[0].Action != Manifest {
		t.Errorf("maintainerr preset %+v", ps[2])
	}
}
