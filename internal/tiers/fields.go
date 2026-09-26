package tiers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

// Condition fields (design §8.2).
const (
	FieldArrManaged         = "arr.managed"
	FieldArrTag             = "arr.tag"
	FieldArrQualityProfile  = "arr.qualityProfile"
	FieldArrRootFolder      = "arr.rootFolder"
	FieldArrMonitored       = "arr.monitored"
	FieldPlexSection        = "plex.section"
	FieldSource             = "source"
	FieldFileSize           = "file.size"
	FieldFileAge            = "file.age"
	FieldMediaGenre         = "media.genre"
	FieldSeerrRequested     = "seerr.requested"
	FieldSeerrRequestedBy   = "seerr.requestedBy"
	FieldTautulliPlayCount  = "tautulli.playCount"
	FieldTautulliLastWatch  = "tautulli.lastWatched"
	FieldMaintainerrPending = "maintainerr.pendingDelete"
	FieldFlagIrreplaceable  = "flag.irreplaceable"
)

// Operators.
const (
	OpIs        = "is"
	OpIsNot     = "isNot"
	OpHas       = "has"
	OpHasNot    = "hasNot"
	OpGt        = "gt"
	OpGte       = "gte"
	OpLt        = "lt"
	OpLte       = "lte"
	OpEq        = "eq"
	OpOlderThan = "olderThan"
	OpNewerThan = "newerThan"
	OpNever     = "never"
	OpIn        = "in"
	OpNotIn     = "notIn"
)

// ValueType is the JSON type of a condition's value.
type ValueType string

// Value types.
const (
	ValueBool   ValueType = "bool"
	ValueString ValueType = "string"
	// ValueInt is a non-negative integer (bytes, days, plays, a source id).
	ValueInt ValueType = "int"
	// ValueInts is a non-empty list of positive integers (Seerr user ids).
	ValueInts ValueType = "ints"
)

// Fact source kinds (Reason.Source.Kind, FieldSpec.Source).
const (
	SourceCatalog     = "catalog"
	SourceArr         = "arr"
	SourcePlex        = "plex"
	SourceTautulli    = "tautulli"
	SourceSeerr       = "seerr"
	SourceMaintainerr = "maintainerr"
	SourceFlag        = "flag"
)

// FieldSpec describes a condition field for validation and the rule editor (GET /tiers/fields).
type FieldSpec struct {
	Field string   `json:"field"`
	Label string   `json:"label"`
	Ops   []string `json:"ops"`
	// ValueType is the value's type; NoValueOps take no value (tautulli.lastWatched never).
	ValueType  ValueType `json:"valueType"`
	NoValueOps []string  `json:"noValueOps,omitempty"`
	// Unit qualifies an int value: bytes, days or plays.
	Unit string `json:"unit,omitempty"`
	// Source is the kind of application the facts come from.
	Source string `json:"source"`
	// provided marks a field whose facts come from a Provider (slice 9): it is available only
	// when one is registered.
	provided bool
}

// fieldSpecs are the fields in the order the editor lists them.
var fieldSpecs = []FieldSpec{
	{Field: FieldArrTag, Label: "*arr tag", Ops: []string{OpHas, OpHasNot}, ValueType: ValueString, Source: SourceArr},
	{Field: FieldArrQualityProfile, Label: "*arr quality profile", Ops: []string{OpIs, OpIsNot}, ValueType: ValueString, Source: SourceArr},
	{Field: FieldArrRootFolder, Label: "*arr root folder", Ops: []string{OpIs, OpIsNot}, ValueType: ValueString, Source: SourceArr},
	{Field: FieldArrMonitored, Label: "Monitored in the *arr", Ops: []string{OpIs}, ValueType: ValueBool, Source: SourceArr},
	{Field: FieldArrManaged, Label: "Managed by an *arr", Ops: []string{OpIs}, ValueType: ValueBool, Source: SourceArr},
	{Field: FieldMediaGenre, Label: "Genre", Ops: []string{OpHas, OpHasNot}, ValueType: ValueString, Source: SourceArr},
	{Field: FieldSource, Label: "Source", Ops: []string{OpIs, OpIsNot}, ValueType: ValueInt, Source: SourceCatalog},
	{Field: FieldFileSize, Label: "File size", Ops: []string{OpGt, OpGte, OpLt, OpLte}, ValueType: ValueInt, Unit: "bytes", Source: SourceCatalog},
	{Field: FieldFileAge, Label: "Added", Ops: []string{OpOlderThan, OpNewerThan}, ValueType: ValueInt, Unit: "days", Source: SourceCatalog},
	{Field: FieldPlexSection, Label: "Plex library", Ops: []string{OpIs, OpIsNot}, ValueType: ValueString, Source: SourcePlex},
	{Field: FieldSeerrRequested, Label: "Requested in Seerr", Ops: []string{OpIs}, ValueType: ValueBool, Source: SourceSeerr, provided: true},
	{Field: FieldSeerrRequestedBy, Label: "Requested by (Seerr)", Ops: []string{OpIn, OpNotIn}, ValueType: ValueInts, Source: SourceSeerr, provided: true},
	{Field: FieldTautulliPlayCount, Label: "Play count (Tautulli)", Ops: []string{OpGt, OpGte, OpLt, OpLte, OpEq}, ValueType: ValueInt, Unit: "plays", Source: SourceTautulli, provided: true},
	{Field: FieldTautulliLastWatch, Label: "Last watched (Tautulli)", Ops: []string{OpOlderThan, OpNewerThan, OpNever}, ValueType: ValueInt, NoValueOps: []string{OpNever}, Unit: "days", Source: SourceTautulli, provided: true},
	{Field: FieldMaintainerrPending, Label: "Pending deletion in Maintainerr", Ops: []string{OpIs}, ValueType: ValueBool, Source: SourceMaintainerr, provided: true},
	{Field: FieldFlagIrreplaceable, Label: "Flagged irreplaceable", Ops: []string{OpIs}, ValueType: ValueBool, Source: SourceFlag},
}

// specOf returns a field's spec.
func specOf(field string) (FieldSpec, bool) {
	for _, s := range fieldSpecs {
		if s.Field == field {
			return s, true
		}
	}
	return FieldSpec{}, false
}

// cond is a validated condition with its value decoded.
type cond struct {
	Condition
	b    bool
	s    string
	n    int64
	ints []int64
}

// maxInt bounds integer values (a size in bytes fits; so do 10^12 days).
const maxInt = math.MaxInt64 / 2

// compileCondition validates c and decodes its value. rule and index place the error.
func compileCondition(c Condition, rule, index int) (cond, error) {
	out := cond{Condition: c}
	spec, ok := specOf(c.Field)
	if !ok {
		return out, invalid(rule, index, "unknown field %q", c.Field)
	}
	if !slices.Contains(spec.Ops, c.Op) {
		return out, invalid(rule, index, "%s does not support the operator %q (use %s)", c.Field, c.Op, strings.Join(spec.Ops, ", "))
	}
	raw := bytes.TrimSpace(c.Value)
	if slices.Contains(spec.NoValueOps, c.Op) {
		if len(raw) != 0 && !bytes.Equal(raw, []byte("null")) {
			return out, invalid(rule, index, "%s %s takes no value", c.Field, c.Op)
		}
		out.Value = nil
		return out, nil
	}
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return out, invalid(rule, index, "%s %s needs a value", c.Field, c.Op)
	}
	wrong := func(want string) error {
		return invalid(rule, index, "the value of %s must be %s", c.Field, want)
	}
	switch spec.ValueType {
	case ValueBool:
		if err := json.Unmarshal(raw, &out.b); err != nil {
			return out, wrong("true or false")
		}
	case ValueString:
		if err := json.Unmarshal(raw, &out.s); err != nil {
			return out, wrong("a string")
		}
		out.s = strings.TrimSpace(out.s)
		if out.s == "" || utf8.RuneCountInString(out.s) > 1024 {
			return out, wrong("a non-empty string of at most 1024 characters")
		}
		if c.Field == FieldPlexSection {
			pid, key, ok := parseSection(out.s)
			if !ok {
				return out, wrong(`"<plexIntegrationId>:<sectionKey>", e.g. "1:2"`)
			}
			out.s = fmt.Sprintf("%d:%s", pid, key)
		}
		if c.Field == FieldArrRootFolder {
			out.s = trimSlash(out.s)
		}
	case ValueInt:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil || f != math.Trunc(f) || f < 0 || f > maxInt {
			return out, wrong("a whole number of at least 0")
		}
		out.n = int64(f)
		if c.Field == FieldSource && out.n < 1 {
			return out, wrong("a source id")
		}
	case ValueInts:
		var fs []float64
		if err := json.Unmarshal(raw, &fs); err != nil || len(fs) == 0 || len(fs) > 1000 {
			return out, wrong("a list of 1 to 1000 ids")
		}
		for _, f := range fs {
			if f != math.Trunc(f) || f < 1 || f > maxInt {
				return out, wrong("a list of positive ids")
			}
			if !slices.Contains(out.ints, int64(f)) {
				out.ints = append(out.ints, int64(f))
			}
		}
		slices.Sort(out.ints)
	}
	norm, err := canonicalValue(spec.ValueType, out)
	if err != nil {
		return out, err
	}
	out.Value = norm
	return out, nil
}

// canonicalValue re-encodes a decoded value (the stored form).
func canonicalValue(t ValueType, c cond) (json.RawMessage, error) {
	var v any
	switch t {
	case ValueBool:
		v = c.b
	case ValueString:
		v = c.s
	case ValueInt:
		v = c.n
	case ValueInts:
		v = c.ints
	}
	return json.Marshal(v)
}

// parseSection splits "<plexIntegrationId>:<sectionKey>".
func parseSection(s string) (int64, string, bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return 0, "", false
	}
	var pid int64
	if _, err := fmt.Sscanf(s[:i], "%d", &pid); err != nil || pid < 1 || fmt.Sprint(pid) != s[:i] {
		return 0, "", false
	}
	key := strings.TrimSpace(s[i+1:])
	if key == "" || strings.ContainsAny(key, "/\x00") || len(key) > 64 {
		return 0, "", false
	}
	return pid, key, true
}

// trimSlash drops trailing slashes of a root folder path ("/movies/" is "/movies").
func trimSlash(p string) string {
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	return p
}

// compiledRule is a validated rule ready for evaluation.
type compiledRule struct {
	Rule
	conds []cond
}

// normalizeInput validates one rule of a save (index i) and returns its canonical form.
func normalizeInput(in RuleInput, i int) (RuleInput, []cond, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return in, nil, invalid(i, -1, "a rule needs a name")
	}
	if utf8.RuneCountInString(in.Name) > MaxNameLen {
		return in, nil, invalid(i, -1, "the name is longer than %d characters", MaxNameLen)
	}
	in.Match = in.match()
	if in.Match != MatchAll && in.Match != MatchAny {
		return in, nil, invalid(i, -1, "match must be all or any, not %q", in.Match)
	}
	if !in.Action.Valid() {
		return in, nil, invalid(i, -1, "action must be full, manifest or skip, not %q", in.Action)
	}
	if len(in.Conditions) > MaxConditions {
		return in, nil, invalid(i, -1, "a rule has at most %d conditions", MaxConditions)
	}
	conds := make([]cond, 0, len(in.Conditions))
	out := make([]Condition, 0, len(in.Conditions))
	for j, c := range in.Conditions {
		cc, err := compileCondition(c, i, j)
		if err != nil {
			return in, nil, err
		}
		conds = append(conds, cc)
		out = append(out, cc.Condition)
	}
	in.Conditions = out
	if in.DestinationIDs != nil {
		ids := make([]int64, 0, len(in.DestinationIDs))
		for _, id := range in.DestinationIDs {
			if id < 1 {
				return in, nil, invalid(i, -1, "destination id %d is not valid", id)
			}
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		in.DestinationIDs = ids
	}
	if in.ID < 0 {
		return in, nil, invalid(i, -1, "rule id %d is not valid", in.ID)
	}
	return in, conds, nil
}

// Validate checks a rule set as PUT /tiers/rules (or a preview draft) carries it: at most
// MaxRules rules of at most MaxConditions conditions, known fields and operators, values of the
// right type, and rule ids used once. The error is a *ValidationError naming the rule and the
// condition. It returns the canonical inputs (trimmed names, defaults, normalized values).
func Validate(rules []RuleInput) ([]RuleInput, error) {
	if len(rules) > MaxRules {
		return nil, invalid(-1, -1, "a rule set has at most %d rules", MaxRules)
	}
	out := make([]RuleInput, 0, len(rules))
	seen := map[int64]bool{}
	for i, in := range rules {
		n, _, err := normalizeInput(in, i)
		if err != nil {
			return nil, err
		}
		if n.ID != 0 {
			if seen[n.ID] {
				return nil, invalid(i, -1, "rule id %d appears twice", n.ID)
			}
			seen[n.ID] = true
		}
		out = append(out, n)
	}
	return out, nil
}

// compile turns stored (or draft) rules into evaluation form. Stored rules were validated when
// saved; a condition that no longer validates (a field removed in a later version) evaluates
// unknown, so it can only protect (S14).
func compile(rules []Rule) []compiledRule {
	out := make([]compiledRule, 0, len(rules))
	for i, r := range rules {
		cr := compiledRule{Rule: r}
		for j, c := range r.Conditions {
			cc, err := compileCondition(c, i, j)
			if err != nil {
				cc = cond{Condition: Condition{Field: c.Field, Op: c.Op, Value: c.Value}}
				cc.Op = "invalid"
			}
			cr.conds = append(cr.conds, cc)
		}
		out = append(out, cr)
	}
	return out
}

// draftRules turns validated inputs into rules (a preview draft). A rule sent with an id keeps
// it; a new rule gets the negative of its position (-1 for the first), so a draft's rules are
// told apart in the per-rule counts before they have ids.
func draftRules(in []RuleInput) []Rule {
	out := make([]Rule, 0, len(in))
	for i, r := range in {
		id := r.ID
		if id == 0 {
			id = -int64(i + 1)
		}
		out = append(out, Rule{ID: id, Priority: i + 1, Name: r.Name, Enabled: r.enabled(), Match: r.match(),
			Conditions: r.Conditions, Action: r.Action, DestinationIDs: r.DestinationIDs})
	}
	return out
}
