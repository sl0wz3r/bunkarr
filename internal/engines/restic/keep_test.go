package restic

import (
	"encoding/json"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
)

// pinned builds the pinned retention table of §14.5: one source, 40 complete snapshots, one a day
// at 02:00 UTC from 2026-08-01 to 2026-09-09; now 2026-09-09 03:00 UTC; daily 7, weekly 4,
// monthly 3, yearly 0. A deleted file's retained row references 2026-08-12 (not expired) and a
// held update's record references 2026-08-20; the row that referenced 2026-08-04 expired first,
// so it references nothing any more.
type pinnedTable struct {
	in    KeepInput
	byDay map[string]string // "08-12" → snapshot id
	day   map[string]string // id → "08-12"
}

func newPinned(t *testing.T) *pinnedTable {
	p := &pinnedTable{byDay: map[string]string{}, day: map[string]string{}}
	p.in = KeepInput{Recorded: map[string]Recorded{}, Base: map[int64]string{}, Refs: map[string]bool{},
		LiveSources: map[int64]bool{1: true}, LiveIntegrations: map[int64]bool{},
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 3}, EngineTag: testEngineTag,
		Now: time.Date(2026, 9, 9, 3, 0, 0, 0, time.UTC), Loc: time.UTC}
	first := day(time.August, 1)
	for i := range 40 {
		ts := first.AddDate(0, 0, i)
		sid := id(1000 + i)
		p.in.Snapshots = append(p.in.Snapshots, Snapshot{ID: sid, Time: ts, Tags: mediaTags(t, testEngineTag, 1, int64(100+i), 1),
			Paths: []string{"/mnt/media"}})
		p.in.Recorded[sid] = Recorded{SourceID: 1, Complete: true, Batch: 1}
		p.byDay[ts.Format("01-02")] = sid
		p.day[sid] = ts.Format("01-02")
	}
	p.in.Base[1] = p.byDay["09-09"]
	p.in.Refs[p.byDay["08-12"]] = true
	p.in.Refs[p.byDay["08-20"]] = true
	return p
}

// kept returns the days KeepMedia keeps of the table's source, sorted.
func (p *pinnedTable) run(t *testing.T) (kept, forgotten []string, keep map[string]string) {
	t.Helper()
	forget, keep := KeepMedia(p.in)
	for _, id := range forget {
		if d, ok := p.day[id]; ok {
			forgotten = append(forgotten, d)
		} else {
			forgotten = append(forgotten, id)
		}
	}
	for id := range keep {
		if d, ok := p.day[id]; ok {
			kept = append(kept, d)
		}
	}
	sort.Strings(kept)
	for i := 1; i < len(forget); i++ {
		if !p.timeOf(forget[i-1]).Before(p.timeOf(forget[i])) && p.timeOf(forget[i-1]) != p.timeOf(forget[i]) {
			t.Errorf("forget list not oldest first: %v", forgotten)
		}
	}
	return kept, forgotten, keep
}

func (p *pinnedTable) timeOf(id string) time.Time {
	for _, s := range p.in.Snapshots {
		if s.ID == id {
			return s.Time
		}
	}
	return time.Time{}
}

func days(from, to string) []string {
	var out []string
	f, _ := time.Parse("01-02", from)
	l, _ := time.Parse("01-02", to)
	for d := f; !d.After(l); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("01-02"))
	}
	return out
}

func joinDays(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	sort.Strings(out)
	return out
}

// TestKeepMediaPinnedTable is acceptance 2's table (§14.5): exactly the 28 listed snapshots are
// forgotten, and each kept one for its reason.
func TestKeepMediaPinnedTable(t *testing.T) {
	p := newPinned(t)
	kept, forgotten, keep := p.run(t)
	wantKept := joinDays(days("09-03", "09-09"), []string{"08-31", "08-30", "08-23", "08-20", "08-12"})
	if !slices.Equal(kept, wantKept) {
		t.Fatalf("kept\n got %v\nwant %v", kept, wantKept)
	}
	wantForgotten := joinDays(days("08-01", "08-11"), days("08-13", "08-19"), []string{"08-21", "08-22"}, days("08-24", "08-29"),
		[]string{"09-01", "09-02"})
	sort.Strings(forgotten)
	if len(forgotten) != 28 || !slices.Equal(forgotten, wantForgotten) {
		t.Fatalf("forgotten (%d)\n got %v\nwant %v", len(forgotten), forgotten, wantForgotten)
	}
	for d, why := range map[string]string{"09-09": KeepNewest, "09-06": KeepDaily, "08-31": KeepMonthly, "08-30": KeepWeekly,
		"08-23": KeepWeekly, "08-20": KeepReferenced, "08-12": KeepReferenced} {
		if got := keep[p.byDay[d]]; got != why {
			t.Errorf("%s kept as %q, want %q", d, got, why)
		}
	}
	// 08-04 is forgotten only once its row expired: while referenced, it stays.
	p.in.Refs[p.byDay["08-04"]] = true
	if _, forgotten, _ := p.run(t); slices.Contains(forgotten, "08-04") || len(forgotten) != 27 {
		t.Fatalf("08-04 referenced: forgotten %v", forgotten)
	}
}

// TestKeepMediaVariants are the variants of §14.5 and §14.1 KeepMedia.
func TestKeepMediaVariants(t *testing.T) {
	t.Run("every keep value 0", func(t *testing.T) {
		p := newPinned(t)
		p.in.Retention = Retention{}
		kept, forgotten, _ := p.run(t)
		if !slices.Equal(kept, []string{"08-12", "08-20", "09-09"}) || len(forgotten) != 37 {
			t.Fatalf("kept %v, forgotten %d", kept, len(forgotten))
		}
	})
	t.Run("every keep value 0 plus a replaced row", func(t *testing.T) {
		p := newPinned(t)
		p.in.Retention = Retention{}
		p.in.Refs[p.byDay["09-01"]] = true // an update on 09-02 kept its old version in 09-01
		kept, _, _ := p.run(t)
		if !slices.Equal(kept, []string{"08-12", "08-20", "09-01", "09-09"}) {
			t.Fatalf("kept %v", kept)
		}
	})
	t.Run("now 2027-01-01 without a new snapshot", func(t *testing.T) {
		p := newPinned(t)
		p.in.Now = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		kept, _, keep := p.run(t)
		if !slices.Contains(kept, "09-09") || keep[p.byDay["09-09"]] != KeepNewest {
			t.Fatalf("kept %v", kept)
		}
		p.in.Retention = Retention{}
		if kept, _, _ := p.run(t); !slices.Equal(kept, []string{"08-12", "08-20", "09-09"}) {
			t.Fatalf("every keep 0: kept %v", kept)
		}
	})
	t.Run("an unrecorded snapshot newer than the base", func(t *testing.T) {
		p := newPinned(t)
		p.in.Retention = Retention{}
		crashed := id(5000)
		p.in.Snapshots = append(p.in.Snapshots, Snapshot{ID: crashed, Time: time.Date(2026, 9, 9, 4, 0, 0, 0, time.UTC),
			Tags: mediaTags(t, testEngineTag, 1, 200, 1)})
		p.in.Now = time.Date(2026, 9, 9, 5, 0, 0, 0, time.UTC)
		forget, keep := KeepMedia(p.in)
		if keep[crashed] != KeepNewest || keep[p.byDay["09-09"]] == "" || slices.Contains(forget, p.byDay["09-09"]) {
			t.Fatalf("crashed %q, base %q", keep[crashed], keep[p.byDay["09-09"]])
		}
	})
	t.Run("the base when a newer complete snapshot is not recorded", func(t *testing.T) {
		p := newPinned(t)
		p.in.Retention = Retention{}
		p.in.Base[1] = p.byDay["09-05"] // the newer rows are gone (a restore of the database)
		for _, d := range days("09-06", "09-09") {
			delete(p.in.Recorded, p.byDay[d])
		}
		_, keep := KeepMedia(p.in)
		if keep[p.byDay["09-05"]] == "" {
			t.Fatal("the base was forgotten")
		}
	})
	t.Run("incomplete snapshots are not bucketed", func(t *testing.T) {
		p := newPinned(t)
		for _, d := range days("09-03", "09-08") {
			r := p.in.Recorded[p.byDay[d]]
			r.Complete = false
			p.in.Recorded[p.byDay[d]] = r
		}
		kept, _, _ := p.run(t)
		// Daily now reaches back through the complete snapshots (09-09, 09-02 … 08-28), weekly
		// takes 09-02 for 2026-W36, and the incomplete 09-03 … 09-08 are forgotten.
		want := joinDays([]string{"09-09"}, days("08-28", "09-02"), []string{"08-23", "08-20", "08-12"})
		if !slices.Equal(kept, want) {
			t.Fatalf("kept\n got %v\nwant %v", kept, want)
		}
	})
	t.Run("time zone", func(t *testing.T) {
		p := newPinned(t)
		p.in.Retention = Retention{Daily: 2}
		// 00:30 and 23:30 UTC are two days in UTC but one day in UTC-3.
		p.in.Snapshots = p.in.Snapshots[:0]
		p.in.Refs = map[string]bool{}
		p.in.Base = map[int64]string{1: id(3)}
		for i, ts := range []time.Time{time.Date(2026, 9, 9, 0, 30, 0, 0, time.UTC), time.Date(2026, 9, 8, 23, 30, 0, 0, time.UTC),
			time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)} {
			sid := id(3 - i)
			p.in.Snapshots = append(p.in.Snapshots, Snapshot{ID: sid, Time: ts, Tags: mediaTags(t, testEngineTag, 1, int64(i+1), 1)})
			p.in.Recorded[sid] = Recorded{SourceID: 1, Complete: true}
		}
		forget, _ := KeepMedia(p.in)
		if !slices.Equal(forget, []string{id(1)}) {
			t.Fatalf("UTC: forgot %v, want the 09-07 snapshot", forget)
		}
		p.in.Loc = time.FixedZone("UTC-3", -3*3600)
		forget, _ = KeepMedia(p.in)
		if !slices.Equal(forget, []string{id(2)}) {
			t.Fatalf("UTC-3: forgot %v, want the 23:30 snapshot (the same local day as 00:30)", forget)
		}
	})
	t.Run("other groups, orphans, other tags and bad tags are untouched", func(t *testing.T) {
		p := newPinned(t)
		wantForget, _ := KeepMedia(p.in)
		var extra []string
		add := func(tags []string, ts time.Time, recorded bool, source int64) {
			sid := id(9000 + len(extra))
			p.in.Snapshots = append(p.in.Snapshots, Snapshot{ID: sid, Time: ts, Tags: tags})
			if recorded {
				p.in.Recorded[sid] = Recorded{SourceID: source, Complete: true}
			}
			extra = append(extra, sid)
		}
		// A second, live source with recent snapshots its own rules keep.
		p.in.LiveSources[2] = true
		for _, d := range days("09-07", "09-09") {
			ts, _ := time.Parse("2006-01-02", "2026-"+d)
			add(mediaTags(t, testEngineTag, 2, 1, 1), ts, true, 2)
		}
		// An orphan group (source 3 deleted) with old snapshots.
		for _, d := range days("08-01", "08-05") {
			ts, _ := time.Parse("2006-01-02", "2026-"+d)
			add(mediaTags(t, testEngineTag, 3, 1, 1), ts, true, 0)
		}
		// Another engine_tag with the same source id (an earlier install).
		for _, d := range days("08-01", "08-05") {
			ts, _ := time.Parse("2006-01-02", "2026-"+d)
			add(mediaTags(t, otherTag, 1, 1, 1), ts, false, 0)
		}
		// Unparseable and duplicated tags, a user's snapshot without tags.
		old := day(time.July, 1)
		add([]string{"bunkarr", "bunkarr-dest:" + testEngineTag, "bunkarr-kind:media"}, old, false, 0)
		add(append(mediaTags(t, testEngineTag, 1, 1, 1), "bunkarr-source:2"), old, false, 0)
		add(nil, old, false, 0)
		// Config versions: their runners decide.
		plex, _ := Tags(TagInput{EngineTag: testEngineTag, Kind: engines.VersionPlexDB, JobID: 5, IntegrationID: 1, Version: "20260701T020000Z"})
		p.in.LiveIntegrations[1] = true
		add(plex, old, false, 0)
		add(plex, old.Add(time.Hour), false, 0)
		forget, keep := KeepMedia(p.in)
		if !slices.Equal(forget, wantForget) {
			t.Fatalf("the extra snapshots changed the forget list:\n got %v\nwant %v", forget, wantForget)
		}
		for _, sid := range extra {
			if keep[sid] == "" {
				t.Errorf("extra snapshot %s has no keep reason", sid[60:])
			}
		}
		if keep[extra[3]] != KeepNewest && keep[extra[3]] != KeepOrphan {
			t.Errorf("orphan: %q", keep[extra[3]])
		}
		if keep[extra[4]] != KeepOrphan || keep[extra[8]] != KeepForeign || keep[extra[13]] != KeepForeign || keep[extra[14]] != KeepForeign {
			t.Errorf("reasons: orphan %q, other tag %q, unparseable %q, duplicate %q", keep[extra[4]], keep[extra[8]], keep[extra[13]], keep[extra[14]])
		}
		if keep[extra[16]] != KeepConfig {
			t.Errorf("config version: %q", keep[extra[16]])
		}
	})
	t.Run("future, duplicates and a source mismatch are kept", func(t *testing.T) {
		p := newPinned(t)
		p.in.Retention = Retention{}
		p.in.Now = day(time.August, 20) // the clock went back
		_, keep := KeepMedia(p.in)
		if keep[p.byDay["08-25"]] != KeepFuture {
			t.Fatalf("future: %q", keep[p.byDay["08-25"]])
		}
		p.in.Now = time.Date(2026, 9, 9, 3, 0, 0, 0, time.UTC)
		p.in.Snapshots = append(p.in.Snapshots, p.in.Snapshots[3])
		r := p.in.Recorded[p.byDay["08-06"]]
		r.SourceID = 7
		p.in.Recorded[p.byDay["08-06"]] = r
		forget, keep := KeepMedia(p.in)
		if keep[p.byDay["08-04"]] != KeepDuplicate || keep[p.byDay["08-06"]] != KeepMismatch ||
			slices.Contains(forget, p.byDay["08-04"]) || slices.Contains(forget, p.byDay["08-06"]) {
			t.Fatalf("duplicate %q, mismatch %q", keep[p.byDay["08-04"]], keep[p.byDay["08-06"]])
		}
	})
}

// TestBucketsMatchRestic: the bucket rules decide exactly what restic's own --keep-daily and
// --keep-weekly decided in the spike (forget-keep-*.json).
func TestBucketsMatchRestic(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		r       Retention
	}{
		{"forget-keep-daily-2-dry-run.json", Retention{Daily: 2}},
		{"forget-keep-daily-3-weekly-1.json", Retention{Daily: 3, Weekly: 1}},
	} {
		f := enginetest.Fixture(t, "restic", tc.fixture)
		var groups []ForgetGroup
		if err := json.Unmarshal(f.Raw, &groups); err != nil {
			t.Fatal(err)
		}
		all := append(slices.Clone(groups[0].Keep), groups[0].Remove...)
		slices.SortFunc(all, func(a, b Snapshot) int { return b.Time.Compare(a.Time) })
		got := bucketKeep(all, tc.r, time.UTC)
		var want []string
		for _, s := range groups[0].Keep {
			want = append(want, s.ID)
		}
		var have []string
		for id := range got {
			have = append(have, id)
		}
		sort.Strings(want)
		sort.Strings(have)
		if !slices.Equal(have, want) {
			t.Errorf("%s: kept %v, restic kept %v", tc.fixture, have, want)
		}
	}
}

// TestFilterForget: config versions' forget requests pass the S24 checks.
func TestFilterForget(t *testing.T) {
	in := KeepInput{EngineTag: testEngineTag, Now: day(time.September, 30), Refs: map[string]bool{},
		LiveIntegrations: map[int64]bool{1: true}, LiveSources: map[int64]bool{1: true}, Recorded: map[string]Recorded{}}
	plex := func(n int, integration int64, d int, engineTag string) Snapshot {
		tags, err := Tags(TagInput{EngineTag: engineTag, Kind: engines.VersionPlexDB, JobID: int64(n), IntegrationID: integration, Version: "v"})
		if err != nil {
			t.Fatal(err)
		}
		return Snapshot{ID: id(n), Time: day(time.September, d), Tags: tags}
	}
	in.Snapshots = []Snapshot{plex(1, 1, 1, testEngineTag), plex(2, 1, 2, testEngineTag), plex(3, 1, 3, testEngineTag),
		plex(4, 2, 1, testEngineTag), plex(5, 2, 2, testEngineTag), plex(6, 1, 1, otherTag),
		{ID: id(7), Time: day(time.September, 1), Tags: mediaTags(t, testEngineTag, 1, 1, 1)},
		{ID: id(8), Time: day(time.September, 2), Tags: mediaTags(t, testEngineTag, 1, 2, 1)}}
	in.Refs[id(2)] = true
	forget, dropped := FilterForget(in, []string{id(1), id(2), id(3), id(4), id(6), id(7), id(99), id(1)})
	if !slices.Equal(forget, []string{id(1)}) {
		t.Fatalf("forget %v, dropped %v", forget, dropped)
	}
	for n, why := range map[int]string{2: KeepReferenced, 3: KeepNewest, 4: KeepOrphan, 6: KeepForeign, 99: "not in the repository",
		7: "a media snapshot (only the keep set forgets those)"} {
		if dropped[id(n)] != why {
			t.Errorf("%d dropped as %q, want %q", n, dropped[id(n)], why)
		}
	}
}
