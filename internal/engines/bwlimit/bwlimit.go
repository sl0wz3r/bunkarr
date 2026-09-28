// Package bwlimit is the bandwidth and transfer-window math of Phase 4 (docs/design/phase4.md §9.1,
// §9.2, safety rule S27): a destination's base upload and download limits, its weekly timetable of
// other limits, its transfer window, and a token-bucket writer that applies a limit to filecopy.
//
// Times are wall-clock "HH:MM" in the container's time zone. Every instant is built with
// time.Date in that zone, so the math stays right across midnight and daylight-saving changes: a
// 01:00-07:00 window is 01:00 to 07:00 on the clock, 5 or 7 hours on the night the clock moves.
// Apart from the writer the package is pure: it reads no clock, no file and no setting.
package bwlimit

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Limits of a Config (§9.1).
const (
	// MaxEntries is the most timetable entries a Config has.
	MaxEntries = 16
	// MaxKiBps is the largest limit in KiB/s (0 means unlimited).
	MaxKiBps = 10_000_000
	// DefaultGraceMinutes is a window's grace when the JSON leaves it out.
	DefaultGraceMinutes = 15
	// MaxGraceMinutes is the largest grace.
	MaxGraceMinutes = 120
)

// Days are the day names of a timetable entry or a window, in week order (Monday first).
var Days = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

const (
	minutesPerDay  = 24 * 60
	minutesPerWeek = 7 * minutesPerDay
)

// Config is a destination's bandwidth column (destinations.bandwidth):
//
//	{"uploadKiBps": 0, "downloadKiBps": 0,
//	 "timetable": [{"days": ["mon"], "from": "08:00", "to": "23:00", "uploadKiBps": 1024, "downloadKiBps": 0}],
//	 "window": {"days": ["mon"], "from": "01:00", "to": "07:00", "graceMinutes": 15, "allowOverrun": false}}
//
// 0 means unlimited. At a time no timetable entry covers, the base values apply. A nil Window is
// always open.
type Config struct {
	UploadKiBps   int64   `json:"uploadKiBps"`
	DownloadKiBps int64   `json:"downloadKiBps"`
	Timetable     []Entry `json:"timetable"`
	Window        *Window `json:"window"`
}

// Entry is one timetable line: on each of Days from From to To, these limits. To before From runs
// into the next day (a Monday 23:00-06:00 entry ends on Tuesday).
type Entry struct {
	Days          []string `json:"days"`
	From          string   `json:"from"`
	To            string   `json:"to"`
	UploadKiBps   int64    `json:"uploadKiBps"`
	DownloadKiBps int64    `json:"downloadKiBps"`
}

// Window is a destination's transfer window (§9.2): sync, verify and retention jobs run only
// inside it. A job still running at the end plus GraceMinutes is stopped and deferred.
// AllowOverrun lets a file that cannot fit a whole window start alone at the window's opening and
// run past its end.
type Window struct {
	Days         []string `json:"days"`
	From         string   `json:"from"`
	To           string   `json:"to"`
	GraceMinutes int      `json:"graceMinutes"`
	AllowOverrun bool     `json:"allowOverrun"`
}

// UnmarshalJSON decodes a window, with GraceMinutes DefaultGraceMinutes when the field is absent.
func (w *Window) UnmarshalJSON(b []byte) error {
	type plain Window
	v := plain{GraceMinutes: DefaultGraceMinutes}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*w = Window(v)
	return nil
}

// ValidationError is a Config that Normalize refused. Field names the part ("timetable[1].from",
// "window.graceMinutes"); the API prefixes it with "bandwidth.".
type ValidationError struct {
	Field   string
	Message string
}

// Error implements error.
func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Normalize checks c and returns it in canonical form (days in week order, an empty timetable as
// [], the window's grace kept). It refuses (*ValidationError) more than MaxEntries entries,
// unknown or repeated days, times that are not "HH:MM" (00:00-23:59), an entry or window whose
// From equals its To, limits outside 0-MaxKiBps, a grace outside 0-MaxGraceMinutes, and entries
// that overlap at any moment of the week (a Monday 23:00-06:00 entry overlaps a Tuesday
// 05:00-08:00 one).
func (c Config) Normalize() (Config, error) {
	out := Config{UploadKiBps: c.UploadKiBps, DownloadKiBps: c.DownloadKiBps, Timetable: []Entry{}}
	if err := checkRate("uploadKiBps", c.UploadKiBps); err != nil {
		return Config{}, err
	}
	if err := checkRate("downloadKiBps", c.DownloadKiBps); err != nil {
		return Config{}, err
	}
	if len(c.Timetable) > MaxEntries {
		return Config{}, invalid("timetable", "at most %d entries (got %d)", MaxEntries, len(c.Timetable))
	}
	type interval struct {
		entry      int
		day        string
		start, end int // minutes of the week; end may pass minutesPerWeek
	}
	var intervals []interval
	for i, e := range c.Timetable {
		field := fmt.Sprintf("timetable[%d]", i)
		days, err := normDays(field+".days", e.Days)
		if err != nil {
			return Config{}, err
		}
		from, to, err := parseSpan(field, e.From, e.To)
		if err != nil {
			return Config{}, err
		}
		if err := checkRate(field+".uploadKiBps", e.UploadKiBps); err != nil {
			return Config{}, err
		}
		if err := checkRate(field+".downloadKiBps", e.DownloadKiBps); err != nil {
			return Config{}, err
		}
		for _, d := range days {
			start := dayIndex(d)*minutesPerDay + from
			intervals = append(intervals, interval{i, d, start, start + spanMinutes(from, to)})
		}
		out.Timetable = append(out.Timetable, Entry{Days: days, From: e.From, To: e.To, UploadKiBps: e.UploadKiBps, DownloadKiBps: e.DownloadKiBps})
	}
	for a := range intervals {
		for b := a + 1; b < len(intervals); b++ {
			x, y := intervals[a], intervals[b]
			if x.entry == y.entry {
				continue
			}
			for _, shift := range []int{-minutesPerWeek, 0, minutesPerWeek} {
				if x.start < y.end+shift && y.start+shift < x.end {
					return Config{}, invalid(fmt.Sprintf("timetable[%d]", y.entry), "overlaps timetable[%d] (%s %s-%s) on %s",
						x.entry, x.day, c.Timetable[x.entry].From, c.Timetable[x.entry].To, y.day)
				}
			}
		}
	}
	if c.Window != nil {
		w := *c.Window
		days, err := normDays("window.days", w.Days)
		if err != nil {
			return Config{}, err
		}
		if _, _, err := parseSpan("window", w.From, w.To); err != nil {
			return Config{}, err
		}
		if w.GraceMinutes < 0 || w.GraceMinutes > MaxGraceMinutes {
			return Config{}, invalid("window.graceMinutes", "must be 0-%d (got %d)", MaxGraceMinutes, w.GraceMinutes)
		}
		w.Days = days
		out.Window = &w
	}
	return out, nil
}

func checkRate(field string, v int64) error {
	if v < 0 || v > MaxKiBps {
		return invalid(field, "must be 0-%d KiB/s (0 = unlimited; got %d)", MaxKiBps, v)
	}
	return nil
}

// normDays checks day names and returns them in week order.
func normDays(field string, days []string) ([]string, error) {
	if len(days) == 0 {
		return nil, invalid(field, "name at least one day (%s)", strings.Join(Days, ", "))
	}
	seen := make(map[string]bool, len(days))
	for _, d := range days {
		if dayIndex(d) < 0 {
			return nil, invalid(field, "unknown day %q (use %s)", d, strings.Join(Days, ", "))
		}
		if seen[d] {
			return nil, invalid(field, "day %q given twice", d)
		}
		seen[d] = true
	}
	out := make([]string, 0, len(days))
	for _, d := range Days {
		if seen[d] {
			out = append(out, d)
		}
	}
	return out, nil
}

// parseSpan parses From and To of an entry or window.
func parseSpan(field, from, to string) (int, int, error) {
	f, ok := parseHM(from)
	if !ok {
		return 0, 0, invalid(field+".from", "%q is not a time HH:MM (00:00-23:59)", from)
	}
	t, ok := parseHM(to)
	if !ok {
		return 0, 0, invalid(field+".to", "%q is not a time HH:MM (00:00-23:59)", to)
	}
	if f == t {
		return 0, 0, invalid(field+".to", "must differ from from (%s)", from)
	}
	return f, t, nil
}

// parseHM parses "HH:MM" into minutes after midnight.
func parseHM(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// spanMinutes is the nominal length of from-to in minutes (to before from crosses midnight).
func spanMinutes(from, to int) int {
	return ((to-from)%minutesPerDay + minutesPerDay) % minutesPerDay
}

// dayIndex is d's position in Days (Monday 0), -1 for an unknown name.
func dayIndex(d string) int { return slices.Index(Days, d) }

// weekdayIndex maps a time.Weekday to its position in Days.
func weekdayIndex(wd time.Weekday) int { return (int(wd) + 6) % 7 }

// at is the instant of minute m (after midnight) on the calendar day of day in loc.
func at(day time.Time, m int, loc *time.Location) time.Time {
	y, mo, d := day.Date()
	return time.Date(y, mo, d, m/60, m%60, 0, 0, loc)
}

// period is one occurrence of an entry or a window: [start, end).
type period struct {
	start, end time.Time
}

// periods returns the occurrences of days/from/to whose start day lies from firstDay to lastDay
// days after t's calendar day (both may be negative), in start order. It is empty when the span
// does not parse.
func periods(days []string, from, to string, t time.Time, loc *time.Location, firstDay, lastDay int) []period {
	f, ok1 := parseHM(from)
	e, ok2 := parseHM(to)
	if !ok1 || !ok2 || f == e {
		return nil
	}
	lt := t.In(loc)
	y, m, d := lt.Date()
	var out []period
	for off := firstDay; off <= lastDay; off++ {
		day := time.Date(y, m, d+off, 12, 0, 0, 0, loc)
		if !slices.Contains(days, Days[weekdayIndex(day.Weekday())]) {
			continue
		}
		p := period{start: at(day, f, loc)}
		if e > f {
			p.end = at(day, e, loc)
		} else {
			p.end = at(time.Date(y, m, d+off+1, 12, 0, 0, 0, loc), e, loc)
		}
		out = append(out, p)
	}
	return out
}

// location returns loc, or UTC when it is nil.
func location(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}

// InForce returns the upload and download limits in KiB/s at t in loc (0 = unlimited): the
// values of the timetable entry that covers t, else the base values.
func (c Config) InForce(t time.Time, loc *time.Location) (upKiBps, downKiBps int64) {
	loc = location(loc)
	for _, e := range c.Timetable {
		for _, p := range periods(e.Days, e.From, e.To, t, loc, -1, 0) {
			if !t.Before(p.start) && t.Before(p.end) {
				return e.UploadKiBps, e.DownloadKiBps
			}
		}
	}
	return c.UploadKiBps, c.DownloadKiBps
}

// NextChange returns the first instant after t at which InForce changes, or the zero time when it
// never does (no timetable, or entries with the base values).
func (c Config) NextChange(t time.Time, loc *time.Location) time.Time {
	loc = location(loc)
	var bounds []time.Time
	for _, e := range c.Timetable {
		for _, p := range periods(e.Days, e.From, e.To, t, loc, -1, 8) {
			for _, b := range []time.Time{p.start, p.end} {
				if b.After(t) {
					bounds = append(bounds, b)
				}
			}
		}
	}
	slices.SortFunc(bounds, func(a, b time.Time) int { return a.Compare(b) })
	up, down := c.InForce(t, loc)
	for _, b := range bounds {
		if u, d := c.InForce(b, loc); u != up || d != down {
			return b
		}
	}
	return time.Time{}
}

// Open reports whether t lies inside one of the window's periods (in loc). A nil window is always
// open.
func (w *Window) Open(t time.Time, loc *time.Location) bool {
	if w == nil {
		return true
	}
	return !w.EndsAt(t, loc).IsZero()
}

// EndsAt returns the end of the window period that contains t: the moment a job must stop (plus
// Grace). It is the zero time when t is outside the window, or when the window is nil (always
// open, no end).
func (w *Window) EndsAt(t time.Time, loc *time.Location) time.Time {
	if w == nil {
		return time.Time{}
	}
	for _, p := range periods(w.Days, w.From, w.To, t, location(loc), -1, 0) {
		if !t.Before(p.start) && t.Before(p.end) {
			return p.end
		}
	}
	return time.Time{}
}

// NextOpen returns t when the window is open at t, else the start of its next period (the zero
// time when the window has no valid period). A nil window returns t.
func (w *Window) NextOpen(t time.Time, loc *time.Location) time.Time {
	if w.Open(t, loc) {
		return t
	}
	for _, p := range periods(w.Days, w.From, w.To, t, location(loc), 0, 8) {
		if p.start.After(t) {
			return p.start
		}
	}
	return time.Time{}
}

// Length is the nominal length of one period (From to To on the clock; the night the clock moves
// the real period is an hour shorter or longer). A nil window returns 0.
func (w *Window) Length() time.Duration {
	if w == nil {
		return 0
	}
	f, ok1 := parseHM(w.From)
	t, ok2 := parseHM(w.To)
	if !ok1 || !ok2 {
		return 0
	}
	return time.Duration(spanMinutes(f, t)) * time.Minute
}

// Grace is how long a job may run past the end of a period before it is stopped.
func (w *Window) Grace() time.Duration {
	if w == nil {
		return 0
	}
	return time.Duration(w.GraceMinutes) * time.Minute
}

// Describe renders the window's times for messages: "01:00–07:00".
func (w *Window) Describe() string {
	if w == nil {
		return ""
	}
	return w.From + "–" + w.To
}
