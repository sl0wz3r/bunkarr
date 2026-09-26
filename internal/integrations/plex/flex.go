package plex

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// Lenient JSON decoding helpers. Plex Media Server and plex.tv mix numbers and numeric strings,
// and "1"/"0"/true booleans, for the same attribute across endpoints and versions; plex.tv answers
// in JSON or XML. flexString and flexInt accept a string or a number and refuse anything else (a
// PMS response with such a value is not trusted); flexBool and list[T] never fail (ported from
// Dupearr).

// flexString decodes a JSON string or number as text (Plex sends ids as either). null leaves "".
type flexString string

// UnmarshalJSON implements json.Unmarshaler.
func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

// String returns the text with surrounding space removed.
func (f flexString) String() string { return strings.TrimSpace(string(f)) }

// flexInt decodes a JSON number or numeric string. null or "" leaves 0.
type flexInt int64

// UnmarshalJSON implements json.Unmarshaler.
func (f *flexInt) UnmarshalJSON(b []byte) error {
	var s flexString
	if err := s.UnmarshalJSON(b); err != nil {
		return err
	}
	if s.String() == "" {
		return nil
	}
	n, err := strconv.ParseInt(s.String(), 10, 64)
	if err != nil {
		return errors.New("not an integer")
	}
	*f = flexInt(n)
	return nil
}

// Int returns the value as int (clamped to the int range).
func (f flexInt) Int() int {
	v := int64(f)
	if v > math.MaxInt {
		return math.MaxInt
	}
	if v < math.MinInt {
		return math.MinInt
	}
	return int(v)
}

// flexBool accepts true/false, 1/0, "1"/"0", "true"/"false", "yes"/"no" (any case); anything else,
// null included, is false. It never fails.
type flexBool bool

// UnmarshalJSON implements json.Unmarshaler.
func (f *flexBool) UnmarshalJSON(b []byte) error {
	*f = flexBool(parseBoolText(scalarText(b)))
	return nil
}

// list is a lenient JSON element array: a JSON array decodes normally, a single object becomes a
// one-element list, and any other value (null, bool, string, number) is ignored. encoding/json
// matches keys case-insensitively, so an attribute such as "device": "PC" could otherwise be
// folded onto a Device element array and fail the whole decode.
type list[T any] []T

// UnmarshalJSON implements json.Unmarshaler.
func (l *list[T]) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil
	}
	switch b[0] {
	case '[':
		var items []T
		if err := json.Unmarshal(b, &items); err != nil {
			return err
		}
		*l = items
	case '{':
		var one T
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*l = list[T]{one}
	}
	return nil
}

// scalarText returns the textual content of a JSON scalar (strings unquoted), "" for null,
// objects and arrays.
func scalarText(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" || b[0] == '{' || b[0] == '[' {
		return ""
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return ""
		}
		return strings.TrimSpace(s)
	}
	return string(b)
}

// parseIntText parses an integer leniently ("12", "12.0", "true"); 0 when unparseable or out of
// range.
func parseIntText(s string) int64 {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "":
		return 0
	case "true":
		return 1
	case "false":
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f >= math.MaxInt64 || f <= math.MinInt64 {
		return 0
	}
	return int64(f)
}

// parseBoolText parses a boolean leniently; anything unrecognized is false.
func parseBoolText(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "y", "t":
		return true
	case "", "0", "false", "no", "off", "n", "f":
		return false
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return f != 0
	}
	return false
}

// firstNonEmpty returns the first value that is not blank, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// trimBody removes surrounding white space and a UTF-8 byte order mark.
func trimBody(b []byte) []byte {
	b = bytes.TrimPrefix(bytes.TrimSpace(b), []byte("\xef\xbb\xbf"))
	return bytes.TrimSpace(b)
}

// unixTime converts an epoch (seconds; milliseconds tolerated) to UTC; zero when unset.
func unixTime(v flexInt) time.Time {
	switch n := int64(v); {
	case n <= 0:
		return time.Time{}
	case n > 100_000_000_000: // milliseconds
		return time.UnixMilli(n).UTC()
	default:
		return time.Unix(n, 0).UTC()
	}
}
