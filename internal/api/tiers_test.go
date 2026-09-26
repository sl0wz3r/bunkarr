package api

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr/seerrtest"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

func TestTierRulesAPI(t *testing.T) {
	e := newEnv(t, nil)
	var rs tiers.RuleSet
	e.call(t, 200, "GET", "/tiers/rules", nil, &rs)
	if rs.Revision != 0 || len(rs.Rules) != 0 {
		t.Fatalf("initial rules %+v", rs)
	}
	var saved TierRulesSaved
	e.call(t, 200, "PUT", "/tiers/rules", map[string]any{"revision": 0, "rules": []map[string]any{
		{"name": "tagged", "action": "full", "conditions": []map[string]any{{"field": "arr.tag", "op": "has", "value": "bunkarr-full"}}},
		{"name": "rest", "action": "manifest", "conditions": []any{}, "destinationIds": nil},
	}}, &saved)
	if saved.Revision != 1 || len(saved.Rules) != 2 || saved.Rules[0].ID == 0 || saved.Rules[1].DestinationIDs != nil {
		t.Fatalf("saved %+v", saved)
	}
	// No fresh *arr index knows the tag: a warning placed on its condition.
	if len(saved.Warnings) != 1 || saved.Warnings[0].RuleIndex != 0 || saved.Warnings[0].ConditionIndex != 0 {
		t.Fatalf("warnings %+v", saved.Warnings)
	}
	if code, msg := e.status(t, "PUT", "/tiers/rules", map[string]any{"revision": 0, "rules": []any{}}); code != 409 || !strings.Contains(msg, "reload") {
		t.Errorf("stale revision: %d %q", code, msg)
	}
	if code, msg := e.status(t, "PUT", "/tiers/rules", map[string]any{"revision": 1, "rules": []map[string]any{
		{"name": "ok", "action": "full"},
		{"name": "bad", "action": "skip", "conditions": []map[string]any{{"field": "file.size", "op": "gt", "value": "big"}}},
	}}); code != 400 || !strings.Contains(msg, "rule 2, condition 1") {
		t.Errorf("invalid rule: %d %q", code, msg)
	}
	if code, _ := e.status(t, "PUT", "/tiers/rules", map[string]any{"rules": []any{}}); code != 400 {
		t.Errorf("no revision: %d", code)
	}
	if code, _ := e.status(t, "PUT", "/tiers/rules", map[string]any{"revision": 1, "rules": []any{}, "extra": 1}); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
	var presets []tiers.Preset
	e.call(t, 200, "GET", "/tiers/presets", nil, &presets)
	if len(presets) != 3 || presets[0].ID != tiers.PresetEverything {
		t.Errorf("presets %+v", presets)
	}
	var fields []tiers.Field
	e.call(t, 200, "GET", "/tiers/fields", nil, &fields)
	unavailable := 0
	for _, f := range fields {
		if !f.Available {
			unavailable++
		}
	}
	// Slice 9's provider (the Plex index, Tautulli, Seerr, Maintainerr) makes every field available.
	if len(fields) != 16 || unavailable != 0 {
		t.Errorf("%d fields, %d unavailable", len(fields), unavailable)
	}
}

func TestTierPreviewAndFlagsAPI(t *testing.T) {
	e := newEnv(t, nil)
	src := e.mkdir(t, "media")
	writeFile(t, filepath.Join(src, "a.mkv"), "aaaa", time.Now())
	writeFile(t, filepath.Join(src, "Kids/b.mkv"), "bbbbbb", time.Now())
	srcID := e.createSource(t, "Media", src)
	destID := e.createDestination(t, "UNAS", e.mkdir(t, "target"), []int64{srcID}, nil)
	var scan jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/sources/%d/scan", srcID), nil, &scan)
	e.waitJob(t, scan.ID)

	var p tiers.TierPreview
	e.call(t, 200, "POST", "/tiers/preview", map[string]any{"rules": []map[string]any{{"name": "skip all", "action": "skip"}}}, &p)
	if p.Revision != "draft" || len(p.Destinations) != 1 || p.Destinations[0].Skip.Files != 2 || p.Destinations[0].DestinationID != destID {
		t.Fatalf("preview %+v", p)
	}
	var page tiers.ItemPage
	e.call(t, 200, "GET", "/tiers/preview/"+p.ID+"/items?tier=skip&pageSize=1&page=2", nil, &page)
	if page.TotalRecords != 2 || len(page.Records) != 1 || page.Records[0].RuleID != -1 {
		t.Fatalf("items %+v", page)
	}
	for path, want := range map[string]int{
		"/tiers/preview/" + p.ID + "/items?tier=gold":            400,
		"/tiers/preview/" + p.ID + "/items?state=lost":           400,
		"/tiers/preview/" + strings.Repeat("0", 32) + "/items":   404,
		"/tiers/preview/nothex/items":                            404,
		"/tiers/preview/" + p.ID + "/items?destinationId=x":      400,
		"/tiers/preview/" + p.ID + "/items?ruleId=-1&state=kept": 200,
	} {
		if code, _ := e.status(t, "GET", path, nil); code != want {
			t.Errorf("GET %s: %d, want %d", path, code, want)
		}
	}
	// The saved (empty) rules: everything full.
	e.call(t, 200, "POST", "/tiers/preview", nil, &p)
	if p.Revision != float64(0) || p.Destinations[0].Full.Files != 2 || p.Destinations[0].ToCopy.Files != 2 {
		t.Fatalf("saved-rules preview %+v", p)
	}

	var f tiers.Flag
	e.call(t, 201, "POST", "/tiers/flags", map[string]any{"target": map[string]any{"sourceId": srcID, "relPath": "Kids"}, "note": "home movies"}, &f)
	if f.Kind != tiers.FlagKindPath || f.Flag != tiers.FlagIrreplaceable || f.Note != "home movies" {
		t.Fatalf("flag %+v", f)
	}
	if code, _ := e.status(t, "POST", "/tiers/flags", map[string]any{"target": map[string]any{"sourceId": srcID, "relPath": "Kids"}}); code != 409 {
		t.Errorf("duplicate flag: %d", code)
	}
	if code, _ := e.status(t, "POST", "/tiers/flags", map[string]any{"target": map[string]any{"sourceId": srcID, "relPath": "/abs"}}); code != 400 {
		t.Errorf("absolute path: %d", code)
	}
	var flags []tiers.Flag
	e.call(t, 200, "GET", "/tiers/flags", nil, &flags)
	if len(flags) != 1 || !flags[0].Resolved {
		t.Fatalf("flags %+v", flags)
	}
	e.call(t, 200, "POST", "/tiers/preview", map[string]any{"rules": []map[string]any{{"name": "skip all", "action": "skip"}}}, &p)
	if p.Destinations[0].Skip.Files != 1 || p.Destinations[0].Full.Files != 1 {
		t.Fatalf("flagged preview %+v", p.Destinations[0])
	}
	// The Library item view: facts and the tier at each linked destination.
	var fileID int64
	if err := e.db.Reader().QueryRow(`SELECT id FROM catalog_files WHERE rel_path = 'Kids/b.mkv'`).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	var fd FileDetail
	e.call(t, 200, "GET", fmt.Sprintf("/catalog/files/%d", fileID), nil, &fd)
	if fd.File.RelPath != "Kids/b.mkv" || fd.File.Size != 6 || fd.Source.ID != srcID || len(fd.Tiers) != 1 || fd.Tiers[0].Tier != tiers.Full ||
		fd.Tiers[0].RuleName != tiers.IrreplaceableRuleName || len(fd.Facts.Flags) != 1 || fd.Tiers[0].Record != nil ||
		fd.Facts.Arr.State != tiers.ArrUnmanaged {
		t.Fatalf("file detail %+v", fd)
	}
	if code, _ := e.status(t, "GET", "/catalog/files/99999", nil); code != 404 {
		t.Errorf("unknown file: %d", code)
	}
	e.call(t, 204, "DELETE", fmt.Sprintf("/tiers/flags/%d", f.ID), nil, nil)
	if code, _ := e.status(t, "DELETE", fmt.Sprintf("/tiers/flags/%d", f.ID), nil); code != 404 {
		t.Errorf("second delete: %d", code)
	}
}

func TestReleaseSyncChecks(t *testing.T) {
	e := newEnv(t, nil)
	src := e.mkdir(t, "media")
	writeFile(t, filepath.Join(src, "a.mkv"), "aaaa", time.Now())
	srcID := e.createSource(t, "Media", src)
	destID := e.createDestination(t, "UNAS", e.mkdir(t, "target"), []int64{srcID}, nil)
	other := e.createDestination(t, "Other", e.mkdir(t, "target2"), []int64{srcID}, nil)
	e.call(t, 200, "PUT", "/tiers/rules", map[string]any{"revision": 0, "rules": []map[string]any{
		{"name": "only there", "action": "manifest", "destinationIds": []int64{destID, other}}}}, nil)
	sync := fmt.Sprintf("/destinations/%d/sync", destID)
	if code, _ := e.status(t, "POST", sync, map[string]any{"releaseOf": 3}); code != 400 {
		t.Errorf("releaseOf without releaseDemoted: %d", code)
	}
	if code, _ := e.status(t, "POST", sync, map[string]any{"releaseDemoted": true}); code != 400 {
		t.Errorf("a real release without its preview: %d", code)
	}
	if code, msg := e.status(t, "POST", sync, map[string]any{"releaseDemoted": true, "releaseOf": 999, "releaseRevision": 1}); code != 409 ||
		!strings.Contains(msg, "rules changed since the preview") {
		t.Errorf("unknown preview: %d %q", code, msg)
	}
	var preview jobs.Job
	e.call(t, 202, "POST", sync, map[string]any{"dryRun": true, "releaseDemoted": true}, &preview)
	preview = e.waitJob(t, preview.ID)
	var st struct {
		TierRevision int64 `json:"tierRevision"`
	}
	if err := json.Unmarshal(preview.Stats, &st); err != nil || st.TierRevision != 1 {
		t.Fatalf("preview stats %s", preview.Stats)
	}
	// The preview of another destination, a stale revision, and rules saved since are refused.
	if code, _ := e.status(t, "POST", fmt.Sprintf("/destinations/%d/sync", other), map[string]any{"releaseDemoted": true,
		"releaseOf": preview.ID, "releaseRevision": 1}); code != 409 {
		t.Errorf("another destination's preview: %d", code)
	}
	if code, _ := e.status(t, "POST", sync, map[string]any{"releaseDemoted": true, "releaseOf": preview.ID, "releaseRevision": 2}); code != 409 {
		t.Errorf("wrong revision: %d", code)
	}
	var ok jobs.Job
	e.call(t, 202, "POST", sync, map[string]any{"releaseDemoted": true, "allowChanges": true, "releaseOf": preview.ID, "releaseRevision": 1}, &ok)
	if !ok.Params.ReleaseDemoted || ok.Params.ReleaseOf != preview.ID || ok.Params.ReleaseRevision != 1 || !ok.Params.AllowChanges {
		t.Fatalf("release job %+v", ok.Params)
	}
	e.waitJob(t, ok.ID)
	e.call(t, 200, "PUT", "/tiers/rules", map[string]any{"revision": 1, "rules": []any{}}, nil)
	if code, _ := e.status(t, "POST", sync, map[string]any{"releaseDemoted": true, "releaseOf": preview.ID, "releaseRevision": 1}); code != 409 {
		t.Errorf("rules changed since the preview: %d", code)
	}

	// Deleting a destination removes it from every rule's "Applies at".
	e.call(t, 200, "PUT", "/tiers/rules", map[string]any{"revision": 2, "rules": []map[string]any{
		{"name": "only there", "action": "manifest", "destinationIds": []int64{other}}}}, nil)
	e.call(t, 204, "DELETE", fmt.Sprintf("/destinations/%d", other), nil, nil)
	var rs tiers.RuleSet
	e.call(t, 200, "GET", "/tiers/rules", nil, &rs)
	if rs.Rules[0].DestinationIDs == nil || len(rs.Rules[0].DestinationIDs) != 0 {
		t.Errorf("destination ids after the delete: %v", rs.Rules[0].DestinationIDs)
	}
}

// TestJobItemTierFilters: GET /jobs/{id}/items filters by the tier and the deciding rule each item
// records, and /items/summary?by=tier adds the tier dimension (design §13).
func TestJobItemTierFilters(t *testing.T) {
	e := newEnv(t, nil)
	src := e.mkdir(t, "media")
	writeFile(t, filepath.Join(src, "small.mkv"), "aaaa", time.Now())
	writeFile(t, filepath.Join(src, "big.mkv"), "bbbbbbbbbb", time.Now())
	srcID := e.createSource(t, "Media", src)
	destID := e.createDestination(t, "UNAS", e.mkdir(t, "target"), []int64{srcID}, nil)
	var saved TierRulesSaved
	e.call(t, 200, "PUT", "/tiers/rules", map[string]any{"revision": 0, "rules": []map[string]any{
		{"name": "big files", "action": "manifest", "conditions": []map[string]any{{"field": "file.size", "op": "gt", "value": 5}}}}}, &saved)
	ruleID := saved.Rules[0].ID
	var dry jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/sync", destID), map[string]any{"dryRun": true}, &dry)
	dry = e.waitJob(t, dry.ID)
	jp := fmt.Sprintf("/jobs/%d", dry.ID)
	paths := func(q string) []string {
		t.Helper()
		var page jobqueue.Page[jobs.Item]
		e.call(t, 200, "GET", jp+"/items"+q, nil, &page)
		var out []string
		for _, it := range page.Records {
			out = append(out, string(it.Action)+" "+path.Base(it.RelPath))
		}
		return out
	}
	for q, want := range map[string][]string{
		"?tier=manifest":                     {"skip big.mkv"},
		"?tier=full":                         {"copy small.mkv"},
		"?tier=skip":                         nil,
		fmt.Sprintf("?ruleId=%d", ruleID):    {"skip big.mkv"},
		"?ruleId=0":                          {"copy small.mkv"},
		"?tier=manifest&action=copy":         nil,
		fmt.Sprintf("?ruleId=%d", ruleID+99): nil,
	} {
		if got := paths(q); !slices.Equal(got, want) {
			t.Errorf("items%s = %v, want %v", q, got, want)
		}
	}
	var counts []jobqueue.TierItemCount
	e.call(t, 200, "GET", jp+"/items/summary?by=tier", nil, &counts)
	byTier := map[string]int64{}
	for _, c := range counts {
		byTier[c.Tier+" "+string(c.Action)] += c.Files
	}
	if byTier["full copy"] != 1 || byTier["manifest skip"] != 1 || len(byTier) != 2 {
		t.Errorf("summary by tier %+v", counts)
	}
	var plain []map[string]any
	e.call(t, 200, "GET", jp+"/items/summary", nil, &plain)
	for _, c := range plain {
		if _, ok := c["tier"]; ok {
			t.Errorf("the plain summary has a tier: %v", c)
		}
	}
	for _, q := range []string{"/items?tier=gold", "/items?ruleId=x", "/items/summary?by=size"} {
		if code, _ := e.status(t, "GET", jp+q, nil); code != 400 {
			t.Errorf("GET %s: %d, want 400", q, code)
		}
	}
}

// The Library item view lists Tautulli, Seerr and Maintainerr as unknown only when such an
// integration is set up: without one every file would carry the warning.
func TestFileDetailUnknownFactsOnlyForSetUpIntegrations(t *testing.T) {
	e := newEnv(t, nil)
	src := e.mkdir(t, "media")
	writeFile(t, filepath.Join(src, "a.mkv"), "aaaa", time.Now())
	srcID := e.createSource(t, "Media", src)
	e.createDestination(t, "UNAS", e.mkdir(t, "target"), []int64{srcID}, nil)
	var scan jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/sources/%d/scan", srcID), nil, &scan)
	e.waitJob(t, scan.ID)
	var fileID int64
	if err := e.db.Reader().QueryRow(`SELECT id FROM catalog_files WHERE rel_path = 'a.mkv'`).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	sources := func() map[string]string {
		t.Helper()
		var fd FileDetail
		e.call(t, 200, "GET", fmt.Sprintf("/catalog/files/%d", fileID), nil, &fd)
		if fd.Facts.Watch == nil || fd.Facts.Requests == nil || fd.Facts.Maintainerr == nil {
			t.Fatalf("facts %+v", fd.Facts)
		}
		out := map[string]string{}
		for _, u := range fd.Facts.Unknown {
			out[u.Source] = u.Reason
		}
		return out
	}
	got := sources()
	for _, name := range []string{tiers.SourceTautulli, tiers.SourceSeerr, tiers.SourceMaintainerr} {
		if _, ok := got[name]; ok {
			t.Errorf("%s listed as unknown with no integration set up: %v", name, got)
		}
	}
	// A set-up integration whose facts are unknown (here: disabled) is listed.
	srr := seerrtest.NewServer(t)
	if code, raw := e.raw(t, "POST", "/integrations", map[string]any{"type": "seerr", "name": "Requests", "url": srr.URL, "apiKey": seerrtest.Key,
		"enabled": false}); code != 201 {
		t.Fatalf("create seerr: %d %s", code, raw)
	}
	got = sources()
	if r, ok := got[tiers.SourceSeerr]; !ok || !strings.Contains(r, "disabled") {
		t.Errorf("seerr unknown: %v", got)
	}
	if _, ok := got[tiers.SourceTautulli]; ok {
		t.Errorf("tautulli listed: %v", got)
	}
}
