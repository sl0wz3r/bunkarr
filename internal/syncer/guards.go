package syncer

import (
	"fmt"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// minHeldChanges is the number of changes a source may always have before the percentage limit of
// the mass-change guard applies (S10b: "exceed maxChangePercent of the live files and 20 files").
const minHeldChanges = 20

// guardResult is what the mass-change guard did to one source's plan.
type guardResult struct {
	// changes is the number of changes counted (retain + update + relink of a changed name + a
	// promote standing for a vanished name).
	changes int64
	// held is the number of items held (including those held with the change they depend on).
	held int64
	// limited reports that the changes exceeded the limits (all of them are held).
	limited bool
	// message describes the hold for the job log and the warning.
	message string
}

// applyGuard applies the mass-change guard (design S10b) to one source's plan: when the source's
// planned changes exceed settings.MaxChangePercent of its live files and 20 files, or exceed
// settings.MaxChangeFiles, every change is held; an update to 0 bytes or to less than half its
// old size is always held. Items that depend on a held item (a promote for a held retain or
// update, a link to a held primary) are held too. allowChanges (the "Apply held changes" sync)
// holds nothing. Items that already failed (S11) are neither counted nor held.
func applyGuard(items []*planItem, liveFiles int64, s destinations.Settings, allowChanges bool) guardResult {
	var res guardResult
	for _, it := range items {
		if it.change && it.status == "" {
			res.changes++
		}
	}
	if allowChanges {
		return res
	}
	pct := int64(s.MaxChangePercent)
	res.limited = (res.changes*100 > pct*liveFiles && res.changes > minHeldChanges) || res.changes > int64(s.MaxChangeFiles)
	hold := func(it *planItem, why string) {
		it.status = jobs.ItemHeld
		it.errMsg = why
		res.held++
	}
	if res.limited {
		res.message = fmt.Sprintf("%d changes (retained, updated or replaced files) exceed the mass-change limit (%d%% of %d files and more than %d, or more than %d); they are held until a sync with allowChanges",
			res.changes, s.MaxChangePercent, liveFiles, minHeldChanges, s.MaxChangeFiles)
	}
	for _, it := range items {
		if it.status != "" {
			continue
		}
		switch {
		case res.limited && it.change:
			hold(it, "held by the mass-change guard: "+res.message)
		case it.shrink:
			hold(it, fmt.Sprintf("held: the new version is %d bytes, less than half of the backed-up %d bytes (possible truncation); run a sync with allowChanges to apply it",
				it.d.Size, it.d.OldSize))
		}
	}
	for changed := true; changed; {
		changed = false
		for _, it := range items {
			if it.status == "" && it.after != nil && it.after.status == jobs.ItemHeld {
				hold(it, fmt.Sprintf("held with %s (%s)", it.after.rel, it.after.action))
				changed = true
			}
		}
	}
	return res
}
