package snapshots

import (
	"cmp"
	"slices"
	"time"
)

// Version is a recorded version as the retention rules see it.
type Version struct {
	ID        int64
	CreatedAt time.Time
	// OK is true when the version's integrity is IntegrityOK.
	OK bool
}

// Versions returns the retention view of list.
func Versions(list []Snapshot) []Version {
	out := make([]Version, 0, len(list))
	for _, s := range list {
		out = append(out, s.Version())
	}
	return out
}

// newestFirst returns a copy of vs ordered newest first (the higher id first at the same instant).
func newestFirst(vs []Version) []Version {
	sorted := slices.Clone(vs)
	slices.SortFunc(sorted, func(a, b Version) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	return sorted
}

// Keep returns the ids of the ok versions the retention keeps (phase1.md §5, phase2-3.md §10):
//
//   - the newest daily ok versions (a number of versions, not of days);
//   - the newest ok version of each of the weekly most recent ISO weeks (in loc) that have an ok
//     version;
//   - so the newest ok version is always kept.
//
// daily below 1 counts as 1, weekly below 0 as 0; a nil loc means time.Local. Failed versions are
// never in the set: Prune keeps them for FailedKeep.
func Keep(versions []Version, daily, weekly int, loc *time.Location) map[int64]bool {
	daily = max(daily, 1)
	weekly = max(weekly, 0)
	if loc == nil {
		loc = time.Local
	}
	keep := map[int64]bool{}
	type week struct{ year, week int }
	weeks := map[week]bool{}
	nOK := 0
	for _, v := range newestFirst(versions) {
		if !v.OK {
			continue
		}
		nOK++
		if nOK <= daily { // includes the newest ok version
			keep[v.ID] = true
		}
		y, w := v.CreatedAt.In(loc).ISOWeek()
		if k := (week{y, w}); !weeks[k] && len(weeks) < weekly {
			weeks[k] = true
			keep[v.ID] = true
		}
	}
	return keep
}

// Prune applies the version retention to the versions of one integration at one destination and
// returns the ids of the versions to delete, oldest first: every ok version that Keep does not
// keep, and every failed version made FailedKeep or longer before now.
func Prune(versions []Version, daily, weekly int, now time.Time, loc *time.Location) []int64 {
	keep := Keep(versions, daily, weekly, loc)
	sorted := newestFirst(versions)
	var remove []int64
	for i := len(sorted) - 1; i >= 0; i-- {
		v := sorted[i]
		switch {
		case v.OK && keep[v.ID]:
		case !v.OK && now.Sub(v.CreatedAt) < FailedKeep:
		default:
			remove = append(remove, v.ID)
		}
	}
	return remove
}
