package plexdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// maxReportedErrors bounds the problem lines an IntegrityReport keeps.
const maxReportedErrors = 50

// IntegrityReport is the result of Verify (ADR 0005). It is stored in the version's manifest.
type IntegrityReport struct {
	// QuickCheck is "ok" when PRAGMA quick_check returned exactly "ok", else "failed".
	QuickCheck string `json:"quickCheck"`
	// IntegrityCheck is "ok" when PRAGMA integrity_check(100000000) returned nothing but "ok" and
	// ignored lines, else "failed".
	IntegrityCheck string `json:"integrityCheck"`
	// IgnoredLines counts the "row N missing from index X" lines ignored because index X uses a
	// collation only stubbed here (IgnoredIndexes): their order cannot be checked without Plex's
	// ICU collation.
	IgnoredLines   int      `json:"ignoredLines"`
	IgnoredIndexes []string `json:"ignoredIndexes"`
	// Collations are the non-built-in collations of the schema (stubbed for the checks).
	Collations []string `json:"collations"`
	// SQLiteVersion is the version of the SQLite that ran the checks.
	SQLiteVersion string `json:"sqliteVersion"`
	// PlexLibrary reports whether the database has Plex's metadata_items and media_parts tables;
	// MetadataItems and MediaParts are their row counts (0 when absent).
	PlexLibrary   bool  `json:"plexLibrary"`
	MetadataItems int64 `json:"metadataItems"`
	MediaParts    int64 `json:"mediaParts"`
	// Errors are the problems found: every kept integrity line, failed queries (at most 50, then
	// a count of the rest).
	Errors []string `json:"errors"`

	dropped int
}

// OK reports whether the database passed: both checks ok and no errors.
func (r IntegrityReport) OK() bool {
	return r.QuickCheck == IntegrityOK && r.IntegrityCheck == IntegrityOK && len(r.Errors) == 0
}

// Result is IntegrityOK or IntegrityFailed.
func (r IntegrityReport) Result() string {
	if r.OK() {
		return IntegrityOK
	}
	return IntegrityFailed
}

// addError records a problem line, keeping at most maxReportedErrors.
func (r *IntegrityReport) addError(msg string) {
	if len(r.Errors) < maxReportedErrors {
		r.Errors = append(r.Errors, msg)
		return
	}
	r.dropped++
}

// finish appends the count of dropped lines.
func (r *IntegrityReport) finish() {
	if r.dropped > 0 {
		r.Errors = append(r.Errors, fmt.Sprintf("… and %d more", r.dropped))
		r.dropped = 0
	}
}

// Verify checks a copy of a Plex database (ADR 0005). The file is opened
// "mode=ro&immutable=1", so nothing is created next to it; it must not be a live database.
//
//  1. The schema is read and every collation that is not SQLite's own (binary, nocase, rtrim) is
//     collected: from COLLATE clauses anywhere in sqlite_master and from each index's effective
//     key collations (PRAGMA index_xinfo, which also covers a column's declared collation).
//     A binary-compare stub is registered for each (once per process).
//  2. PRAGMA quick_check must return "ok".
//  3. PRAGMA integrity_check(100000000) (the raised limit keeps ignored lines from crowding out
//     real ones) must return nothing but "ok" and lines "row N missing from index X" where X uses
//     one of those collations; every other line fails the check.
//  4. The rows of metadata_items and media_parts are counted when the tables exist.
//
// Problems SQLite reports (a corrupt page, a truncated file, not a database) and an empty file
// make a failed report, not an error. The error is for a file that cannot be examined at all (missing, not a
// regular file) and for cancellation (ctx.Err()).
func Verify(ctx context.Context, path string) (rep IntegrityReport, err error) {
	rep = IntegrityReport{QuickCheck: IntegrityFailed, IntegrityCheck: IntegrityFailed,
		IgnoredIndexes: []string{}, Collations: []string{}, Errors: []string{}}
	defer rep.finish()
	fi, err := os.Lstat(path)
	if err != nil {
		return rep, fmt.Errorf("verify: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return rep, fmt.Errorf("verify %s: %w", path, filecopy.ErrNotRegular)
	}
	if fi.Size() == 0 {
		// SQLite treats an empty file as an empty database; a backup never is one.
		rep.addError("the file is empty")
		return rep, nil
	}
	uri, err := fileURI(path, "mode=ro&immutable=1")
	if err != nil {
		return rep, err
	}
	sch, err := readSchema(ctx, uri)
	if err != nil {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		rep.addError("read the schema: " + err.Error())
		return rep, nil
	}
	rep.Collations = sch.collations
	rep.PlexLibrary = sch.tables["metadata_items"] && sch.tables["media_parts"]
	if err := registerStubs(sch.collations); err != nil {
		return rep, err
	}

	db := openDB(uri)
	defer db.Close()
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&rep.SQLiteVersion); err != nil {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		rep.addError("open: " + err.Error())
		return rep, nil
	}

	quick, err := pragmaLines(ctx, db, "PRAGMA quick_check")
	switch {
	case ctx.Err() != nil:
		return rep, ctx.Err()
	case err != nil:
		rep.addError("quick_check: " + err.Error())
	case len(quick) == 1 && quick[0] == "ok":
		rep.QuickCheck = IntegrityOK
	default:
		for _, l := range quick {
			rep.addError("quick_check: " + l)
		}
	}

	lines, err := pragmaLines(ctx, db, "PRAGMA integrity_check(100000000)")
	switch {
	case ctx.Err() != nil:
		return rep, ctx.Err()
	case err != nil:
		rep.addError("integrity_check: " + err.Error())
	default:
		kept, ignored, indexes := filterIntegrity(lines, sch.customIndexes)
		rep.IgnoredLines, rep.IgnoredIndexes = ignored, indexes
		if len(kept) == 0 {
			rep.IntegrityCheck = IntegrityOK
		}
		for _, l := range kept {
			rep.addError("integrity_check: " + l)
		}
	}

	if rep.PlexLibrary {
		for table, dst := range map[string]*int64{"metadata_items": &rep.MetadataItems, "media_parts": &rep.MediaParts} {
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(dst); err != nil {
				if ctx.Err() != nil {
					return rep, ctx.Err()
				}
				rep.addError(fmt.Sprintf("count %s: %v", table, err))
			}
		}
	}
	return rep, nil
}

// QuickCheck runs PRAGMA quick_check on a copy of a SQLite database that is not Plex's, such as
// an *arr's database extracted from its backup zip (docs/design/phase2-3.md §10 step 6). Like
// Verify, it opens the file "mode=ro&immutable=1" (nothing is created next to it; it must not be
// a live database) on this package's private driver, after registering a binary-compare stub for
// every collation of the schema that SQLite does not have, so an unknown collation is never
// reported as damage.
//
// It returns the problem lines, at most 50 and then a count of the rest; an empty list means the
// database passed. Problems SQLite reports (a corrupt page, a truncated file, not a database) and
// an empty file are problems, not errors. The error is for a file that cannot be examined at all
// (missing, not a regular file) and for cancellation (ctx.Err()).
func QuickCheck(ctx context.Context, path string) ([]string, error) {
	rep := IntegrityReport{Errors: []string{}}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("quick check: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("quick check %s: %w", path, filecopy.ErrNotRegular)
	}
	if fi.Size() == 0 {
		return []string{"the file is empty"}, nil
	}
	uri, err := fileURI(path, "mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	sch, err := readSchema(ctx, uri)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []string{"read the schema: " + err.Error()}, nil
	}
	if err := registerStubs(sch.collations); err != nil {
		return nil, err
	}
	db := openDB(uri)
	defer db.Close()
	lines, err := pragmaLines(ctx, db, "PRAGMA quick_check")
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		rep.addError("quick_check: " + err.Error())
	case len(lines) == 1 && lines[0] == "ok":
	case len(lines) == 0:
		rep.addError("quick_check: no result")
	default:
		for _, l := range lines {
			rep.addError("quick_check: " + l)
		}
	}
	rep.finish()
	return rep.Errors, nil
}

// missingRe matches the integrity_check line a stub collation causes.
var missingRe = regexp.MustCompile(`^row \d+ missing from index (.+)$`)

// filterIntegrity drops "ok" and the "row N missing from index X" lines for indexes in custom
// (whose collation is a stub). It returns the remaining lines, the number of ignored lines and
// the indexes they named, sorted.
func filterIntegrity(lines []string, custom map[string]bool) (kept []string, ignored int, indexes []string) {
	seen := map[string]bool{}
	indexes = []string{}
	for _, l := range lines {
		if l == "ok" {
			continue
		}
		if m := missingRe.FindStringSubmatch(l); m != nil && custom[m[1]] {
			ignored++
			if !seen[m[1]] {
				seen[m[1]] = true
				indexes = append(indexes, m[1])
			}
			continue
		}
		kept = append(kept, l)
	}
	slices.Sort(indexes)
	return kept, ignored, indexes
}

// schemaInfo is what Verify needs from a database's schema.
type schemaInfo struct {
	// collations are the non-built-in collation names (lowercase, sorted).
	collations []string
	// customIndexes are the indexes with at least one key column in such a collation.
	customIndexes map[string]bool
	// tables are the table names (lowercase).
	tables map[string]bool
}

// collateRe finds COLLATE clauses; the name may be bare or quoted "…", '…', `…` or […].
var collateRe = regexp.MustCompile("(?i)\\bCOLLATE\\s+(?:\"((?:[^\"]|\"\")+)\"|'((?:[^']|'')+)'|`([^`]+)`|\\[([^\\]]+)\\]|([A-Za-z_][A-Za-z0-9_$]*))")

// collateName returns the collation name of a collateRe match, unquoted.
func collateName(m []string) string {
	switch {
	case m[1] != "":
		return strings.ReplaceAll(m[1], `""`, `"`)
	case m[2] != "":
		return strings.ReplaceAll(m[2], `''`, `'`)
	case m[3] != "":
		return m[3]
	case m[4] != "":
		return m[4]
	}
	return m[5]
}

// readSchema reads the schema of the database at uri without any stub registered (collations are
// only looked up when a statement uses an index, never while the schema is loaded).
func readSchema(ctx context.Context, uri string) (schemaInfo, error) {
	db := openDB(uri)
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT type, name, coalesce(sql, '') FROM sqlite_master`)
	if err != nil {
		return schemaInfo{}, err
	}
	info := schemaInfo{customIndexes: map[string]bool{}, tables: map[string]bool{}}
	colls := map[string]bool{}
	var indexes []string
	for rows.Next() {
		var typ, name, text string
		if err := rows.Scan(&typ, &name, &text); err != nil {
			_ = rows.Close()
			return schemaInfo{}, err
		}
		custom := false
		for _, m := range collateRe.FindAllStringSubmatch(text, -1) {
			if c := collateName(m); !isBuiltinCollation(c) {
				colls[strings.ToLower(c)] = true
				custom = true
			}
		}
		switch typ {
		case "index":
			indexes = append(indexes, name)
			if custom {
				info.customIndexes[name] = true
			}
		case "table":
			info.tables[strings.ToLower(name)] = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return schemaInfo{}, err
	}
	if err := rows.Close(); err != nil {
		return schemaInfo{}, err
	}
	for _, idx := range indexes {
		keyColls, err := indexCollations(ctx, db, idx)
		if err != nil {
			return schemaInfo{}, fmt.Errorf("index %s: %w", idx, err)
		}
		for _, c := range keyColls {
			if c != "" && !isBuiltinCollation(c) {
				colls[strings.ToLower(c)] = true
				info.customIndexes[idx] = true
			}
		}
	}
	info.collations = make([]string, 0, len(colls))
	for c := range colls {
		info.collations = append(info.collations, c)
	}
	slices.Sort(info.collations)
	return info, nil
}

// indexCollations returns the collations of an index's key columns.
func indexCollations(ctx context.Context, db *sql.DB, index string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT coalesce(coll, '') FROM pragma_index_xinfo(?) WHERE key = 1`, index)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// pragmaLines runs a pragma that returns one text column and returns its rows.
func pragmaLines(ctx context.Context, db *sql.DB, q string) ([]string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return out, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
