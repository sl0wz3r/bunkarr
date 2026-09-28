package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// EscapedForms returns the forms other than value itself in which value can appear in text that
// Bunkarr logs or stores: JSON-escaped (with and without HTML escaping, as encoding/json and a
// JSON log line write a string) and Go-quoted (as rclone passes a key_pem value and prints a
// config value at debug level), each without its surrounding quotes. A form equal to value is
// left out, so a plain token has none. RegisterSecret and SetSecrets register these forms with
// the value (docs/design/phase4.md S22).
func EscapedForms(value string) []string {
	var out []string
	add := func(f string) {
		if f != value && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	if b, err := json.Marshal(value); err == nil {
		add(string(b[1 : len(b)-1]))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err == nil {
		b := bytes.TrimRight(buf.Bytes(), "\n")
		add(string(b[1 : len(b)-1]))
	}
	q := strconv.Quote(value)
	add(q[1 : len(q)-1])
	return out
}

// secretForms is value followed by its EscapedForms.
func secretForms(value string) []string {
	return append([]string{value}, EscapedForms(value)...)
}

// redactAny renders v (a value slog would print with fmt or encoding/json) with the fields whose
// names look like secrets (sensitiveKey) and every registered secret value replaced, at any
// depth, as internal/jobqueue does for job logs. It returns the redacted value and whether
// anything was replaced; an unchanged value is logged as it was, so its format stays the same.
// A byte slice is treated as text. A value that does not marshal to JSON is rendered with fmt.
func redactAny(v any) (any, bool) {
	switch x := v.(type) {
	case nil:
		return nil, false
	case []byte:
		if s := string(x); ContainsSecret(s) {
			return RedactSecrets(s), true
		}
		return v, false
	}
	raw, err := marshalNoEscape(v)
	if err != nil {
		s := fmt.Sprint(v)
		if ContainsSecret(s) {
			return RedactSecrets(s), true
		}
		return v, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return v, false
	}
	tree, changed := redactTree(tree)
	if !changed {
		return v, false
	}
	out, err := marshalNoEscape(tree)
	if err != nil {
		return Redacted, true
	}
	return json.RawMessage(out), true
}

// redactTree redacts a decoded JSON value in place and reports whether it changed.
func redactTree(v any) (any, bool) {
	switch x := v.(type) {
	case map[string]any:
		changed := false
		for k, e := range x {
			if sensitiveKey(k) {
				if s, ok := e.(string); !ok || s != Redacted {
					x[k] = Redacted
					changed = true
				}
				continue
			}
			r, c := redactTree(e)
			x[k] = r
			changed = changed || c
		}
		return x, changed
	case []any:
		changed := false
		for i, e := range x {
			r, c := redactTree(e)
			x[i] = r
			changed = changed || c
		}
		return x, changed
	case string:
		if ContainsSecret(x) {
			return RedactSecrets(x), true
		}
		return x, false
	default:
		return v, false
	}
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// RedactTruncated returns s with every registered secret and every one of values (as
// RedactValues does) replaced by [REDACTED], cut to at most limit bytes (backed off to a UTF-8
// boundary). Redaction happens before the cut, so a secret across the cut is never half kept.
// partial says s is only the start of a longer text (a line that was cut while it was read): a
// secret at its very end may continue past it, so nothing of s's last L bytes is kept, where L is
// the length of the longest secret, unless it belongs to a secret that starts earlier and is
// replaced whole. The engines' line readers use it (docs/design/phase4.md §10.2).
func RedactTruncated(s string, limit int, partial bool, values ...string) string {
	return redactSpans(s, max(limit, 0), partial, values)
}

// redactSpans replaces every occurrence of a registered secret or of values (those of at least 8
// bytes) in s by Redacted. Occurrences that overlap or nest are replaced as one, so no part of
// either survives. limit < 0 means no limit; otherwise the result is cut to limit bytes after the
// replacement (see RedactTruncated, also for partial).
func redactSpans(s string, limit int, partial bool, values []string) string {
	if limit >= 0 && !partial && len(s) <= limit {
		limit = -1
	}
	type span struct{ start, end int }
	var spans []span
	longest := 0
	scan := func(v string) {
		longest = max(longest, len(v))
		for off := 0; off < len(s); {
			i := strings.Index(s[off:], v)
			if i < 0 {
				return
			}
			spans = append(spans, span{off + i, off + i + len(v)})
			off += i + 1
		}
	}
	for _, v := range values {
		if len(v) >= minSecretLen {
			scan(v)
		}
	}
	secretsMu.RLock()
	for _, v := range secrets.sorted {
		scan(v)
	}
	secretsMu.RUnlock()
	keep := len(s)
	if partial {
		keep = max(0, len(s)-longest)
	}
	if len(spans) == 0 && limit < 0 {
		return s
	}
	// Leftmost first, longest first at one position; a span that overlaps the current one is
	// merged into it.
	slices.SortFunc(spans, func(a, b span) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return b.end - a.end
	})
	full := func(n int) bool { return limit >= 0 && n >= limit }
	var out strings.Builder
	pos := 0
	for i := 0; i < len(spans) && !full(out.Len()); {
		sp := spans[i]
		for i++; i < len(spans) && spans[i].start < sp.end; i++ {
			sp.end = max(sp.end, spans[i].end)
		}
		if sp.start >= keep {
			break
		}
		out.WriteString(s[pos:sp.start])
		out.WriteString(Redacted)
		pos = sp.end
	}
	if pos < keep && !full(out.Len()) {
		out.WriteString(s[pos:keep])
	}
	r := out.String()
	if limit >= 0 && len(r) > limit {
		r = r[:limit]
		// Drop a rune the cut split (at most UTFMax-1 bytes).
		for range utf8.UTFMax - 1 {
			if ru, size := utf8.DecodeLastRuneInString(r); ru != utf8.RuneError || size != 1 {
				break
			}
			r = r[:len(r)-1]
		}
	}
	return r
}
