package manifest

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
)

func TestKeepSetAndPruneSet(t *testing.T) {
	day := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, time.UTC) }
	// 2026-09-21 is a Monday (ISO week 39); 09-14..09-20 is week 38.
	vs := []Retained{
		{1, day(14, 10), true},  // week 38
		{2, day(18, 9), true},   // week 38, newest of it
		{3, day(21, 8), true},   // week 39
		{4, day(21, 20), true},  // week 39, newest of 09-21
		{5, day(23, 12), false}, // damaged, 2 days before now
		{6, day(24, 6), true},
		{7, day(24, 18), true}, // newest of 09-24
		{8, day(25, 1), true},  // newest
		{9, day(10, 1), false}, // damaged, 15 days old
	}
	now := day(25, 12)
	cases := []struct {
		days, weeks int
		keep        []int64
		prune       []int64
	}{
		// The newest of each of the last 3 days with a version; no weeks.
		{3, 0, []int64{4, 7, 8}, []int64{9, 1, 2, 3, 6}},
		// One day, and the newest of each of the last 2 weeks (39 → 8, 38 → 2).
		{1, 2, []int64{2, 8}, []int64{9, 1, 3, 4, 6, 7}},
		// Days below 1 count as 1: the newest ok version is always kept.
		{0, 0, []int64{8}, []int64{9, 1, 2, 3, 4, 6, 7}},
		// Everything: every day is kept, damaged versions only for 7 days.
		{30, 12, []int64{1, 2, 4, 7, 8}, []int64{9, 3, 6}},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("days=%d weeks=%d", c.days, c.weeks), func(t *testing.T) {
			keep := KeepSet(vs, c.days, c.weeks, time.UTC)
			var got []int64
			for id := range keep {
				got = append(got, id)
			}
			slices.Sort(got)
			if !slices.Equal(got, c.keep) {
				t.Fatalf("KeepSet = %v, want %v", got, c.keep)
			}
			if p := PruneSet(vs, c.days, c.weeks, now, time.UTC); !slices.Equal(p, c.prune) {
				t.Fatalf("PruneSet = %v, want %v (oldest first)", p, c.prune)
			}
		})
	}
	// Days are calendar days in the given zone: 23:30 UTC on the 24th is the 25th in Berlin.
	berlin, _ := time.LoadLocation("Europe/Berlin")
	two := []Retained{{1, time.Date(2026, 9, 24, 23, 30, 0, 0, time.UTC), true}, {2, day(25, 10), true}}
	if k := KeepSet(two, 2, 0, berlin); len(k) != 1 || !k[2] {
		t.Fatalf("in Berlin: %v", k)
	}
	if k := KeepSet(two, 2, 0, time.UTC); len(k) != 2 {
		t.Fatalf("in UTC: %v", k)
	}
}

func TestParseKeep(t *testing.T) {
	for raw, want := range map[string]Keep{
		"":                                     {30, 12},
		"{}":                                   {30, 12},
		"not json":                             {30, 12},
		`{"manifestDays":7,"manifestWeeks":4}`: {7, 4},
		`{"manifestDays":0,"manifestWeeks":0}`: {30, 0},
		`{"manifestWeeks":-1}`:                 {30, 12},
		`{"manifestWeeks":null}`:               {30, 12},
		`{"manifestDays":3651}`:                {30, 12},
		`{"manifestDays":3650,"manifestWeeks":520}`: {3650, 520},
		`{"manifestWeeks":521,"deletedDays":5}`:     {30, 12},
	} {
		if got := ParseKeep(raw); got != want {
			t.Errorf("ParseKeep(%s) = %+v, want %+v", raw, got, want)
		}
	}
}

// setRetention sets manifestDays and manifestWeeks the way the API does (PUT /destinations/{id}
// with a retention body, decoded strictly into destinations.Input) and checks that the runner
// reads them back from the stored retention.
func (e *testEnv) setRetention(days, weeks int) {
	e.t.Helper()
	body := fmt.Sprintf(`{"retention":{"manifestDays":%d,"manifestWeeks":%d}}`, days, weeks)
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var in destinations.Input
	if err := dec.Decode(&in); err != nil {
		e.t.Fatalf("decode %s: %v", body, err)
	}
	if _, err := e.dests.Update(e.ctx, e.dest.ID, in); err != nil {
		e.t.Fatal(err)
	}
	k, err := keepOf(e.ctx, e.db.Reader(), e.dest.ID)
	if err != nil || k != (Keep{Days: days, Weeks: weeks}) {
		e.t.Fatalf("stored manifest retention %+v, %v; want %d days, %d weeks", k, err, days, weeks)
	}
}

func TestRunPrunes(t *testing.T) {
	e := newEnv(t)
	e.setRetention(2, 1)
	// One changed version a day for five days.
	for i := range 5 {
		e.writeFile(fmt.Sprintf("movies/day-%d.mkv", i), 10)
		e.scan()
		e.refresh(e.rad.ID)
		e.refresh(e.son.ID)
		st, _ := e.runOK()
		if st.Unchanged {
			t.Fatalf("day %d unchanged", i)
		}
		e.clock.Advance(24 * time.Hour)
	}
	// The newest of the last 2 days (the last one also the newest of its week).
	vs := e.assertConverged(2)
	if !vs[0].CreatedAt.Equal(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)) || !vs[1].CreatedAt.Equal(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("kept %+v", vs)
	}
}

func TestRunPrunesWithoutWeeklyVersions(t *testing.T) {
	// manifestWeeks 0 keeps no weekly versions (design §11.2 step 6: 0-520): with manifestDays 1,
	// versions made Friday to Monday leave only Monday's (12 weeks would also keep Sunday's, the
	// newest of the week before).
	e := newEnv(t)
	e.setRetention(1, 0)
	for i := range 4 {
		e.writeFile(fmt.Sprintf("movies/day-%d.mkv", i), 10)
		e.scan()
		e.refresh(e.rad.ID)
		e.refresh(e.son.ID)
		if st, _ := e.runOK(); st.Unchanged {
			t.Fatalf("day %d unchanged", i)
		}
		e.clock.Advance(24 * time.Hour)
	}
	vs := e.assertConverged(1)
	if !vs[0].CreatedAt.Equal(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("kept %+v", vs)
	}
}
