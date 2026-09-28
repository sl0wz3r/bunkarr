package restic

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
)

// The keep set of the restic retention job (docs/design/phase4.md §6.5 step 3, S24, D20).
// Bunkarr never runs forget with a --keep-* policy: it computes what to keep here, as a pure
// function, and forgets the rest by id. Per group of this destination row (ParseTags):
//
//   - the newest snapshot, and the newest complete one;
//   - the base of each source (its newest recorded snapshot: every NULL record depends on it);
//   - every snapshot a record or a config version row references (Refs);
//   - media groups: the newest complete snapshot of each of the last Daily days, Weekly ISO
//     weeks, Monthly months and Yearly years that have one, in Loc (restic's bucket semantics:
//     a period without a complete snapshot is not counted);
//   - everything of an orphan group (a source or integration deleted in Bunkarr), until a user
//     action.
//
// An incomplete snapshot (a batch of a job, or one no row recorded) is kept only by the first
// three rules. Config versions are kept here: their runners decide (engine_forget requests, which
// FilterForget checks against the same protections). A snapshot whose tags do not parse into
// exactly one group of this row (another engine_tag, a missing or duplicated tag), a duplicated
// id and one dated after Now (a clock that went back) are never forgotten.

// Reasons of the keep map.
const (
	KeepForeign    = "not a group of this destination"
	KeepDuplicate  = "listed twice"
	KeepFuture     = "newer than now"
	KeepNewest     = "newest of its group"
	KeepComplete   = "newest complete of its group"
	KeepBase       = "base of its source"
	KeepReferenced = "referenced by a record"
	KeepOrphan     = "orphan group (its source or integration was deleted)"
	KeepConfig     = "config version (its runner decides)"
	KeepMismatch   = "recorded for another source"
	KeepDaily      = "daily"
	KeepWeekly     = "weekly"
	KeepMonthly    = "monthly"
	KeepYearly     = "yearly"
)

// Recorded is what engine_snapshots says of a snapshot of this destination.
type Recorded struct {
	// SourceID is the row's source (0 once the source was deleted).
	SourceID int64
	// Complete marks the last batch of a job (retention's buckets count complete snapshots only).
	Complete bool
	Batch    int
}

// Retention is a destination's snapshot retention (snapshotDaily, snapshotWeekly,
// snapshotMonthly, snapshotYearly; every value may be 0).
type Retention struct {
	Daily, Weekly, Monthly, Yearly int
}

// KeepInput is what KeepMedia and FilterForget decide from.
type KeepInput struct {
	// Snapshots is the repository's listing (restic snapshots --json), taken right before the
	// forget (S24).
	Snapshots []Snapshot
	// Recorded are the engine_snapshots rows of this destination by snapshot id.
	Recorded map[string]Recorded
	// Base maps a source id to its base snapshot id (its newest recorded one).
	Base map[int64]string
	// Refs are the snapshots a live record, a retained record not yet expired (replaced rows
	// included) or a config version row references.
	Refs map[string]bool
	// LiveSources and LiveIntegrations are the ids that still exist in Bunkarr; a group of an id
	// missing here is an orphan group.
	LiveSources      map[int64]bool
	LiveIntegrations map[int64]bool
	Retention        Retention
	EngineTag        string
	// Now is the retention job's clock; Loc the container's time zone for the buckets (UTC when
	// nil).
	Now time.Time
	Loc *time.Location
}

// keepPlan is the per-snapshot analysis shared by KeepMedia and FilterForget.
type keepPlan struct {
	keep   map[string]string
	groups map[Group][]Snapshot
	listed map[string]bool
}

// protect computes the protections of S24 (everything but the buckets).
func protect(in KeepInput) keepPlan {
	p := keepPlan{keep: map[string]string{}, groups: map[Group][]Snapshot{}, listed: map[string]bool{}}
	count := map[string]int{}
	for _, s := range in.Snapshots {
		count[s.ID]++
	}
	for _, s := range in.Snapshots {
		p.listed[s.ID] = true
		info, ok := ParseTags(s.Tags, in.EngineTag)
		switch {
		case count[s.ID] > 1:
			p.keep[s.ID] = KeepDuplicate
		case !ok || s.ID == "":
			p.keep[s.ID] = KeepForeign
		default:
			p.groups[info.Group] = append(p.groups[info.Group], s)
		}
	}
	for g, snaps := range p.groups {
		slices.SortStableFunc(snaps, func(a, b Snapshot) int {
			if c := b.Time.Compare(a.Time); c != 0 {
				return c
			}
			return cmp.Compare(b.ID, a.ID)
		})
		p.groups[g] = snaps
		mark := func(id, why string) {
			if _, done := p.keep[id]; !done {
				p.keep[id] = why
			}
		}
		orphan := false
		switch g.Kind {
		case engines.VersionMedia:
			orphan = !in.LiveSources[g.SourceID]
		case engines.VersionPlexDB, engines.VersionArr:
			orphan = !in.LiveIntegrations[g.IntegrationID]
		}
		mark(snaps[0].ID, KeepNewest)
		for _, s := range snaps {
			if in.Recorded[s.ID].Complete {
				mark(s.ID, KeepComplete)
				break
			}
		}
		for _, s := range snaps {
			switch {
			case s.Time.After(in.Now):
				mark(s.ID, KeepFuture)
			case in.Refs[s.ID]:
				mark(s.ID, KeepReferenced)
			case g.Kind == engines.VersionMedia && in.Base[g.SourceID] == s.ID:
				mark(s.ID, KeepBase)
			case orphan:
				mark(s.ID, KeepOrphan)
			case g.Kind != engines.VersionMedia:
			default:
				if r, ok := in.Recorded[s.ID]; ok && r.SourceID != 0 && r.SourceID != g.SourceID {
					mark(s.ID, KeepMismatch)
				}
			}
		}
	}
	// Every source's base, wherever it is listed.
	for src, id := range in.Base {
		if _, done := p.keep[id]; !done && p.listed[id] && src != 0 {
			p.keep[id] = KeepBase
		}
	}
	return p
}

// KeepMedia computes the retention job's forget list (§6.5 step 3, S24): every snapshot of a
// media group of this destination row that no rule keeps, oldest first, and the reason each kept
// snapshot is kept. Config version snapshots are all kept here (FilterForget checks their
// requests).
func KeepMedia(in KeepInput) (forget []string, keep map[string]string) {
	p := protect(in)
	loc := in.Loc
	if loc == nil {
		loc = time.UTC
	}
	var candidates []Snapshot
	for g, snaps := range p.groups {
		if g.Kind != engines.VersionMedia {
			for _, s := range snaps {
				if _, ok := p.keep[s.ID]; !ok {
					p.keep[s.ID] = KeepConfig
				}
			}
			continue
		}
		var complete []Snapshot
		for _, s := range snaps {
			if in.Recorded[s.ID].Complete {
				complete = append(complete, s)
			}
		}
		for id, why := range bucketKeep(complete, in.Retention, loc) {
			if _, ok := p.keep[id]; !ok {
				p.keep[id] = why
			}
		}
		for _, s := range snaps {
			if _, ok := p.keep[s.ID]; !ok {
				candidates = append(candidates, s)
			}
		}
	}
	slices.SortStableFunc(candidates, func(a, b Snapshot) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	for _, s := range candidates {
		forget = append(forget, s.ID)
	}
	return forget, p.keep
}

// bucketKeep applies the daily, weekly, monthly and yearly rules to snapshots sorted newest
// first, with restic's semantics: walking from the newest, a snapshot is kept for a rule when
// its period (day, ISO week, month, year in loc) differs from the last one that rule kept, until
// the rule's count is used up. Periods without a snapshot are therefore not counted.
func bucketKeep(newestFirst []Snapshot, r Retention, loc *time.Location) map[string]string {
	keep := map[string]string{}
	rules := []struct {
		n    int
		why  string
		key  func(time.Time) int
		last int
	}{
		{r.Daily, KeepDaily, func(t time.Time) int { y, m, d := t.Date(); return y*10000 + int(m)*100 + d }, -1},
		{r.Weekly, KeepWeekly, func(t time.Time) int { y, w := t.ISOWeek(); return y*100 + w }, -1},
		{r.Monthly, KeepMonthly, func(t time.Time) int { return t.Year()*100 + int(t.Month()) }, -1},
		{r.Yearly, KeepYearly, func(t time.Time) int { return t.Year() }, -1},
	}
	for _, s := range newestFirst {
		t := s.Time.In(loc)
		for i := range rules {
			rule := &rules[i]
			if rule.n <= 0 {
				continue
			}
			if k := rule.key(t); k != rule.last {
				rule.last = k
				rule.n--
				if _, ok := keep[s.ID]; !ok {
					keep[s.ID] = rule.why
				}
			}
		}
	}
	return keep
}

// FilterForget checks forget requests (engine_forget: config versions their runners pruned)
// against the protections of S24, on the listing taken right before the forget: an id that is not
// listed (already gone) or that a protection keeps (the newest of its group, a reference, another
// engine_tag, an orphan group …) is dropped with the reason. A media snapshot is never forgotten
// through a request.
func FilterForget(in KeepInput, requested []string) (forget []string, dropped map[string]string) {
	p := protect(in)
	dropped = map[string]string{}
	media := map[string]bool{}
	for g, snaps := range p.groups {
		if g.Kind == engines.VersionMedia {
			for _, s := range snaps {
				media[s.ID] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, id := range requested {
		switch {
		case seen[id]:
			continue
		case !p.listed[id]:
			dropped[id] = "not in the repository"
		case p.keep[id] != "":
			dropped[id] = p.keep[id]
		case media[id]:
			dropped[id] = "a media snapshot (only the keep set forgets those)"
		default:
			forget = append(forget, id)
		}
		seen[id] = true
	}
	return forget, dropped
}

// String renders a retention for messages.
func (r Retention) String() string {
	return fmt.Sprintf("daily %d, weekly %d, monthly %d, yearly %d", r.Daily, r.Weekly, r.Monthly, r.Yearly)
}
