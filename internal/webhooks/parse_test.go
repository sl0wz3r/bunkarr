package webhooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
)

// fixtureDir is the recorded payloads of the fixture spike (Sonarr 4.0.20, Radarr 6.4.4,
// Lidarr 3.1.0).
var fixtureDir = filepath.Join("..", "..", "testdata", "webhooks")

// fixture returns the body of a recorded webhook (the capture's "body" member, compacted as the
// *arrs send it).
func fixture(t testing.TB, app integrations.Type, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, string(app), name))
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil || len(capture.Body) == 0 {
		t.Fatalf("%s/%s: no body (%v)", app, name, err)
	}
	var out bytes.Buffer
	if err := json.Compact(&out, capture.Body); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestParseFixtures parses every recorded payload: its event type, the item it names, its class
// and the files of its summary.
func TestParseFixtures(t *testing.T) {
	type want struct {
		eventType string
		class     Class
		target    int64 // 0: none
		files     int
	}
	cases := map[string]want{
		"lidarr/AlbumDelete.json":                          {"AlbumDelete", ClassChange, 1, 0},
		"lidarr/ArtistAdd.json":                            {"ArtistAdd", ClassChange, 1, 0},
		"lidarr/ArtistDelete.json":                         {"ArtistDelete", ClassChange, 1, 0},
		"lidarr/Download-replaceexisting.json":             {"Download", ClassDownload, 1, 10},
		"lidarr/Download.json":                             {"Download", ClassDownload, 1, 10},
		"lidarr/Health-warning.json":                       {"Health", ClassIgnored, 0, 0},
		"lidarr/Health.json":                               {"Health", ClassIgnored, 0, 0},
		"lidarr/Rename.json":                               {"Rename", ClassChange, 1, 19},
		"lidarr/Retag.json":                                {"Retag", ClassChange, 1, 1},
		"lidarr/Test-basic-auth.json":                      {"Test", ClassTest, 0, 0},
		"lidarr/Test-header-auth.json":                     {"Test", ClassTest, 0, 0},
		"lidarr/Test-query-auth.json":                      {"Test", ClassTest, 0, 0},
		"lidarr/Test.json":                                 {"Test", ClassTest, 0, 0},
		"radarr/Download-downloadclient.json":              {"Download", ClassDownload, 2, 1},
		"radarr/Download-upgrade-recyclebin.json":          {"Download", ClassDownload, 4, 2},
		"radarr/Download-upgrade-samepath.json":            {"Download", ClassDownload, 2, 2},
		"radarr/Download-upgrade.json":                     {"Download", ClassDownload, 1, 2},
		"radarr/Download.json":                             {"Download", ClassDownload, 3, 1},
		"radarr/Grab.json":                                 {"Grab", ClassIgnored, 0, 0},
		"radarr/Health-warning.json":                       {"Health", ClassIgnored, 0, 0},
		"radarr/Health.json":                               {"Health", ClassIgnored, 0, 0},
		"radarr/HealthRestored.json":                       {"HealthRestored", ClassIgnored, 0, 0},
		"radarr/ManualInteractionRequired.json":            {"ManualInteractionRequired", ClassIgnored, 0, 0},
		"radarr/MovieAdded.json":                           {"MovieAdded", ClassChange, 1, 0},
		"radarr/MovieDelete-deletedFiles.json":             {"MovieDelete", ClassDelete, 1, 0},
		"radarr/MovieDelete.json":                          {"MovieDelete", ClassChange, 3, 0},
		"radarr/MovieFileDelete-manual.json":               {"MovieFileDelete", ClassChange, 2, 1},
		"radarr/MovieFileDelete-upgrade-recyclebin.json":   {"MovieFileDelete", ClassUpgradeDelete, 4, 1},
		"radarr/MovieFileDelete-upgrade-samepath.json":     {"MovieFileDelete", ClassUpgradeDelete, 2, 1},
		"radarr/MovieFileDelete-upgrade.json":              {"MovieFileDelete", ClassUpgradeDelete, 1, 1},
		"radarr/Rename.json":                               {"Rename", ClassChange, 2, 1},
		"radarr/Test-basic-auth.json":                      {"Test", ClassTest, 0, 0},
		"radarr/Test-header-auth.json":                     {"Test", ClassTest, 0, 0},
		"radarr/Test-query-auth.json":                      {"Test", ClassTest, 0, 0},
		"radarr/Test.json":                                 {"Test", ClassTest, 0, 0},
		"sonarr/Download-downloadclient.json":              {"Download", ClassDownload, 1, 1},
		"sonarr/Download-importcomplete-upgrade.json":      {"Download", ClassDownload, 1, 1},
		"sonarr/Download-importcomplete.json":              {"Download", ClassDownload, 1, 1},
		"sonarr/Download-multiepisode.json":                {"Download", ClassDownload, 1, 1},
		"sonarr/Download-upgrade-recyclebin.json":          {"Download", ClassDownload, 1, 2},
		"sonarr/Download-upgrade-samepath.json":            {"Download", ClassDownload, 1, 2},
		"sonarr/Download-upgrade.json":                     {"Download", ClassDownload, 1, 2},
		"sonarr/Download.json":                             {"Download", ClassDownload, 1, 1},
		"sonarr/EpisodeFileDelete-manual.json":             {"EpisodeFileDelete", ClassChange, 1, 1},
		"sonarr/EpisodeFileDelete-upgrade-recyclebin.json": {"EpisodeFileDelete", ClassUpgradeDelete, 1, 1},
		"sonarr/EpisodeFileDelete-upgrade-samepath.json":   {"EpisodeFileDelete", ClassUpgradeDelete, 1, 1},
		"sonarr/EpisodeFileDelete-upgrade.json":            {"EpisodeFileDelete", ClassUpgradeDelete, 1, 1},
		"sonarr/Grab.json":                                 {"Grab", ClassIgnored, 0, 0},
		"sonarr/Health-warning.json":                       {"Health", ClassIgnored, 0, 0},
		"sonarr/Health.json":                               {"Health", ClassIgnored, 0, 0},
		"sonarr/ManualInteractionRequired.json":            {"ManualInteractionRequired", ClassIgnored, 0, 0},
		"sonarr/Rename.json":                               {"Rename", ClassChange, 1, 3},
		"sonarr/SeriesAdd.json":                            {"SeriesAdd", ClassChange, 1, 0},
		"sonarr/SeriesDelete-deletedFiles.json":            {"SeriesDelete", ClassDelete, 1, 0},
		"sonarr/SeriesDelete.json":                         {"SeriesDelete", ClassChange, 2, 0},
		"sonarr/Test-basic-auth.json":                      {"Test", ClassTest, 0, 0},
		"sonarr/Test-header-auth.json":                     {"Test", ClassTest, 0, 0},
		"sonarr/Test-query-auth.json":                      {"Test", ClassTest, 0, 0},
		"sonarr/Test.json":                                 {"Test", ClassTest, 0, 0},
	}
	files, err := filepath.Glob(filepath.Join(fixtureDir, "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 59 || len(cases) != 59 {
		t.Fatalf("%d fixture files, %d cases; the spike recorded 59", len(files), len(cases))
	}
	for _, f := range files {
		rel := filepath.ToSlash(strings.TrimPrefix(f, fixtureDir+string(filepath.Separator)))
		w, ok := cases[rel]
		if !ok {
			t.Errorf("%s has no expectation", rel)
			continue
		}
		app := integrations.Type(filepath.Base(filepath.Dir(f)))
		body := fixture(t, app, filepath.Base(f))
		ev, err := Parse(app, bytes.NewReader(body))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		var targets []int64
		if w.target != 0 {
			targets = []int64{w.target}
		}
		if ev.EventType != w.eventType || ev.Class != w.class || !slices.Equal(ev.Targets, targets) || len(ev.Summary.Files) != w.files {
			t.Errorf("%s: got %s %s %v files %d; want %s %s %v files %d", rel, ev.EventType, ev.Class, ev.Targets,
				len(ev.Summary.Files), w.eventType, w.class, targets, w.files)
		}
		switch {
		case ev.Class == ClassIgnored:
			if string(ev.Payload) != "{}" || ev.Truncated {
				t.Errorf("%s: ignored event stored %s", rel, ev.Payload)
			}
		case !bytes.Equal(ev.Payload, body) || ev.Truncated:
			t.Errorf("%s: payload is not the compact body", rel)
		}
		if w.class != ClassIgnored && ev.Summary.Title == "" {
			t.Errorf("%s: no title in the summary", rel)
		}
	}
}

// TestParseImportCompleteAndMultiEpisode checks the Sonarr forms the spike found: the
// ImportComplete form (episodeFiles plural, sourcePath, destinationPath, no isUpgrade) and a
// multi-episode file.
func TestParseImportCompleteAndMultiEpisode(t *testing.T) {
	ev, err := Parse(integrations.TypeSonarr, bytes.NewReader(fixture(t, integrations.TypeSonarr, "Download-importcomplete.json")))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Class != ClassDownload || !slices.Equal(ev.Targets, []int64{1}) ||
		!slices.Equal(ev.Summary.Files, []string{"/tv/The Beverly Hillbillies/Season 1/The Beverly Hillbillies - S01E06 - Trick or Treat HDTV-720p.mkv"}) {
		t.Fatalf("ImportComplete = %+v", ev)
	}
	ev, err = Parse(integrations.TypeSonarr, bytes.NewReader(fixture(t, integrations.TypeSonarr, "Download-multiepisode.json")))
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Summary.Files) != 1 || !strings.Contains(ev.Summary.Files[0], "S01E04-E05") || ev.Summary.Title != "The Beverly Hillbillies" {
		t.Fatalf("multi-episode = %+v", ev.Summary)
	}
}

// TestParseAppDecidesTheItem: only the route's app's item object names a target, and the event
// types of another app are ignored.
func TestParseAppDecidesTheItem(t *testing.T) {
	body := fixture(t, integrations.TypeRadarr, "Download.json")
	ev, err := Parse(integrations.TypeSonarr, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Class != ClassDownload || len(ev.Targets) != 0 {
		t.Fatalf("a Radarr body on the Sonarr route = %+v", ev)
	}
	ev, err = Parse(integrations.TypeRadarr, strings.NewReader(`{"eventType":"SeriesAdd","series":{"id":7}}`))
	if err != nil || ev.Class != ClassIgnored {
		t.Fatalf("SeriesAdd on the Radarr route = %+v, %v", ev, err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", ``},
		{"array", `[{"eventType":"Test"}]`},
		{"string", `"Test"`},
		{"no eventType", `{"movie":{"id":1}}`},
		{"eventType not a string", `{"eventType":1}`},
		{"empty eventType", `{"eventType":""}`},
		{"long eventType", `{"eventType":"` + strings.Repeat("x", 65) + `"}`},
		{"truncated", `{"eventType":"Download","movie":{"id":1`},
		{"two values", `{"eventType":"Test"} {}`},
		{"garbage", `{"eventType":"Test",}`},
	} {
		_, err := Parse(integrations.TypeRadarr, strings.NewReader(tc.body))
		var ie *InvalidError
		if !errors.As(err, &ie) {
			t.Errorf("%s: err = %v, want an InvalidError", tc.name, err)
		}
	}
	// 64 characters and duplicate names are accepted; odd ids name no target.
	ev, err := Parse(integrations.TypeRadarr, strings.NewReader(`{"eventType":"`+strings.Repeat("é", 64)+`","eventType":"Rename","movie":{"id":-3}}`))
	if err != nil || ev.EventType != "Rename" || len(ev.Targets) != 0 {
		t.Fatalf("Parse = %+v, %v", ev, err)
	}
}

// TestParseLargeBodies: a 3 MiB Rename is accepted and stored truncated with its ids; a body over
// 16 MiB is refused.
func TestParseLargeBodies(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"series":{"id":42,"title":"Big"},"renamedEpisodeFiles":[`)
	for i := 0; b.Len() < 3<<20; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"path":"/tv/Big/Season 1/Big - S01E%04d - A long episode title that takes some room.mkv","previousPath":"/tv/Big/old-%d.mkv"}`, i, i, i)
	}
	b.WriteString(`],"eventType":"Rename"}`)
	ev, err := Parse(integrations.TypeSonarr, strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Truncated || ev.Class != ClassChange || !slices.Equal(ev.Targets, []int64{42}) || len(ev.Summary.Files) != maxSummaryFiles {
		t.Fatalf("3 MiB Rename = %s %v truncated %v files %d", ev.Class, ev.Targets, ev.Truncated, len(ev.Summary.Files))
	}
	if string(ev.Payload) != `{"truncated":true,"eventType":"Rename","ids":[42]}` {
		t.Fatalf("stored payload = %s", ev.Payload)
	}
	if s := Summarize(integrations.TypeSonarr, ev.Payload, true); !slices.Equal(s.ItemIDs, []int64{42}) {
		t.Fatalf("summary of the stored payload = %+v", s)
	}

	// A compact body just over 64 KiB is stored truncated too.
	pad := strings.Repeat("x", MaxPayload)
	ev, err = Parse(integrations.TypeRadarr, strings.NewReader(`{"eventType":"Download","movie":{"id":5,"overview":"`+pad+`"}}`))
	if err != nil || !ev.Truncated {
		t.Fatalf("65 KiB body = %v, %v", ev.Truncated, err)
	}

	huge := `{"eventType":"Rename","movie":{"id":1,"overview":"` + strings.Repeat("x", MaxBody) + `"}}`
	if _, err := Parse(integrations.TypeRadarr, strings.NewReader(huge)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("17 MiB body = %v, want ErrTooLarge", err)
	}
}

func TestSummarize(t *testing.T) {
	body := fixture(t, integrations.TypeRadarr, "Download-upgrade.json")
	s := Summarize(integrations.TypeRadarr, body, false)
	if s.Title != "Night of the Living Dead" || !slices.Equal(s.ItemIDs, []int64{1}) || len(s.Files) != 2 {
		t.Fatalf("summary = %+v", s)
	}
	if s := Summarize(integrations.TypeRadarr, []byte("{}"), false); len(s.ItemIDs) != 0 || s.Files == nil {
		t.Fatalf("summary of {} = %+v", s)
	}
}
