package bwlimit

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // DST tests need Europe/Berlin also where the system has no zoneinfo
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// day returns the time on 2026-09-<date> (2026-09-07 is a Monday) at hh:mm in loc.
func day(loc *time.Location, date, hh, mm int) time.Time {
	return time.Date(2026, 9, date, hh, mm, 0, 0, loc)
}

func TestNormalize(t *testing.T) {
	valid := Config{
		UploadKiBps: 100,
		Timetable: []Entry{
			{Days: []string{"fri", "mon"}, From: "08:00", To: "23:00", UploadKiBps: 1024},
			{Days: []string{"mon"}, From: "23:00", To: "06:00", UploadKiBps: 512},
			{Days: []string{"tue"}, From: "06:00", To: "08:00", DownloadKiBps: MaxKiBps},
		},
		Window: &Window{Days: []string{"sun", "mon"}, From: "01:00", To: "07:00", GraceMinutes: 0},
	}
	got, err := valid.Normalize()
	if err != nil {
		t.Fatalf("Normalize(valid) = %v", err)
	}
	if strings.Join(got.Timetable[0].Days, ",") != "mon,fri" || strings.Join(got.Window.Days, ",") != "mon,sun" {
		t.Errorf("days not in week order: %v %v", got.Timetable[0].Days, got.Window.Days)
	}
	if got.Window.GraceMinutes != 0 {
		t.Errorf("an explicit grace 0 changed to %d", got.Window.GraceMinutes)
	}
	if b, _ := json.Marshal(Config{}.mustNormalize(t)); string(b) != `{"uploadKiBps":0,"downloadKiBps":0,"timetable":[],"window":null}` {
		t.Errorf("empty config JSON = %s", b)
	}

	many := make([]Entry, MaxEntries+1)
	for i := range many {
		many[i] = Entry{Days: []string{Days[i%7]}, From: fmt.Sprintf("%02d:00", i), To: fmt.Sprintf("%02d:30", i)}
	}
	tests := []struct {
		name  string
		c     Config
		field string
	}{
		{"too many entries", Config{Timetable: many}, "timetable"},
		{"negative base", Config{UploadKiBps: -1}, "uploadKiBps"},
		{"base too high", Config{DownloadKiBps: MaxKiBps + 1}, "downloadKiBps"},
		{"no days", Config{Timetable: []Entry{{From: "01:00", To: "02:00"}}}, "timetable[0].days"},
		{"unknown day", Config{Timetable: []Entry{{Days: []string{"Mon"}, From: "01:00", To: "02:00"}}}, "timetable[0].days"},
		{"repeated day", Config{Timetable: []Entry{{Days: []string{"mon", "mon"}, From: "01:00", To: "02:00"}}}, "timetable[0].days"},
		{"hour 24", Config{Timetable: []Entry{{Days: []string{"mon"}, From: "24:00", To: "02:00"}}}, "timetable[0].from"},
		{"one-digit hour", Config{Timetable: []Entry{{Days: []string{"mon"}, From: "01:00", To: "8:00"}}}, "timetable[0].to"},
		{"signed hour", Config{Timetable: []Entry{{Days: []string{"mon"}, From: "+1:00", To: "08:00"}}}, "timetable[0].from"},
		{"from equals to", Config{Timetable: []Entry{{Days: []string{"mon"}, From: "01:00", To: "01:00"}}}, "timetable[0].to"},
		{"entry rate", Config{Timetable: []Entry{{Days: []string{"mon"}, From: "01:00", To: "02:00", UploadKiBps: -5}}}, "timetable[0].uploadKiBps"},
		{"overlap on one day", Config{Timetable: []Entry{
			{Days: []string{"mon"}, From: "08:00", To: "12:00"},
			{Days: []string{"mon", "wed"}, From: "11:59", To: "13:00"},
		}}, "timetable[1]"},
		{"overlap across midnight", Config{Timetable: []Entry{
			{Days: []string{"mon"}, From: "23:00", To: "06:00"},
			{Days: []string{"tue"}, From: "05:00", To: "08:00"},
		}}, "timetable[1]"},
		{"overlap across the week's end", Config{Timetable: []Entry{
			{Days: []string{"sun"}, From: "23:00", To: "02:00"},
			{Days: []string{"mon"}, From: "01:00", To: "03:00"},
		}}, "timetable[1]"},
		{"window without days", Config{Window: &Window{From: "01:00", To: "07:00"}}, "window.days"},
		{"window from equals to", Config{Window: &Window{Days: []string{"mon"}, From: "01:00", To: "01:00"}}, "window.to"},
		{"window grace too long", Config{Window: &Window{Days: []string{"mon"}, From: "01:00", To: "07:00", GraceMinutes: 121}}, "window.graceMinutes"},
		{"window negative grace", Config{Window: &Window{Days: []string{"mon"}, From: "01:00", To: "07:00", GraceMinutes: -1}}, "window.graceMinutes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.c.Normalize()
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Field != tc.field {
				t.Fatalf("Normalize = %v, want a ValidationError on %s", err, tc.field)
			}
		})
	}

	adjacent := Config{Timetable: []Entry{
		{Days: []string{"mon"}, From: "08:00", To: "12:00"},
		{Days: []string{"mon"}, From: "12:00", To: "13:00"},
		{Days: []string{"sun"}, From: "23:00", To: "00:00"},
		{Days: []string{"mon"}, From: "00:00", To: "01:00"},
	}}
	if _, err := adjacent.Normalize(); err != nil {
		t.Errorf("adjacent entries refused: %v", err)
	}
}

func (c Config) mustNormalize(t *testing.T) Config {
	t.Helper()
	n, err := c.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWindowJSONDefaultGrace(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"uploadKiBps":1,"window":{"days":["mon"],"from":"01:00","to":"07:00"}}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Window == nil || c.Window.GraceMinutes != DefaultGraceMinutes || c.Window.AllowOverrun {
		t.Fatalf("window = %+v", c.Window)
	}
	if err := json.Unmarshal([]byte(`{"window":{"days":["mon"],"from":"01:00","to":"07:00","graceMinutes":0,"allowOverrun":true}}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Window.GraceMinutes != 0 || !c.Window.AllowOverrun {
		t.Fatalf("window = %+v", c.Window)
	}
	if err := json.Unmarshal([]byte(`{"window":null}`), &c); err != nil || c.Window != nil {
		t.Fatalf("window null = %+v, %v", c.Window, err)
	}
}

// timetable is the table of the InForce and NextChange tests: base 100 up; weekdays 08:00-23:00
// 1024 up; Monday 23:00 to Tuesday 06:00 512 up and 64 down; Sunday 23:00 to Monday 02:00 2048.
var timetable = Config{
	UploadKiBps: 100,
	Timetable: []Entry{
		{Days: []string{"mon", "tue", "wed", "thu", "fri"}, From: "08:00", To: "23:00", UploadKiBps: 1024},
		{Days: []string{"mon"}, From: "23:00", To: "06:00", UploadKiBps: 512, DownloadKiBps: 64},
		{Days: []string{"sun"}, From: "23:00", To: "02:00", UploadKiBps: 2048},
	},
}

func TestInForceAcrossMidnightAndWeekdays(t *testing.T) {
	loc := mustLoad(t, "Europe/Berlin")
	if _, err := timetable.Normalize(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		at       time.Time
		up, down int64
	}{
		{day(loc, 7, 7, 59), 100, 0},  // Monday 07:59: base
		{day(loc, 7, 1, 59), 2048, 0}, // Monday 01:59: Sunday's entry runs into Monday
		{day(loc, 7, 2, 0), 100, 0},   // Monday 02:00: base
		{day(loc, 7, 8, 0), 1024, 0},
		{day(loc, 7, 22, 59), 1024, 0},
		{day(loc, 7, 23, 0), 512, 64}, // Monday 23:00 to Tuesday 06:00
		{day(loc, 8, 5, 59), 512, 64},
		{day(loc, 8, 6, 0), 100, 0},
		{day(loc, 8, 8, 0), 1024, 0},
		{day(loc, 8, 23, 0), 100, 0}, // Tuesday 23:00: no Tuesday night entry
		{day(loc, 12, 12, 0), 100, 0},
		{day(loc, 13, 23, 30), 2048, 0}, // Sunday 23:30
	}
	for _, tc := range tests {
		up, down := timetable.InForce(tc.at, loc)
		if up != tc.up || down != tc.down {
			t.Errorf("InForce(%s) = %d/%d, want %d/%d", tc.at.Format("Mon 15:04"), up, down, tc.up, tc.down)
		}
		// The same instant given in another zone answers the same.
		if up2, down2 := timetable.InForce(tc.at.UTC(), loc); up2 != up || down2 != down {
			t.Errorf("InForce depends on the zone of t")
		}
	}
}

func TestNextChange(t *testing.T) {
	loc := time.UTC
	tests := []struct {
		from, want time.Time
	}{
		{day(loc, 7, 7, 0), day(loc, 7, 8, 0)},
		{day(loc, 7, 12, 0), day(loc, 7, 23, 0)},
		{day(loc, 7, 23, 30), day(loc, 8, 6, 0)},
		{day(loc, 8, 6, 30), day(loc, 8, 8, 0)},
		{day(loc, 11, 23, 0), day(loc, 13, 23, 0)}, // Friday 23:00 -> Sunday 23:00
		{day(loc, 13, 23, 0), day(loc, 14, 2, 0)},  // at a change point: the next one
	}
	for _, tc := range tests {
		if got := timetable.NextChange(tc.from, loc); !got.Equal(tc.want) {
			t.Errorf("NextChange(%s) = %s, want %s", tc.from.Format("Mon 02 15:04"), got.Format("Mon 02 15:04"), tc.want.Format("Mon 02 15:04"))
		}
	}
	if got := (Config{UploadKiBps: 5}).NextChange(day(loc, 7, 0, 0), loc); !got.IsZero() {
		t.Errorf("NextChange without a timetable = %v", got)
	}
	same := Config{UploadKiBps: 5, Timetable: []Entry{{Days: []string{"mon"}, From: "01:00", To: "02:00", UploadKiBps: 5}}}
	if got := same.NextChange(day(loc, 7, 0, 0), loc); !got.IsZero() {
		t.Errorf("NextChange with entries equal to the base = %v", got)
	}
}

func TestWindow(t *testing.T) {
	loc := time.UTC
	nightly := &Window{Days: Days, From: "01:00", To: "07:00", GraceMinutes: 15}
	mondayNight := &Window{Days: []string{"mon"}, From: "23:00", To: "06:00"}
	tests := []struct {
		name     string
		w        *Window
		at       time.Time
		open     bool
		endsAt   time.Time
		nextOpen time.Time
	}{
		{"before", nightly, day(loc, 7, 0, 59), false, time.Time{}, day(loc, 7, 1, 0)},
		{"opening", nightly, day(loc, 7, 1, 0), true, day(loc, 7, 7, 0), day(loc, 7, 1, 0)},
		{"inside", nightly, day(loc, 7, 6, 59), true, day(loc, 7, 7, 0), day(loc, 7, 6, 59)},
		{"closing", nightly, day(loc, 7, 7, 0), false, time.Time{}, day(loc, 8, 1, 0)},
		{"across midnight, next day", mondayNight, day(loc, 8, 5, 0), true, day(loc, 8, 6, 0), day(loc, 8, 5, 0)},
		{"across midnight, before", mondayNight, day(loc, 7, 22, 0), false, time.Time{}, day(loc, 7, 23, 0)},
		{"a week ahead", mondayNight, day(loc, 8, 6, 0), false, time.Time{}, day(loc, 14, 23, 0)},
		{"nil is always open", nil, day(loc, 8, 6, 0), true, time.Time{}, day(loc, 8, 6, 0)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.Open(tc.at, loc); got != tc.open {
				t.Errorf("Open = %v", got)
			}
			if got := tc.w.EndsAt(tc.at, loc); !got.Equal(tc.endsAt) {
				t.Errorf("EndsAt = %v, want %v", got, tc.endsAt)
			}
			if got := tc.w.NextOpen(tc.at, loc); !got.Equal(tc.nextOpen) {
				t.Errorf("NextOpen = %v, want %v", got, tc.nextOpen)
			}
		})
	}
	if nightly.Length() != 6*time.Hour || mondayNight.Length() != 7*time.Hour || nightly.Grace() != 15*time.Minute {
		t.Errorf("Length %v %v, Grace %v", nightly.Length(), mondayNight.Length(), nightly.Grace())
	}
	if nightly.Describe() != "01:00–07:00" {
		t.Errorf("Describe = %q", nightly.Describe())
	}
}

// TestWindowAroundDST: the next open and the time left are wall-clock based on the nights the
// clock moves (Europe/Berlin: 2026-03-29 02:00 -> 03:00, 2026-10-25 03:00 -> 02:00).
func TestWindowAroundDST(t *testing.T) {
	loc := mustLoad(t, "Europe/Berlin")
	nightly := &Window{Days: Days, From: "01:00", To: "07:00"}
	// Spring forward: 01:00 CET to 07:00 CEST is 5 hours.
	start := time.Date(2026, 3, 29, 1, 0, 0, 0, loc)
	if got := nightly.NextOpen(time.Date(2026, 3, 28, 12, 0, 0, 0, loc), loc); !got.Equal(start) {
		t.Errorf("NextOpen before spring forward = %v, want %v", got, start)
	}
	if end := nightly.EndsAt(start.Add(30*time.Minute), loc); end.Sub(start.Add(30*time.Minute)) != 4*time.Hour+30*time.Minute {
		t.Errorf("time left after spring forward = %v", end.Sub(start.Add(30*time.Minute)))
	}
	if end := nightly.EndsAt(start, loc); !end.Equal(time.Date(2026, 3, 29, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("end on spring forward = %v", end.UTC())
	}
	// Fall back: 01:00 CEST to 07:00 CET is 7 hours; the repeated 02:30 is inside.
	start = time.Date(2026, 10, 25, 1, 0, 0, 0, loc)
	end := nightly.EndsAt(start, loc)
	if end.Sub(start) != 7*time.Hour {
		t.Errorf("fall back window = %v", end.Sub(start))
	}
	secondHalfPast2 := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC) // 02:30 CET, the second one
	if !nightly.Open(secondHalfPast2, loc) || nightly.EndsAt(secondHalfPast2, loc).Sub(secondHalfPast2) != 5*time.Hour+30*time.Minute {
		t.Errorf("repeated hour: open %v, left %v", nightly.Open(secondHalfPast2, loc), nightly.EndsAt(secondHalfPast2, loc).Sub(secondHalfPast2))
	}
	if got := nightly.NextOpen(end, loc); !got.Equal(time.Date(2026, 10, 26, 1, 0, 0, 0, loc)) {
		t.Errorf("NextOpen after fall back = %v", got)
	}
	// A timetable change point on the spring-forward night is wall-clock too.
	c := Config{Timetable: []Entry{{Days: Days, From: "06:00", To: "22:00", UploadKiBps: 10}}}
	if got := c.NextChange(time.Date(2026, 3, 29, 0, 0, 0, 0, loc), loc); !got.Equal(time.Date(2026, 3, 29, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("NextChange across spring forward = %v", got.UTC())
	}
}
