// Package manifesttest decodes an *arr's live state from its raw API JSON into the JSON of a
// Plan, for the manifest round-trip tests (docs/design/phase2-3.md acceptance 3 and §15:
// "the test decodes the *arr state itself from the raw API JSON"). It deliberately uses neither
// the *arr client's DTOs (internal/integrations/arr), the metadata index nor the manifest
// package's own types, so a decoding mistake there cannot hide behind the same mistake here; the
// caller unmarshals the result into a Plan. It is imported by tests only: the unit tests
// read the recorded fixtures through arrtest, the Docker suite a real Radarr and Sonarr.
package manifesttest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// Getter returns the raw body of an *arr API request, e.g. "movie" or "episode?seriesId=1"
// (relative to the API prefix).
type Getter func(target string) ([]byte, error)

// HTTPGetter reads an *arr API over HTTP: baseURL plus apiPrefix ("/api/v3", or "/api/v1" for
// Lidarr) plus the target, with the key in the X-Api-Key header.
func HTTPGetter(baseURL, apiPrefix, key string) Getter {
	c := &http.Client{Timeout: 2 * time.Minute}
	return func(target string) ([]byte, error) {
		u := strings.TrimRight(baseURL, "/") + apiPrefix + "/" + target
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Api-Key", key)
		req.Header.Set("Accept", "application/json")
		res, err := c.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", target, err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, 256<<20))
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", target, err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: HTTP %d", target, res.StatusCode)
		}
		return body, nil
	}
}

// LivePlan decodes the state of an *arr of kind "radarr", "sonarr" or "lidarr" into the JSON of a
// Plan whose items carry integrationID (the manifest's id of that *arr):
//
//   - Radarr: GET movie (with the nested movieFile), qualityprofile and tag;
//   - Sonarr: GET series, qualityprofile and tag, and per series episodefile?seriesId= and
//     episode?seriesId=;
//   - Lidarr: GET artist, qualityprofile, metadataprofile and tag, and per artist
//     trackfile?artistId= and album?artistId=.
func LivePlan(kind string, integrationID int64, get Getter) ([]byte, error) {
	profiles, err := named(get, "qualityprofile", "name")
	if err != nil {
		return nil, err
	}
	tags, err := named(get, "tag", "label")
	if err != nil {
		return nil, err
	}
	d := decoder{get: get, id: integrationID, profiles: profiles, tags: tags}
	var p Plan
	switch kind {
	case "radarr":
		p, err = d.radarr()
	case "sonarr":
		p, err = d.sonarr()
	case "lidarr":
		if d.metaProfiles, err = named(get, "metadataprofile", "name"); err != nil {
			return nil, err
		}
		p, err = d.lidarr()
	default:
		return nil, fmt.Errorf("unknown *arr kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// Plan, PlanItem, PlanFile, ExternalIDs, ItemDetail, Season, Episode and Album have the JSON
// shape of their manifest namesakes (Plan), declared again on purpose.
type Plan struct {
	Items []PlanItem `json:"items"`
}

// PlanItem is an item of a Plan.
type PlanItem struct {
	IntegrationID   int64       `json:"integrationId"`
	ArrID           int64       `json:"arrId"`
	Kind            string      `json:"kind"`
	ExternalIDs     ExternalIDs `json:"externalIds"`
	Title           string      `json:"title"`
	Year            int         `json:"year"`
	RootFolder      string      `json:"rootFolder"`
	QualityProfile  string      `json:"qualityProfile"`
	MetadataProfile string      `json:"metadataProfile"`
	Monitored       bool        `json:"monitored"`
	Tags            []string    `json:"tags"`
	Detail          ItemDetail  `json:"detail"`
	Files           []PlanFile  `json:"files"`
}

// PlanFile is an *arr file.
type PlanFile struct {
	RelativePath string `json:"relativePath"`
	Size         int64  `json:"size"`
}

// ExternalIDs are an item's ids in the metadata databases.
type ExternalIDs struct {
	TMDB   int64  `json:"tmdb,omitempty"`
	IMDB   string `json:"imdb,omitempty"`
	TVDB   int64  `json:"tvdb,omitempty"`
	TVMaze int64  `json:"tvmaze,omitempty"`
	MBID   string `json:"mbid,omitempty"`
}

// ItemDetail holds the kind-specific fields.
type ItemDetail struct {
	MinimumAvailability string    `json:"minimumAvailability,omitempty"`
	SeriesType          string    `json:"seriesType,omitempty"`
	SeasonFolder        *bool     `json:"seasonFolder,omitempty"`
	MonitorNewItems     string    `json:"monitorNewItems,omitempty"`
	UseSceneNumbering   *bool     `json:"useSceneNumbering,omitempty"`
	LanguageProfileID   int64     `json:"languageProfileId,omitempty"`
	Seasons             []Season  `json:"seasons,omitempty"`
	Episodes            []Episode `json:"episodes,omitempty"`
	Albums              []Album   `json:"albums,omitempty"`
}

// Season is a Sonarr season.
type Season struct {
	SeasonNumber int  `json:"seasonNumber"`
	Monitored    bool `json:"monitored"`
}

// Episode is a Sonarr episode.
type Episode struct {
	Season    int  `json:"season"`
	Episode   int  `json:"episode"`
	Monitored bool `json:"monitored"`
}

// Album is a Lidarr album.
type Album struct {
	ID        int64  `json:"id"`
	MBID      string `json:"mbid"`
	Title     string `json:"title"`
	Monitored bool   `json:"monitored"`
}

type decoder struct {
	get          Getter
	id           int64
	profiles     map[int64]string
	metaProfiles map[int64]string
	tags         map[int64]string
}

func (d decoder) list(target string) ([]map[string]any, error) {
	raw, err := d.get(target)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", target, err)
	}
	return out, nil
}

// named decodes a list of {id, <field>} into a map.
func named(get Getter, target, field string) (map[int64]string, error) {
	raw, err := get(target)
	if err != nil {
		return nil, err
	}
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode %s: %w", target, err)
	}
	out := map[int64]string{}
	for _, x := range list {
		out[num(x["id"])] = str(x[field])
	}
	return out, nil
}

func (d decoder) common(x map[string]any, kind string) PlanItem {
	it := PlanItem{IntegrationID: d.id, ArrID: num(x["id"]), Kind: kind, Title: str(x["title"]), Year: int(num(x["year"])),
		RootFolder: str(x["rootFolderPath"]), QualityProfile: d.profiles[num(x["qualityProfileId"])], Monitored: x["monitored"] == true,
		Tags: []string{}, Files: []PlanFile{}}
	if it.RootFolder == "" {
		it.RootFolder = path.Dir(str(x["path"]))
	}
	for _, t := range list(x["tags"]) {
		it.Tags = append(it.Tags, d.tags[num(t)])
	}
	return it
}

func (d decoder) radarr() (Plan, error) {
	movies, err := d.list("movie")
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Items: []PlanItem{}}
	for _, m := range movies {
		it := d.common(m, "movie")
		it.ExternalIDs = ExternalIDs{TMDB: num(m["tmdbId"]), IMDB: str(m["imdbId"])}
		it.Detail.MinimumAvailability = str(m["minimumAvailability"])
		if f, ok := m["movieFile"].(map[string]any); ok && num(f["id"]) != 0 {
			it.Files = append(it.Files, PlanFile{RelativePath: str(f["relativePath"]), Size: num(f["size"])})
		}
		p.Items = append(p.Items, it)
	}
	return p, nil
}

func (d decoder) sonarr() (Plan, error) {
	series, err := d.list("series")
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Items: []PlanItem{}}
	for _, s := range series {
		it := d.common(s, "series")
		it.ExternalIDs = ExternalIDs{TVDB: num(s["tvdbId"]), TMDB: num(s["tmdbId"]), IMDB: str(s["imdbId"]), TVMaze: num(s["tvMazeId"])}
		seasonFolder, scene := s["seasonFolder"] == true, s["useSceneNumbering"] == true
		it.Detail = ItemDetail{SeriesType: str(s["seriesType"]), SeasonFolder: &seasonFolder, MonitorNewItems: str(s["monitorNewItems"]),
			UseSceneNumbering: &scene, LanguageProfileID: num(s["languageProfileId"])}
		for _, se := range list(s["seasons"]) {
			sm, _ := se.(map[string]any)
			it.Detail.Seasons = append(it.Detail.Seasons, Season{SeasonNumber: int(num(sm["seasonNumber"])), Monitored: sm["monitored"] == true})
		}
		id := strconv.FormatInt(it.ArrID, 10)
		eps, err := d.list("episode?seriesId=" + id)
		if err != nil {
			return Plan{}, err
		}
		for _, e := range eps {
			it.Detail.Episodes = append(it.Detail.Episodes, Episode{Season: int(num(e["seasonNumber"])), Episode: int(num(e["episodeNumber"])),
				Monitored: e["monitored"] == true})
		}
		files, err := d.list("episodefile?seriesId=" + id)
		if err != nil {
			return Plan{}, err
		}
		for _, f := range files {
			it.Files = append(it.Files, PlanFile{RelativePath: str(f["relativePath"]), Size: num(f["size"])})
		}
		p.Items = append(p.Items, it)
	}
	return p, nil
}

func (d decoder) lidarr() (Plan, error) {
	artists, err := d.list("artist")
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Items: []PlanItem{}}
	for _, a := range artists {
		it := d.common(a, "artist")
		it.Title = str(a["artistName"])
		it.ExternalIDs = ExternalIDs{MBID: str(a["foreignArtistId"])}
		it.MetadataProfile = d.metaProfiles[num(a["metadataProfileId"])]
		it.Detail.MonitorNewItems = str(a["monitorNewItems"])
		id := strconv.FormatInt(it.ArrID, 10)
		albums, err := d.list("album?artistId=" + id)
		if err != nil {
			return Plan{}, err
		}
		for _, al := range albums {
			it.Detail.Albums = append(it.Detail.Albums, Album{ID: num(al["id"]), MBID: str(al["foreignAlbumId"]), Title: str(al["title"]),
				Monitored: al["monitored"] == true})
		}
		files, err := d.list("trackfile?artistId=" + id)
		if err != nil {
			return Plan{}, err
		}
		folder := strings.TrimRight(str(a["path"]), "/") + "/"
		for _, f := range files {
			it.Files = append(it.Files, PlanFile{RelativePath: strings.TrimPrefix(str(f["path"]), folder), Size: num(f["size"])})
		}
		p.Items = append(p.Items, it)
	}
	return p, nil
}

func num(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}
