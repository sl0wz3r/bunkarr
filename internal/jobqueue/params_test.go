package jobqueue

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// manyPaths returns n distinct valid paths "<prefix> 0000" … .
func manyPaths(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s %04d", prefix, i)
	}
	return out
}

// manyIDs returns the ids from, from+1, … (n of them).
func manyIDs(from, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(from + i)
	}
	return out
}

func TestCreateJobValidationPhase2(t *testing.T) {
	st := NewStore(openDB(t))
	sync := func(p jobs.Params) jobs.Params { p.DestinationID = 1; return p }
	cases := []struct {
		name string
		spec jobs.Spec
	}{
		{"refresh without integration", jobs.Spec{Type: jobs.TypeRefresh}},
		{"arr_backup without integration", jobs.Spec{Type: jobs.TypeArrBackup, Params: jobs.Params{DestinationID: 1}}},
		{"manifest_export without destination", jobs.Spec{Type: jobs.TypeManifestExport}},
		{"paths without a source", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{Paths: []string{"Heat (1995)"}})}},
		{"paths with two sources", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{1, 2}, Paths: []string{"Heat (1995)"}})}},
		{"path ./x", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{1}, Paths: []string{"./x"}})}},
		{"path a/../b", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{1}, Paths: []string{"a/../b"}})}},
		{"path /x", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{1}, Paths: []string{"/x"}})}},
		{"path ..", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{1}, Paths: []string{".."}})}},
		{"path .", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{1}, Paths: []string{"."}})}},
		{"empty path", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{1}, Paths: []string{""}})}},
		{"path with NUL", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{1}, Paths: []string{"a\x00b"}})}},
		{"too many paths", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{1}, Paths: manyPaths("M", jobs.MaxTargetPaths+1)})}},
		{"release with paths", jobs.Spec{Type: jobs.TypeSync, DryRun: true,
			Params: sync(jobs.Params{SourceIDs: []int64{1}, Paths: []string{"x"}, ReleaseDemoted: true})}},
		{"real release without releaseOf", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{ReleaseDemoted: true, ReleaseRevision: 3})}},
		{"real release without releaseRevision", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{ReleaseDemoted: true, ReleaseOf: 9})}},
		{"releaseOf without releaseDemoted", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{ReleaseOf: 9, ReleaseRevision: 3})}},
		{"negative releaseOf", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{ReleaseDemoted: true, ReleaseOf: -1, ReleaseRevision: 3})}},
		{"paths on a verify", jobs.Spec{Type: jobs.TypeVerify, Params: jobs.Params{DestinationID: 1, SourceIDs: []int64{1}, Paths: []string{"x"}}}},
		{"release on a retention", jobs.Spec{Type: jobs.TypeRetention, DryRun: true, Params: jobs.Params{ReleaseDemoted: true}}},
		{"arrItemIds on a sync", jobs.Spec{Type: jobs.TypeSync, Params: sync(jobs.Params{ArrItemIDs: []int64{1}})}},
		{"syncAfter on an arr_backup", jobs.Spec{Type: jobs.TypeArrBackup, Params: jobs.Params{IntegrationID: 1, SyncAfter: true}}},
		{"zero arr item id", jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1, ArrItemIDs: []int64{0}}}},
		{"too many arr item ids", jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1, ArrItemIDs: manyIDs(1, jobs.MaxTargetItems+1)}}},
		{"syncAfter without arrItemIds", jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1, SyncAfter: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := st.CreateJob(context.Background(), tc.spec); !isValidation(err) {
				t.Fatalf("err = %v; want a ValidationError", err)
			}
		})
	}
	if page, _ := st.ListJobs(context.Background(), JobQuery{}); page.TotalRecords != 0 {
		t.Fatalf("refused specs queued %d jobs", page.TotalRecords)
	}

	ok := []jobs.Spec{
		{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1}},
		{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1, AllowChanges: true}},
		{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1, ArrItemIDs: manyIDs(1, jobs.MaxTargetItems), SyncAfter: true}},
		{Type: jobs.TypeArrBackup, Params: jobs.Params{IntegrationID: 1}},
		{Type: jobs.TypeArrBackup, Params: jobs.Params{IntegrationID: 1, DestinationID: 2}},
		{Type: jobs.TypeManifestExport, Params: jobs.Params{DestinationID: 2}},
		{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{1}, Paths: []string{".hack SIGN (2002)", "Movie..Name (2001)", "a..b/c"}})},
		{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{1, 1}, Paths: manyPaths("P", jobs.MaxTargetPaths)})},
		// Duplicates and nested paths do not count against the limit.
		{Type: jobs.TypeSync, Params: sync(jobs.Params{SourceIDs: []int64{2}, Paths: append(manyPaths("Q", jobs.MaxTargetPaths), "Q 0001/Season 1", "Q 0002")})},
		{Type: jobs.TypeSync, DryRun: true, Params: sync(jobs.Params{ReleaseDemoted: true})},
		{Type: jobs.TypeSync, Params: sync(jobs.Params{ReleaseDemoted: true, AllowChanges: true, ReleaseOf: 9, ReleaseRevision: 3})},
	}
	for _, spec := range ok {
		if _, _, err := st.CreateJob(context.Background(), spec); err != nil {
			t.Errorf("CreateJob(%s %+v): %v", spec.Type, spec.Params, err)
		}
	}
}

func TestCanonicalTargets(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	j, _, err := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 1, SourceIDs: []int64{4},
		Paths: []string{"Show/Season 2", "Movie B", "Show", "Movie A", "Movie B", "Show/Season 1/E01.mkv", "Show 2", ".hack SIGN"}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".hack SIGN", "Movie A", "Movie B", "Show", "Show 2"}; !reflect.DeepEqual(j.Params.Paths, want) {
		t.Fatalf("paths = %q; want %q", j.Params.Paths, want)
	}
	r, _, err := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 3, ArrItemIDs: []int64{9, 2, 9, 5}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{2, 5, 9}; !reflect.DeepEqual(r.Params.ArrItemIDs, want) {
		t.Fatalf("arrItemIds = %v; want %v", r.Params.ArrItemIDs, want)
	}
	// The canonical form is what is stored, so an equal selection in another order is the same job.
	again, created, err := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 3, ArrItemIDs: []int64{5, 9, 2}}})
	if err != nil || created || again.ID != r.ID {
		t.Fatalf("same items in another order: %+v created %v, %v", again, created, err)
	}
	var raw string
	if err := st.db.Reader().QueryRowContext(ctx, `SELECT params FROM jobs WHERE id = ?`, j.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if want := `{"destinationId":1,"sourceIds":[4],"paths":[".hack SIGN","Movie A","Movie B","Show","Show 2"]}`; raw != want {
		t.Fatalf("stored params %s; want %s", raw, want)
	}
}

func TestLockKeysPhase2(t *testing.T) {
	cases := []struct {
		typ    jobs.Type
		params jobs.Params
		want   []string
	}{
		{jobs.TypeRefresh, jobs.Params{IntegrationID: 4}, []string{"integration:4"}},
		{jobs.TypeRefresh, jobs.Params{IntegrationID: 4, ArrItemIDs: []int64{1}, SyncAfter: true}, []string{"integration:4"}},
		{jobs.TypeArrBackup, jobs.Params{IntegrationID: 4, DestinationID: 2}, []string{"arrbackup:4"}},
		{jobs.TypeManifestExport, jobs.Params{DestinationID: 2}, []string{"manifest:2", ManifestBuildKey}},
		{jobs.TypeSync, jobs.Params{DestinationID: 2, SourceIDs: []int64{1}, Paths: []string{"x"}}, []string{"dest:2"}},
		{jobs.TypeRefresh, jobs.Params{}, nil},
		{jobs.TypeArrBackup, jobs.Params{}, nil},
		{jobs.TypeManifestExport, jobs.Params{}, nil},
	}
	for _, tc := range cases {
		if got := LockKeys(tc.typ, tc.params); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("LockKeys(%s, %+v) = %v; want %v", tc.typ, tc.params, got, tc.want)
		}
	}
}

func TestIntegrationIDColumnAndFilter(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	st := NewStore(d)
	refresh, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 5}})
	backup, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeArrBackup, Params: jobs.Params{IntegrationID: 5, DestinationID: 2}})
	plex, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypePlexDBBackup, Params: jobs.Params{IntegrationID: 6, DestinationID: 2}})
	sync, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 2}})
	for id, want := range map[int64]sql.NullInt64{refresh.ID: {Int64: 5, Valid: true}, backup.ID: {Int64: 5, Valid: true},
		plex.ID: {Int64: 6, Valid: true}, sync.ID: {}} {
		var got sql.NullInt64
		if err := d.Reader().QueryRowContext(ctx, `SELECT integration_id FROM jobs WHERE id = ?`, id).Scan(&got); err != nil || got != want {
			t.Errorf("job %d integration_id = %v, %v; want %v", id, got, err, want)
		}
	}
	page, err := st.ListJobs(ctx, JobQuery{IntegrationID: 5})
	if err != nil || page.TotalRecords != 2 || page.Records[0].ID != backup.ID || page.Records[1].ID != refresh.ID {
		t.Fatalf("ListJobs(integration 5) = %+v, %v", page, err)
	}
	page, _ = st.ListJobs(ctx, JobQuery{IntegrationID: 5, Type: jobs.TypeRefresh})
	if page.TotalRecords != 1 || page.Records[0].ID != refresh.ID {
		t.Fatalf("ListJobs(integration 5, refresh) = %+v", page)
	}
	page, _ = st.ListJobs(ctx, JobQuery{Type: jobs.TypeManifestExport})
	if page.TotalRecords != 0 {
		t.Fatalf("ListJobs(manifest_export) = %+v", page)
	}
	if active, err := st.ActiveForIntegration(ctx, 6); err != nil || !active {
		t.Fatalf("ActiveForIntegration(6) = %v, %v", active, err)
	}
	if _, err := st.cancelQueued(ctx, plex.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if active, err := st.ActiveForIntegration(ctx, 6); err != nil || active {
		t.Fatalf("ActiveForIntegration(6) after the cancel = %v, %v", active, err)
	}
	if active, _ := st.ActiveForDestination(ctx, 2); !active {
		t.Fatal("ActiveForDestination(2) = false with an arr_backup and a sync queued")
	}
}

func TestSchedulesOfTheNewTypes(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	seeded, _ := st.ListSchedules(ctx) // the migrations seed the global retention schedule
	for _, sc := range []struct {
		typ jobs.Type
		p   jobs.Params
	}{
		{jobs.TypeRefresh, jobs.Params{IntegrationID: 1}},
		{jobs.TypeArrBackup, jobs.Params{IntegrationID: 1, DestinationID: 2}},
		{jobs.TypeManifestExport, jobs.Params{DestinationID: 2}},
	} {
		if _, err := st.UpsertSchedule(ctx, sc.typ, sc.p, "15 */6 * * *", true); err != nil {
			t.Errorf("UpsertSchedule(%s): %v", sc.typ, err)
		}
	}
	// A schedule is a real run: a release needs its dry run, which a schedule cannot name.
	if _, err := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 2, ReleaseDemoted: true}, "0 2 * * *", true); !isValidation(err) {
		t.Fatalf("a release schedule: %v", err)
	}
	if _, err := st.UpsertSchedule(ctx, jobs.TypeRefresh, jobs.Params{IntegrationID: 1, SyncAfter: true}, "0 2 * * *", true); !isValidation(err) {
		t.Fatalf("an untargeted syncAfter schedule: %v", err)
	}
	if list, _ := st.ListSchedules(ctx); len(list) != len(seeded)+3 {
		t.Fatalf("schedules = %d; want %d", len(list), len(seeded)+3)
	}
}
