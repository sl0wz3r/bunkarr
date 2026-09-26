package tiers

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// opText renders an operator in words.
var opText = map[string]string{
	OpIs: "is", OpIsNot: "is not", OpHas: "has", OpHasNot: "has not", OpGt: ">", OpGte: "≥", OpLt: "<", OpLte: "≤", OpEq: "=",
	OpOlderThan: "older than", OpNewerThan: "newer than", OpNever: "never", OpIn: "in", OpNotIn: "not in",
}

// Text renders a reason in one line for job logs; the UI renders the structured reason itself.
// Examples: `*arr tag has "bunkarr-full": true (bunkarr-full)`, `irreplaceable (flag #3)`,
// `Pending deletion in Maintainerr is true: unknown (Maintainerr cache is 30 h old)`.
func (r Reason) Text() string {
	if r.RuleID == 0 && r.Field == FieldFlagIrreplaceable && r.ConditionIndex < 0 {
		return "irreplaceable (" + flagList(r.Actual) + ")"
	}
	label := r.Field
	if spec, ok := specOf(r.Field); ok {
		label = spec.Label
	}
	op := opText[r.Op]
	if op == "" {
		op = r.Op
	}
	var b strings.Builder
	b.WriteString(label + " " + op)
	if len(r.Value) > 0 {
		b.WriteString(" " + valueText(r.Field, r.Value))
	}
	b.WriteString(": " + r.Result.String())
	switch {
	case r.Result == Unknown && r.Why != "":
		b.WriteString(" (" + r.Why + ")")
	case r.Actual != nil:
		b.WriteString(" (" + actualText(r.Actual) + ")")
	}
	return b.String()
}

func valueText(field string, v json.RawMessage) string {
	var n float64
	if field == FieldFileSize && json.Unmarshal(v, &n) == nil {
		return fmt.Sprintf("%d bytes", int64(n))
	}
	if (field == FieldFileAge || field == FieldTautulliLastWatch) && json.Unmarshal(v, &n) == nil {
		return fmt.Sprintf("%d days", int64(n))
	}
	return string(v)
}

func actualText(a any) string {
	switch v := a.(type) {
	case []string:
		if len(v) == 0 {
			return "none"
		}
		return strings.Join(v, ", ")
	case *time.Time:
		if v == nil {
			return "never"
		}
		return v.UTC().Format(time.DateOnly)
	case time.Time:
		return v.UTC().Format(time.DateOnly)
	case string:
		return v
	}
	b, err := json.Marshal(a)
	if err != nil {
		return fmt.Sprint(a)
	}
	return string(b)
}

func flagList(a any) string {
	ids, _ := a.([]int64)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("flag #%d", id))
	}
	if len(parts) == 0 {
		return "flag"
	}
	return strings.Join(parts, ", ")
}

// Text renders a decision with its reasons in one line (job logs).
func (d Decision) Text() string {
	var b strings.Builder
	b.WriteString(d.Summary())
	for i, r := range d.Reasons {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(r.Text())
	}
	return b.String()
}
