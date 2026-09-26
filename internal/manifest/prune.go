package manifest

import (
	"cmp"
	"slices"
	"time"
)

// DamagedKeep is how long a damaged version is kept (for diagnosis) before the retention deletes
// it, like a Plex DB version that failed verification.
const DamagedKeep = 7 * 24 * time.Hour

// Retained is a recorded version as the retention sees it.
type Retained struct {
	ID        int64
	CreatedAt time.Time
	// OK is true when the version's integrity is ok.
	OK bool
}

// newestFirst orders versions newest first (the higher id first at the same instant).
func newestFirst(vs []Retained) []Retained {
	out := slices.Clone(vs)
	slices.SortFunc(out, func(a, b Retained) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	return out
}

// KeepSet returns the ids of the ok versions the retention keeps (design §11.2 step 6): the
// newest version of each of the days most recent calendar days (in loc) that have an ok version,
// and the newest of each of the weeks most recent ISO weeks that have one (restic's keep-daily
// and keep-weekly). So the newest ok version is always kept, and versions made while exports
// stopped for a while are not all deleted at once. days below 1 counts as 1, weeks below 0 as 0;
// a nil loc is time.Local. Damaged versions are never in the set (PruneSet).
func KeepSet(vs []Retained, days, weeks int, loc *time.Location) map[int64]bool {
	days = max(days, 1)
	weeks = max(weeks, 0)
	if loc == nil {
		loc = time.Local
	}
	type day struct {
		y int
		m time.Month
		d int
	}
	type week struct{ y, w int }
	keep := map[int64]bool{}
	seenDays := map[day]bool{}
	seenWeeks := map[week]bool{}
	for _, v := range newestFirst(vs) {
		if !v.OK {
			continue
		}
		t := v.CreatedAt.In(loc)
		y, m, d := t.Date()
		if k := (day{y, m, d}); !seenDays[k] && len(seenDays) < days {
			seenDays[k] = true
			keep[v.ID] = true
		}
		wy, ww := t.ISOWeek()
		if k := (week{wy, ww}); !seenWeeks[k] && len(seenWeeks) < weeks {
			seenWeeks[k] = true
			keep[v.ID] = true
		}
	}
	return keep
}

// PruneSet returns the ids of the versions to delete, oldest first: every ok version KeepSet
// does not keep, and every damaged version made DamagedKeep or longer before now.
func PruneSet(vs []Retained, days, weeks int, now time.Time, loc *time.Location) []int64 {
	keep := KeepSet(vs, days, weeks, loc)
	sorted := newestFirst(vs)
	var out []int64
	for _, v := range slices.Backward(sorted) {
		switch {
		case v.OK && keep[v.ID]:
		case !v.OK && now.Sub(v.CreatedAt) < DamagedKeep:
		default:
			out = append(out, v.ID)
		}
	}
	return out
}
