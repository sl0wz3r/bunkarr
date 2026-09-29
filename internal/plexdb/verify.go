package plexdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// maxReportedErrors bounds the problem lines an IntegrityReport keeps (and the names in
// IgnoredIndexes).
const maxReportedErrors = 50

// maxLineBytes bounds each problem line a report keeps, and so what reaches the manifest, the job
// log and the job's error: SQLite's lines name tables and indexes, and a crafted schema's names
// can be megabytes long.
const maxLineBytes = 512

// IntegrityReport is the result of Verify (ADR 0005). It is stored in the version's manifest.
type IntegrityReport struct {
	// QuickCheck is "ok" when PRAGMA quick_check returned exactly "ok", else "failed".
	QuickCheck string `json:"quickCheck"`
	// IntegrityCheck is "ok" when PRAGMA integrity_check(100000000) returned nothing but "ok" and
	// ignored lines, else "failed".
	IntegrityCheck string `json:"integrityCheck"`
	// IgnoredLines counts the "row N missing from index X" lines ignored because index X uses a
	// collation only stubbed here (IgnoredIndexes, at most 50): their order cannot be checked
	// without Plex's ICU collation.
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
	// Errors are the problems found: every kept integrity line, failed queries (at most 50 of at
	// most 512 bytes each, then a count of the rest, or "… and more" when a check stopped
	// reading there).
	Errors []string `json:"errors"`

	dropped int
	stopped bool
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

// addError records a problem line (cut to maxLineBytes), keeping at most maxReportedErrors.
func (r *IntegrityReport) addError(msg string) {
	if len(r.Errors) < maxReportedErrors {
		r.Errors = append(r.Errors, cutLine(msg))
		return
	}
	r.dropped++
}

// checkLine records a problem line of a check and reports whether to read on: once the report
// is full, the check stops at its next problem instead of counting the rest, since a crafted
// database can make integrity_check return a line per row.
func (r *IntegrityReport) checkLine(msg string) bool {
	if len(r.Errors) >= maxReportedErrors {
		r.stopped = true
		return false
	}
	r.addError(msg)
	return true
}

// finish appends the count of dropped lines, or "… and more" when a check stopped reading.
func (r *IntegrityReport) finish() {
	switch {
	case r.stopped:
		r.Errors = append(r.Errors, "… and more (the check stopped there)")
	case r.dropped > 0:
		r.Errors = append(r.Errors, fmt.Sprintf("… and %d more", r.dropped))
	}
	r.dropped, r.stopped = 0, false
}

// cutLine returns s cut to at most maxLineBytes on a rune boundary, marked with "…". The result
// is always a new string, so it never keeps a longer line's memory alive.
func cutLine(s string) string {
	if len(s) <= maxLineBytes {
		return strings.Clone(s)
	}
	n := maxLineBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// quickCheck runs PRAGMA quick_check on db and records its problem lines; ok reports whether it
// returned exactly "ok". SQLite cuts each line (substr), so a table name megabytes long is never
// copied into Go once per line.
func (r *IntegrityReport) quickCheck(ctx context.Context, db *sql.DB) (ok bool, err error) {
	lines, bad := 0, false
	err = eachLine(ctx, db, `SELECT substr(quick_check, 1, 512) FROM pragma_quick_check`, func(l string) bool {
		lines++
		if l == "ok" {
			return true
		}
		bad = true
		return r.checkLine("quick_check: " + l)
	})
	if err == nil && lines == 0 {
		r.addError("quick_check: no result")
	}
	return err == nil && lines == 1 && !bad, err
}

// refuseExpressions returns the problem for a schema whose check would run SQL the copy itself
// supplies, row by row, or "": quick_check computes virtual generated columns (NOT NULL and
// STRICT checks) and, when integrity is true, integrity_check also computes index expressions
// and partial-index WHERE clauses. SQLite cannot be interrupted inside one evaluation, and
// modernc interrupts only a statement's first step, so a crafted expression (printf and instr
// on huge strings) could hold a job worker for as long as it likes, across restarts. Plex's and
// the *arrs' schemas have none of these (checked on PMS, Sonarr, Radarr and Lidarr, 2026-09).
func refuseExpressions(sch schemaInfo, integrity bool) string {
	const why = ": checking it would run SQL from the copy itself, so the copy is refused"
	switch {
	case sch.generated != "":
		return "the schema has a virtual generated column (" + sch.generated + ")" + why
	case integrity && sch.indexExpr != "":
		return "the schema has an index on an expression or with a WHERE clause (" + sch.indexExpr + ")" + why
	}
	return ""
}

// Verify checks a copy of a Plex database (ADR 0005). The file is opened
// "mode=ro&immutable=1", so nothing is created next to it; it must not be a live database.
//
//  1. The schema is read and every collation that is not SQLite's own (binary, nocase, rtrim) is
//     collected: from COLLATE clauses anywhere in sqlite_master and from each index's effective
//     key collations (PRAGMA index_xinfo, which also covers a column's declared collation).
//     A schema naming more than 16 such collations, or one with a control character or longer
//     than 64 bytes, fails; so does one whose checks would run its own SQL (refuseExpressions).
//     The checks run on a driver of their own with a binary-compare stub for each collation.
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
	if msg := refuseExpressions(sch, true); msg != "" {
		rep.addError(msg)
		return rep, nil
	}
	drv, err := stubDriver(sch.collations)
	if err != nil {
		return rep, err
	}

	db := openDB(drv, uri)
	defer db.Close()
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&rep.SQLiteVersion); err != nil {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		rep.addError("open: " + err.Error())
		return rep, nil
	}

	ok, err := rep.quickCheck(ctx, db)
	switch {
	case ctx.Err() != nil:
		return rep, ctx.Err()
	case err != nil:
		rep.addError("quick_check: " + err.Error())
	case ok:
		rep.QuickCheck = IntegrityOK
	}

	// Lines are classified as they arrive (a crafted database makes a line per row); the check
	// stops at the first problem once the report is full.
	f := integrityFilter{custom: sch.customIndexes}
	kept := false
	err = eachLine(ctx, db, "PRAGMA integrity_check(100000000)", func(l string) bool {
		if !f.keep(l) {
			return true
		}
		kept = true
		return rep.checkLine("integrity_check: " + l)
	})
	rep.IgnoredLines, rep.IgnoredIndexes = f.ignored, f.sorted()
	switch {
	case ctx.Err() != nil:
		return rep, ctx.Err()
	case err != nil:
		rep.addError("integrity_check: " + err.Error())
	case !kept:
		rep.IntegrityCheck = IntegrityOK
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
// a live database) on a private driver of its own with a binary-compare stub for every collation
// of the schema that SQLite does not have, so an unknown collation is never reported as damage.
//
// A schema with a virtual generated column is refused (refuseExpressions), as are more than 16
// collations or a collation name with a control character or longer than 64 bytes (Verify).
//
// It returns the problem lines, at most 50 of at most 512 bytes each, then a count of the rest;
// an empty list means the database passed. Problems SQLite reports (a corrupt page, a truncated
// file, not a database) and an empty file are problems, not errors. The error is for a file that
// cannot be examined at all (missing, not a regular file) and for cancellation (ctx.Err()).
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
		rep.addError("read the schema: " + err.Error())
		return rep.Errors, nil
	}
	if msg := refuseExpressions(sch, false); msg != "" {
		rep.addError(msg)
		return rep.Errors, nil
	}
	drv, err := stubDriver(sch.collations)
	if err != nil {
		return nil, err
	}
	db := openDB(drv, uri)
	defer db.Close()
	if _, err := rep.quickCheck(ctx, db); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rep.addError("quick_check: " + err.Error())
	}
	rep.finish()
	return rep.Errors, nil
}

// missingRe matches the integrity_check line a stub collation causes.
var missingRe = regexp.MustCompile(`^row \d+ missing from index (.+)$`)

// integrityFilter classifies integrity_check lines one at a time: "ok" and the "row N missing
// from index X" lines for indexes in custom (whose collation is a stub) are ignored and counted,
// X noted (at most maxReportedErrors names, each cut to maxLineBytes); every other line is kept.
type integrityFilter struct {
	custom  map[string]bool
	ignored int
	indexes []string
}

// keep reports whether line l is a problem.
func (f *integrityFilter) keep(l string) bool {
	if l == "ok" {
		return false
	}
	m := missingRe.FindStringSubmatch(l)
	if m == nil || !f.custom[m[1]] {
		return true
	}
	f.ignored++
	if name := cutLine(m[1]); len(f.indexes) < maxReportedErrors && !slices.Contains(f.indexes, name) {
		f.indexes = append(f.indexes, name)
	}
	return false
}

// sorted returns the noted indexes, sorted (never nil).
func (f *integrityFilter) sorted() []string {
	out := append([]string{}, f.indexes...)
	slices.Sort(out)
	return out
}

// schemaInfo is what Verify needs from a database's schema.
type schemaInfo struct {
	// collations are the non-built-in collation names (lowercase, sorted).
	collations []string
	// customIndexes are the indexes with at least one key column in such a collation.
	customIndexes map[string]bool
	// tables are the table names (lowercase).
	tables map[string]bool
	// generated is the first virtual generated column ("table.column"), indexExpr the first index
	// on an expression or with a WHERE clause; "" when there is none (refuseExpressions).
	generated, indexExpr string
}

// collateRe finds COLLATE clauses; the name may be bare or quoted "…", '…', `…` or […].
var collateRe = regexp.MustCompile("(?i)\\bCOLLATE\\s+(?:\"((?:[^\"]|\"\")+)\"|'((?:[^']|'')+)'|`([^`]+)`|\\[([^\\]]+)\\]|([A-Za-z_][A-Za-z0-9_$]*))")

// eachCollate calls fn with the name of each COLLATE clause in text, unquoted, until fn fails.
// Matches are found one at a time: a crafted schema entry can hold millions.
func eachCollate(text string, fn func(string) error) error {
	m := make([]string, 6)
	for rest := text; ; {
		loc := collateRe.FindStringSubmatchIndex(rest)
		if loc == nil {
			return nil
		}
		for i := range m {
			m[i] = ""
			if loc[2*i] >= 0 {
				m[i] = rest[loc[2*i]:loc[2*i+1]]
			}
		}
		if err := fn(collateName(m)); err != nil {
			return err
		}
		rest = rest[loc[1]:]
	}
}

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

// readSchema reads the schema of the database at uri with plainDriver (collations are only
// looked up when a statement uses an index, never while the schema is loaded). It fails for a
// collation name checkCollation refuses and for more than maxCollations of them.
func readSchema(ctx context.Context, uri string) (schemaInfo, error) {
	db := openDB(plainDriver, uri)
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT type, name, coalesce(sql, '') FROM sqlite_master`)
	if err != nil {
		return schemaInfo{}, err
	}
	info := schemaInfo{customIndexes: map[string]bool{}, tables: map[string]bool{}}
	colls := map[string]bool{}
	// addColl notes a collation name, reporting whether it is custom (not built in).
	addColl := func(c string) (bool, error) {
		if isBuiltinCollation(c) {
			return false, nil
		}
		if err := checkCollation(c); err != nil {
			return false, err
		}
		colls[strings.ToLower(c)] = true
		if len(colls) > maxCollations {
			return false, fmt.Errorf("the schema names more than %d collations SQLite does not have", maxCollations)
		}
		return true, nil
	}
	var indexes []string
	for rows.Next() {
		var typ, name, text string
		if err := rows.Scan(&typ, &name, &text); err != nil {
			_ = rows.Close()
			return schemaInfo{}, err
		}
		custom := false
		if err := eachCollate(text, func(c string) error {
			isCustom, err := addColl(c)
			custom = custom || isCustom
			return err
		}); err != nil {
			_ = rows.Close()
			return schemaInfo{}, err
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
		keyColls, expr, err := indexKeys(ctx, db, idx)
		if err != nil {
			return schemaInfo{}, fmt.Errorf("index %s: %w", idx, err)
		}
		if expr && info.indexExpr == "" {
			info.indexExpr = idx
		}
		for _, c := range keyColls {
			if c == "" {
				continue
			}
			isCustom, err := addColl(c)
			if err != nil {
				return schemaInfo{}, err
			}
			if isCustom {
				info.customIndexes[idx] = true
			}
		}
	}
	// The ordinary tables' virtual generated columns and partial indexes (virtual tables are
	// left out: modernc lacks Plex's fts4 and spellfix1 modules, which pragma_table_xinfo would
	// load).
	for _, q := range []struct {
		sql string
		dst *string
	}{
		{`SELECT t.name || '.' || x.name FROM pragma_table_list AS t, pragma_table_xinfo(t.name) AS x
			WHERE t.schema = 'main' AND t.type IN ('table', 'shadow') AND x.hidden = 2 LIMIT 1`, &info.generated},
		{`SELECT i.name FROM pragma_table_list AS t, pragma_index_list(t.name) AS i
			WHERE t.schema = 'main' AND t.type IN ('table', 'shadow') AND i.partial = 1 LIMIT 1`, &info.indexExpr},
	} {
		if *q.dst != "" {
			continue
		}
		if err := db.QueryRowContext(ctx, q.sql).Scan(q.dst); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return schemaInfo{}, err
		}
	}
	info.collations = make([]string, 0, len(colls))
	for c := range colls {
		info.collations = append(info.collations, c)
	}
	slices.Sort(info.collations)
	return info, nil
}

// indexKeys returns the collations of an index's key columns and whether a key is an
// expression.
func indexKeys(ctx context.Context, db *sql.DB, index string) (colls []string, expr bool, err error) {
	rows, err := db.QueryContext(ctx, `SELECT cid, coalesce(coll, '') FROM pragma_index_xinfo(?) WHERE key = 1`, index)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int64
		var c string
		if err := rows.Scan(&cid, &c); err != nil {
			return nil, false, err
		}
		expr = expr || cid == -2
		colls = append(colls, c)
	}
	return colls, expr, rows.Err()
}

// eachLine runs a query that returns one text column and calls fn with each row until fn
// returns false. Rows are handled as they arrive, never collected.
func eachLine(ctx context.Context, db *sql.DB, q string, fn func(string) bool) error {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		if !fn(s) {
			return nil
		}
	}
	return rows.Err()
}
