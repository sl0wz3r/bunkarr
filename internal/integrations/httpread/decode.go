package httpread

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// ErrNotJSON means a body is not the JSON the client expected.
var ErrNotJSON = errors.New("the response is not the expected JSON")

// DecodeJSON decodes body into out. Its error describes the problem without quoting the body.
func DecodeJSON(body []byte, out any) error {
	if err := json.Unmarshal(body, out); err != nil {
		var syn *json.SyntaxError
		var typ *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syn):
			return fmt.Errorf("%w: the body is not JSON", ErrNotJSON)
		case errors.As(err, &typ):
			return fmt.Errorf("%w: unexpected JSON type for %q", ErrNotJSON, typ.Field)
		}
		return fmt.Errorf("%w: the body could not be decoded", ErrNotJSON)
	}
	return nil
}

// Int is a lenient integer: a JSON number, a numeric string, "", or null. Valid is false for "",
// null and a missing field. Tautulli sends many numbers as strings, and empty strings for keys
// that do not apply ("parent_rating_key": "").
type Int struct {
	V     int64
	Valid bool
}

// UnmarshalJSON implements json.Unmarshaler. A value that is neither a number nor a numeric
// string (nor "" or null) is an error.
func (n *Int) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	*n = Int{}
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}
	s := string(b)
	if b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*n = Int{V: v, Valid: true}
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f != float64(int64(f)) {
		return &json.UnmarshalTypeError{Value: "value", Type: reflect.TypeFor[int64]()}
	}
	*n = Int{V: int64(f), Valid: true}
	return nil
}

// Text is a lenient string: a JSON string (HTML entities decoded, surrounding space trimmed), a
// number (its digits), or null (""). Tautulli HTML-escapes some strings it returns.
type Text string

// UnmarshalJSON implements json.Unmarshaler.
func (t *Text) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || bytes.Equal(b, []byte("null")):
		*t = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*t = Text(strings.TrimSpace(html.UnescapeString(s)))
	case b[0] == '-' || (b[0] >= '0' && b[0] <= '9'):
		*t = Text(string(b))
	default:
		return &json.UnmarshalTypeError{Value: "value", Type: reflect.TypeFor[string]()}
	}
	return nil
}

// String returns the text.
func (t Text) String() string { return string(t) }

// ParseTime reads the timestamps the applications send: RFC 3339 ("2026-09-26T08:28:27.000Z"),
// and "2026-09-26 08:28:58" or "2026-09-26 00:00:00.000" (Maintainerr's SQLite form, UTC). ok is
// false for anything else.
func ParseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// Version is a dotted release number ("v2.18.1", "3.4.1"); pre-release and build suffixes are
// ignored.
type Version [3]int

// ParseVersion reads "v2.18.1", "2.18.1-beta" or "3.4"; ok is false when there is no leading
// number.
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	var v Version
	parts := strings.Split(s, ".")
	if len(parts) == 0 || parts[0] == "" {
		return v, false
	}
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return v, i > 0
		}
		v[i] = n
	}
	return v, true
}

// Less reports whether v is older than w.
func (v Version) Less(w Version) bool {
	for i := range v {
		if v[i] != w[i] {
			return v[i] < w[i]
		}
	}
	return false
}

// String renders v as "2.18.1".
func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }
