package manifest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func basePlanItem() PlanItem {
	return PlanItem{IntegrationID: 1, ArrID: 2, Kind: "series", ExternalIDs: ExternalIDs{TVDB: 71471, TMDB: 1930, IMDB: "tt1"}, Title: "Show",
		Year: 1962, RootFolder: "/tv", QualityProfile: "HD-1080p", Monitored: true, Tags: []string{"a", "B"},
		Detail: ItemDetail{SeriesType: "standard", SeasonFolder: ptr(true), MonitorNewItems: "all", UseSceneNumbering: ptr(false), LanguageProfileID: 1,
			Seasons: []Season{{0, false}, {1, true}}, Episodes: []Episode{{1, 1, true}, {1, 2, false}}},
		Files: []PlanFile{{"Season 1/e1.mkv", 10}, {"Season 1/e2.mkv", 20}}}
}

func TestComparePlanEqual(t *testing.T) {
	p := basePlanItem()
	l := basePlanItem()
	// Case, order and a trailing slash do not matter; the pairing ignores the *arr id.
	l.ArrID = 99
	l.QualityProfile = "hd-1080P"
	l.Tags = []string{"b", "A", "a"}
	l.RootFolder = "/tv/"
	l.Detail.Seasons = []Season{{1, true}, {0, false}}
	l.Detail.Episodes = []Episode{{1, 2, false}, {1, 1, true}}
	l.Files = []PlanFile{{"Season 1/e2.mkv", 20}, {"Season 1/e1.mkv", 10}}
	if d := ComparePlan(Plan{Items: []PlanItem{p}}, Plan{Items: []PlanItem{l}}); len(d) != 0 {
		t.Fatalf("differences %v", d)
	}
}

func TestComparePlanFindsEveryDifference(t *testing.T) {
	cases := map[string]func(*PlanItem){
		"externalIds":                 func(i *PlanItem) { i.ExternalIDs.IMDB = "tt2" },
		"title":                       func(i *PlanItem) { i.Title = "show" },
		"year":                        func(i *PlanItem) { i.Year = 1963 },
		"rootFolder":                  func(i *PlanItem) { i.RootFolder = "/tv2" },
		"qualityProfile":              func(i *PlanItem) { i.QualityProfile = "Any" },
		"metadataProfile":             func(i *PlanItem) { i.MetadataProfile = "Standard" },
		"monitored":                   func(i *PlanItem) { i.Monitored = false },
		"tags":                        func(i *PlanItem) { i.Tags = []string{"a"} },
		"detail.minimumAvailability":  func(i *PlanItem) { i.Detail.MinimumAvailability = "released" },
		"detail.seriesType":           func(i *PlanItem) { i.Detail.SeriesType = "anime" },
		"detail.seasonFolder":         func(i *PlanItem) { i.Detail.SeasonFolder = ptr(false) },
		"detail.monitorNewItems":      func(i *PlanItem) { i.Detail.MonitorNewItems = "none" },
		"detail.useSceneNumbering":    func(i *PlanItem) { i.Detail.UseSceneNumbering = nil },
		"detail.languageProfileId":    func(i *PlanItem) { i.Detail.LanguageProfileID = 2 },
		"detail.seasons":              func(i *PlanItem) { i.Detail.Seasons[1].Monitored = false },
		"detail.episodes":             func(i *PlanItem) { i.Detail.Episodes = i.Detail.Episodes[:1] },
		"detail.albums":               func(i *PlanItem) { i.Detail.Albums = []Album{{ID: 1, MBID: "m", Title: "A"}} },
		"files[Season 1/e2.mkv].size": func(i *PlanItem) { i.Files[1].Size = 21 },
		"files[Season 1/e3.mkv]":      func(i *PlanItem) { i.Files = append(i.Files, PlanFile{"Season 1/e3.mkv", 1}) },
		"files[Season 1/e1.mkv]":      func(i *PlanItem) { i.Files = i.Files[1:] },
	}
	for field, mutate := range cases {
		t.Run(field, func(t *testing.T) {
			l := basePlanItem()
			mutate(&l)
			d := ComparePlan(Plan{Items: []PlanItem{basePlanItem()}}, Plan{Items: []PlanItem{l}})
			if len(d) != 1 || d[0].Field != field || !strings.Contains(d[0].String(), "tvdb:71471 Show") {
				t.Fatalf("differences %v", d)
			}
		})
	}
}

func TestComparePlanPairsItems(t *testing.T) {
	a := basePlanItem()
	b := basePlanItem()
	b.ExternalIDs.TVDB = 5
	c := basePlanItem()
	c.IntegrationID = 2 // the same series in another Sonarr is another item
	d := ComparePlan(Plan{Items: []PlanItem{a, b}}, Plan{Items: []PlanItem{a, c}})
	if len(d) != 2 || d[0].Field != "item" || d[0].Plan != "present" || !strings.Contains(d[0].Item, "tvdb:5") ||
		d[1].Field != "item" || d[1].Live != "present" {
		t.Fatalf("differences %v", d)
	}
	// Without the main external id the *arr id pairs.
	m := PlanItem{IntegrationID: 1, ArrID: 7, Kind: "movie", Title: "X"}
	if d := ComparePlan(Plan{Items: []PlanItem{m}}, Plan{Items: []PlanItem{m}}); len(d) != 0 {
		t.Fatalf("differences %v", d)
	}
}

func TestRoundTripDetectsAStaleIndex(t *testing.T) {
	// The round trip is not tautological: a tag renamed and a movie file replaced in the *arr
	// after the index was refreshed are both reported, and a refresh makes them agree again.
	e := newEnv(t)
	var tags []map[string]any
	if err := json.Unmarshal(mustGet(t, e.radarr.URL+"/api/v3/tag"), &tags); err != nil {
		t.Fatal(err)
	}
	tags[0]["label"] = "renamed"
	raw, _ := json.Marshal(tags)
	e.radarr.SetJSON(http.MethodGet, "tag", raw)
	var movies []map[string]any
	if err := json.Unmarshal(mustGet(t, e.radarr.URL+"/api/v3/movie"), &movies); err != nil {
		t.Fatal(err)
	}
	for _, m := range movies {
		if m["id"] == float64(2) {
			f := m["movieFile"].(map[string]any)
			f["size"] = f["size"].(float64) + 1
		}
	}
	raw, _ = json.Marshal(movies)
	e.radarr.SetJSON(http.MethodGet, "movie", raw)
	d := e.dest
	plan := e.build(&d).ReimportPlan()
	var fields []string
	for _, x := range ComparePlan(plan.ForIntegration(e.rad.ID), e.livePlans()[e.rad.ID]) {
		fields = append(fields, x.Field)
	}
	if strings.Join(fields, ",") != "tags,files[His Girl Friday (1940) [Bluray-1080p].mkv].size,tags" {
		t.Fatalf("differences %v", fields)
	}
	e.refresh(e.rad.ID)
	plan = e.build(&d).ReimportPlan()
	if diffs := ComparePlan(plan.ForIntegration(e.rad.ID), e.livePlans()[e.rad.ID]); len(diffs) != 0 {
		t.Fatalf("after a refresh: %v", diffs)
	}
}

func mustGet(t *testing.T, url string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Api-Key", arrKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
