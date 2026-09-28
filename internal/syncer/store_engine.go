package syncer

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// Record operations for the restic and rclone executors (docs/design/phase4.md §3.3, §6.2, §6.3,
// §7.3, §7.4; internal/enginerun). The SQL of destination_files stays in this package. The
// operations that must be atomic with the caller's own writes (a batch's recording transaction,
// §6.2 step 5) take the caller's *sql.Tx; run them inside db.Write.
//
// restic references (D31): a live row's engine_ref NULL means "in the source's base snapshot"; it
// is only ever set from NULL to the base when a new snapshot lacks the recorded version, or
// cleared when a new snapshot holds it, and a retained row's reference never changes. So every
// write here that sets a reference uses COALESCE(engine_ref, base): a reference is never moved.

// ErrRecordChanged means a record an operation expected is not there any more, or changed (a
// retention intent on a path whose record went away).
var ErrRecordChanged = errors.New("the record changed")

// ContentDone is a content item an engine executed and read back (§6.2 step 5, §7.3): the live
// record at RelPath now holds the source file's version, with engine_ref NULL.
type ContentDone struct {
	DestinationID int64
	SourceID      int64
	// RecordID is the item's existing record (an update, a repair, a relink, a move's old record);
	// 0 for a new path.
	RecordID int64
	// RelPath is the destination path (a move's new path); SourceRelPath the path in the source.
	RelPath       string
	SourceRelPath string
	// Size and MtimeNs are the version's (the catalog's at plan time, which the read-back found).
	Size, MtimeNs int64
	// HeadTail is the head/tail hash read before the upload ("" when the file changed since).
	HeadTail string
	// State is StatePresent, StateLinked (restic: another name of a hardlink group) or
	// StateLinkRecorded (rclone); LinkOf the group's primary record for the last two.
	State  State
	LinkOf int64
	JobID  int64
	// CopiedAt is when the item finished.
	CopiedAt time.Time
	// Moved keeps the record's hash and verified_at (a move keeps its content); otherwise they are
	// cleared, since the content changed.
	Moved bool
}

// RecordContentTx records a done content item (§6.2 step 5, §7.3): the item's record (RecordID, or
// the live record at RelPath of the same source, or a new row) takes the path, size, mtime,
// head_tail, state, copied_at and job_id, with engine_ref NULL and any retention intent cleared. A
// live path recorded for another source is refused. It returns the record's id.
func (s *Store) RecordContentTx(ctx context.Context, tx *sql.Tx, c ContentDone) (int64, error) {
	switch c.State {
	case StatePresent:
		c.LinkOf = 0
	case StateLinked, StateLinkRecorded:
		if c.LinkOf == 0 {
			return 0, fmt.Errorf("record %s: a %s record needs its primary", c.RelPath, c.State)
		}
	default:
		return 0, fmt.Errorf("record %s: state %q is not a content state", c.RelPath, c.State)
	}
	var rec Record
	found := false
	if c.RecordID != 0 {
		r, ok, err := getTx(ctx, tx, c.RecordID)
		if err != nil {
			return 0, err
		}
		if ok && r.State.Live() && r.DestinationID == c.DestinationID {
			rec, found = r, true
		}
	}
	at, atOK, err := liveAtTx(ctx, tx, c.DestinationID, c.RelPath)
	if err != nil {
		return 0, err
	}
	if atOK && (!found || at.ID != rec.ID) {
		if at.SourceID != c.SourceID || found {
			return 0, fmt.Errorf("%w: %s is recorded for another record or source", ErrRecordChanged, c.RelPath)
		}
		rec, found = at, true
	}
	copied := c.CopiedAt
	next := Record{DestinationID: c.DestinationID, SourceID: c.SourceID, RelPath: c.RelPath, SourceRelPath: c.SourceRelPath,
		Size: c.Size, MtimeNs: c.MtimeNs, LinkOf: c.LinkOf, State: c.State, JobID: c.JobID, CopiedAt: &copied, HeadTail: c.HeadTail}
	if !found {
		return insertRecord(ctx, tx, next)
	}
	next.ID = rec.ID
	if c.Moved {
		next.Hash, next.VerifiedAt = rec.Hash, rec.VerifiedAt
		if rec.RelPath != c.RelPath {
			if err := followMoveTx(ctx, tx, rec, c.RelPath, c.SourceRelPath); err != nil {
				return 0, err
			}
		}
	}
	if err := updateRecord(ctx, tx, next); err != nil {
		return 0, err
	}
	return rec.ID, nil
}

// followMoveTx moves the old versions of live record rec along with it to its new path (a rename,
// §3.3): its retained rows of reason replaced or damaged at its old path, those retained since the
// path last held another file (a retained row of another reason there ends the record's history,
// since only rec was live at the path after it), take the new rel_path and source_rel_path; their
// retained_path and engine_ref, which say where the version is, stay. The replaced-version hold
// (internal/enginerun, phase4.md §6.5, §7.5) then looks at the record's new path: without it the
// old path has neither a live record nor a newer version, and such a row would be held forever.
func followMoveTx(ctx context.Context, tx *sql.Tx, rec Record, rel, sourceRel string) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+recordColumns+` FROM destination_files
		WHERE destination_id = ? AND source_id IS ? AND source_rel_path = ? AND state = 'retained'`,
		rec.DestinationID, nullInt(rec.SourceID), rec.SourceRelPath)
	if err != nil {
		return fmt.Errorf("read the old versions of %s: %w", rec.RelPath, err)
	}
	var at []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		if r.RelPath == rec.RelPath && r.RetainedAt != nil {
			at = append(at, r)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read the old versions of %s: %w", rec.RelPath, err)
	}
	slices.SortFunc(at, func(a, b Record) int { // newest first
		if c := b.RetainedAt.Compare(*a.RetainedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	for _, r := range at {
		if r.Reason != ReasonReplaced && r.Reason != ReasonDamaged {
			break
		}
		if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET rel_path = ?, source_rel_path = ? WHERE id = ?`,
			rel, sourceRel, r.ID); err != nil {
			return fmt.Errorf("move the old version %d of %s to %s: %w", r.ID, rec.RelPath, rel, err)
		}
	}
	return nil
}

// RetainedVersion is a version kept in retention, beside or instead of a live row: an update's
// old version (reason replaced, or damaged when the live record was missing), a displaced object,
// or a vanished file's version.
type RetainedVersion struct {
	DestinationID int64
	SourceID      int64
	// RelPath is the live path the version had; SourceRelPath its path in the source.
	RelPath       string
	SourceRelPath string
	Size, MtimeNs int64
	Hash          string
	HeadTail      string
	// EngineRef is the restic snapshot that holds the version ("" on rclone).
	EngineRef string
	// RetainedPath is where the version is: the rclone retention path, or its absolute path in the
	// restic snapshot.
	RetainedPath string
	Reason       string
	JobID        int64
	RetainedAt   time.Time
	ExpiresAt    time.Time
}

// InsertRetainedTx inserts a retained row (§6.2 step 5: the old version of a restic update, beside
// the live row, which the live partial unique index allows; §7.3: what moved into rclone's backup
// dir). It is idempotent: a row with the same source path, retained path and reference is reused.
// It returns the row's id.
func (s *Store) InsertRetainedTx(ctx context.Context, tx *sql.Tx, v RetainedVersion) (int64, error) {
	if v.RetainedPath == "" || v.ExpiresAt.IsZero() {
		return 0, fmt.Errorf("retain %s: a retained path and an expiry are required", v.RelPath)
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM destination_files WHERE destination_id = ? AND source_id IS ? AND source_rel_path = ?
		AND state = 'retained' AND retained_path = ? AND engine_ref IS ?`,
		v.DestinationID, nullInt(v.SourceID), v.SourceRelPath, v.RetainedPath, nullStr(v.EngineRef)).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("look up retained version %s: %w", v.RetainedPath, err)
	}
	retained, expires := v.RetainedAt, v.ExpiresAt
	return insertRecord(ctx, tx, Record{DestinationID: v.DestinationID, SourceID: v.SourceID, RelPath: v.RelPath,
		SourceRelPath: v.SourceRelPath, Size: v.Size, MtimeNs: v.MtimeNs, Hash: v.Hash, HeadTail: v.HeadTail, EngineRef: v.EngineRef,
		State: StateRetained, RetainedPath: v.RetainedPath, Reason: v.Reason, JobID: v.JobID, RetainedAt: &retained, ExpiresAt: &expires})
}

// Retain is how RetainTx retains a live record.
type Retain struct {
	// Base is the source's base snapshot (restic): engine_ref becomes COALESCE(engine_ref, Base).
	// "" leaves the reference as it is (rclone).
	Base string
	// RetainedPath is where the version is (the rclone retention path, or its absolute path in the
	// snapshot that holds it).
	RetainedPath string
	// Reason is the retained row's reason (deleted, released, displaced, damaged).
	Reason     string
	JobID      int64
	RetainedAt time.Time
	ExpiresAt  time.Time
}

// RetainTx turns live record id retained (§6.2 step 6: a retain or release decided after the last
// batch; §7.3 step 5: an rclone retain after its move): state retained with retained_path, reason,
// job_id, retained_at and expires_at, and engine_ref = COALESCE(engine_ref, r.Base), so a set
// reference is never moved. A record that is not live any more, or that still has live
// hardlinks (the planner promotes those first), is refused with ErrRecordChanged.
func (s *Store) RetainTx(ctx context.Context, tx *sql.Tx, id int64, r Retain) error {
	if r.RetainedPath == "" || r.ExpiresAt.IsZero() {
		return fmt.Errorf("retain record %d: a retained path and an expiry are required", id)
	}
	deps, err := liveDependentsTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if len(deps) > 0 {
		return fmt.Errorf("%w: record %d still has the live hardlink %s", ErrRecordChanged, id, deps[0].RelPath)
	}
	res, err := tx.ExecContext(ctx, `UPDATE destination_files SET state = 'retained', engine_ref = COALESCE(engine_ref, ?),
		retained_path = ?, reason = ?, job_id = ?, retained_at = ?, expires_at = ?, link_of = NULL
		WHERE id = ? AND state IN `+liveStates, nullStr(r.Base), r.RetainedPath, nullStr(r.Reason), nullInt(r.JobID),
		db.FormatTime(r.RetainedAt), db.FormatTime(r.ExpiresAt), id)
	if err != nil {
		return fmt.Errorf("retain record %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("retain record %d: %w", id, err)
	} else if n == 0 {
		return fmt.Errorf("%w: record %d is not live", ErrRecordChanged, id)
	}
	return nil
}

// PinTx sets live record id's reference to base when it has none (engine_ref =
// COALESCE(engine_ref, base)): the record keeps its version in the base while a new snapshot
// lacks it (a held item, a waiting retain; §6.2 steps 5-6).
func (s *Store) PinTx(ctx context.Context, tx *sql.Tx, id int64, base string) error {
	if base == "" {
		return errors.New("pin: no base snapshot")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET engine_ref = COALESCE(engine_ref, ?) WHERE id = ? AND state IN `+liveStates,
		base, id); err != nil {
		return fmt.Errorf("pin record %d: %w", id, err)
	}
	return nil
}

// tableName is an SQL identifier, optionally schema-qualified ("temp.readback_42").
var tableName = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_]*\.)?[A-Za-z_][A-Za-z0-9_]*$`)

// ReadBack is the reference rule of a restic batch's recording transaction (§6.2 step 5) for every
// live record of one source: a record whose version the new snapshot holds gets engine_ref NULL;
// every other gets engine_ref = COALESCE(engine_ref, Base). Run it after the batch's content items
// are recorded (RecordContentTx), in the same transaction, with Base the source's base before the
// batch.
type ReadBack struct {
	DestinationID int64
	SourceID      int64
	// Base is the source's base snapshot before this batch ("" when the source had none: a record
	// the snapshot does not hold then keeps NULL, which must not happen and is reported).
	Base string
	// Holds reports whether the new snapshot holds a record's version (the listing has the
	// record's absolute path as a file with its size and mtime). Set Holds or Table.
	Holds func(Record) bool
	// Table names a temporary table (path TEXT, size INTEGER, mtime_ns INTEGER) of the snapshot's
	// files by absolute path; Root is the source's root that prefixes a record's source path.
	// mtimes match to the nanosecond, or to the second when either side has no fraction.
	Table string
	Root  string
}

// ApplyReadBackTx applies the reference rule (ReadBack) inside the caller's transaction and returns
// how many records it cleared and how many kept (or got) a reference.
func (s *Store) ApplyReadBackTx(ctx context.Context, tx *sql.Tx, rb ReadBack) (cleared, pinned int64, err error) {
	if rb.Table != "" {
		return s.applyReadBackTable(ctx, tx, rb)
	}
	if rb.Holds == nil {
		return 0, 0, errors.New("read-back: no predicate and no table")
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+recordColumns+` FROM destination_files
		WHERE destination_id = ? AND source_id = ? AND state IN `+liveStates+` ORDER BY id`, rb.DestinationID, rb.SourceID)
	if err != nil {
		return 0, 0, fmt.Errorf("read-back: %w", err)
	}
	var held, notHeld []int64
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			_ = rows.Close()
			return 0, 0, fmt.Errorf("read-back: %w", err)
		}
		if rb.Holds(r) {
			held = append(held, r.ID)
		} else {
			notHeld = append(notHeld, r.ID)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, 0, fmt.Errorf("read-back: %w", err)
	}
	for _, id := range held {
		if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET engine_ref = NULL WHERE id = ?`, id); err != nil {
			return 0, 0, fmt.Errorf("read-back: %w", err)
		}
	}
	if rb.Base != "" {
		for _, id := range notHeld {
			if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET engine_ref = COALESCE(engine_ref, ?) WHERE id = ?`, rb.Base, id); err != nil {
				return 0, 0, fmt.Errorf("read-back: %w", err)
			}
		}
	}
	return int64(len(held)), int64(len(notHeld)), nil
}

// holdsSQL matches a destination_files row f against the read-back table t (ReadBack.Table).
const holdsSQL = `EXISTS (SELECT 1 FROM %s t WHERE t.path = ? || f.source_rel_path AND t.size = f.size
	AND (t.mtime_ns = f.mtime_ns OR ((t.mtime_ns %% 1000000000 = 0 OR f.mtime_ns %% 1000000000 = 0)
		AND t.mtime_ns / 1000000000 = f.mtime_ns / 1000000000)))`

func (s *Store) applyReadBackTable(ctx context.Context, tx *sql.Tx, rb ReadBack) (int64, int64, error) {
	if !tableName.MatchString(rb.Table) {
		return 0, 0, fmt.Errorf("read-back: %q is not a table name", rb.Table)
	}
	prefix := strings.TrimSuffix(rb.Root, "/") + "/"
	holds := fmt.Sprintf(holdsSQL, rb.Table)
	res, err := tx.ExecContext(ctx, `UPDATE destination_files AS f SET engine_ref = NULL
		WHERE f.destination_id = ? AND f.source_id = ? AND f.state IN `+liveStates+` AND `+holds, rb.DestinationID, rb.SourceID, prefix)
	if err != nil {
		return 0, 0, fmt.Errorf("read-back: %w", err)
	}
	cleared, _ := res.RowsAffected()
	var pinned int64
	if rb.Base != "" {
		res, err = tx.ExecContext(ctx, `UPDATE destination_files AS f SET engine_ref = COALESCE(f.engine_ref, ?)
			WHERE f.destination_id = ? AND f.source_id = ? AND f.state IN `+liveStates+` AND NOT `+holds,
			rb.Base, rb.DestinationID, rb.SourceID, prefix)
		if err != nil {
			return 0, 0, fmt.Errorf("read-back: %w", err)
		}
		pinned, _ = res.RowsAffected()
	}
	return cleared, pinned, nil
}

// MarkMissingTx marks live record id missing (verify found its version damaged, §6.6, §7.6; an
// rclone update whose old version moved to the backup dir while no new object arrived, §7.3).
// Its reference is kept (the version may still be where it points).
func (s *Store) MarkMissingTx(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET state = 'missing' WHERE id = ? AND state IN ('present', 'linked')`, id); err != nil {
		return fmt.Errorf("mark record %d missing: %w", id, err)
	}
	return nil
}

// MarkMissingByRefTx marks every live record of a destination whose reference is ref missing, with
// no reference: the snapshot was removed outside Bunkarr (§6.2 step 1), so the plan uploads them
// again. It returns how many it marked.
func (s *Store) MarkMissingByRefTx(ctx context.Context, tx *sql.Tx, destinationID int64, ref string) (int64, error) {
	if ref == "" {
		return 0, errors.New("mark missing: no reference")
	}
	res, err := tx.ExecContext(ctx, `UPDATE destination_files SET state = 'missing', engine_ref = NULL
		WHERE destination_id = ? AND engine_ref = ? AND state IN ('present', 'linked', 'missing')`, destinationID, ref)
	if err != nil {
		return 0, fmt.Errorf("mark the records of snapshot %s missing: %w", ref, err)
	}
	return res.RowsAffected()
}

// MarkMissingBaseTx marks every live record of one source whose version is in its base (engine_ref
// NULL) missing: the base snapshot was removed outside Bunkarr (§6.2 step 1). It returns how many
// it marked.
func (s *Store) MarkMissingBaseTx(ctx context.Context, tx *sql.Tx, destinationID, sourceID int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `UPDATE destination_files SET state = 'missing'
		WHERE destination_id = ? AND source_id = ? AND engine_ref IS NULL AND state IN ('present', 'linked')`, destinationID, sourceID)
	if err != nil {
		return 0, fmt.Errorf("mark the base records of source %d missing: %w", sourceID, err)
	}
	return res.RowsAffected()
}

// DeleteRetainedByRefTx deletes the retained rows of a destination whose version is in snapshot
// ref, which was removed outside Bunkarr: the version is lost (§6.2 step 1). It returns the rows
// it deleted, for the "version lost" warning.
func (s *Store) DeleteRetainedByRefTx(ctx context.Context, tx *sql.Tx, destinationID int64, ref string) ([]Record, error) {
	if ref == "" {
		return nil, errors.New("delete retained versions: no reference")
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+recordColumns+` FROM destination_files
		WHERE destination_id = ? AND engine_ref = ? AND state = 'retained' ORDER BY id`, destinationID, ref)
	if err != nil {
		return nil, fmt.Errorf("read the retained versions of snapshot %s: %w", ref, err)
	}
	var lost []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		lost = append(lost, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read the retained versions of snapshot %s: %w", ref, err)
	}
	for _, r := range lost {
		if err := deleteRecord(ctx, tx, r.ID); err != nil {
			return nil, err
		}
	}
	return lost, nil
}

// DeleteRecordTx deletes a record that nothing live links to (an rclone link_recorded name that
// vanished: it has no object of its own).
func (s *Store) DeleteRecordTx(ctx context.Context, tx *sql.Tx, id int64) error {
	deps, err := liveDependentsTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if len(deps) > 0 {
		return fmt.Errorf("%w: record %d still has the live hardlink %s", ErrRecordChanged, id, deps[0].RelPath)
	}
	return deleteRecord(ctx, tx, id)
}

// SetHeadTailTx records the head/tail hash of live record id's content (§6.2 step 3, §7.3).
func (s *Store) SetHeadTailTx(ctx context.Context, tx *sql.Tx, id int64, headTail string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET head_tail = ? WHERE id = ?`, nullStr(headTail), id); err != nil {
		return fmt.Errorf("record the head/tail hash of %d: %w", id, err)
	}
	return nil
}

// SetIntentTx records a retention intent on the live record at rel (§3.3, rclone): retained is the
// retention path the object moves to, reason its reason. A path without a live record gives
// ErrRecordChanged.
func (s *Store) SetIntentTx(ctx context.Context, tx *sql.Tx, destinationID int64, rel, retained, reason string) error {
	err := setIntentTx(ctx, tx, destinationID, rel, retained, reason)
	var ie *itemError
	if errors.As(err, &ie) {
		return fmt.Errorf("%w: %s", ErrRecordChanged, ie.msg)
	}
	return err
}

// ClearIntentTx removes the retention intent retained from live record id (if it still has it).
func (s *Store) ClearIntentTx(ctx context.Context, tx *sql.Tx, id int64, retained string) error {
	return clearIntentTx(ctx, tx, id, retained)
}

// Intents returns the live records of a destination that carry a retention intent (the
// reconciliation of an rclone job settles them, §3.3).
func (s *Store) Intents(ctx context.Context, destinationID int64) ([]Record, error) {
	return s.intents(ctx, destinationID)
}

// Refs returns the snapshots a restic destination's records reference: the engine_ref of every
// live and retained row (replaced rows included), distinct and sorted. Retention keeps each of
// them (S5, S24). Retained rows are included whatever their expiry: the retention job's expire
// step deletes the expired rows it may expire before it forgets, so a row still here is one a
// hold keeps.
func (s *Store) Refs(ctx context.Context, destinationID int64) ([]string, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT DISTINCT engine_ref FROM destination_files
		WHERE destination_id = ? AND engine_ref IS NOT NULL ORDER BY engine_ref`, destinationID)
	if err != nil {
		return nil, fmt.Errorf("read the references of destination %d: %w", destinationID, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("read the references of destination %d: %w", destinationID, err)
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// ByRef returns the records (live and retained) of a destination that reference snapshot ref.
func (s *Store) ByRef(ctx context.Context, destinationID int64, ref string) ([]Record, error) {
	return s.query(ctx, `WHERE destination_id = ? AND engine_ref = ? ORDER BY id`, destinationID, ref)
}

// RetentionRun is one run directory of retained versions (.bunkarr/retention/<run>/) at a
// destination: the listing of Session.List for rclone and filecopy.
type RetentionRun struct {
	// Path is the run's directory, relative to the destination root.
	Path  string
	Files int64
	Bytes int64
	// RetainedAt is the oldest retained_at of its rows; ExpiresAt the latest expires_at.
	RetainedAt time.Time
	ExpiresAt  time.Time
}

// RetentionRuns lists the retention runs of a destination, by path: its retained rows grouped by
// their run directory under .bunkarr/retention/ (restic retained rows, which name a path in a
// snapshot, are not runs).
func (s *Store) RetentionRuns(ctx context.Context, destinationID int64) ([]RetentionRun, error) {
	recs, err := s.query(ctx, `WHERE destination_id = ? AND state = 'retained' AND retained_path LIKE ? ORDER BY retained_path`,
		destinationID, filecopy.RetentionRoot+"/%")
	if err != nil {
		return nil, err
	}
	byRun := map[string]*RetentionRun{}
	var order []string
	for _, r := range recs {
		rest := strings.TrimPrefix(r.RetainedPath, filecopy.RetentionRoot+"/")
		run, _, ok := strings.Cut(rest, "/")
		if !ok || run == "" {
			continue
		}
		p := path.Join(filecopy.RetentionRoot, run)
		rr := byRun[p]
		if rr == nil {
			rr = &RetentionRun{Path: p}
			byRun[p] = rr
			order = append(order, p)
		}
		rr.Files++
		rr.Bytes += r.Size
		if r.RetainedAt != nil && (rr.RetainedAt.IsZero() || r.RetainedAt.Before(rr.RetainedAt)) {
			rr.RetainedAt = *r.RetainedAt
		}
		if r.ExpiresAt != nil && r.ExpiresAt.After(rr.ExpiresAt) {
			rr.ExpiresAt = *r.ExpiresAt
		}
	}
	slices.Sort(order)
	out := make([]RetentionRun, 0, len(order))
	for _, p := range order {
		out = append(out, *byRun[p])
	}
	return out, nil
}

// LinkManifest returns the content of a destination's hardlink manifest (LinkManifestRel): the
// filecopy sync writes it through its root, the rclone engine uploads it when it changed (§7.3
// step 6).
func (s *Store) LinkManifest(ctx context.Context, destinationID int64) ([]byte, error) {
	links, err := s.links(context.WithoutCancel(ctx), destinationID)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(linkManifestHeader)
	for _, l := range links {
		b.WriteString(manifestPath(l.name))
		b.WriteByte('\t')
		b.WriteString(manifestPath(l.primary))
		b.WriteByte('\t')
		b.WriteString(string(l.state))
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}
