package plex_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
)

func listAll(t *testing.T, c *plex.Client, section string, typ, size int) ([]plex.Item, error) {
	t.Helper()
	var out []plex.Item
	err := c.AllItems(context.Background(), section, typ, plex.ListOptions{PageSize: size}, func(it plex.Item) error {
		out = append(out, it)
		return nil
	})
	return out, err
}

func TestAllItemsRecordedLibrary(t *testing.T) {
	srv := plextest.NewServer(t, "tok")
	srv.ServeLibrary(t)
	c := newClient(t, srv.URL, "tok", plex.Options{})
	counts := map[string]int{}
	for _, tt := range []struct {
		section string
		types   []int
	}{{"1", plex.TypesOf("movie")}, {"2", plex.TypesOf("show")}, {"3", plex.TypesOf("artist")}} {
		for _, typ := range tt.types {
			items, err := listAll(t, c, tt.section, typ, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, it := range items {
				counts[it.Type]++
			}
		}
	}
	want := map[string]int{"movie": 6, "show": 2, "season": 4, "episode": 10, "artist": 2, "album": 2, "track": 6}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("%s: %d items, want %d", k, counts[k], v)
		}
	}
	for _, r := range srv.Requests() {
		if strings.HasSuffix(r.Path, "/all") {
			if !strings.Contains(r.RawQuery, "includeGuids=1") || !strings.Contains(r.RawQuery, "X-Plex-Container-Size=500") {
				t.Errorf("listing query %q", r.RawQuery)
			}
			if r.Header.Get("X-Plex-Token") != "tok" || strings.Contains(r.RawQuery, "tok") {
				t.Errorf("token handling of %s", r.Path)
			}
		}
	}
}

func TestAllItemsFields(t *testing.T) {
	srv := plextest.NewServer(t, "tok")
	srv.ServeLibrary(t)
	c := newClient(t, srv.URL, "tok", plex.Options{})
	eps, err := listAll(t, c, "2", plex.TypeEpisode, 0)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(eps, func(it plex.Item) bool { return it.RatingKey == "19" })
	if i < 0 {
		t.Fatal("episode 19 missing")
	}
	e := eps[i]
	if e.Type != "episode" || e.ParentRatingKey != "18" || e.GrandparentRatingKey != "17" || e.Index == nil || *e.Index != 1 ||
		e.ParentIndex == nil || *e.ParentIndex != 1 || e.GUID != "plex://episode/5d9c13bf4eefaa001f652c9d" ||
		!slices.Equal(e.GUIDs, []string{"imdb://tt0508235", "tmdb://356608", "tvdb://121340"}) ||
		!slices.Equal(e.Files, []string{"/data/tv/Alfred Hitchcock Presents (1955)/Season 01/Alfred Hitchcock Presents (1955) - S01E01.mkv"}) ||
		e.AddedAt == nil || !e.AddedAt.Equal(time.Unix(1790411047, 0)) {
		t.Fatalf("episode = %+v", e)
	}
	seasons, err := listAll(t, c, "2", plex.TypeSeason, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range seasons {
		if s.Index == nil || s.ParentRatingKey == "" || len(s.Files) != 0 {
			t.Errorf("season = %+v", s)
		}
	}
	movies, err := listAll(t, c, "1", plex.TypeMovie, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range movies {
		if len(m.Files) != 1 || m.ParentRatingKey != "" || m.Index != nil {
			t.Errorf("movie = %+v", m)
		}
	}
}

// TestAllItemsPaging reads the episodes 4 at a time; the fake's pages equal the recorded paging
// samples, and the client advances by the rows returned.
func TestAllItemsPaging(t *testing.T) {
	srv := plextest.NewServer(t, "tok")
	lib := srv.ServeLibrary(t)
	c := newClient(t, srv.URL, "tok", plex.Options{})
	for _, start := range []int{0, 4, 8} {
		var rec struct {
			MediaContainer struct {
				Metadata []json.RawMessage `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		b, err := os.ReadFile(filepath.Join(plextest.LibraryDir(), "library-sections-2-all-type-4-start-"+itoa(start)+"-size-4.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Fatal(err)
		}
		all := lib.Rows("2", plex.TypeEpisode)
		page := all[start:min(start+4, len(all))]
		if len(page) != len(rec.MediaContainer.Metadata) {
			t.Fatalf("page at %d: %d rows, recorded %d", start, len(page), len(rec.MediaContainer.Metadata))
		}
		for i := range page {
			if !jsonEqual(page[i], rec.MediaContainer.Metadata[i]) {
				t.Fatalf("page at %d row %d differs from the recording", start, i)
			}
		}
	}
	eps, err := listAll(t, c, "2", plex.TypeEpisode, 4)
	if err != nil || len(eps) != 10 {
		t.Fatalf("%d episodes, %v", len(eps), err)
	}
	pages := 0
	for _, r := range srv.Requests() {
		if strings.HasSuffix(r.Path, "/2/all") {
			pages++
		}
	}
	if pages != 3 {
		t.Fatalf("%d pages, want 3", pages)
	}
	// Plex returning fewer rows than asked: the client advances by what it got.
	all := lib.Rows("2", plex.TypeEpisode)
	lib.SetListing("2", plex.TypeEpisode, all[:3])
	short, err := listAll(t, c, "2", plex.TypeEpisode, 4)
	if err != nil || len(short) != 3 {
		t.Fatalf("%d episodes, %v", len(short), err)
	}
}

func TestAllItemsIncomplete(t *testing.T) {
	srv := plextest.NewServer(t, "tok")
	srv.ServeLibrary(t)
	srv.Handle("/library/sections/1/all", func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("X-Plex-Container-Start")
		w.Header().Set("Content-Type", "application/json")
		if start == "0" {
			_, _ = w.Write([]byte(`{"MediaContainer":{"size":1,"totalSize":6,"Metadata":[{"ratingKey":"9","type":"movie"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"size":0,"totalSize":6,"Metadata":[]}}`))
	})
	c := newClient(t, srv.URL, "tok", plex.Options{})
	if _, err := listAll(t, c, "1", plex.TypeMovie, 0); !errors.Is(err, plex.ErrIncomplete) {
		t.Fatalf("error = %v, want ErrIncomplete", err)
	}
	if _, err := listAll(t, c, "1/../2", plex.TypeMovie, 0); err == nil {
		t.Fatal("an invalid section key was accepted")
	}
}

// A section key comes from the server's /library/sections answer; a rejected one is never copied
// into the error text (S16), and the error is the client's *Error with a templated path.
func TestAllItemsInvalidKeyNotEchoed(t *testing.T) {
	srv := plextest.NewServer(t, "tok")
	c := newClient(t, srv.URL, "tok", plex.Options{})
	const key = "9/../SECRET-MARKER\n\x1b[31m"
	_, err := listAll(t, c, key, plex.TypeMovie, 0)
	if err == nil {
		t.Fatal("an invalid section key was accepted")
	}
	if strings.Contains(err.Error(), "SECRET-MARKER") || strings.Contains(err.Error(), "..") {
		t.Fatalf("error echoes the server-supplied key: %q", err)
	}
	var pe *plex.Error
	if !errors.As(err, &pe) || pe.Method != http.MethodGet || pe.Path != "/library/sections/{key}/all" {
		t.Fatalf("error = %#v, want *plex.Error GET /library/sections/{key}/all", err)
	}
	if !errors.Is(err, plex.ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
}

func TestTypesOf(t *testing.T) {
	if !slices.Equal(plex.TypesOf("show"), []int{2, 3, 4}) || !slices.Equal(plex.TypesOf("artist"), []int{8, 9, 10}) ||
		!slices.Equal(plex.TypesOf("movie"), []int{1}) || plex.TypesOf("photo") != nil {
		t.Fatal("TypesOf")
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(ax) == string(by)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
