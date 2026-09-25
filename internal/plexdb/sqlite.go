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
	"sync"

	"modernc.org/sqlite"
)

// The SQLite driver plexdb opens Plex's databases and their copies with: a private modernc
// Driver, not the process-wide "sqlite" one Bunkarr's own database uses.
//
// This is the package's one piece of global mutable state. ADR 0005 registers a binary-compare
// stub for each non-built-in collation of Plex's schema (icu_root) once per process. modernc
// reads a driver's collation table without a lock when it opens a connection and allows
// registrations only before the first Open; registering on the shared driver while Bunkarr's
// database pools open connections would race. So the stubs live on this private driver, and
// driverMu orders registrations (write lock) against every Open of it (read lock). The stubs
// therefore never reach Bunkarr's own connections either.
var (
	driverMu  sync.RWMutex
	sqlDriver = &sqlite.Driver{}
	// stubbed holds the lowercased names of the collations registered on sqlDriver.
	stubbed = map[string]bool{}
)

// builtinCollations are SQLite's own collations (names are case-insensitive).
var builtinCollations = []string{"binary", "nocase", "rtrim"}

// isBuiltinCollation reports whether name is one of SQLite's built-in collations.
func isBuiltinCollation(name string) bool {
	return slices.Contains(builtinCollations, strings.ToLower(name))
}

// connector opens connections of sqlDriver for one DSN.
type connector struct{ dsn string }

// Connect implements driver.Connector.
func (c connector) Connect(context.Context) (driver.Conn, error) {
	driverMu.RLock()
	defer driverMu.RUnlock()
	return sqlDriver.Open(c.dsn)
}

// Driver implements driver.Connector.
func (connector) Driver() driver.Driver { return sqlDriver }

// openDB returns a pool of at most one connection of sqlDriver for dsn. Nothing is opened until
// the first query.
func openDB(dsn string) *sql.DB {
	db := sql.OpenDB(connector{dsn: dsn})
	db.SetMaxOpenConns(1)
	return db
}

// registerStubs registers a binary-compare stub collation for each name not registered yet
// (names are compared case-insensitively, as SQLite does). Connections opened afterwards have
// them.
func registerStubs(names []string) error {
	driverMu.Lock()
	defer driverMu.Unlock()
	for _, n := range names {
		key := strings.ToLower(n)
		if key == "" || stubbed[key] || isBuiltinCollation(key) {
			continue
		}
		if err := sqlDriver.RegisterCollationUtf8(key, strings.Compare); err != nil {
			return fmt.Errorf("register stub collation %q: %w", key, err)
		}
		stubbed[key] = true
	}
	return nil
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
