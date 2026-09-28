package rclone

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
)

// rcloneDays are rclone's weekday names, in bwlimit.Days order.
var rcloneDays = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// BWLimit renders a destination's limits in rclone's --bwlimit form for RCLONE_BWLIMIT (§9.1):
// "" when there is no limit, "UP:DOWN" for base limits only, else the week's change points in
// rclone's weekday timetable form, "Mon-08:00,1024k:off Mon-23:00,off:off", where each entry sets
// the rate until the next and the last wraps around the week. Values are KiB/s ("1024k"), "off"
// for 0. rclone switches the rate itself during a transfer. cfg is checked with Normalize first.
func BWLimit(cfg bwlimit.Config) (string, error) {
	cfg, err := cfg.Normalize()
	if err != nil {
		return "", err
	}
	base := rate{cfg.UploadKiBps, cfg.DownloadKiBps}
	const day, week = 24 * 60, 7 * 24 * 60
	// Change points in minutes of the week; an entry's start wins over another entry's end at the
	// same minute.
	type point struct {
		minute int
		r      rate
		start  bool
	}
	var points []point
	for _, e := range cfg.Timetable {
		from, to := hm(e.From), hm(e.To)
		length := ((to-from)%day + day) % day
		for _, d := range e.Days {
			start := slices.Index(bwlimit.Days, d)*day + from
			points = append(points,
				point{start, rate{e.UploadKiBps, e.DownloadKiBps}, true},
				point{(start + length) % week, base, false})
		}
	}
	slices.SortStableFunc(points, func(a, b point) int {
		if a.minute != b.minute {
			return a.minute - b.minute
		}
		if a.start != b.start {
			if a.start {
				return 1 // the start is applied last, so it wins
			}
			return -1
		}
		return 0
	})
	// One value per minute (the last applied), then drop points that change nothing, cyclically.
	var merged []point
	for _, p := range points {
		if n := len(merged); n > 0 && merged[n-1].minute == p.minute {
			merged[n-1] = p
		} else {
			merged = append(merged, p)
		}
	}
	var kept []point
	for i, p := range merged {
		prev := merged[(i+len(merged)-1)%len(merged)]
		if len(merged) == 1 || p.r != prev.r {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		if len(merged) > 0 {
			base = merged[0].r
		}
		if base == (rate{}) {
			return "", nil
		}
		return base.String(), nil
	}
	parts := make([]string, len(kept))
	for i, p := range kept {
		parts[i] = fmt.Sprintf("%s-%02d:%02d,%s", rcloneDays[p.minute/day], p.minute%day/60, p.minute%60, p.r)
	}
	return strings.Join(parts, " "), nil
}

// rate is an upload and download limit in KiB/s (0 = unlimited).
type rate struct{ up, down int64 }

// String renders a rate as rclone's "UP:DOWN".
func (r rate) String() string {
	one := func(v int64) string {
		if v == 0 {
			return "off"
		}
		return fmt.Sprintf("%dk", v)
	}
	return one(r.up) + ":" + one(r.down)
}

// hm parses a normalized "HH:MM".
func hm(s string) int {
	var h, m int
	_, _ = fmt.Sscanf(s, "%d:%d", &h, &m)
	return h*60 + m
}
