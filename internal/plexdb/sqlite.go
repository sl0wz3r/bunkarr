package plexdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"modernc.org/sqlite"
)

// The SQLite drivers plexdb opens Plex's databases and their copies with: private modernc
// Drivers, never the process-wide "sqlite" one Bunkarr's own database uses.
//
// plainDriver has nothing registered on it; it opens the live database for the online backup and
// a copy for readSchema. The checks of Verify and QuickCheck open their copy with a driver of
// their own (stubDriver) that carries a binary-compare stub for each collation of that copy's
// schema that SQLite does not have (ADR 0005: icu_root for Plex). The names come from the copy,
// which is untrusted (an *arr's zip, a Plex database), so no stub outlives its check: modernc
// keeps a driver's collations for the driver's life and creates every one again on each Open, so
// on a shared driver one crafted schema would slow every later Open, and a name modernc hands to
// C cut at a NUL ("nocase\x00x") would replace a built-in collation for every later check. A
// driver of its own also needs no lock: modernc reads a driver's collation table without one when
// it opens a connection, and stubDriver registers everything before the first Open. (modernc
// never frees a registration: a few bytes per stub and check.)
var plainDriver = &sqlite.Driver{}

// Limits on the collations a schema may name (checkCollation). Plex's schema names one
// (icu_root), the *arrs' none; a crafted schema could name millions, each a stub to create on
// every Open.
const (
	maxCollations     = 16
	maxCollationBytes = 64
)

// builtinCollations are SQLite's own collations (names are case-insensitive).
var builtinCollations = []string{"binary", "nocase", "rtrim"}

// isBuiltinCollation reports whether name is one of SQLite's built-in collations.
func isBuiltinCollation(name string) bool {
	return slices.Contains(builtinCollations, strings.ToLower(name))
}

// checkCollation refuses a collation name read from a schema that is too long or not plain
// text: SQLite reads a name only up to a NUL, so "nocase\x00x" would stub NOCASE itself.
func checkCollation(name string) error {
	if len(name) > maxCollationBytes {
		return fmt.Errorf("the schema names a collation longer than %d bytes", maxCollationBytes)
	}
	if !utf8.ValidString(name) || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("the schema names a collation with a control character: %q", name)
	}
	return nil
}

// connector opens connections of one driver for one DSN.
type connector struct {
	drv *sqlite.Driver
	dsn string
}

// Connect implements driver.Connector.
func (c connector) Connect(context.Context) (driver.Conn, error) { return c.drv.Open(c.dsn) }

// Driver implements driver.Connector.
func (c connector) Driver() driver.Driver { return c.drv }

// openDB returns a pool of at most one connection of drv for dsn. Nothing is opened until the
// first query.
func openDB(drv *sqlite.Driver, dsn string) *sql.DB {
	db := sql.OpenDB(connector{drv: drv, dsn: dsn})
	db.SetMaxOpenConns(1)
	return db
}

// stubDriver returns a new driver with a binary-compare stub for each of names: the lowercased,
// distinct, checked (checkCollation) non-built-in collations of readSchema.
func stubDriver(names []string) (*sqlite.Driver, error) {
	drv := &sqlite.Driver{}
	for _, n := range names {
		if err := drv.RegisterCollationUtf8(n, strings.Compare); err != nil {
			return nil, fmt.Errorf("register stub collation %q: %w", n, err)
		}
	}
	return drv, nil
}

// fileURI returns a SQLite URI for the file at path (made absolute) with the given query
// ("mode=ro", "mode=ro&immutable=1", or "" for none). The path is percent-escaped, so spaces
// ("Plug-in Support"), "?" and "#" in it are safe.
func fileURI(path, query string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	u := "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()
	if query != "" {
		u += "?" + query
	}
	return u, nil
}
