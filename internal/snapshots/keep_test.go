package snapshots

import (
	"maps"
	"slices"
	"testing"
	"time"
)

// pruneNow is Thursday 2026-09-24 12:00 UTC, in ISO week 39 (Monday 09-21 to Sunday 09-27).
var pruneNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// dayVersions returns one version per day for days [0, n) before pruneNow; the version of day d
// has id d, integrity ok unless d is in failed.
func dayVersions(n int, failed ...int) []Version {
	out := make([]Version, 0, n)
	for d := range n {
		out = append(out, Version{ID: int64(d), CreatedAt: pruneNow.AddDate(0, 0, -d), OK: !slices.Contains(failed, d)})
	}
	return out
}

// idsExcept returns the ids [0, n) that are not in keep, oldest (highest day) first.
func idsExcept(n int, keep ...int64) []int64 {
	var out []int64
	for d := int64(n - 1); d >= 0; d-- {
		if !slices.Contains(keep, d) {
			out = append(out, d)
		}
	}
	return out
}

func span(from, to int64) []int64 {
	var out []int64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// TestPrune is the Plex DB retention table of phase1.md §5 (moved from internal/plexdb with the
// selection; the *arr backups use the same rules).
func TestPrune(t *testing.T) {
	berlinSummer := time.FixedZone("UTC+2", 2*3600)
	tests := []struct {
		name          string
		versions      []Version
		daily, weekly int
		loc           *time.Location
		want          []int64
	}{
		{name: "fewer than the daily count", versions: dayVersions(5), daily: 14, weekly: 8, want: nil},
		{name: "daily only", versions: dayVersions(20), daily: 14, weekly: 0, want: idsExcept(20, span(0, 13)...)},
		{
			// Days 0-13 are the daily versions; the 8 most recent weeks (39 … 32) keep their
			// newest version: days 0 (w39), 4 (Sunday of w38), 11, 18, 25, 32, 39, 46.
			name: "defaults: 14 daily and 8 weekly", versions: dayVersions(60), daily: 14, weekly: 8,
			want: idsExcept(60, append(span(0, 13), 18, 25, 32, 39, 46)...),
		},
		{
			name: "the newest ok version survives a newer failed one", versions: dayVersions(3, 0), daily: 1, weekly: 0,
			want: []int64{2},
		},
		{
			name:     "only old ok versions: the newest ok one is kept",
			versions: append(dayVersions(10, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9), Version{ID: 30, CreatedAt: pruneNow.AddDate(0, 0, -30), OK: true}),
			daily:    1, weekly: 0,
			want: []int64{9, 8, 7},
		},
		{
			name:     "failed versions are kept for 7 days",
			versions: dayVersions(10, 5, 6, 7, 8),
			daily:    14, weekly: 0,
			want: []int64{8, 7},
		},
		{
			name: "several versions a day: the daily count is a number of versions",
			versions: []Version{
				{ID: 1, CreatedAt: pruneNow, OK: true},
				{ID: 2, CreatedAt: pruneNow.Add(-time.Hour), OK: true},
				{ID: 3, CreatedAt: pruneNow.Add(-2 * time.Hour), OK: true},
				{ID: 4, CreatedAt: pruneNow.AddDate(0, 0, -1), OK: true},
			},
			daily: 2, weekly: 1,
			want: []int64{4, 3},
		},
		{
			name: "weeks without versions are not counted",
			versions: []Version{
				{ID: 1, CreatedAt: pruneNow, OK: true},                   // w39
				{ID: 2, CreatedAt: pruneNow.AddDate(0, 0, -1), OK: true}, // w39
				{ID: 3, CreatedAt: pruneNow.AddDate(0, 0, -63), OK: true},
				{ID: 4, CreatedAt: pruneNow.AddDate(0, 0, -64), OK: true},
				{ID: 5, CreatedAt: pruneNow.AddDate(0, 0, -140), OK: true},
			},
			daily: 1, weekly: 2,
			want: []int64{5, 4, 2},
		},
		{
			name: "same instant, id breaks the tie",
			versions: []Version{
				{ID: 7, CreatedAt: pruneNow, OK: true},
				{ID: 8, CreatedAt: pruneNow, OK: true},
			},
			daily: 1, weekly: 0,
			want: []int64{7},
		},
		{
			// 2026-09-20 23:30 UTC is Sunday (week 38) in UTC but Monday (week 39) at UTC+2.
			name: "ISO weeks in UTC",
			versions: []Version{
				{ID: 1, CreatedAt: pruneNow, OK: true},
				{ID: 2, CreatedAt: time.Date(2026, 9, 21, 0, 30, 0, 0, time.UTC), OK: true},
				{ID: 3, CreatedAt: time.Date(2026, 9, 20, 23, 30, 0, 0, time.UTC), OK: true},
			},
			daily: 1, weekly: 2, loc: time.UTC,
			want: []int64{2},
		},
		{
			name: "ISO weeks in the local zone",
			versions: []Version{
				{ID: 1, CreatedAt: pruneNow, OK: true},
				{ID: 2, CreatedAt: time.Date(2026, 9, 21, 0, 30, 0, 0, time.UTC), OK: true},
				{ID: 3, CreatedAt: time.Date(2026, 9, 20, 23, 30, 0, 0, time.UTC), OK: true},
			},
			daily: 1, weekly: 2, loc: berlinSummer,
			want: []int64{3, 2},
		},
		{name: "a daily count below 1 keeps the newest", versions: dayVersions(3), daily: 0, weekly: 0, want: []int64{2, 1}},
		{name: "a negative weekly count keeps no week", versions: dayVersions(3), daily: 1, weekly: -3, want: []int64{2, 1}},
		{name: "nothing to prune", versions: nil, daily: 14, weekly: 8, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc := tt.loc
			if loc == nil {
				loc = time.UTC
			}
			got := Prune(tt.versions, tt.daily, tt.weekly, pruneNow, loc)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("remove %v, want %v", got, tt.want)
			}
			// Keep and Prune agree: an ok version is removed exactly when Keep does not keep it.
			keep := Keep(tt.versions, tt.daily, tt.weekly, loc)
			for _, v := range tt.versions {
				if !v.OK {
					if keep[v.ID] {
						t.Errorf("Keep holds failed version %d", v.ID)
					}
					continue
				}
				if keep[v.ID] == slices.Contains(got, v.ID) {
					t.Errorf("version %d: kept %v, removed %v", v.ID, keep[v.ID], slices.Contains(got, v.ID))
				}
			}
		})
	}
}

func TestKeep(t *testing.T) {
	// 14 daily and 8 weekly over 60 days: days 0-13, plus days 18, 25, 32, 39 and 46, the newest
	// versions of the older ones of the 8 most recent ISO weeks.
	got := slices.Sorted(maps.Keys(Keep(dayVersions(60), 14, 8, time.UTC)))
	want := append(span(0, 13), 18, 25, 32, 39, 46)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Keep = %v; want %v", got, want)
	}
	// Only failed versions: nothing is kept (Prune keeps them for FailedKeep).
	if k := Keep(dayVersions(3, 0, 1, 2), 14, 8, time.UTC); len(k) != 0 {
		t.Fatalf("Keep of failed versions = %v", k)
	}
	// A nil location means the local zone.
	if k := Keep(dayVersions(2), 1, 0, nil); len(k) != 1 || !k[0] {
		t.Fatalf("Keep with a nil location = %v", k)
	}
}

func TestVersions(t *testing.T) {
	at := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	got := Versions([]Snapshot{{ID: 1, CreatedAt: at, Integrity: IntegrityOK}, {ID: 2, CreatedAt: at, Integrity: IntegrityFailed}})
	want := []Version{{ID: 1, CreatedAt: at, OK: true}, {ID: 2, CreatedAt: at, OK: false}}
	if !slices.Equal(got, want) {
		t.Fatalf("Versions = %+v", got)
	}
}
