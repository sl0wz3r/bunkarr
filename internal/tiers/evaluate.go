package tiers

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Evaluator decides tiers from facts (design §8.1). It is pure: the rules, the revision and the
// evaluation time are fixed when it is made, and Decide reads nothing but its arguments.
type Evaluator struct {
	// rules are the enabled rules in priority order.
	rules    []compiledRule
	revision int64
	now      time.Time
}

// NewEvaluator returns an evaluator of rules (the enabled ones, in the order given) at revision,
// with now as the time file.age and tautulli.lastWatched compare against.
func NewEvaluator(rules []Rule, revision int64, now time.Time) *Evaluator {
	var enabled []Rule
	for _, r := range rules {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	return &Evaluator{rules: compile(enabled), revision: revision, now: now}
}

// Revision is the revision of the evaluator's rules.
func (e *Evaluator) Revision() int64 { return e.revision }

// Applies reports whether any enabled rule is evaluated at destination destID. When none is,
// every file there is full by the fallback (or the irreplaceable override), whatever its facts.
func (e *Evaluator) Applies(destID int64) bool {
	for _, r := range e.rules {
		if r.AppliesAt(destID) {
			return true
		}
	}
	return false
}

// Decide decides f's tier at destination destID:
//  1. a file flagged irreplaceable is full;
//  2. the enabled rules applying at destID are evaluated in order; the first that is true
//     decides, unless an earlier rule was unknown and its action is more protective: the most
//     protective unknown rule then decides (UnknownPromoted, S14);
//  3. no rule is true: the built-in fallback, full.
//
// Sidecars and hardlink groups are the engine's (they need other files' decisions).
func (e *Evaluator) Decide(destID int64, f *Facts) Decision {
	d := Decision{Revision: e.revision, Reasons: []Reason{}, Unknown: []Reason{}}
	if len(f.Flags) > 0 {
		v, _ := json.Marshal(true)
		d.Tier, d.RuleName = Full, IrreplaceableRuleName
		d.Reasons = []Reason{{ConditionIndex: -1, Field: FieldFlagIrreplaceable, Op: OpIs, Value: v, Actual: f.Flags,
			Result: True, Source: ReasonSource{Kind: SourceFlag}}}
		return d
	}
	var (
		promo        *compiledRule
		promoReasons []Reason
	)
	for i := range e.rules {
		r := &e.rules[i]
		if !r.AppliesAt(destID) {
			continue
		}
		res, reasons := e.evalRule(r, f)
		switch res {
		case True:
			if promo != nil && promo.Action.Protection() > r.Action.Protection() {
				d.Tier, d.RuleID, d.RuleName, d.Reasons, d.UnknownPromoted = promo.Action, promo.ID, promo.Name, promoReasons, true
				return d
			}
			d.Tier, d.RuleID, d.RuleName = r.Action, r.ID, r.Name
			d.Reasons = filterReasons(reasons, True)
			return d
		case Unknown:
			unknown := filterReasons(reasons, Unknown)
			d.Unknown = append(d.Unknown, unknown...)
			if promo == nil || r.Action.Protection() > promo.Action.Protection() {
				promo, promoReasons = r, unknown
			}
		}
	}
	// Nothing is more protective than the fallback: it is never unknown-promoted.
	d.Tier, d.RuleName = Full, FallbackRuleName
	return d
}

func filterReasons(rs []Reason, want Result) []Reason {
	out := []Reason{}
	for _, r := range rs {
		if r.Result == want {
			out = append(out, r)
		}
	}
	return out
}

// evalRule evaluates one rule: all is false if any condition is false, true if all are true,
// otherwise unknown; any is true if any is true, false if all are false, otherwise unknown; a
// rule without conditions is true.
func (e *Evaluator) evalRule(r *compiledRule, f *Facts) (Result, []Reason) {
	if len(r.conds) == 0 {
		return True, nil
	}
	reasons := make([]Reason, 0, len(r.conds))
	var trues, falses int
	for i, c := range r.conds {
		reason := e.evalCondition(c, f)
		reason.RuleID, reason.ConditionIndex = r.ID, i
		reasons = append(reasons, reason)
		switch reason.Result {
		case True:
			trues++
		case False:
			falses++
		}
	}
	n := len(r.conds)
	if r.Match == MatchAny {
		switch {
		case trues > 0:
			return True, reasons
		case falses == n:
			return False, reasons
		}
		return Unknown, reasons
	}
	switch {
	case falses > 0:
		return False, reasons
	case trues == n:
		return True, reasons
	}
	return Unknown, reasons
}

// unknownOf is an unknown reason.
func unknownOf(r Reason, why string) Reason {
	r.Result, r.Why = Unknown, why
	return r
}

// evalCondition evaluates one condition against f.
func (e *Evaluator) evalCondition(c cond, f *Facts) Reason {
	r := Reason{Field: c.Field, Op: c.Op, Value: c.Value}
	spec, _ := specOf(c.Field)
	r.Source = ReasonSource{Kind: spec.Source}
	if c.Op == "invalid" {
		return unknownOf(r, "the condition is not valid in this version of Bunkarr")
	}
	switch spec.Source {
	case SourceArr:
		return e.evalArr(c, f, r)
	case SourceCatalog:
		return e.evalCatalog(c, f, r)
	case SourcePlex:
		return evalPlex(c, f, r)
	case SourceSeerr:
		return evalSeerr(c, f, r)
	case SourceTautulli:
		return e.evalTautulli(c, f, r)
	case SourceMaintainerr:
		return evalMaintainerr(c, f, r)
	case SourceFlag:
		r.Actual = f.Flags
		r.Result = resultOf((len(f.Flags) > 0) == c.b)
		return r
	}
	return unknownOf(r, "unknown field")
}

// negated applies isNot, hasNot and notIn.
func negated(op string, res Result) Result {
	if op == OpIsNot || op == OpHasNot || op == OpNotIn {
		return res.Not()
	}
	return res
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

func (e *Evaluator) evalArr(c cond, f *Facts, r Reason) Reason {
	a := f.Arr
	r.Source.IntegrationID = a.IntegrationID
	if c.Field == FieldArrManaged {
		switch a.State {
		case ArrItem:
			r.Actual, r.Result = true, resultOf(c.b)
		case ArrUnmanaged:
			r.Actual, r.Result = false, resultOf(!c.b)
		default:
			return unknownOf(r, a.Why)
		}
		return r
	}
	if a.State == ArrUnknown {
		return unknownOf(r, a.Why)
	}
	if a.State == ArrUnmanaged {
		if c.Field == FieldMediaGenre {
			return unknownOf(r, "no *arr item holds this file")
		}
		// Outside every root folder of every *arr (all fresh): arr.* conditions are false.
		r.Actual = "unmanaged"
		r.Result = negated(c.Op, False)
		return r
	}
	it := a.Item
	var res Result
	switch c.Field {
	case FieldArrTag:
		r.Actual, res = it.Tags, resultOf(containsFold(it.Tags, c.s))
	case FieldArrQualityProfile:
		if it.QualityProfile == "" {
			r.Actual = it.QualityProfileID
			return unknownOf(r, fmt.Sprintf("quality profile #%d is not in the %s index", it.QualityProfileID, it.App))
		}
		r.Actual, res = it.QualityProfile, resultOf(strings.EqualFold(it.QualityProfile, c.s))
	case FieldArrRootFolder:
		r.Actual, res = it.RootFolder, resultOf(trimSlash(it.RootFolder) == c.s)
	case FieldArrMonitored:
		r.Actual, res = it.Monitored, resultOf(it.Monitored == c.b)
	case FieldMediaGenre:
		r.Actual, res = it.Genres, resultOf(containsFold(it.Genres, c.s))
	}
	r.Result = negated(c.Op, res)
	return r
}

func (e *Evaluator) evalCatalog(c cond, f *Facts, r Reason) Reason {
	var res Result
	switch c.Field {
	case FieldSource:
		r.Actual, res = f.SourceID, resultOf(f.SourceID == c.n)
	case FieldFileSize:
		r.Actual, res = f.Size, compareInt(c.Op, f.Size, c.n)
	case FieldFileAge:
		added := addedAt(f)
		r.Actual = added
		age := e.now.Sub(added)
		limit := time.Duration(c.n) * 24 * time.Hour
		if c.Op == OpOlderThan {
			res = resultOf(age > limit)
		} else {
			res = resultOf(age < limit)
		}
	}
	r.Result = negated(c.Op, res)
	return r
}

// addedAt is when the file was added: the *arr file's dateAdded, else Plex's addedAt, else the
// catalog's first sight of it (design §8.2 file.age).
func addedAt(f *Facts) time.Time {
	switch {
	case f.Arr.DateAdded != nil:
		return *f.Arr.DateAdded
	case f.Plex != nil && f.Plex.Known && f.Plex.AddedAt != nil:
		return *f.Plex.AddedAt
	}
	return f.FirstSeenAt
}

func compareInt(op string, got, want int64) Result {
	switch op {
	case OpGt:
		return resultOf(got > want)
	case OpGte:
		return resultOf(got >= want)
	case OpLt:
		return resultOf(got < want)
	case OpLte:
		return resultOf(got <= want)
	case OpEq:
		return resultOf(got == want)
	}
	return Unknown
}

func evalPlex(c cond, f *Facts, r Reason) Reason {
	p := f.Plex
	if p == nil {
		return unknownOf(r, "the file's Plex library is not known: set the source's Plex library")
	}
	r.Source.IntegrationID = p.IntegrationID
	if !p.Known {
		return unknownOf(r, p.Why)
	}
	r.Actual = p.Section
	r.Result = negated(c.Op, resultOf(p.Section == c.s))
	return r
}

// notProvided is the why of a field whose provider is not registered.
func notProvided(app string) string {
	return fmt.Sprintf("Bunkarr does not read %s facts yet", app)
}

func evalSeerr(c cond, f *Facts, r Reason) Reason {
	q := f.Requests
	if q == nil {
		return unknownOf(r, notProvided("Seerr"))
	}
	r.Source.IntegrationID = q.IntegrationID
	if q.Requested == Unknown {
		return unknownOf(r, q.Why)
	}
	switch c.Field {
	case FieldSeerrRequested:
		r.Actual = q.Requested == True
		r.Result = resultOf((q.Requested == True) == c.b)
	case FieldSeerrRequestedBy:
		r.Actual = q.Users
		hit := false
		if q.Requested == True {
			for _, u := range q.Users {
				hit = hit || slices.Contains(c.ints, u)
			}
		}
		r.Result = negated(c.Op, resultOf(hit))
	}
	return r
}

func (e *Evaluator) evalTautulli(c cond, f *Facts, r Reason) Reason {
	w := f.Watch
	if w == nil {
		return unknownOf(r, notProvided("Tautulli"))
	}
	r.Source.IntegrationID = w.IntegrationID
	if !w.Known {
		return unknownOf(r, w.Why)
	}
	const lowerBound = "play history is turned off for a user or this library, so the count is only a lower bound"
	if c.Field == FieldTautulliPlayCount {
		r.Actual = w.Plays
		res := compareInt(c.Op, w.Plays, c.n)
		if w.LowerBound && (c.Op == OpLt || c.Op == OpLte || c.Op == OpEq || res != True) {
			return unknownOf(r, lowerBound)
		}
		r.Result = res
		return r
	}
	r.Actual = w.LastWatched
	limit := time.Duration(c.n) * 24 * time.Hour
	switch {
	case w.LowerBound:
		if c.Op == OpNewerThan && w.LastWatched != nil && e.now.Sub(*w.LastWatched) < limit {
			r.Result = True
			return r
		}
		return unknownOf(r, lowerBound)
	case w.LastWatched == nil && w.Plays == 0:
		// Never watched: never is true, older than N true, newer than N false.
		r.Result = resultOf(c.Op != OpNewerThan)
	case w.LastWatched == nil:
		if c.Op == OpNever {
			r.Result = False
			return r
		}
		return unknownOf(r, "Tautulli counts plays of this file but has no date for them")
	case c.Op == OpNever:
		r.Result = False
	case c.Op == OpOlderThan:
		r.Result = resultOf(e.now.Sub(*w.LastWatched) > limit)
	default:
		r.Result = resultOf(e.now.Sub(*w.LastWatched) < limit)
	}
	return r
}

func evalMaintainerr(c cond, f *Facts, r Reason) Reason {
	m := f.Maintainerr
	if m == nil {
		return unknownOf(r, notProvided("Maintainerr"))
	}
	r.Source.IntegrationID = m.IntegrationID
	if m.Pending == Unknown {
		return unknownOf(r, m.Why)
	}
	if m.DeleteAfter != nil {
		r.Actual = m.DeleteAfter
	} else {
		r.Actual = m.Pending == True
	}
	r.Result = resultOf((m.Pending == True) == c.b)
	return r
}

// moreProtective returns the more protective of two decisions (a on a tie).
func moreProtective(a, b Decision) Decision {
	if b.Tier.Protection() > a.Tier.Protection() {
		return b
	}
	return a
}
