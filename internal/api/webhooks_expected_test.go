package api

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
	"github.com/sl0wz3r/bunkarr/internal/webhooks"
)

// TestExpectedFilesSingleReadConnection: the expected-files check runs no query while its index
// cursor is open, so it completes with a read pool of one connection (a nested query there waited
// for a second connection, and syncs holding every connection of the pool that way hung).
func TestExpectedFilesSingleReadConnection(t *testing.T) {
	e := newEnv(t, nil)
	_, it, srcID := arrSetup(t, e)
	src, err := e.app.Catalog.Get(context.Background(), srcID)
	if err != nil {
		t.Fatal(err)
	}
	e.db.Reader().SetMaxOpenConns(1)
	t.Cleanup(func() { e.db.Reader().SetMaxOpenConns(4) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exp, err := e.app.expectedFiles(ctx, src, []string{"."})
	if err != nil {
		t.Fatalf("expectedFiles: %v", err)
	}
	if len(exp) != 3 {
		t.Fatalf("expected files = %+v", exp)
	}
	for _, f := range exp {
		if f.App != it.Name || f.Size <= 0 || strings.HasPrefix(f.RelPath, "/") {
			t.Fatalf("expected file %+v (integration %q)", f, it.Name)
		}
	}
}

// TestExpectedFilesSkipExcluded: the files the *arr index expects exclude what the source's
// exclude patterns exclude, exactly as the scanner does: after a scan, every expected file is in
// the catalog and every indexed file left out is not (else each webhook sync would wait for
// them with the locks held and warn that they are not backed up).
func TestExpectedFilesSkipExcluded(t *testing.T) {
	e := newEnv(t, nil)
	_, _, srcID := arrSetup(t, e)
	ctx := context.Background()
	src, err := e.app.Catalog.Get(ctx, srcID)
	if err != nil {
		t.Fatal(err)
	}
	exclude := []string{
		"Charade (1963)/",                  // a folder, by its name
		"/His Girl Friday (1940)/*].mkv",   // a file, by its anchored path
		"Night of the Living Dead*.mkv/",   // folders only: the file stays
		"night of the living dead (1968)/", // own patterns are case-sensitive
	}
	e.call(t, 200, "PUT", fmt.Sprintf("/sources/%d", srcID), map[string]any{"name": src.Name, "path": src.Path, "exclude": exclude}, nil)
	var scan jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/sources/%d/scan", srcID), nil, &scan)
	if j := e.waitJob(t, scan.ID); j.Status != jobs.StatusCompleted {
		t.Fatalf("scan = %+v", j)
	}
	if src, err = e.app.Catalog.Get(ctx, srcID); err != nil || !slices.Equal(src.Exclude, exclude) {
		t.Fatalf("source = %+v, %v", src, err)
	}

	exp, err := e.app.expectedFiles(ctx, src, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range exp {
		got = append(got, f.RelPath)
	}
	want := []string{"Night of the Living Dead (1968)/Night of the Living Dead (1968) [Bluray-1080p].mkv"}
	if !slices.Equal(got, want) {
		t.Fatalf("expected files = %q, want %q", got, want)
	}

	// Parity with the scanner: an indexed file is expected exactly when the catalog lists it.
	var all []catalog.Location
	if err := e.app.Index.FilesUnder(ctx, nil, src.Path, func(f mediaindex.File) error {
		rel, _ := filepath.Rel(src.Path, f.LocalPath)
		all = append(all, catalog.Location{SourceID: srcID, Rel: filepath.ToSlash(rel)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("indexed files = %+v", all)
	}
	live, err := e.app.Catalog.LiveFilesAt(ctx, nil, all)
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range all {
		_, listed := live[loc]
		if expected := slices.Contains(got, loc.Rel); expected != listed {
			t.Errorf("%s: expected %v, in the catalog %v", loc.Rel, expected, listed)
		}
	}
}

// TestSourceExcludesMatchScanner: sourceExcludes decides entries as the catalog's matcher does
// (the table of catalog's TestMatcher), and a file under an excluded folder is excluded.
func TestSourceExcludesMatchScanner(t *testing.T) {
	ex := newSourceExcludes([]string{"*.nfo", "/Extras/", "Samples/", "Show/S01/*.srt", "/top.mkv"})
	for _, tc := range []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{".DS_Store", false, true},
		{"a/b/.ds_store", false, true},
		{"a/Thumbs.db", false, true},
		{"x/._movie.mkv", false, true},
		{"@eaDir", true, true},
		{"a/@eaDir", true, true},
		{"@eaDir", false, false},
		{"#recycle", true, true},
		{"a/.Trash-1000", true, true},
		{"lost+found", true, true},
		{"dl/m.mkv.part", false, true},
		{"dl/m.mkv.PARTIAL", false, true},
		{"dl/m.mkv.!ut", false, true},
		{"dl/m.mkv.partial~", false, true},
		{".grab", true, true},
		{".bunkarr", true, true},
		{"x/.bunkarr-tmp-a.mkv-123", false, true},
		{"movie.mkv", false, false},
		{"a/movie.nfo", false, true},
		{"a/movie.NFO", false, false},
		{"Extras", true, true},
		{"Show/Extras", true, false},
		{"Movie/Samples", true, true},
		{"Movie/Samples", false, false},
		{"Show/S01/e1.srt", false, true},
		{"Show/S02/e1.srt", false, false},
		{"top.mkv", false, true},
		{"sub/top.mkv", false, false},
	} {
		base := tc.rel[strings.LastIndexByte(tc.rel, '/')+1:]
		if got := ex.match(tc.rel, base, tc.isDir); got != tc.want {
			t.Errorf("match(%q, dir=%v) = %v, want %v", tc.rel, tc.isDir, got, tc.want)
		}
	}
	for _, tc := range []struct {
		rel  string
		want bool
	}{
		{"Movie/Samples/s.mkv", true},
		{"Extras/x.mkv", true},
		{"Show/Extras/x.mkv", false},
		{"Show/@EADIR/thumb.jpg", true},
		{"Show/Season 00/e1.mkv", false},
		{"Show/S01/e1.srt", true},
		{"Samples", false},
		{"movie.mkv", false},
	} {
		if got := ex.excluded(tc.rel); got != tc.want {
			t.Errorf("excluded(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

// TestOpenAPIWebhookPayloadSizes: openapi.json states the sizes the webhook intake enforces: the
// largest body read (webhooks.MaxBody) and the largest payload stored as it came
// (webhooks.MaxPayload), and no other stored or cut size anywhere.
func TestOpenAPIWebhookPayloadSizes(t *testing.T) {
	stored := fmt.Sprintf("%d KiB", webhooks.MaxPayload>>10)
	body := fmt.Sprintf("%d MiB", webhooks.MaxBody>>20)
	if webhooks.MaxPayload%(1<<10) != 0 || webhooks.MaxBody%(1<<20) != 0 {
		t.Fatalf("limits %d and %d are not whole KiB and MiB", webhooks.MaxPayload, webhooks.MaxBody)
	}
	var spec map[string]any
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatal(err)
	}
	at := func(keys ...any) string {
		var v any = spec
		for _, k := range keys {
			switch k := k.(type) {
			case string:
				m, _ := v.(map[string]any)
				v = m[k]
			case int:
				a, _ := v.([]any)
				if k >= len(a) {
					return ""
				}
				v = a[k]
			}
		}
		s, _ := v.(string)
		return s
	}
	for name, d := range map[string]string{
		"WebhookEvent.truncated":     at("components", "schemas", "WebhookEvent", "properties", "truncated", "description"),
		"WebhookEventDetail.payload": at("components", "schemas", "WebhookEventDetail", "allOf", 1, "properties", "payload", "description"),
	} {
		if !strings.Contains(d, stored) {
			t.Errorf("%s description %q does not state %s", name, d, stored)
		}
	}
	for _, p := range []string{"/webhook/{app}", "/webhook/{app}/{integrationId}"} {
		d := at("paths", p, "post", "requestBody", "content", "application/json", "schema", "description")
		if !strings.Contains(d, stored) || !strings.Contains(d, body) {
			t.Errorf("POST %s body description %q does not state %s and %s", p, d, body, stored)
		}
	}
	// Every size said to be stored or cut at is the stored limit.
	sizeRe := regexp.MustCompile(`(?i)(\d+ [KMG]iB)[^.;)"]*\bstored|stored[^.;)"]*?(\d+ [KMG]iB)|cut at (\d+ [KMG]iB)`)
	for _, m := range sizeRe.FindAllStringSubmatch(string(openAPISpec), -1) {
		for _, size := range m[1:] {
			if size != "" && size != stored {
				t.Errorf("openapi.json: %q states %s, the intake stores at most %s", m[0], size, stored)
			}
		}
	}
}
