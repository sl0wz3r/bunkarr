// Package tiers is Bunkarr's tier engine (docs/design/phase2-3.md §8): an ordered rule set that
// decides, per live catalog file and per destination, whether the file is copied (full), only
// listed in the destination's manifests (manifest), or neither (skip), and why.
//
// It owns the tables tier_rules and item_flags and the setting tiers.revision. Tiers are
// evaluated, never stored (D5): the facts come from the catalog and the metadata caches
// (internal/mediaindex) at evaluation time.
//
// Safety rules that live here:
//   - D1 (the user's decision): with no rules every file is full, the fallback after the last
//     rule is full, and a file flagged irreplaceable is full at every destination. The spec's
//     manifest-by-default rule set is only a preset, loaded into the editor, never applied.
//   - S14 (unknown never lowers protection): every condition is true, false or unknown; unknown is
//     never true and its negation is unknown. A rule that is unknown and more protective than the
//     rule that matches later decides instead (the decision is unknownPromoted). Stale caches,
//     missing integrations, unmapped or mismatched paths and conflicting claims (D12) are unknown.
//   - S15 is the syncer's (a tier change never removes a backup); this package only decides.
//
// The evaluator is pure (Evaluator: facts in, decision out). LoadFacts builds the facts of a
// source's files; Engine combines the rules, the facts and the flags for the syncer
// (Decisions), the manifest builder and the API (Preview, FactsFor, Fields).
package tiers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Queryer runs read queries: *sql.DB (the read pool) and *sql.Tx satisfy it, so the manifest
// builder can evaluate tiers inside its read transaction. It has the method set of
// mediaindex.Queryer.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tier is what a destination does with a file (design §8, D9).
type Tier string

// Tiers, from the most protective.
const (
	// Full: the file is copied.
	Full Tier = "full"
	// Manifest: the file is not copied, but listed in the destination's manifests.
	Manifest Tier = "manifest"
	// Skip: the file is not copied (an *arr file is still listed in its item).
	Skip Tier = "skip"
)

// Valid reports whether t is a known tier.
func (t Tier) Valid() bool { return t == Full || t == Manifest || t == Skip }

// Protection orders tiers: full > manifest > skip (S14).
func (t Tier) Protection() int {
	switch t {
	case Full:
		return 3
	case Manifest:
		return 2
	case Skip:
		return 1
	}
	return 0
}

// Match says how a rule's conditions combine.
type Match string

// Matches.
const (
	// MatchAll: false if any condition is false, true if all are true, otherwise unknown.
	MatchAll Match = "all"
	// MatchAny: true if any condition is true, false if all are false, otherwise unknown.
	MatchAny Match = "any"
)

// Result is a tri-state truth value (S14).
type Result int8

// Results.
const (
	False Result = iota
	True
	Unknown
)

// Not negates r; unknown stays unknown.
func (r Result) Not() Result {
	switch r {
	case True:
		return False
	case False:
		return True
	}
	return Unknown
}

// resultOf converts a known boolean.
func resultOf(b bool) Result {
	if b {
		return True
	}
	return False
}

// String renders r as "true", "false" or "unknown".
func (r Result) String() string {
	switch r {
	case True:
		return "true"
	case False:
		return "false"
	}
	return "unknown"
}

// MarshalJSON renders r as "true", "false" or "unknown".
func (r Result) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

// UnmarshalJSON reads "true", "false" or "unknown".
func (r *Result) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch s {
	case "true":
		*r = True
	case "false":
		*r = False
	case "unknown":
		*r = Unknown
	default:
		return fmt.Errorf("invalid result %q", s)
	}
	return nil
}

// Condition is one test of a rule: {field, op, value} (design §8.2).
type Condition struct {
	Field string          `json:"field"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value,omitempty"`
}

// Rule is a stored tier rule (TierRule of design §13).
type Rule struct {
	ID int64 `json:"id"`
	// Priority is the evaluation order, 1..n.
	Priority   int         `json:"priority"`
	Name       string      `json:"name"`
	Enabled    bool        `json:"enabled"`
	Match      Match       `json:"match"`
	Conditions []Condition `json:"conditions"`
	Action     Tier        `json:"action"`
	// DestinationIDs is "Applies at": nil means every destination; a list means only those (the
	// rule is not evaluated elsewhere); an empty list means none.
	DestinationIDs []int64   `json:"destinationIds"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// AppliesAt reports whether the rule is evaluated at destination destID.
func (r Rule) AppliesAt(destID int64) bool {
	if r.DestinationIDs == nil {
		return true
	}
	for _, id := range r.DestinationIDs {
		if id == destID {
			return true
		}
	}
	return false
}

// RuleInput is a rule as PUT /tiers/rules and the presets carry it. ID 0 is a new rule.
type RuleInput struct {
	ID         int64       `json:"id,omitempty"`
	Name       string      `json:"name"`
	Enabled    *bool       `json:"enabled,omitempty"`
	Match      Match       `json:"match,omitempty"`
	Conditions []Condition `json:"conditions"`
	Action     Tier        `json:"action"`
	// DestinationIDs: null (or absent) is every destination, [] none.
	DestinationIDs []int64 `json:"destinationIds"`
}

// enabled returns the input's enabled flag (default true).
func (in RuleInput) enabled() bool { return in.Enabled == nil || *in.Enabled }

// match returns the input's match (default all).
func (in RuleInput) match() Match {
	if in.Match == "" {
		return MatchAll
	}
	return in.Match
}

// RuleSet is the stored rules in priority order and their revision (GET /tiers/rules).
type RuleSet struct {
	Revision int64  `json:"revision"`
	Rules    []Rule `json:"rules"`
}

// ReasonSource names where a fact came from: kind is catalog, arr, plex, tautulli, seerr,
// maintainerr or flag; IntegrationID the integration, when there is one.
type ReasonSource struct {
	Kind          string `json:"kind"`
	IntegrationID int64  `json:"integrationId,omitempty"`
}

// Reason is one evaluated condition (design §8.1): what was compared (Actual), the result, and,
// for an unknown result, why. A Seerr user appears by id only. RuleID 0 with field
// flag.irreplaceable is the built-in irreplaceable override.
type Reason struct {
	RuleID         int64           `json:"ruleId"`
	ConditionIndex int             `json:"conditionIndex"`
	Field          string          `json:"field"`
	Op             string          `json:"op"`
	Value          json.RawMessage `json:"value,omitempty"`
	Actual         any             `json:"actual,omitempty"`
	Result         Result          `json:"result"`
	Source         ReasonSource    `json:"source"`
	Why            string          `json:"why,omitempty"`
}

// Built-in rule names (rule id 0).
const (
	// FallbackRuleName names the built-in fallback: no rule matched, so the file is full.
	FallbackRuleName = "no rule matched"
	// IrreplaceableRuleName names the built-in override for flagged files.
	IrreplaceableRuleName = "irreplaceable"
)

// Decision is a file's tier at one destination and why (design §8.1).
type Decision struct {
	Tier Tier `json:"tier"`
	// RuleID and RuleName name the deciding rule; 0 is a built-in (the fallback or the
	// irreplaceable override).
	RuleID   int64  `json:"ruleId"`
	RuleName string `json:"ruleName"`
	// Reasons are the deciding rule's conditions that made it decide (for an unknownPromoted
	// decision: its unknown conditions).
	Reasons []Reason `json:"reasons"`
	// Unknown are the unknown conditions of every unknown rule evaluated before the decision.
	Unknown []Reason `json:"unknown"`
	// UnknownPromoted is set when the tier is only this protective because a more protective rule
	// could not be decided (S14): a copy that exists only because of it is a change for the
	// mass-change guard (S10).
	UnknownPromoted bool `json:"unknownPromoted"`
	// Revision is the tiers.revision the rules had.
	Revision int64 `json:"revision"`
	// Follows is the path (inside the source) of the file whose decision this is: the media file
	// of a sidecar, or the most protective name of a hardlink group.
	Follows string `json:"follows,omitempty"`
}

// FallbackDecision is the built-in fallback: full, no rule matched.
func FallbackDecision(revision int64) Decision {
	return Decision{Tier: Full, RuleName: FallbackRuleName, Reasons: []Reason{}, Unknown: []Reason{}, Revision: revision}
}

// UnknownWhy returns the first reason why a fact of the decision is unknown ("Radarr cache is
// 31 h old"), for the hold messages of unknown-promoted copies.
func (d Decision) UnknownWhy() string {
	for _, r := range d.Reasons {
		if r.Result == Unknown && r.Why != "" {
			return r.Why
		}
	}
	for _, r := range d.Unknown {
		if r.Why != "" {
			return r.Why
		}
	}
	return "a fact is unknown"
}

// Summary renders a decision in one line for job logs: `manifest (rule "Keep tagged")`.
func (d Decision) Summary() string {
	var b strings.Builder
	b.WriteString(string(d.Tier))
	switch {
	case d.RuleID == 0 && d.RuleName == IrreplaceableRuleName:
		b.WriteString(" (irreplaceable)")
	case d.RuleID == 0:
		b.WriteString(" (" + FallbackRuleName + ")")
	default:
		fmt.Fprintf(&b, " (rule %q)", d.RuleName)
	}
	if d.UnknownPromoted {
		b.WriteString("; unknown: " + d.UnknownWhy())
	}
	if d.Follows != "" {
		b.WriteString("; follows " + d.Follows)
	}
	return b.String()
}

// Limits of a rule set (design §8.2).
const (
	// MaxRules is the most rules a rule set holds.
	MaxRules = 100
	// MaxConditions is the most conditions a rule holds.
	MaxConditions = 32
	// MaxNameLen is the longest rule name, in characters.
	MaxNameLen = 100
)

// ErrRevisionMismatch means a save was based on another revision of the rules (409).
var ErrRevisionMismatch = errors.New("the tier rules changed since they were loaded; reload them and try again")

// ErrNotFound means a flag (or a preview) does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict means a flag already exists.
var ErrConflict = errors.New("conflict")

// ValidationError is an invalid rule, condition or flag (400). RuleIndex and ConditionIndex are
// 0-based positions in the request (-1: not about one).
type ValidationError struct {
	RuleIndex      int
	ConditionIndex int
	Msg            string
}

// Error implements error.
func (e *ValidationError) Error() string {
	switch {
	case e.RuleIndex >= 0 && e.ConditionIndex >= 0:
		return fmt.Sprintf("rule %d, condition %d: %s", e.RuleIndex+1, e.ConditionIndex+1, e.Msg)
	case e.RuleIndex >= 0:
		return fmt.Sprintf("rule %d: %s", e.RuleIndex+1, e.Msg)
	}
	return e.Msg
}

func invalid(rule, cond int, format string, args ...any) error {
	return &ValidationError{RuleIndex: rule, ConditionIndex: cond, Msg: fmt.Sprintf(format, args...)}
}

// Warning is a rule value that no fresh index knows (a renamed tag or profile, design §8.7): the
// condition then evaluates false (or unknown) everywhere, silently without the warning.
type Warning struct {
	RuleIndex      int    `json:"ruleIndex"`
	ConditionIndex int    `json:"conditionIndex"`
	Message        string `json:"message"`
}

// UnknownSource is a cache whose facts are unknown now, and why (the preview's banner).
type UnknownSource struct {
	IntegrationID int64  `json:"integrationId"`
	Name          string `json:"name"`
	Reason        string `json:"reason"`
}
