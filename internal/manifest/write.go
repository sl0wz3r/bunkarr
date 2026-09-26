package manifest

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CSVHeader is manifest.csv's header row (design §11.1).
var CSVHeader = []string{"integration", "kind", "arrId", "title", "year", "tmdbId", "tvdbId", "imdbId", "mbid", "season",
	"episode", "qualityProfile", "rootFolder", "monitored", "tags", "arrPath", "located", "source", "relPath", "size",
	"quality", "tier", "kept", "backedUp"}

// WriteJSON writes m as manifest.json: indented, UTF-8 without HTML escaping, one trailing
// newline (what a json.Encoder with SetIndent("", "  ") and SetEscapeHTML(false) writes). It is
// streamed: the whole document is never held in memory.
func WriteJSON(w io.Writer, m *Manifest) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	if err := streamJSON(bw, m, styleFile); err != nil {
		return fmt.Errorf("encode manifest.json: %w", err)
	}
	if err := bw.WriteByte('\n'); err != nil {
		return fmt.Errorf("write manifest.json: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write manifest.json: %w", err)
	}
	return nil
}

// WriteCSV writes m as manifest.csv (design §11.1): RFC 4180 (CRLF, quoting), the CSVHeader
// row, one row per listed file (an item's *arr files, then its extras) plus one row for each item
// that lists no file, then the other files. Tags are joined with ";", and a multi-episode file is
// one row whose episode cell joins the episode numbers with ";" ("4;5"). A cell starting with
// =, +, -, @, a tab or a CR gets a leading "'" (spreadsheet formula guard), so the CSV is
// informational and manifest.json is canonical.
func WriteCSV(w io.Writer, m *Manifest) error {
	bw := bufio.NewWriter(w)
	cw := csv.NewWriter(bw)
	cw.UseCRLF = true
	write := func(rec []string) error {
		for i, c := range rec {
			rec[i] = guardCell(c)
		}
		return cw.Write(rec)
	}
	if err := write(slices.Clone(CSVHeader)); err != nil {
		return fmt.Errorf("encode manifest.csv: %w", err)
	}
	intNames := map[int64]string{}
	for _, it := range m.Integrations {
		intNames[it.ID] = it.Name
	}
	srcNames := map[int64]string{}
	for _, s := range m.Sources {
		srcNames[s.ID] = s.Name
	}
	sourceName := func(id *int64) string {
		if id == nil {
			return ""
		}
		if n, ok := srcNames[*id]; ok {
			return n
		}
		return "#" + strconv.FormatInt(*id, 10)
	}
	for _, it := range m.Items {
		name, ok := intNames[it.IntegrationID]
		if !ok {
			name = "#" + strconv.FormatInt(it.IntegrationID, 10)
		}
		head := []string{name, it.Kind, strconv.FormatInt(it.ArrID, 10), it.Title, optInt(int64(it.Year)),
			optInt(it.ExternalIDs.TMDB), optInt(it.ExternalIDs.TVDB), it.ExternalIDs.IMDB, it.ExternalIDs.MBID}
		profile := []string{it.QualityProfile, it.RootFolder, strconv.FormatBool(it.Monitored), strings.Join(it.Tags, ";")}
		located := strconv.FormatBool(it.Located)
		for _, f := range it.Files {
			season, episode := episodeCells(f.Episodes)
			var src *int64
			rel := ""
			if f.Source != nil {
				id := f.Source.ID
				src, rel = &id, f.Source.RelPath
			}
			rec := concat(head, []string{season, episode}, profile, []string{f.Path, located, sourceName(src), rel,
				strconv.FormatInt(f.Size, 10), f.Quality, tierCell(f.Tier), boolCell(f.Kept), boolCell(f.BackedUp)})
			if err := write(rec); err != nil {
				return fmt.Errorf("encode manifest.csv: %w", err)
			}
		}
		for _, x := range it.ExtraFiles {
			rec := concat(head, []string{"", ""}, profile, []string{"", located, sourceName(x.Source), x.RelPath,
				strconv.FormatInt(x.Size, 10), "", tierCell(x.Tier), boolCell(x.Kept), boolCell(x.BackedUp)})
			if err := write(rec); err != nil {
				return fmt.Errorf("encode manifest.csv: %w", err)
			}
		}
		if len(it.Files) == 0 && len(it.ExtraFiles) == 0 {
			rec := concat(head, []string{"", ""}, profile, []string{it.Path, located, "", "", "", "", "", "", ""})
			if err := write(rec); err != nil {
				return fmt.Errorf("encode manifest.csv: %w", err)
			}
		}
	}
	for _, x := range m.OtherFiles {
		rec := make([]string, 17, len(CSVHeader))
		rec = append(rec, sourceName(x.Source), x.RelPath, strconv.FormatInt(x.Size, 10), "", tierCell(x.Tier), boolCell(x.Kept), boolCell(x.BackedUp))
		if err := write(rec); err != nil {
			return fmt.Errorf("encode manifest.csv: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("encode manifest.csv: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write manifest.csv: %w", err)
	}
	return nil
}

// EncodeJSON returns WriteJSON's output.
func EncodeJSON(m *Manifest) ([]byte, error) {
	var b bytes.Buffer
	if err := WriteJSON(&b, m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// EncodeCSV returns WriteCSV's output.
func EncodeCSV(m *Manifest) ([]byte, error) {
	var b bytes.Buffer
	if err := WriteCSV(&b, m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// guardCell prefixes a cell a spreadsheet would read as a formula with "'".
func guardCell(c string) string {
	if c != "" && strings.ContainsRune("=+-@\t\r", rune(c[0])) {
		return "'" + c
	}
	return c
}

// episodeCells renders a Sonarr file's episodes: the season numbers and the episode numbers,
// each joined with ";" (one season for all but a file spanning seasons).
func episodeCells(eps []FileEpisode) (season, episode string) {
	var seasons, episodes []string
	for _, e := range eps {
		s := strconv.Itoa(e.Season)
		if !slices.Contains(seasons, s) {
			seasons = append(seasons, s)
		}
		episodes = append(episodes, strconv.Itoa(e.Episode))
	}
	return strings.Join(seasons, ";"), strings.Join(episodes, ";")
}

func optInt(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func tierCell(t *Tier) string {
	if t == nil {
		return ""
	}
	return string(*t)
}

func boolCell(b *bool) string {
	if b == nil {
		return ""
	}
	return strconv.FormatBool(*b)
}

func concat(parts ...[]string) []string {
	out := make([]string, 0, len(CSVHeader))
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// SHA256Hex returns the lowercase hex sha256 of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// FormatSums renders SHA256SUMS: "<hex>  manifest.json" and "<hex>  manifest.csv" lines.
func FormatSums(jsonHex, csvHex string) []byte {
	return []byte(jsonHex + "  " + JSONName + "\n" + csvHex + "  " + CSVName + "\n")
}

// ParseSums reads SHA256SUMS: exactly one "<64 lowercase hex>  <name>" line for manifest.json and
// one for manifest.csv, nothing else. It returns the hashes by name.
func ParseSums(data []byte) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 || strings.ToLower(sum) != sum {
			return nil, fmt.Errorf("%w: %s has an invalid line", ErrDamaged, SumsName)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("%w: %s has an invalid line", ErrDamaged, SumsName)
		}
		if (name != JSONName && name != CSVName) || out[name] != "" {
			return nil, fmt.Errorf("%w: %s names an unexpected file", ErrDamaged, SumsName)
		}
		out[name] = sum
	}
	if out[JSONName] == "" || out[CSVName] == "" {
		return nil, fmt.Errorf("%w: %s does not list both files", ErrDamaged, SumsName)
	}
	return out, nil
}

// Checksum is the manifests.checksum form of a hex sha256: "sha256:<hex>".
func Checksum(hexSum string) string { return "sha256:" + hexSum }

// ContentHash returns "sha256:<hex>" of m's canonical content: its JSON without createdAt, job,
// and each integration's refreshedAt, appVersion and lastError (design §11.2 step 3). status and
// fresh stay in, so a cache going stale is a change. Two manifests with the same hash list the
// same library in the same state; a parsed manifest has the hash of the one that was written.
func ContentHash(m *Manifest) (string, error) {
	c := *m
	c.CreatedAt = time.Time{}
	c.Job = nil
	c.Integrations = make([]Integration, len(m.Integrations))
	for i, it := range m.Integrations {
		it.RefreshedAt, it.AppVersion, it.LastError = nil, "", nil
		c.Integrations[i] = it
	}
	// The hash is of json.Marshal's encoding, streamed into the hash.
	h := sha256.New()
	bw := bufio.NewWriterSize(h, 64<<10)
	if err := streamJSON(bw, &c, styleCanon); err != nil {
		return "", fmt.Errorf("hash the manifest: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return "", fmt.Errorf("hash the manifest: %w", err)
	}
	return Checksum(hex.EncodeToString(h.Sum(nil))), nil
}
