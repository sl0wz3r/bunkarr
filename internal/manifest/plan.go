package manifest

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Plan is what a restore would send to a fresh *arr for each item of a manifest (design §11.1):
// the kind, external ids, title and year, root folder, quality profile name, metadata profile,
// monitored, tag labels, the kind-specific detail, and each *arr file's relative path and size.
// A tool that actually re-imports is deferred; ComparePlan checks a plan against an *arr's live
// state (acceptance 3).
type Plan struct {
	Items []PlanItem `json:"items"`
}

// PlanItem is one item of a Plan.
type PlanItem struct {
	// IntegrationID and ArrID say where the item comes from; ComparePlan pairs items by them and
	// the kind's main external id, and never compares them.
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

// PlanFile is an *arr file of a PlanItem.
type PlanFile struct {
	RelativePath string `json:"relativePath"`
	Size         int64  `json:"size"`
}

// ReimportPlan returns the plan of every item of m, in manifest order.
func (m *Manifest) ReimportPlan() Plan {
	p := Plan{Items: make([]PlanItem, 0, len(m.Items))}
	for _, it := range m.Items {
		pi := PlanItem{IntegrationID: it.IntegrationID, ArrID: it.ArrID, Kind: it.Kind, ExternalIDs: it.ExternalIDs, Title: it.Title,
			Year: it.Year, RootFolder: it.RootFolder, QualityProfile: it.QualityProfile, MetadataProfile: it.MetadataProfile,
			Monitored: it.Monitored, Tags: slices.Clone(it.Tags), Detail: it.Detail, Files: make([]PlanFile, 0, len(it.Files))}
		for _, f := range it.Files {
			pi.Files = append(pi.Files, PlanFile{RelativePath: f.RelativePath, Size: f.Size})
		}
		p.Items = append(p.Items, pi)
	}
	return p
}

// ForIntegration returns the plan's items of one integration.
func (p Plan) ForIntegration(id int64) Plan {
	out := Plan{Items: []PlanItem{}}
	for _, it := range p.Items {
		if it.IntegrationID == id {
			out.Items = append(out.Items, it)
		}
	}
	return out
}

// Difference is one way a plan differs from a live state.
type Difference struct {
	// Item names the item ("movie tmdb:10331 Night of the Living Dead").
	Item string
	// Field is the field that differs ("tags", "files[Season 1/x.mkv].size"), or "item" when the
	// item is on one side only.
	Field string
	// Plan and Live are the two values as text ("" when absent).
	Plan string
	Live string
}

// String renders the difference for a test failure or a log line.
func (d Difference) String() string {
	return fmt.Sprintf("%s: %s: plan %q, live %q", d.Item, d.Field, d.Plan, d.Live)
}

// ComparePlan compares a plan with a live state and returns every difference, in item order.
// Items are paired by integration, kind and the kind's main external id (tmdb for a movie, tvdb
// for a series, mbid for an artist; the *arr id when that is unknown). Exactly the plan's field
// set is compared: tag labels and profile names ignoring case, lists (tags, seasons, episodes,
// albums, files) ignoring order, root folders without a trailing slash.
func ComparePlan(plan, live Plan) []Difference {
	var diffs []Difference
	liveByKey := map[string][]PlanItem{}
	var liveKeys []string
	for _, it := range live.Items {
		k := pairKey(it)
		if _, ok := liveByKey[k]; !ok {
			liveKeys = append(liveKeys, k)
		}
		liveByKey[k] = append(liveByKey[k], it)
	}
	seen := map[string]int{}
	for _, p := range plan.Items {
		k := pairKey(p)
		i := seen[k]
		seen[k] = i + 1
		if i >= len(liveByKey[k]) {
			diffs = append(diffs, Difference{Item: itemName(p), Field: "item", Plan: "present", Live: "absent"})
			continue
		}
		diffs = append(diffs, compareItem(p, liveByKey[k][i])...)
	}
	for _, k := range liveKeys {
		for _, l := range liveByKey[k][min(seen[k], len(liveByKey[k])):] {
			diffs = append(diffs, Difference{Item: itemName(l), Field: "item", Plan: "absent", Live: "present"})
		}
	}
	return diffs
}

// pairKey is the key ComparePlan pairs items by.
func pairKey(it PlanItem) string {
	id := ""
	switch it.Kind {
	case "movie":
		id = optInt(it.ExternalIDs.TMDB)
	case "series":
		id = optInt(it.ExternalIDs.TVDB)
	case "artist":
		id = it.ExternalIDs.MBID
	}
	if id == "" {
		id = "arr:" + strconv.FormatInt(it.ArrID, 10)
	}
	return fmt.Sprintf("%d/%s/%s", it.IntegrationID, it.Kind, id)
}

// itemName names an item in a Difference.
func itemName(it PlanItem) string {
	id := "arr:" + strconv.FormatInt(it.ArrID, 10)
	switch {
	case it.Kind == "movie" && it.ExternalIDs.TMDB != 0:
		id = "tmdb:" + strconv.FormatInt(it.ExternalIDs.TMDB, 10)
	case it.Kind == "series" && it.ExternalIDs.TVDB != 0:
		id = "tvdb:" + strconv.FormatInt(it.ExternalIDs.TVDB, 10)
	case it.Kind == "artist" && it.ExternalIDs.MBID != "":
		id = "mbid:" + it.ExternalIDs.MBID
	}
	return fmt.Sprintf("%s %s %s", it.Kind, id, it.Title)
}

func compareItem(p, l PlanItem) []Difference {
	var out []Difference
	name := itemName(p)
	diff := func(field, pv, lv string) {
		if pv != lv {
			out = append(out, Difference{Item: name, Field: field, Plan: pv, Live: lv})
		}
	}
	diff("externalIds", idsText(p.ExternalIDs), idsText(l.ExternalIDs))
	diff("title", p.Title, l.Title)
	diff("year", strconv.Itoa(p.Year), strconv.Itoa(l.Year))
	diff("rootFolder", trimFolder(p.RootFolder), trimFolder(l.RootFolder))
	diff("qualityProfile", strings.ToLower(p.QualityProfile), strings.ToLower(l.QualityProfile))
	diff("metadataProfile", strings.ToLower(p.MetadataProfile), strings.ToLower(l.MetadataProfile))
	diff("monitored", strconv.FormatBool(p.Monitored), strconv.FormatBool(l.Monitored))
	diffSet := func(field string, pl, ll []string) {
		onlyP, onlyL := setDiff(pl, ll)
		if len(onlyP) > 0 || len(onlyL) > 0 {
			out = append(out, Difference{Item: name, Field: field, Plan: strings.Join(onlyP, "; "), Live: strings.Join(onlyL, "; ")})
		}
	}
	diffSet("tags", lowerAll(p.Tags), lowerAll(l.Tags))
	pd, ld := p.Detail, l.Detail
	diff("detail.minimumAvailability", pd.MinimumAvailability, ld.MinimumAvailability)
	diff("detail.seriesType", pd.SeriesType, ld.SeriesType)
	diff("detail.seasonFolder", boolPtrText(pd.SeasonFolder), boolPtrText(ld.SeasonFolder))
	diff("detail.monitorNewItems", pd.MonitorNewItems, ld.MonitorNewItems)
	diff("detail.useSceneNumbering", boolPtrText(pd.UseSceneNumbering), boolPtrText(ld.UseSceneNumbering))
	diff("detail.languageProfileId", strconv.FormatInt(pd.LanguageProfileID, 10), strconv.FormatInt(ld.LanguageProfileID, 10))
	diffSet("detail.seasons", seasonsText(pd.Seasons), seasonsText(ld.Seasons))
	diffSet("detail.episodes", episodesText(pd.Episodes), episodesText(ld.Episodes))
	diffSet("detail.albums", albumsText(pd.Albums), albumsText(ld.Albums))
	pf, lf := map[string]int64{}, map[string]int64{}
	for _, f := range p.Files {
		pf[f.RelativePath] = f.Size
	}
	for _, f := range l.Files {
		lf[f.RelativePath] = f.Size
	}
	paths := make([]string, 0, len(pf)+len(lf))
	for k := range pf {
		paths = append(paths, k)
	}
	for k := range lf {
		if _, ok := pf[k]; !ok {
			paths = append(paths, k)
		}
	}
	slices.Sort(paths)
	for _, rel := range paths {
		ps, pok := pf[rel]
		ls, lok := lf[rel]
		switch {
		case !lok:
			diff("files["+rel+"]", "present", "absent")
		case !pok:
			diff("files["+rel+"]", "absent", "present")
		default:
			diff("files["+rel+"].size", strconv.FormatInt(ps, 10), strconv.FormatInt(ls, 10))
		}
	}
	if len(p.Files) != len(pf) || len(l.Files) != len(lf) {
		diff("files", strconv.Itoa(len(p.Files))+" files", strconv.Itoa(len(l.Files))+" files")
	}
	return out
}

func idsText(ids ExternalIDs) string {
	return fmt.Sprintf("tmdb=%s tvdb=%s imdb=%s tvmaze=%s mbid=%s", optInt(ids.TMDB), optInt(ids.TVDB), ids.IMDB, optInt(ids.TVMaze), ids.MBID)
}

func trimFolder(p string) string {
	if len(p) > 1 {
		return strings.TrimRight(p, "/")
	}
	return p
}

func boolPtrText(b *bool) string {
	if b == nil {
		return ""
	}
	return strconv.FormatBool(*b)
}

// setDiff returns the elements only in a and only in b, each sorted and without duplicates
// (the lists are compared as sets).
func setDiff(a, b []string) (onlyA, onlyB []string) {
	as, bs := map[string]bool{}, map[string]bool{}
	for _, s := range a {
		as[s] = true
	}
	for _, s := range b {
		bs[s] = true
	}
	for s := range as {
		if !bs[s] {
			onlyA = append(onlyA, s)
		}
	}
	for s := range bs {
		if !as[s] {
			onlyB = append(onlyB, s)
		}
	}
	slices.Sort(onlyA)
	slices.Sort(onlyB)
	return onlyA, onlyB
}

func lowerAll(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = strings.ToLower(s)
	}
	return out
}

func seasonsText(ss []Season) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, fmt.Sprintf("%d:%t", s.SeasonNumber, s.Monitored))
	}
	return out
}

func episodesText(es []Episode) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, fmt.Sprintf("S%02dE%02d:%t", e.Season, e.Episode, e.Monitored))
	}
	return out
}

func albumsText(as []Album) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, fmt.Sprintf("%d|%s|%s|%t", a.ID, a.MBID, a.Title, a.Monitored))
	}
	return out
}
