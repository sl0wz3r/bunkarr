package mediaindex

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Lidarr's index (design D2, D8). Lidarr sends no webhook for a ManualImport without "replace
// existing files" and none for a deleted track file, and replaces an album without saying so: the
// full refresh is what notices these, by comparing each artist's track files with the index, and
// it queues a targeted sync of the artist's folder.

// lidarrArtistFolder is the recorded artist's folder relative to the source (the *arr's /music).
const lidarrArtistFolder = "Scott Joplin"

// editTracks serves artist 1's track files after fn.
func (e *testEnv) editTracks(fn func([]map[string]any) []map[string]any) {
	e.t.Helper()
	e.editList("trackfile?artistId=1", "trackfile-artistId-1.json", fn)
}

// lidarrTrack is a track file of artist 1 as Lidarr lists it.
func lidarrTrack(id, albumID int64, p string, size int64) map[string]any {
	return map[string]any{"id": id, "artistId": 1, "albumId": albumID, "path": p, "size": size, "dateAdded": "2026-09-25T13:00:00Z",
		"quality": map[string]any{"quality": map[string]any{"id": 6, "name": "FLAC"}, "revision": map[string]any{"version": 1}}}
}

func TestLidarrReconcileWithoutWebhooks(t *testing.T) {
	entertainer := "/music/Scott Joplin/The Entertainer (1996)"
	cases := []struct {
		name string
		// change edits the track file list (and writes new files); nil: nothing changed.
		change func(e *testEnv, list []map[string]any) []map[string]any
		// files is the index's file count after the refresh; deleted the files it removed;
		// unscanned the listed files the catalog does not know yet (the targeted sync scans them).
		files, deleted, unscanned int
		sync                      bool
	}{
		{"manual import without a webhook", func(e *testEnv, list []map[string]any) []map[string]any {
			for i, name := range []string{"01 - Maple Leaf Rag.flac", "02 - The Entertainer.flac"} {
				p := entertainer + "/Scott Joplin - The Entertainer - " + name
				e.writeFile(p, int64(40_000+i))
				list = append(list, lidarrTrack(int64(30+i), 1, p, int64(40_000+i)))
			}
			return list
		}, 21, 0, 2, true},
		{"track file deleted (no event exists)", func(_ *testEnv, list []map[string]any) []map[string]any {
			return slices.DeleteFunc(list, func(m map[string]any) bool { return m["id"] == 5.0 })
		}, 18, 1, 0, true},
		{"album replaced (FLAC over MP3, new ids and names)", func(e *testEnv, list []map[string]any) []map[string]any {
			out := list[:0]
			for _, m := range list {
				if m["albumId"] != 13.0 {
					out = append(out, m)
					continue
				}
				p := strings.TrimSuffix(m["path"].(string), ".mp3") + ".flac"
				size := int64(m["size"].(float64)) * 5
				e.writeFile(p, size)
				out = append(out, lidarrTrack(int64(m["id"].(float64))+100, 13, p, size))
			}
			return out
		}, 19, 9, 9, true},
		{"tracks renamed (same ids, new paths)", func(e *testEnv, list []map[string]any) []map[string]any {
			for _, m := range list {
				if m["albumId"] == 46.0 {
					p := strings.Replace(m["path"].(string), "/Ragtime (1994)/", "/Ragtime/", 1)
					e.writeFile(p, int64(m["size"].(float64)))
					m["path"] = p
				}
			}
			return list
		}, 19, 0, 10, true},
		// A retag changes the content at the same path and id: the index cannot see it (the Retag
		// webhook and the scheduled syncs can).
		{"retagged in place", nil, 19, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, arr.KindLidarr)
			first, _ := e.full()
			if first.ChangedItems != 0 || len(e.enq.queued()) != 0 {
				t.Fatalf("the first complete refresh reconciled: %+v, %v", first, e.enq.queued())
			}
			if tc.change != nil {
				e.editTracks(func(list []map[string]any) []map[string]any { return tc.change(e, list) })
			}
			e.clock.Advance(time.Hour)
			st, rep := e.full()
			if len(e.files()) != tc.files || st.FilesDeleted != int64(tc.deleted) || st.GuardHeld {
				t.Fatalf("files %d, stats %+v (%s)", len(e.files()), st, rep)
			}
			var want []string
			if tc.sync {
				want = []string{"1/[" + strconv.FormatInt(e.src.ID, 10) + "]:" + lidarrArtistFolder}
			}
			if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
				t.Fatalf("syncs = %v, want %v", got, want)
			}
			if tc.sync {
				if q := e.enq.queued()[0]; q.Trigger != jobs.TriggerSchedule || st.ChangedItems != 1 || !slices.Equal(st.FollowUpJobs, []int64{q.ID}) {
					t.Fatalf("sync %+v, stats %+v", q, st)
				}
			}
			if st.FilesMismatched != int64(tc.unscanned) {
				t.Fatalf("%d files match no catalog file, want %d", st.FilesMismatched, tc.unscanned)
			}
		})
	}
}

// TestLidarrTargetedRefreshAfterDeletes: the refresh of a webhook's artist after an ArtistDelete
// (GET artist/{id} answers 404 while system/status still names Lidarr) marks the artist deleted
// and syncs its old folder; after an AlbumDelete (the artist is still there, the album and its
// track files are gone) it removes those files from the index and syncs the artist's folder.
func TestLidarrTargetedRefreshAfterDeletes(t *testing.T) {
	t.Run("ArtistDelete", func(t *testing.T) {
		e := newEnv(t, arr.KindLidarr)
		e.full()
		e.srv.SetStatus(http.MethodGet, "artist/1", http.StatusNotFound)
		res, rep, err := e.refresh(jobs.Params{ArrItemIDs: []int64{1}, SyncAfter: true}, false, jobs.TriggerWebhook)
		if err != nil {
			t.Fatalf("%v\n%s", err, rep)
		}
		st := res.Stats.(Stats)
		if st.ItemsDeleted != 1 || st.FilesDeleted != 19 || e.items()[1].DeletedAt == nil || len(e.files()) != 0 {
			t.Fatalf("stats %+v, files %d", st, len(e.files()))
		}
		want := []string{fmt.Sprintf("1/[%d]:%s", e.src.ID, lidarrArtistFolder)}
		if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) || e.enq.queued()[0].Trigger != jobs.TriggerWebhook {
			t.Fatalf("syncs = %v, want %v", got, want)
		}
	})
	t.Run("AlbumDelete", func(t *testing.T) {
		e := newEnv(t, arr.KindLidarr)
		e.full()
		e.editTracks(func(list []map[string]any) []map[string]any {
			return slices.DeleteFunc(list, func(m map[string]any) bool { return m["albumId"] == 46.0 })
		})
		e.editList("album?artistId=1", "album-artistId-1.json", func(list []map[string]any) []map[string]any {
			return slices.DeleteFunc(list, func(m map[string]any) bool { return m["id"] == 46.0 })
		})
		res, rep, err := e.refresh(jobs.Params{ArrItemIDs: []int64{1}, SyncAfter: true}, false, jobs.TriggerWebhook)
		if err != nil {
			t.Fatalf("%v\n%s", err, rep)
		}
		st := res.Stats.(Stats)
		a := e.items()[1]
		if st.ItemsDeleted != 0 || st.ItemsUpdated != 1 || st.FilesDeleted != 10 || a.DeletedAt != nil || len(a.Detail.Albums) != 3 || len(e.files()) != 9 {
			t.Fatalf("stats %+v, artist %+v, files %d", st, a, len(e.files()))
		}
		for _, f := range e.files() {
			if f.Detail.Album == nil || f.Detail.Album.ID != 13 {
				t.Fatalf("file %+v survived its album", f)
			}
		}
		want := []string{fmt.Sprintf("1/[%d]:%s", e.src.ID, lidarrArtistFolder)}
		if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
			t.Fatalf("syncs = %v, want %v", got, want)
		}
	})
	t.Run("an artist without track files is not checked on disk", func(t *testing.T) {
		e := newEnv(t, arr.KindLidarr)
		e.full()
		var a map[string]any
		if err := json.Unmarshal(arrtest.Fixture(t, arr.KindLidarr, "artist-1.json"), &a); err != nil {
			t.Fatal(err)
		}
		a["path"] = "/music/Nobody Yet"
		a["statistics"] = map[string]any{"albumCount": 52, "trackFileCount": 0, "sizeOnDisk": 0}
		b, _ := json.Marshal(a)
		e.srv.SetJSON(http.MethodGet, "artist/1", b)
		e.srv.SetJSON(http.MethodGet, "trackfile?artistId=1", []byte(`[]`))
		res, rep, err := e.refresh(jobs.Params{ArrItemIDs: []int64{1}, SyncAfter: true}, false, jobs.TriggerWebhook)
		if err != nil {
			t.Fatalf("%v\n%s", err, rep)
		}
		// The new folder does not exist yet and needs no check (no files); the old one is synced
		// (the targeted sync decides what is gone).
		if res.Warnings != 0 || rep.warned("does not exist") {
			t.Fatalf("warnings %v", rep.warns)
		}
		want := []string{"1/[" + strconv.FormatInt(e.src.ID, 10) + "]:Nobody Yet," + lidarrArtistFolder}
		if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
			t.Fatalf("syncs = %v, want %v", got, want)
		}
	})
}

// TestLidarrRefreshIsAllOrNothing: a per-artist request that fails fails the whole full refresh
// before anything is removed, and an unreadable music share keeps every indexed track file.
func TestLidarrRefreshIsAllOrNothing(t *testing.T) {
	t.Run("a failed per-artist request", func(t *testing.T) {
		e := newEnv(t, arr.KindLidarr)
		e.full()
		before := *e.state().RefreshedAt
		e.srv.SetStatus(http.MethodGet, "album?artistId=1", http.StatusInternalServerError)
		e.srv.SetJSON(http.MethodGet, "trackfile?artistId=1", []byte(`[]`))
		e.clock.Advance(time.Hour)
		if _, _, err := e.refresh(jobs.Params{}, false, jobs.TriggerSchedule); err == nil {
			t.Fatal("the refresh succeeded")
		}
		s := e.state()
		if len(e.files()) != 19 || !s.RefreshedAt.Equal(before) || s.Status != StatusFailed || len(e.enq.queued()) != 0 {
			t.Fatalf("files %d, state %+v, queued %v", len(e.files()), s, e.enq.queued())
		}
	})
	t.Run("the music root folder is not accessible", func(t *testing.T) {
		e := newEnv(t, arr.KindLidarr)
		e.full()
		e.srv.SetRootFolderAccessible("/music", false)
		e.srv.SetJSON(http.MethodGet, "trackfile?artistId=1", []byte(`[]`))
		e.clock.Advance(time.Hour)
		st, rep := e.full()
		if st.FilesDeleted != 0 || len(e.files()) != 19 || !slices.Equal(st.InaccessibleRootFolders, []string{"/music"}) || !rep.warned("not accessible") {
			t.Fatalf("stats %+v, files %d, warnings %v", st, len(e.files()), rep.warns)
		}
	})
}

// TestLidarrMetadata: Lidarr's metadata profiles and named root folder reach the index's
// metadata, and each artist names its metadata profile.
func TestLidarrMetadata(t *testing.T) {
	e := newEnv(t, arr.KindLidarr)
	e.full()
	m, err := e.runner.Store().Meta(context.Background(), nil, e.it.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := map[int64]string{}
	for _, p := range m.MetadataProfiles {
		names[p.ID] = p.Name
	}
	if len(names) != 2 || names[1] != "Standard" || names[2] != "None" || len(m.RootFolders) != 1 || m.RootFolders[0].Path != "/music" ||
		!m.RootFolders[0].Accessible || m.RootFolders[0].SourceID == nil || *m.RootFolders[0].SourceID != e.src.ID ||
		len(m.Tags) != 2 || len(m.QualityProfiles) != 3 {
		t.Fatalf("meta = %+v", m)
	}
	a := e.items()[1]
	if names[a.MetadataProfileID] != "Standard" || a.RootFolder != "/music" || a.Path != "/music/Scott Joplin" || !slices.Equal(a.Tags, []int64{1}) {
		t.Fatalf("artist %+v", a)
	}
	// Lidarr lists no relative path: it is the path inside the artist's folder.
	for _, f := range e.files() {
		if !strings.HasPrefix(f.Path, a.Path+"/"+f.Detail.RelativePath) || strings.Count(f.Detail.RelativePath, "/") != 1 {
			t.Fatalf("file %+v", f)
		}
	}
}

// lidarrArtist is a Lidarr artist of the fake with n track files in one album, written to disk.
func (e *testEnv) lidarrArtist(id int64, name string, n int) (artist map[string]any, tracks []map[string]any) {
	e.t.Helper()
	folder := "/music/" + name
	artist = map[string]any{"id": id, "artistName": name, "foreignArtistId": fmt.Sprintf("00000000-0000-0000-0000-%012d", id),
		"path": folder, "rootFolderPath": "/music", "qualityProfileId": 1, "metadataProfileId": 1, "monitored": true,
		"monitorNewItems": "all", "tags": []int64{}, "statistics": map[string]any{"albumCount": 1, "trackFileCount": n}}
	for i := range n {
		p := fmt.Sprintf("%s/Album (2001)/%02d - Track.flac", folder, i+1)
		e.writeFile(p, int64(1000*id+int64(i)))
		tracks = append(tracks, lidarrTrack(1000*id+int64(i), 500+id, p, int64(1000*id+int64(i))))
	}
	return artist, tracks
}

// serveLidarr makes the fake Lidarr list the recorded artist 1 plus the given artists, each with
// its track files and one album (artist 1: as recorded).
func (e *testEnv) serveLidarr(extra map[int64][2]any) {
	e.t.Helper()
	e.editList("artist", "artist.json", func(list []map[string]any) []map[string]any {
		for _, id := range slices.Sorted(maps.Keys(extra)) {
			list = append(list, extra[id][0].(map[string]any))
		}
		return list
	})
	for id, a := range extra {
		b, _ := json.Marshal(a[0])
		e.srv.SetJSON(http.MethodGet, "artist/"+strconv.FormatInt(id, 10), b)
		b, _ = json.Marshal(a[1])
		e.srv.SetJSON(http.MethodGet, "trackfile?artistId="+strconv.FormatInt(id, 10), b)
		b, _ = json.Marshal([]map[string]any{{"id": 500 + id, "artistId": id, "title": "Album", "foreignAlbumId": fmt.Sprintf("album-%d", id), "monitored": true}})
		e.srv.SetJSON(http.MethodGet, "album?artistId="+strconv.FormatInt(id, 10), b)
	}
}

// changeLidarr is the crash matrix's change, made without a webhook: artist 2 deleted, artist 3
// added with files, and artist 1's Ragtime album replaced by new track files.
func changeLidarr(e *testEnv) {
	e.t.Helper()
	a3, t3 := e.lidarrArtist(3, "Erik Satie", 2)
	e.serveLidarr(map[int64][2]any{3: {a3, t3}})
	e.srv.SetStatus(http.MethodGet, "artist/2", http.StatusNotFound)
	e.editTracks(func(list []map[string]any) []map[string]any {
		for _, m := range list {
			if m["albumId"] == 46.0 {
				p := strings.TrimSuffix(m["path"].(string), ".flac") + " (remaster).flac"
				e.writeFile(p, 77)
				m["id"], m["path"], m["size"] = m["id"].(float64)+200, p, 77
			}
		}
		return list
	})
}

// TestCrashMatrixRefreshLidarr is the refresh crash matrix for Lidarr, whose artists are listed
// first and then read artist by artist: a crash at every fault point of a reconciling full refresh
// (batches of one artist), then a resume, ends with the index and follow-up syncs of a clean run,
// and nothing is removed before the complete fetch.
func TestCrashMatrixRefreshLidarr(t *testing.T) {
	setup := func(t *testing.T) *testEnv {
		e := newEnv(t, arr.KindLidarr)
		a2, t2 := e.lidarrArtist(2, "Claude Debussy", 3)
		e.serveLidarr(map[int64][2]any{2: {a2, t2}})
		e.scan()
		e.full()
		changeLidarr(e)
		return e
	}
	clean := setup(t)
	clean.full()
	want := snapshot(clean)
	wantSyncs := []string{fmt.Sprintf("1/[%d]:Claude Debussy,Erik Satie,%s", clean.src.ID, lidarrArtistFolder)}
	if got := syncTargets(clean.enq.queued()); !reflect.DeepEqual(got, wantSyncs) || clean.items()[2].DeletedAt == nil || len(want.Files) != 21 {
		t.Fatalf("clean run: syncs %v, index %v", got, want)
	}
	for _, c := range []struct {
		point string
		n     int
	}{
		{PointAfterIntents, 1}, {PointAfterIntents, 2}, {PointAfterBatch, 1}, {PointAfterBatch, 2},
		{PointBeforeMarkDeleted, 1}, {PointAfterFollowUps, 1},
	} {
		t.Run(c.point+"#"+strconv.Itoa(c.n), func(t *testing.T) {
			e := setup(t)
			first := *e.state().RefreshedAt
			r := e.newRunner(1)
			job := jobs.Job{ID: 70, Type: jobs.TypeRefresh, Trigger: jobs.TriggerSchedule, Attempt: 1, Params: jobs.Params{IntegrationID: e.it.ID}}
			items := &memItems{}
			if crashed, err := crashRun(t, e, r, job, items, faultinject.CrashAt(c.point, c.n)); !crashed {
				t.Fatalf("did not reach %s #%d (err %v)", c.point, c.n, err)
			}
			if c.point != PointAfterFollowUps && (e.items()[2].DeletedAt != nil || !e.state().RefreshedAt.Equal(first)) {
				t.Fatal("a removal or refreshed_at happened before the complete fetch")
			}
			e.clock.Advance(1)
			job.Attempt, job.Trigger = 2, jobs.TriggerResume
			if _, rep, err := e.runJob(r, job, items); err != nil {
				t.Fatalf("resume: %v\n%s", err, rep)
			}
			if got := snapshot(e); !reflect.DeepEqual(got, want) {
				t.Fatalf("index after resume =\n%v\nwant\n%v", got, want)
			}
			if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, wantSyncs) {
				t.Fatalf("syncs = %v, want %v", got, wantSyncs)
			}
			for _, it := range items.all() {
				if it.Status != jobs.ItemDone {
					t.Fatalf("intent left %s: %+v", it.Status, it)
				}
			}
		})
	}
}
