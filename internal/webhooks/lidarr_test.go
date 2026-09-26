package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
)

// Lidarr's webhooks (design D2): the quirks the fixture spike found in Lidarr 3.1.0 must not
// matter, because Bunkarr's safety rests on scanning, not on events:
//   - a Download that replaces existing files has isUpgrade=false and no deletedFiles (and no
//     delete event comes first);
//   - a ManualImport without "replace existing files" sends no webhook at all, and there is no
//     track-file-delete event (the full refresh reconciles both, internal/mediaindex);
//   - an ArtistDelete is followed by one AlbumDelete per album.

// lidarrHarness adds a Lidarr integration to the processor harness.
func lidarrHarness(t *testing.T) (*procHarness, int64) {
	t.Helper()
	h := newProcHarness(t)
	return h, addIntegration(t, h.db, integrations.TypeLidarr, "Lidarr", true)
}

// withDeletedFiles returns a recorded Lidarr body with deletedFiles set to on.
func withDeletedFiles(t *testing.T, name string, on bool) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(fixture(t, integrations.TypeLidarr, name), &body); err != nil {
		t.Fatal(err)
	}
	body["deletedFiles"] = on
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestLidarrArtistDeleteBurstIsOneRefresh: an ArtistDelete and the AlbumDelete Lidarr sends for
// each of the artist's albums (51 in the spike) name the same artist, so they coalesce into one
// refresh of it: 5 s after the last event without deleted files, 60 s after it with them.
func TestLidarrArtistDeleteBurstIsOneRefresh(t *testing.T) {
	for _, tc := range []struct {
		name         string
		deletedFiles bool
		class        Class
		due          time.Duration // after the last event
	}{
		{"files kept", false, ClassChange, DefaultQuiet},
		{"files deleted", true, ClassDelete, DefaultDeleteDelay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, lidarr := lidarrHarness(t)
			var events []Record
			events = append(events, h.receive(integrations.TypeLidarr, lidarr, withDeletedFiles(t, "ArtistDelete.json", tc.deletedFiles)))
			const albums = 51
			step := 20 * time.Millisecond
			for i := range albums {
				h.at(time.Duration(i+1) * step)
				events = append(events, h.receive(integrations.TypeLidarr, lidarr, withDeletedFiles(t, "AlbumDelete.json", tc.deletedFiles)))
			}
			last := albums * step
			for _, ev := range events {
				if ev.Class != tc.class || !slices.Equal(ev.Targets, []int64{1}) {
					t.Fatalf("event %s: class %s targets %v, want %s [1]", ev.EventType, ev.Class, ev.Targets, tc.class)
				}
			}
			// The cap of the quiet window (20 s after the first event) never cuts the burst short:
			// it lasts about a second.
			h.at(last + tc.due - time.Millisecond)
			if n := len(h.refreshes()); n != 0 {
				t.Fatalf("flushed %d refreshes before the burst's window ended", n)
			}
			h.at(last + tc.due)
			rs := h.refreshes()
			if len(rs) != 1 || !slices.Equal(rs[0].Params.ArrItemIDs, []int64{1}) || !rs[0].Params.SyncAfter ||
				rs[0].Params.IntegrationID != lidarr {
				t.Fatalf("refreshes = %+v", rs)
			}
			for _, ev := range events {
				if e := h.event(ev.ID); outcomeOf(e) != OutcomeQueued || e.JobID == nil || *e.JobID != rs[0].ID {
					t.Fatalf("event %d = %+v", ev.ID, e)
				}
			}
			h.at(last + tc.due + time.Hour)
			if n := len(h.refreshes()); n != 1 || h.proc.Backlog() != 0 {
				t.Fatalf("after the flush: %d refreshes, backlog %d", n, h.proc.Backlog())
			}
		})
	}
}

// TestLidarrReplaceExistingIsAPlainDownload: Lidarr replaces an album's files by deleting them
// first and then importing, and says neither (isUpgrade false, no deletedFiles, no delete event).
// The Download alone is refreshed 5 s later, like any import: the targeted sync then finds the
// new files and the vanished ones in the same folder by scanning (D14 lets it retain those).
func TestLidarrReplaceExistingIsAPlainDownload(t *testing.T) {
	h, lidarr := lidarrHarness(t)
	body := fixture(t, integrations.TypeLidarr, "Download-replaceexisting.json")
	for _, key := range []string{`"deletedFiles"`, `"deleteReason"`} {
		if bytes.Contains(body, []byte(key)) {
			t.Fatalf("the recorded replacement has %s: the fixture is not the spike's", key)
		}
	}
	ev := h.receive(integrations.TypeLidarr, lidarr, string(body))
	if ev.Class != ClassDownload || !slices.Equal(ev.Targets, []int64{1}) || len(ev.Summary.Files) != 10 || ev.Summary.Title != "Scott Joplin" {
		t.Fatalf("event = %+v", ev)
	}
	for _, f := range ev.Summary.Files {
		if !strings.HasPrefix(f, "/music/Scott Joplin/") || !strings.HasSuffix(f, ".flac") {
			t.Fatalf("summary file %q", f)
		}
	}
	h.at(DefaultQuiet - time.Millisecond)
	if len(h.refreshes()) != 0 {
		t.Fatal("flushed before the quiet window")
	}
	h.at(DefaultQuiet)
	rs := h.refreshes()
	if len(rs) != 1 || !slices.Equal(rs[0].Params.ArrItemIDs, []int64{1}) || !rs[0].Params.SyncAfter {
		t.Fatalf("refreshes = %+v", rs)
	}
}

// TestLidarrClassifiesEvents: Lidarr's event types in the table of design §7.2 are refreshed with
// their class; everything else Lidarr can send (failures, grabs, health, updates, and event types
// of the other apps) is stored as ignored with an empty payload and refreshes nothing.
func TestLidarrClassifiesEvents(t *testing.T) {
	cases := []struct {
		body  string
		class Class
		ids   []int64
	}{
		{`{"eventType":"Download","isUpgrade":true,"artist":{"id":3,"name":"A"},"trackFiles":[{"path":"/music/A/1.flac"}]}`, ClassDownload, []int64{3}},
		{`{"eventType":"Rename","artist":{"id":3},"renamedTrackFiles":[{"previousPath":"/music/A/x.flac","path":"/music/A/y.flac"}]}`, ClassChange, []int64{3}},
		{`{"eventType":"Retag","artist":{"id":3},"trackFile":{"path":"/music/A/y.flac"}}`, ClassChange, []int64{3}},
		{`{"eventType":"ArtistAdd","artist":{"id":4}}`, ClassChange, []int64{4}},
		{`{"eventType":"AlbumDelete","deletedFiles":false,"artist":{"id":3},"album":{"id":9}}`, ClassChange, []int64{3}},
		{`{"eventType":"AlbumDelete","deletedFiles":true,"artist":{"id":3},"album":{"id":9}}`, ClassDelete, []int64{3}},
		{`{"eventType":"ArtistDelete","deletedFiles":true,"artist":{"id":3}}`, ClassDelete, []int64{3}},
		// No deleteReason drives a Lidarr class: there is no track-file-delete event to hold.
		{`{"eventType":"AlbumDelete","deleteReason":"upgrade","artist":{"id":3}}`, ClassChange, []int64{3}},
		{`{"eventType":"Test","artist":{"id":1},"albums":[{"id":1}]}`, ClassTest, nil},
		{`{"eventType":"Grab","artist":{"id":3}}`, ClassIgnored, nil},
		{`{"eventType":"DownloadFailure","artist":{"id":3}}`, ClassIgnored, nil},
		{`{"eventType":"ImportFailure","artist":{"id":3},"trackFiles":[{"path":"/music/A/1.flac"}]}`, ClassIgnored, nil},
		{`{"eventType":"TrackFileDelete","artist":{"id":3},"trackFile":{"path":"/music/A/1.flac"}}`, ClassIgnored, nil},
		{`{"eventType":"ApplicationUpdate","previousVersion":"3.0.0","newVersion":"3.1.0"}`, ClassIgnored, nil},
		{`{"eventType":"HealthRestored","message":"ok"}`, ClassIgnored, nil},
		// The other apps' events on a Lidarr route.
		{`{"eventType":"MovieDelete","deletedFiles":true,"movie":{"id":3},"artist":{"id":3}}`, ClassIgnored, nil},
		{`{"eventType":"SeriesAdd","series":{"id":3},"artist":{"id":3}}`, ClassIgnored, nil},
		{`{"eventType":"EpisodeFileDelete","deleteReason":"upgrade","artist":{"id":3}}`, ClassIgnored, nil},
	}
	for _, tc := range cases {
		ev, err := Parse(integrations.TypeLidarr, strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if ev.Class != tc.class || !slices.Equal(ev.Targets, tc.ids) {
			t.Errorf("%s: class %s targets %v, want %s %v", tc.body, ev.Class, ev.Targets, tc.class, tc.ids)
		}
		if ev.Class == ClassIgnored && string(ev.Payload) != "{}" {
			t.Errorf("%s: an ignored event stored %s", tc.body, ev.Payload)
		}
	}
	// Lidarr's events on the other apps' routes name no movie or series: ignored, or nothing to
	// refresh.
	for _, app := range []integrations.Type{integrations.TypeRadarr, integrations.TypeSonarr} {
		for _, name := range []string{"ArtistAdd.json", "AlbumDelete.json", "Retag.json"} {
			ev, err := Parse(app, bytes.NewReader(fixture(t, integrations.TypeLidarr, name)))
			if err != nil || ev.Class != ClassIgnored || len(ev.Targets) != 0 {
				t.Errorf("lidarr/%s on a %s route: %+v, %v", name, app, ev, err)
			}
		}
		ev, err := Parse(app, bytes.NewReader(fixture(t, integrations.TypeLidarr, "Download.json")))
		if err != nil || len(ev.Targets) != 0 {
			t.Errorf("lidarr/Download.json on a %s route: %+v, %v", app, ev, err)
		}
	}
}

// TestLidarrIntake: the recorded Test events of every auth form Lidarr offers are answered 200,
// stored as test events without a lookup, and a Download is stored with its artist for the
// processor. (The credential itself is checked by internal/api before Serve.)
func TestLidarrIntake(t *testing.T) {
	h, lidarr := lidarrHarness(t)
	in := NewIntake(IntakeOptions{Store: h.store, Processor: h.proc, Now: func() time.Time { return h.now }})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in.Serve(w, r, lidarr, integrations.TypeLidarr)
	}))
	t.Cleanup(srv.Close)
	post := func(body []byte) int {
		t.Helper()
		res, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	for _, name := range []string{"Test.json", "Test-basic-auth.json", "Test-header-auth.json", "Test-query-auth.json"} {
		if code := post(fixture(t, integrations.TypeLidarr, name)); code != http.StatusOK {
			t.Fatalf("%s = %d", name, code)
		}
	}
	if h.proc.Backlog() != 0 {
		t.Fatalf("Test events are in the backlog: %d", h.proc.Backlog())
	}
	page, err := h.store.List(h.ctx, Query{IntegrationID: lidarr, Outcome: "test"})
	if err != nil || page.TotalRecords != 4 {
		t.Fatalf("test events = %+v, %v", page, err)
	}
	for _, r := range page.Records {
		if r.Source != integrations.TypeLidarr || r.ProcessedAt == nil || len(r.Targets) != 0 {
			t.Fatalf("test event = %+v", r)
		}
	}
	if code := post(fixture(t, integrations.TypeLidarr, "Download.json")); code != http.StatusOK || h.proc.Backlog() != 1 {
		t.Fatalf("Download = %d, backlog %d", code, h.proc.Backlog())
	}
	h.at(DefaultQuiet)
	if rs := h.refreshes(); len(rs) != 1 || rs[0].Params.IntegrationID != lidarr || !slices.Equal(rs[0].Params.ArrItemIDs, []int64{1}) {
		t.Fatalf("refreshes = %+v", rs)
	}
}

// TestLidarrBurstCrashResumesIntoTheSameJob: a crash between the enqueue of the artist's refresh
// and the write that marks the ArtistDelete and AlbumDelete events leaves all of them
// unprocessed; after the restart they are merged into the refresh already queued.
func TestLidarrBurstCrashResumesIntoTheSameJob(t *testing.T) {
	h, lidarr := lidarrHarness(t)
	ids := []int64{h.receive(integrations.TypeLidarr, lidarr, string(fixture(t, integrations.TypeLidarr, "ArtistDelete.json"))).ID}
	for range 51 {
		ids = append(ids, h.receive(integrations.TypeLidarr, lidarr, string(fixture(t, integrations.TypeLidarr, "AlbumDelete.json"))).ID)
	}
	func() {
		faultinject.SetHook(faultinject.CrashAt(PointAfterEnqueue, 1))
		defer faultinject.SetHook(nil)
		defer func() {
			if p := recover(); p != nil {
				if _, ok := p.(faultinject.Crash); !ok {
					panic(p)
				}
			}
		}()
		h.at(DefaultQuiet)
		t.Fatal("did not crash")
	}()
	before := h.refreshes()
	if len(before) != 1 || outcomeOf(h.event(ids[0])) != "" {
		t.Fatalf("after the crash: refreshes %+v, event %+v", before, h.event(ids[0]))
	}
	h.proc = h.newProcessor()
	if err := h.proc.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.proc.Stop(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for outcomeOf(h.event(ids[len(ids)-1])) == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for _, id := range ids {
		if e := h.event(id); outcomeOf(e) != OutcomeCoalesced || e.JobID == nil || *e.JobID != before[0].ID {
			t.Fatalf("event %d after the restart = %+v", id, e)
		}
	}
	if rs := h.refreshes(); len(rs) != 1 || !slices.Equal(rs[0].Params.ArrItemIDs, []int64{1}) {
		t.Fatalf("refreshes = %+v", rs)
	}
}
