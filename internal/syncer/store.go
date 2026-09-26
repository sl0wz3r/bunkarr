package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// State is the state of a destination_files row (design §4.2).
type State string

// Record states.
const (
	// StatePresent: a file at RelPath holds the content.
	StatePresent State = "present"
	// StateLinked: RelPath is a hardlink to LinkOf's file.
	StateLinked State = "linked"
	// StateLinkRecorded: no file at RelPath (the destination keeps one copy); the content is
	// LinkOf's.
	StateLinkRecorded State = "link_recorded"
	// StateMissing: verify found the file gone or damaged; the next sync copies it again.
	StateMissing State = "missing"
	// StateRetained: the file was moved to RetainedPath and is deleted after ExpiresAt.
	StateRetained State = "retained"
)

// Live reports whether the state describes a live path of the mirror (every state but retained).
func (s State) Live() bool {
	return s == StatePresent || s == StateLinked || s == StateLinkRecorded || s == StateMissing
}

// Reasons a row was retained (Record.Reason).
const (
	// ReasonDeleted: the file disappeared from the source.
	ReasonDeleted = "deleted"
	// ReasonReplaced: the old version of an updated (or relinked) file.
	ReasonReplaced = "replaced"
	// ReasonDisplaced: an unmanaged file that was in the way of a path Bunkarr needed (S2).
	ReasonDisplaced = "displaced"
	// ReasonDamaged: a file verify had marked missing that was still there when it was replaced.
	ReasonDamaged = "damaged"
)

// Record is one destination_files row: what Bunkarr put (or found and adopted) at a destination.
type Record struct {
	ID            int64 `json:"id"`
	DestinationID int64 `json:"destinationId"`
	// SourceID is 0 when the source was deleted (the row is an orphan).
	SourceID int64 `json:"sourceId"`
	// RelPath is relative to the destination target: "<destFolder>/<relPath in the source>".
	RelPath string `json:"relPath"`
	// SourceRelPath is the path inside the source (catalog_files.rel_path).
	SourceRelPath string `json:"sourceRelPath"`
	// Size and MtimeNs are the source file's when it was copied (for a retained row: the retained
	// file's own size, which expiry checks).
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtimeNs"`
	Hash    string `json:"hash,omitempty"`
	// LinkOf is the row this path is a hardlink of (0 = none).
	LinkOf       int64      `json:"linkOf,omitempty"`
	State        State      `json:"state"`
	RetainedPath string     `json:"retainedPath,omitempty"`
	Reason       string     `json:"reason,omitempty"`
	JobID        int64      `json:"jobId,omitempty"`
	CopiedAt     *time.Time `json:"copiedAt,omitempty"`
	VerifiedAt   *time.Time `json:"verifiedAt,omitempty"`
	RetainedAt   *time.Time `json:"retainedAt,omitempty"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
}

// ErrNotFound means a record does not exist.
var ErrNotFound = errors.New("destination file record not found")

// Store reads and writes destination_files. Safe for concurrent use; every write is one db.Write
// transaction.
type Store struct {
	db *db.DB
	// traceRead, when set (tests only), is called with every read's SQL and arguments before it
	// runs, so a test can check the query plan of what a function actually runs.
	traceRead func(query string, args []any)
}

// NewStore returns a store over d.
func NewStore(d *db.DB) *Store { return &Store{db: d} }

const recordColumns = `id, destination_id, source_id, rel_path, source_rel_path, size, mtime_ns, hash, link_of, state,
	retained_path, reason, job_id, copied_at, verified_at, retained_at, expires_at`

// liveStates is the SQL list of live states.
const liveStates = `('present', 'linked', 'link_recorded', 'missing')`

type rowScanner interface{ Scan(dest ...any) error }

func scanRecord(r rowScanner) (Record, error) {
	var (
		rec                                         Record
		sourceID, linkOf, jobID                     sql.NullInt64
		hash, retained, reason                      sql.NullString
		copiedAt, verifiedAt, retainedAt, expiresAt sql.NullString
		state                                       string
	)
	if err := r.Scan(&rec.ID, &rec.DestinationID, &sourceID, &rec.RelPath, &rec.SourceRelPath, &rec.Size, &rec.MtimeNs,
		&hash, &linkOf, &state, &retained, &reason, &jobID, &copiedAt, &verifiedAt, &retainedAt, &expiresAt); err != nil {
		return Record{}, err
	}
	rec.SourceID, rec.LinkOf, rec.JobID = sourceID.Int64, linkOf.Int64, jobID.Int64
	rec.Hash, rec.RetainedPath, rec.Reason = hash.String, retained.String, reason.String
	rec.State = State(state)
	var err error
	for _, t := range []struct {
		src sql.NullString
		dst **time.Time
		col string
	}{{copiedAt, &rec.CopiedAt, "copied_at"}, {verifiedAt, &rec.VerifiedAt, "verified_at"},
		{retainedAt, &rec.RetainedAt, "retained_at"}, {expiresAt, &rec.ExpiresAt, "expires_at"}} {
		if !t.src.Valid {
			continue
		}
		var v time.Time
		if v, err = db.ParseTime(t.src.String); err != nil {
			return Record{}, fmt.Errorf("destination file %d: %s: %w", rec.ID, t.col, err)
		}
		*t.dst = &v
	}
	return rec, nil
}

// reader returns the connection pool for a read of query with args (see traceRead).
func (s *Store) reader(query string, args []any) *sql.DB {
	if s.traceRead != nil {
		s.traceRead(query, args)
	}
	return s.db.Reader()
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Record, error) {
	q = `SELECT ` + recordColumns + ` FROM destination_files ` + q
	rows, err := s.reader(q, args).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read destination files: %w", err)
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("read destination files: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read destination files: %w", err)
	}
	return out, nil
}

func (s *Store) one(ctx context.Context, q string, args ...any) (Record, bool, error) {
	q = `SELECT ` + recordColumns + ` FROM destination_files ` + q
	rec, err := scanRecord(s.reader(q, args).QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read destination file: %w", err)
	}
	return rec, true, nil
}

// HasBackups reports whether any destination holds records (live or retained) of a source. It is
// catalog.StoreOptions.HasBackups: while it is true the source's destFolder cannot change.
func (s *Store) HasBackups(ctx context.Context, sourceID int64) (bool, error) {
	var has bool
	err := s.db.Reader().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM destination_files WHERE source_id = ?)`, sourceID).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("check backups of source %d: %w", sourceID, err)
	}
	return has, nil
}

// Get returns one record (ErrNotFound when there is none).
func (s *Store) Get(ctx context.Context, id int64) (Record, error) {
	rec, ok, err := s.one(ctx, `WHERE id = ?`, id)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, fmt.Errorf("destination file %d: %w", id, ErrNotFound)
	}
	return rec, nil
}

// List returns every record of a destination (live and retained), by path then id.
func (s *Store) List(ctx context.Context, destinationID int64) ([]Record, error) {
	return s.query(ctx, `WHERE destination_id = ? ORDER BY rel_path, id`, destinationID)
}

// LiveAt returns the live record at a destination path, if any.
func (s *Store) LiveAt(ctx context.Context, destinationID int64, relPath string) (Record, bool, error) {
	return s.one(ctx, `WHERE destination_id = ? AND rel_path = ? AND state IN `+liveStates, destinationID, relPath)
}

// LiveForSource returns the live records of one source at a destination, by source path.
func (s *Store) LiveForSource(ctx context.Context, destinationID, sourceID int64) ([]Record, error) {
	return s.query(ctx, `WHERE destination_id = ? AND source_id = ? AND state IN `+liveStates+` ORDER BY source_rel_path, id`,
		destinationID, sourceID)
}

// retainedForSource returns the retained records of one source at a destination.
func (s *Store) retainedForSource(ctx context.Context, destinationID, sourceID int64) ([]Record, error) {
	return s.query(ctx, `WHERE destination_id = ? AND source_id = ? AND state = 'retained' ORDER BY source_rel_path, id`,
		destinationID, sourceID)
}

// liveForSourcePath returns the live record of a source path at a destination, if any.
func (s *Store) liveForSourcePath(ctx context.Context, destinationID, sourceID int64, sourceRel string) (Record, bool, error) {
	return s.one(ctx, `WHERE destination_id = ? AND source_id = ? AND source_rel_path = ? AND state IN `+liveStates,
		destinationID, sourceID, sourceRel)
}

// Dependents returns the live records that are hardlinks (made or only recorded) of record id.
func (s *Store) Dependents(ctx context.Context, id int64) ([]Record, error) {
	return s.query(ctx, `WHERE link_of = ? AND state IN `+liveStates+` ORDER BY rel_path, id`, id)
}

// Expired returns the retained records of a destination whose expiry is at or before now, oldest
// first.
func (s *Store) Expired(ctx context.Context, destinationID int64, now time.Time) ([]Record, error) {
	return s.query(ctx, `WHERE destination_id = ? AND state = 'retained' AND expires_at <= ? ORDER BY expires_at, id`,
		destinationID, db.FormatTime(now))
}

// eachLive calls fn for every live record of a destination (keyset pages by id, so no read
// transaction stays open while fn runs).
func (s *Store) eachLive(ctx context.Context, destinationID int64, fn func(Record) error) error {
	const page = 2000
	after := int64(0)
	for {
		recs, err := s.query(ctx, `WHERE destination_id = ? AND id > ? AND state IN `+liveStates+` ORDER BY id LIMIT ?`,
			destinationID, after, page)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if err := fn(r); err != nil {
				return err
			}
		}
		if len(recs) < page {
			return nil
		}
		after = recs[len(recs)-1].ID
	}
}

// --- writes (inside a db.Write transaction) ---

func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }

func nullStr(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }

func nullTime(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: db.FormatTime(*t), Valid: true}
}

// insertRecord inserts rec (its ID is ignored) and returns the new id.
func insertRecord(ctx context.Context, tx *sql.Tx, rec Record) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO destination_files (destination_id, source_id, rel_path, source_rel_path, size,
		mtime_ns, hash, link_of, state, retained_path, reason, job_id, copied_at, verified_at, retained_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.DestinationID, nullInt(rec.SourceID), rec.RelPath, rec.SourceRelPath, rec.Size, rec.MtimeNs, nullStr(rec.Hash),
		nullInt(rec.LinkOf), string(rec.State), nullStr(rec.RetainedPath), nullStr(rec.Reason), nullInt(rec.JobID),
		nullTime(rec.CopiedAt), nullTime(rec.VerifiedAt), nullTime(rec.RetainedAt), nullTime(rec.ExpiresAt))
	if err != nil {
		return 0, fmt.Errorf("record %s: %w", rec.RelPath, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record %s: %w", rec.RelPath, err)
	}
	return id, nil
}

// updateRecord rewrites every column of rec's row.
func updateRecord(ctx context.Context, tx *sql.Tx, rec Record) error {
	res, err := tx.ExecContext(ctx, `UPDATE destination_files SET source_id = ?, rel_path = ?, source_rel_path = ?, size = ?,
		mtime_ns = ?, hash = ?, link_of = ?, state = ?, retained_path = ?, reason = ?, job_id = ?, copied_at = ?, verified_at = ?,
		retained_at = ?, expires_at = ? WHERE id = ?`,
		nullInt(rec.SourceID), rec.RelPath, rec.SourceRelPath, rec.Size, rec.MtimeNs, nullStr(rec.Hash), nullInt(rec.LinkOf),
		string(rec.State), nullStr(rec.RetainedPath), nullStr(rec.Reason), nullInt(rec.JobID), nullTime(rec.CopiedAt),
		nullTime(rec.VerifiedAt), nullTime(rec.RetainedAt), nullTime(rec.ExpiresAt), rec.ID)
	if err != nil {
		return fmt.Errorf("update record %s: %w", rec.RelPath, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("update record %s: %w", rec.RelPath, err)
	} else if n == 0 {
		return fmt.Errorf("update record %d: %w", rec.ID, ErrNotFound)
	}
	return nil
}

// getTx reads one record inside a transaction.
func getTx(ctx context.Context, tx *sql.Tx, id int64) (Record, bool, error) {
	rec, err := scanRecord(tx.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM destination_files WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read destination file %d: %w", id, err)
	}
	return rec, true, nil
}

// liveAtTx reads the live record at a path inside a transaction.
func liveAtTx(ctx context.Context, tx *sql.Tx, destinationID int64, relPath string) (Record, bool, error) {
	rec, err := scanRecord(tx.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM destination_files
		WHERE destination_id = ? AND rel_path = ? AND state IN `+liveStates, destinationID, relPath))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read destination file %s: %w", relPath, err)
	}
	return rec, true, nil
}

// retainedLookup counts the retained rows of a source path that record a retained path. Naming the
// source path lets the destination_files_source index find the few rows of that path instead of
// visiting every retained row of the destination (there is no index on retained_path). IS matches
// the NULL source of an orphan row.
const retainedLookup = `SELECT COUNT(*) FROM destination_files
	WHERE destination_id = ? AND source_id IS ? AND source_rel_path = ? AND state = 'retained' AND retained_path = ?`

// retainedLookupArgs are retainedLookup's arguments for the retained row rec (an item's retained
// row always has the item's source, or none once the source is deleted, and source path).
func retainedLookupArgs(rec Record) []any {
	return []any{rec.DestinationID, nullInt(rec.SourceID), rec.SourceRelPath, rec.RetainedPath}
}

// sourceExists reports whether a source still exists (a stopped job's items keep a deleted one's id).
func sourceExists(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sources WHERE id = ?)`, id).Scan(&ok); err != nil {
		return false, fmt.Errorf("look up source %d: %w", id, err)
	}
	return ok, nil
}

// retainedExists reports whether the retained row rec is already recorded (an idempotent insert
// after a crash between the database write and the item's finish).
func retainedExists(ctx context.Context, tx *sql.Tx, rec Record) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, retainedLookup, retainedLookupArgs(rec)...).Scan(&n); err != nil {
		return false, fmt.Errorf("look up retained file %s: %w", rec.RetainedPath, err)
	}
	return n > 0, nil
}

// repoint makes every row that links to from link to to instead (live rows), and clears the link of
// retained rows (they hold their own file), so from can change state or be deleted.
func repoint(ctx context.Context, tx *sql.Tx, from, to int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET link_of = ? WHERE link_of = ? AND id <> ? AND state IN `+liveStates, to, from, to); err != nil {
		return fmt.Errorf("re-point hardlinks of record %d: %w", from, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET link_of = NULL WHERE link_of = ? AND state = 'retained'`, from); err != nil {
		return fmt.Errorf("re-point hardlinks of record %d: %w", from, err)
	}
	return nil
}

// deleteRecord deletes a row that nothing links to any more.
func deleteRecord(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET link_of = NULL WHERE link_of = ? AND state = 'retained'`, id); err != nil {
		return fmt.Errorf("delete record %d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM destination_files WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete record %d: %w", id, err)
	}
	return nil
}

// liveDependentsTx returns the live rows linking to id.
func liveDependentsTx(ctx context.Context, tx *sql.Tx, id int64) ([]Record, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+recordColumns+` FROM destination_files WHERE link_of = ? AND state IN `+liveStates+` ORDER BY rel_path, id`, id)
	if err != nil {
		return nil, fmt.Errorf("read hardlinks of record %d: %w", id, err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("read hardlinks of record %d: %w", id, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- retention intents ---
//
// Before a file with a live record is renamed (or hardlinked) into retention, the record gets the
// chosen retained path and the reason in its retained_path and reason columns, which are otherwise
// only used by retained rows: a retention intent. The write that records the outcome (the record
// retained, or a retained row for an old version) clears it. An intent that is still there when
// the item that wrote it will not run again (its job failed or was cancelled) is settled by the next
// job of the destination (reconcile.go), so a file is never left in retention without a record.

// intentStates are the live states whose record has a file of its own (link_recorded has none).
const intentStates = `('present', 'linked', 'missing')`

// setIntentTx records a retention intent on the live record at rel.
func setIntentTx(ctx context.Context, tx *sql.Tx, destinationID int64, rel, retained, reason string) error {
	res, err := tx.ExecContext(ctx, `UPDATE destination_files SET retained_path = ?, reason = ?
		WHERE destination_id = ? AND rel_path = ? AND state IN `+intentStates, retained, nullStr(reason), destinationID, rel)
	if err != nil {
		return fmt.Errorf("record the retention of %s: %w", rel, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("record the retention of %s: %w", rel, err)
	} else if n == 0 {
		return &itemError{msg: fmt.Sprintf("the record of %s changed; it is not retained", rel)}
	}
	return nil
}

// clearIntentTx removes the retention intent retained from the live record id (if it still has it).
func clearIntentTx(ctx context.Context, tx *sql.Tx, id int64, retained string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET retained_path = NULL, reason = NULL
		WHERE id = ? AND retained_path = ? AND state IN `+liveStates, id, retained); err != nil {
		return fmt.Errorf("clear the retention intent of record %d: %w", id, err)
	}
	return nil
}

// intents returns the live records of a destination that carry a retention intent.
func (s *Store) intents(ctx context.Context, destinationID int64) ([]Record, error) {
	return s.query(ctx, `WHERE destination_id = ? AND state IN `+liveStates+` AND retained_path IS NOT NULL ORDER BY id`,
		destinationID)
}

// linkRow is a hardlinked name and the path of its primary (the link manifest).
type linkRow struct {
	name, primary string
	state         State
}

// links returns every live hardlinked name of a destination (linked or link_recorded) with its
// primary's path, by name.
func (s *Store) links(ctx context.Context, destinationID int64) ([]linkRow, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT d.rel_path, p.rel_path, d.state FROM destination_files d
		JOIN destination_files p ON p.id = d.link_of
		WHERE d.destination_id = ? AND d.state IN ('linked', 'link_recorded') ORDER BY d.rel_path`, destinationID)
	if err != nil {
		return nil, fmt.Errorf("read hardlinks: %w", err)
	}
	defer rows.Close()
	var out []linkRow
	for rows.Next() {
		var l linkRow
		var state string
		if err := rows.Scan(&l.name, &l.primary, &state); err != nil {
			return nil, fmt.Errorf("read hardlinks: %w", err)
		}
		l.state = State(state)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read hardlinks: %w", err)
	}
	return out, nil
}

// setVerified records a successful verification (and the hash when the record had none).
func (s *Store) setVerified(ctx context.Context, id int64, hash string, at time.Time) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE destination_files SET verified_at = ?, hash = COALESCE(hash, ?) WHERE id = ?`,
			db.FormatTime(at), nullStr(hash), id)
		if err != nil {
			return fmt.Errorf("record verification of %d: %w", id, err)
		}
		return nil
	})
}

// markMissing sets a live present or linked record to missing (verify found it gone or damaged).
func (s *Store) markMissing(ctx context.Context, id int64) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE destination_files SET state = 'missing' WHERE id = ? AND state IN ('present', 'linked')`, id)
		if err != nil {
			return fmt.Errorf("mark record %d missing: %w", id, err)
		}
		return nil
	})
}

// isUniqueViolation reports a UNIQUE constraint failure (a live path recorded twice).
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
