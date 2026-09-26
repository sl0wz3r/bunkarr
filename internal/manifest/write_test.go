package manifest

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func ptr[T any](v T) *T { return &v }

// goldenManifest is a destination version with every CSV case: quoting (commas, quotes, a line
// break), the formula guard (=, +, -, @, tab), a multi-episode file, an item without files, an
// unlocated item, extras, a kept skip file, other files, and a record whose source was deleted.
func goldenManifest() *Manifest {
	at := time.Date(2026, 9, 25, 12, 30, 0, 0, time.UTC)
	added := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	full, skip, manifestTier := TierFull, TierSkip, TierManifest
	fallback := &RuleRef{ID: 0, Name: FallbackRuleName}
	return &Manifest{
		Format: FormatName, FormatVersion: FormatVersion, CreatedAt: at, Generator: Generator{Name: "Bunkarr", Version: "1.0.0"},
		Scope: Scope{Kind: ScopeDestination, DestinationID: 2, DestinationName: "UNAS"},
		Job:   &JobRef{ID: 42, QueuedAt: at.Add(-time.Minute)},
		Integrations: []Integration{
			{ID: 1, Type: "radarr", Name: "Radarr", AppVersion: "6.4.4.10685", RefreshedAt: ptr(at.Add(-time.Hour)), Status: "ok", Fresh: true,
				QualityProfiles: []Named{{ID: 6, Name: "HD-1080p"}}, MetadataProfiles: []Named{},
				RootFolders: []RootFolder{{ID: 1, Path: "/movies", Accessible: true, LocalPath: ptr("/media/movies"), SourceID: ptr(int64(3))}},
				Tags:        []Tag{{ID: 1, Label: "bunkarr-full"}}},
			{ID: 4, Type: "sonarr", Name: "Sonarr, \"TV\"", AppVersion: "4.0.20.3014", Status: "failed", Fresh: false, LastError: ptr("GET series: HTTP 500"),
				QualityProfiles: []Named{}, MetadataProfiles: []Named{}, RootFolders: []RootFolder{}, Tags: []Tag{}},
		},
		Sources: []Source{{ID: 3, Name: "Media", DestFolder: "media"}},
		Items: []Item{
			{IntegrationID: 1, Kind: "movie", ArrID: 1, Title: "=HYPERLINK(\"x\")", Year: 1968, ExternalIDs: ExternalIDs{TMDB: 10331, IMDB: "tt0063350"},
				Path: "/movies/Night, of the Living Dead", Located: true, RootFolder: "/movies", QualityProfile: "HD-1080p", Monitored: true,
				Tags: []string{"+sum", "bunkarr-full"}, Genres: []string{"Horror"}, Added: &added, Detail: ItemDetail{MinimumAvailability: "released"},
				Files: []File{{ArrFileID: 4, Path: "/movies/Night, of the Living Dead/night.mkv", RelativePath: "night.mkv", Size: 3412521,
					Quality: "Bluray-1080p", DateAdded: &added, Source: &FileSource{ID: 3, RelPath: "movies/Night, of the Living Dead/night.mkv"},
					Tier: &full, Rule: fallback, Kept: ptr(false), BackedUp: ptr(true), SHA256: ptr(strings.Repeat("ab", 32))}},
				ExtraFiles: []ExtraFile{
					{Source: ptr(int64(3)), RelPath: "movies/Night, of the Living Dead/night.en.srt", Size: 1200, Tier: &full, Kept: ptr(false), BackedUp: ptr(true)},
					{Source: ptr(int64(3)), RelPath: "movies/Night, of the Living Dead/-poster.jpg", Size: 300, Tier: &skip, Kept: ptr(true), BackedUp: ptr(true)},
				},
				SkippedFiles: 1},
			{IntegrationID: 1, Kind: "movie", ArrID: 4, Title: "The General", Year: 1926, ExternalIDs: ExternalIDs{TMDB: 961},
				Path: "/movies/The General (1926)", Located: true, RootFolder: "/movies", QualityProfile: "HD-1080p", Tags: []string{}, Genres: []string{},
				Files: []File{}, ExtraFiles: []ExtraFile{}},
			{IntegrationID: 1, Kind: "movie", ArrID: 5, Title: "Nosferatu", Year: 1922, ExternalIDs: ExternalIDs{TMDB: 653},
				Path: "/movies-4k/Nosferatu (1922)", Located: false, RootFolder: "/movies-4k", QualityProfile: "@Ultra-HD", Monitored: true,
				Tags: []string{}, Genres: []string{},
				Files: []File{{ArrFileID: 5, Path: "/movies-4k/Nosferatu (1922)/n.mkv", RelativePath: "n.mkv", Size: 5000, Quality: "Bluray-2160p",
					Kept: ptr(false), BackedUp: ptr(false)}},
				ExtraFiles: []ExtraFile{}},
			{IntegrationID: 4, Kind: "series", ArrID: 1, Title: "The Beverly Hillbillies", Year: 1962,
				ExternalIDs: ExternalIDs{TVDB: 71471, TMDB: 1930, IMDB: "tt0055662"}, Path: "/tv/The Beverly Hillbillies", Located: true,
				RootFolder: "/tv", QualityProfile: "HD-1080p", Monitored: true, Tags: []string{"line\nbreak", "\ttab"}, Genres: []string{},
				Detail: ItemDetail{SeriesType: "standard", SeasonFolder: ptr(true), MonitorNewItems: "all", UseSceneNumbering: ptr(false), LanguageProfileID: 1,
					Seasons: []Season{{0, false}, {1, true}}, Episodes: []Episode{{1, 4, true}, {1, 5, true}}},
				Files: []File{{ArrFileID: 7, Path: "/tv/The Beverly Hillbillies/Season 1/S01E04-E05.mkv", RelativePath: "Season 1/S01E04-E05.mkv",
					Size: 941725, Quality: "HDTV-720p", Episodes: []FileEpisode{{1, 4}, {1, 5}},
					Source: &FileSource{ID: 3, RelPath: "tv/The Beverly Hillbillies/Season 1/S01E04-E05.mkv"},
					Tier:   &manifestTier, Rule: &RuleRef{ID: 2, Name: "Manifest by default"}, Kept: ptr(false), BackedUp: ptr(false)}},
				ExtraFiles: []ExtraFile{}},
		},
		OtherFiles: []ExtraFile{
			{Source: ptr(int64(3)), RelPath: "-rf/birthday.mp4", Size: 5000, Tier: &full, Kept: ptr(false), BackedUp: ptr(false)},
			{Source: nil, RelPath: "old source/file.mkv", Size: 70, Kept: ptr(false), BackedUp: ptr(true)},
		},
		Summary: Summary{Items: 4, UnlocatedItems: 1, Files: 7, Bytes: 3412521 + 1200 + 300 + 5000 + 941725 + 5000 + 70,
			Tiers: TierCounts{Full: Count{3, 3412521 + 1200 + 5000}, Manifest: Count{1, 941725}, Skip: Count{1, 300}}, KeptFiles: 1,
			BackedUpBytes: 3412521 + 1200 + 300 + 70, LeftOut: 1},
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden file (run with -update after checking):\n%s", name, got)
	}
}

func TestGoldenJSON(t *testing.T) {
	data, err := EncodeJSON(goldenManifest())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "golden.json", data)
	m, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := EncodeJSON(m)
	if !bytes.Equal(again, data) {
		t.Fatal("Parse then WriteJSON does not reproduce the file")
	}
}

func TestGoldenCSV(t *testing.T) {
	data, err := EncodeCSV(goldenManifest())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "golden.csv", data)
	if !bytes.Contains(data, []byte("\r\n")) {
		t.Fatal("rows are not CRLF-terminated (RFC 4180)")
	}
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		t.Fatalf("the CSV does not parse: %v", err)
	}
	// Header, movie 1 (file + 2 extras), movie 4 (no files: one row), movie 5, the series'
	// multi-episode file, 2 other files.
	if len(rows) != 1+3+1+1+1+2 {
		t.Fatalf("%d rows", len(rows))
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	if strings.Join(rows[0], ",") != strings.Join(CSVHeader, ",") {
		t.Fatalf("header %v", rows[0])
	}
	movie := rows[1]
	if movie[col["title"]] != `'=HYPERLINK("x")` || movie[col["tags"]] != "'+sum;bunkarr-full" || movie[col["arrPath"]] != "/movies/Night, of the Living Dead/night.mkv" ||
		movie[col["tier"]] != "full" || movie[col["backedUp"]] != "true" || movie[col["source"]] != "Media" {
		t.Fatalf("movie row %q", movie)
	}
	if rows[3][col["relPath"]] != "movies/Night, of the Living Dead/-poster.jpg" || rows[3][col["kept"]] != "true" {
		t.Fatalf("extra row %q", rows[3])
	}
	general := rows[4]
	if general[col["arrPath"]] != "/movies/The General (1926)" || general[col["size"]] != "" || general[col["located"]] != "true" {
		t.Fatalf("item row %q", general)
	}
	nos := rows[5]
	if nos[col["qualityProfile"]] != "'@Ultra-HD" || nos[col["located"]] != "false" || nos[col["source"]] != "" || nos[col["tier"]] != "" || nos[col["backedUp"]] != "false" {
		t.Fatalf("unlocated row %q", nos)
	}
	ep := rows[6]
	if ep[col["season"]] != "1" || ep[col["episode"]] != "4;5" || ep[col["tags"]] != "line\nbreak;\ttab" || ep[col["integration"]] != `Sonarr, "TV"` {
		t.Fatalf("multi-episode row %q", ep)
	}
	if rows[7][col["relPath"]] != "'-rf/birthday.mp4" || rows[7][col["kind"]] != "" {
		t.Fatalf("other file row %q", rows[7])
	}
	orphan := rows[8]
	if orphan[col["integration"]] != "" || orphan[col["source"]] != "" || orphan[col["relPath"]] != "old source/file.mkv" || orphan[col["tier"]] != "" {
		t.Fatalf("other file row %q", orphan)
	}
}

func TestGuardCell(t *testing.T) {
	for in, want := range map[string]string{
		"":        "",
		"=1+1":    "'=1+1",
		"+1":      "'+1",
		"-1":      "'-1",
		"@SUM":    "'@SUM",
		"\tx":     "'\tx",
		"\rx":     "'\rx",
		"plain":   "plain",
		" =space": " =space",
		"a=b":     "a=b",
	} {
		if got := guardCell(in); got != want {
			t.Errorf("guardCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSums(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	ok, err := ParseSums(FormatSums(a, b))
	if err != nil || ok[JSONName] != a || ok[CSVName] != b {
		t.Fatalf("ParseSums = %v, %v", ok, err)
	}
	for name, in := range map[string]string{
		"empty":      "",
		"one file":   a + "  manifest.json\n",
		"other name": a + "  manifest.json\n" + b + "  other.csv\n",
		"twice":      a + "  manifest.json\n" + a + "  manifest.json\n" + b + "  manifest.csv\n",
		"short hash": "abc  manifest.json\n" + b + "  manifest.csv\n",
		"upper hex":  strings.ToUpper(a) + "  manifest.json\n" + b + "  manifest.csv\n",
		"not hex":    strings.Repeat("z", 64) + "  manifest.json\n" + b + "  manifest.csv\n",
		"one space":  a + " manifest.json\n" + b + "  manifest.csv\n",
	} {
		if _, err := ParseSums([]byte(in)); !errors.Is(err, ErrDamaged) {
			t.Errorf("%s: err %v", name, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	good, _ := EncodeJSON(goldenManifest())
	for name, in := range map[string]string{
		"not json":      "nope",
		"other format":  strings.Replace(string(good), FormatName, "other", 1),
		"version 2":     strings.Replace(string(good), `"formatVersion": 1`, `"formatVersion": 2`, 1),
		"bad scope":     strings.Replace(string(good), `"kind": "destination"`, `"kind": "galaxy"`, 1),
		"trailing data": string(good) + "{}",
		"array":         "[]",
	} {
		if _, err := Parse(strings.NewReader(in)); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err %v", name, err)
		}
	}
	// Unknown fields (a later writer's additions) are ignored.
	extra := strings.Replace(string(good), `"format":`, `"futureField": {"a": 1}, "format":`, 1)
	if _, err := Parse(strings.NewReader(extra)); err != nil {
		t.Fatalf("an unknown field: %v", err)
	}
}

func TestContentHash(t *testing.T) {
	m := goldenManifest()
	base, err := ContentHash(m)
	if err != nil || !strings.HasPrefix(base, "sha256:") {
		t.Fatalf("ContentHash = %q, %v", base, err)
	}
	// Parsed back, the hash is the same.
	data, _ := EncodeJSON(m)
	parsed, _ := Parse(bytes.NewReader(data))
	if h, _ := ContentHash(parsed); h != base {
		t.Fatalf("parsed hash %s, built %s", h, base)
	}
	same := []func(*Manifest){
		func(m *Manifest) { m.CreatedAt = m.CreatedAt.Add(time.Hour) },
		func(m *Manifest) { m.Job = &JobRef{ID: 99} },
		func(m *Manifest) { m.Integrations[0].RefreshedAt = nil },
		func(m *Manifest) { m.Integrations[0].AppVersion = "7" },
		func(m *Manifest) { m.Integrations[1].LastError = nil },
	}
	for i, f := range same {
		c := goldenManifest()
		f(c)
		if h, _ := ContentHash(c); h != base {
			t.Errorf("change %d changed the content hash", i)
		}
		if c.Integrations[0].AppVersion == "" && i == 3 {
			t.Fatal("ContentHash modified its argument")
		}
	}
	changed := []func(*Manifest){
		func(m *Manifest) { m.Integrations[0].Fresh = false },
		func(m *Manifest) { m.Integrations[0].Status = "failed" },
		func(m *Manifest) { m.Items[0].Files[0].BackedUp = ptr(false) },
		func(m *Manifest) { m.Items[1].Monitored = true },
		func(m *Manifest) { m.OtherFiles = m.OtherFiles[:1] },
	}
	for i, f := range changed {
		c := goldenManifest()
		f(c)
		if h, _ := ContentHash(c); h == base {
			t.Errorf("change %d did not change the content hash", i)
		}
	}
	if m.Integrations[0].AppVersion != "6.4.4.10685" || m.Job == nil {
		t.Fatal("ContentHash modified its argument")
	}
}

// referenceJSON is manifest.json as one json.Encoder call writes it (what WriteJSON streams).
func referenceJSON(t *testing.T, m *Manifest) []byte {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// referenceHash is ContentHash as json.Marshal of the whole canonical manifest computes it.
func referenceHash(t *testing.T, m *Manifest) string {
	t.Helper()
	c := *m
	c.CreatedAt, c.Job = time.Time{}, nil
	c.Integrations = make([]Integration, len(m.Integrations))
	for i, it := range m.Integrations {
		it.RefreshedAt, it.AppVersion, it.LastError = nil, "", nil
		c.Integrations[i] = it
	}
	b, err := json.Marshal(&c)
	if err != nil {
		t.Fatal(err)
	}
	return Checksum(SHA256Hex(b))
}

// bigManifest is a destination version of a library of n movies with two files and two extras
// each, and n other files.
func bigManifest(n int) *Manifest {
	m := goldenManifest()
	full := TierFull
	tmpl := m.Items[0]
	m.Items = m.Items[:0]
	m.OtherFiles = m.OtherFiles[:0]
	for i := range n {
		it := tmpl
		it.ArrID, it.Title = int64(i+1), fmt.Sprintf("Movie <%d> & \"friends\"", i)
		it.Files = []File{tmpl.Files[0], tmpl.Files[0]}
		it.ExtraFiles = []ExtraFile{tmpl.ExtraFiles[0], tmpl.ExtraFiles[1]}
		m.Items = append(m.Items, it)
		m.OtherFiles = append(m.OtherFiles, ExtraFile{Source: ptr(int64(3)), RelPath: fmt.Sprintf("home videos/%d.mp4", i), Size: int64(i),
			Tier: &full, Kept: ptr(false), BackedUp: ptr(true)})
	}
	return m
}

func TestWriteJSONAndContentHashMatchEncodingJSON(t *testing.T) {
	// WriteJSON and ContentHash stream the manifest; the bytes are encoding/json's, whatever the
	// manifest holds: no job, nil and empty lists, HTML characters, one element or many.
	cases := map[string]func() *Manifest{
		"golden": goldenManifest,
		"export": func() *Manifest {
			m := goldenManifest()
			m.Job, m.Scope = nil, Scope{Kind: ScopeExport}
			return m
		},
		"nil lists": func() *Manifest {
			m := goldenManifest()
			m.Integrations, m.Sources, m.Items, m.OtherFiles = nil, []Source{}, nil, []ExtraFile{}
			return m
		},
		"one of each": func() *Manifest {
			m := goldenManifest()
			m.Items, m.OtherFiles = m.Items[:1], m.OtherFiles[:1]
			return m
		},
		"big":   func() *Manifest { return bigManifest(50) },
		"empty": func() *Manifest { return &Manifest{} },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			m := mk()
			got, err := EncodeJSON(m)
			if err != nil {
				t.Fatal(err)
			}
			if want := referenceJSON(t, m); !bytes.Equal(got, want) {
				t.Fatalf("WriteJSON differs from encoding/json:\n%s\nwant:\n%s", got, want)
			}
			h, err := ContentHash(m)
			if err != nil {
				t.Fatal(err)
			}
			if want := referenceHash(t, m); h != want {
				t.Fatalf("ContentHash %s, encoding/json %s", h, want)
			}
		})
	}
}

// maxWriter records the largest single write.
type maxWriter struct{ n, max int }

func (w *maxWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	w.max = max(w.max, len(p))
	return len(p), nil
}

// allocated returns the bytes f allocates.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestWriteJSONAndContentHashStream(t *testing.T) {
	// A large library's manifest is written in pieces and never held in memory whole: WriteJSON
	// never hands its writer the whole document, and neither it nor ContentHash allocates
	// anything near the document's size.
	m := bigManifest(3000)
	var w maxWriter
	if err := WriteJSON(&w, m); err != nil {
		t.Fatal(err)
	}
	if w.n < 4<<20 {
		t.Fatalf("the test manifest is only %d bytes", w.n)
	}
	if w.max > 64<<10 {
		t.Fatalf("WriteJSON wrote %d of %d bytes in one write", w.max, w.n)
	}
	if raceEnabled {
		return // allocation counts are meaningless under the race detector
	}
	size := uint64(w.n)
	if a := allocated(func() {
		if err := WriteJSON(io.Discard, m); err != nil {
			t.Fatal(err)
		}
	}); a > size/4 {
		t.Fatalf("WriteJSON allocated %d bytes for a %d-byte manifest", a, size)
	}
	if a := allocated(func() {
		if _, err := ContentHash(m); err != nil {
			t.Fatal(err)
		}
	}); a > size/4 {
		t.Fatalf("ContentHash allocated %d bytes for a %d-byte manifest", a, size)
	}
}
