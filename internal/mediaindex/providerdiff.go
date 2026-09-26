package mediaindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// A provider refresh replaces its cache in one transaction (design §6), but it writes only what
// changed: the rows the cache holds are read from the read pool first (which does not hold the
// writer), compared with the new rows, and the transaction then deletes the rows that are gone,
// inserts the new ones and updates the changed ones. A nightly refresh of a large, mostly
// unchanged library therefore holds SQLite's only writer connection for the few rows that
// changed, not for a delete and re-insert of the whole index; the result is the same set of rows.
// When most of the cached rows are gone (a Plex library removed and added again, or another Plex
// server: every rating_key changes), deleting them one at a time by key costs more than the full
// replacement did, so the diff falls back to it: one DELETE of the integration's rows, then an
// INSERT of every row. New rows are plain INSERTs, never INSERT … ON CONFLICT DO UPDATE: that
// upsert is several times slower on plex_files (three secondary indexes), and a new key cannot
// conflict: the refreshes of an integration are serialized by its lock key (jobqueue/jobqueue.go),
// so no other writer adds its rows between the read and the write.

// cacheTable is the new content of one table of a provider cache: its key columns (after
// integration_id: the rest of the primary key) and value columns, and its n rows, which row
// renders one at a time (key values, then value values, in column order) so a large index is not
// held a second time as boxed values. A later row with the key of an earlier one is ignored (the
// first one wins, as the plain INSERT … ON CONFLICT DO NOTHING of a full replacement did).
type cacheTable struct {
	name       string
	keys, vals []string
	n          int
	// row appends row i's values to dst.
	row func(dst []any, i int) []any
}

// tableDiff is what a replacement must write to one table.
type tableDiff struct {
	t *cacheTable
	// cached is how many rows the table held for the integration when it was read.
	cached int
	// all is set when more than half of the cached rows are gone: apply then deletes all of the
	// integration's rows with one statement and inserts every row (inserts holds them all, without
	// the repeated keys), as a full replacement did. Deleting that many rows one at a time by key
	// held the writer about twice as long as the full replacement.
	all bool
	// deletes are the key values of the rows that are no longer in the cache (none when all).
	deletes [][]any
	// inserts are the indexes (rows of t) of the new rows (of every row when all); updates are
	// those of the changed rows (none when all).
	inserts, updates []int
}

// rowsChanged is how many rows the diff writes.
func (d tableDiff) rowsChanged() int {
	if d.all {
		return d.cached + len(d.inserts)
	}
	return len(d.deletes) + len(d.inserts) + len(d.updates)
}

// The state of a new row in diffTable.
const (
	rowNew     uint8 = iota // not cached
	rowChanged              // cached with other values
	rowSame                 // cached with the same values
	rowDup                  // a repeated key: never written
)

// diffTable compares the new rows of t with integration id's rows of the table, read through q
// (the read pool: the writer is not held while the cache is read).
func diffTable(ctx context.Context, q Queryer, id int64, t *cacheTable) (tableDiff, error) {
	d := tableDiff{t: t}
	nk := len(t.keys)
	state := make([]uint8, t.n)
	byKey := make(map[[sha256.Size]byte]int32, t.n)
	var (
		buf, other []byte
		row        []any
	)
	for i := 0; i < t.n; i++ {
		row = t.row(row[:0], i)
		if len(row) != nk+len(t.vals) {
			return d, fmt.Errorf("store %s: a row has %d values for %d columns", t.name, len(row), nk+len(t.vals))
		}
		buf = appendCanon(buf[:0], row[:nk])
		h := sha256.Sum256(buf)
		if _, dup := byKey[h]; dup {
			state[i] = rowDup
			continue
		}
		byKey[h] = int32(i)
	}
	cols := append(append([]string{}, t.keys...), t.vals...)
	rows, err := q.QueryContext(ctx, `SELECT `+strings.Join(cols, ", ")+` FROM `+t.name+` WHERE integration_id = ?`, id)
	if err != nil {
		return d, fmt.Errorf("read the cached %s rows: %w", t.name, err)
	}
	defer rows.Close()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return d, fmt.Errorf("read the cached %s rows: %w", t.name, err)
		}
		d.cached++
		buf = appendCanon(buf[:0], vals[:nk])
		i, ok := byKey[sha256.Sum256(buf)]
		if !ok {
			d.deletes = append(d.deletes, append([]any(nil), vals[:nk]...))
			continue
		}
		buf = appendCanon(buf[:0], vals[nk:])
		row = t.row(row[:0], int(i))
		other = appendCanon(other[:0], row[nk:])
		if bytes.Equal(buf, other) {
			state[i] = rowSame
		} else {
			state[i] = rowChanged
		}
	}
	if err := rows.Err(); err != nil {
		return d, fmt.Errorf("read the cached %s rows: %w", t.name, err)
	}
	if len(d.deletes)*2 > d.cached {
		d.all, d.deletes = true, nil
	}
	for i, st := range state {
		switch {
		case st == rowDup:
		case st == rowNew || d.all:
			d.inserts = append(d.inserts, i)
		case st == rowChanged:
			d.updates = append(d.updates, i)
		}
	}
	return d, nil
}

// apply writes the diff in tx: it deletes the rows that are gone (all of them when d.all), then
// inserts the new rows and updates the changed ones.
func (d tableDiff) apply(ctx context.Context, tx *sql.Tx, id int64) error {
	t := d.t
	where := "integration_id = ?"
	for _, k := range t.keys {
		where += " AND " + k + " = ?"
	}
	if d.all {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t.name+` WHERE integration_id = ?`, id); err != nil {
			return fmt.Errorf("replace the cache: %w", err)
		}
	} else if len(d.deletes) > 0 {
		stmt, err := tx.PrepareContext(ctx, `DELETE FROM `+t.name+` WHERE `+where)
		if err != nil {
			return fmt.Errorf("replace the cache: %w", err)
		}
		defer stmt.Close()
		args := make([]any, 1+len(t.keys))
		args[0] = id
		for _, k := range d.deletes {
			copy(args[1:], k)
			if _, err := stmt.ExecContext(ctx, args...); err != nil {
				return fmt.Errorf("replace the cache: %w", err)
			}
		}
	}
	if len(d.inserts) > 0 {
		cols := append(append([]string{"integration_id"}, t.keys...), t.vals...)
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO `+t.name+` (`+strings.Join(cols, ", ")+`) VALUES (?`+strings.Repeat(", ?", len(cols)-1)+`)`)
		if err != nil {
			return fmt.Errorf("store the %s rows: %w", t.name, err)
		}
		defer stmt.Close()
		args := make([]any, 1, len(cols))
		args[0] = id
		for _, i := range d.inserts {
			args = t.row(args[:1], i)
			if _, err := stmt.ExecContext(ctx, args...); err != nil {
				return fmt.Errorf("store the %s rows: %w", t.name, err)
			}
		}
	}
	if len(d.updates) > 0 {
		set := make([]string, len(t.vals))
		for i, v := range t.vals {
			set[i] = v + " = ?"
		}
		stmt, err := tx.PrepareContext(ctx, `UPDATE `+t.name+` SET `+strings.Join(set, ", ")+` WHERE `+where)
		if err != nil {
			return fmt.Errorf("store the %s rows: %w", t.name, err)
		}
		defer stmt.Close()
		nk := len(t.keys)
		var row []any
		args := make([]any, 0, 1+nk+len(t.vals))
		for _, i := range d.updates {
			row = t.row(row[:0], i)
			// SET the values, WHERE integration_id and the keys.
			args = append(append(append(args[:0], row[nk:]...), id), row[:nk]...)
			if _, err := stmt.ExecContext(ctx, args...); err != nil {
				return fmt.Errorf("store the %s rows: %w", t.name, err)
			}
		}
	}
	return nil
}

// appendCanon appends an exact, type-tagged encoding of values as SQLite stores and returns them:
// an integer (a bool is bound as 0 or 1), text (a blob compares as text), a real, NULL. A new row
// and the row read back encode the same only when every column is unchanged. A type the driver
// would convert otherwise never compares equal, so its row is always rewritten (never skipped).
func appendCanon(b []byte, vals []any) []byte {
	for _, v := range vals {
		switch x := v.(type) {
		case nil:
			b = append(b, 'n')
		case bool:
			n := int64(0)
			if x {
				n = 1
			}
			b = binary.AppendVarint(append(b, 'i'), n)
		case int:
			b = binary.AppendVarint(append(b, 'i'), int64(x))
		case int32:
			b = binary.AppendVarint(append(b, 'i'), int64(x))
		case int64:
			b = binary.AppendVarint(append(b, 'i'), x)
		case float64:
			b = binary.BigEndian.AppendUint64(append(b, 'f'), math.Float64bits(x))
		case string:
			b = append(binary.AppendUvarint(append(b, 't'), uint64(len(x))), x...)
		case []byte:
			b = append(binary.AppendUvarint(append(b, 't'), uint64(len(x))), x...)
		default:
			s := fmt.Sprintf("%T:%v", x, x)
			b = append(binary.AppendUvarint(append(b, '?'), uint64(len(s))), s...)
		}
	}
	return b
}
