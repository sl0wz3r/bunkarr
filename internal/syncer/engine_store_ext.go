package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// Record operations the restic and rclone executors of internal/enginerun need besides those of
// store_engine.go (docs/design/phase4.md §6.2, §6.6, §7.3, §7.6): the promote of a hardlink
// group's surviving name, the verify marks, and the sample selection. The SQL of
// destination_files stays in this package; the Tx variants run inside the caller's db.Write.

// ErrNotDependent means a promote's target is not a live dependent of its primary any more.
var ErrNotDependent = errors.New("the hardlink records changed since planning")

// Promote kinds (Detail.For of a promote item).
const (
	PromoteForRetain = "retain"
	PromoteForUpdate = "update"
)

// PromoteTx makes the live dependent targetID hold the content of its primary primaryID (§7.3
// step 1; on restic record-only, §6.2): the target becomes present with no link, the primary's
// hash and head_tail (the same content), the job and, when the content moved, copied_at; every
// other live dependent of the primary is re-pointed to it. moved says the primary's object was
// moved to the target's path (rclone): the primary's record is then deleted (forWhat retain: its
// content lives on under the target) or marked missing (forWhat update: the update writes the new
// version). Without moved (restic: every name is in the snapshot) the primary stays live for its
// own retain or update item. A target that is already present with no link is a promote an
// earlier attempt recorded: nothing changes. A target that is not a live dependent of the primary
// gives ErrNotDependent.
func (s *Store) PromoteTx(ctx context.Context, tx *sql.Tx, primaryID, targetID int64, moved bool, forWhat string, jobID int64, at time.Time) error {
	t, ok, err := getTx(ctx, tx, targetID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: the hardlinked name %d is no longer recorded", ErrNotDependent, targetID)
	}
	if t.State == StatePresent && t.LinkOf == 0 {
		return nil
	}
	v, ok, err := getTx(ctx, tx, primaryID)
	if err != nil {
		return err
	}
	if !ok || !v.State.Live() || !t.State.Live() || t.LinkOf != v.ID || (t.State != StateLinked && t.State != StateLinkRecorded) {
		return fmt.Errorf("%w (%s)", ErrNotDependent, t.RelPath)
	}
	t.State, t.LinkOf, t.Hash, t.JobID = StatePresent, 0, v.Hash, jobID
	if t.HeadTail == "" {
		t.HeadTail = v.HeadTail
	}
	if moved {
		copied := at
		t.CopiedAt = &copied
	}
	if err := updateRecord(ctx, tx, t); err != nil {
		return err
	}
	if err := repoint(ctx, tx, v.ID, t.ID); err != nil {
		return err
	}
	if !moved {
		return nil
	}
	if forWhat == PromoteForRetain {
		return deleteRecord(ctx, tx, v.ID)
	}
	v.State = StateMissing
	return updateRecord(ctx, tx, v)
}

// SetVerifiedTx records a successful verification of record id (§6.6, §7.6): verified_at, and
// the hash when the record has none.
func (s *Store) SetVerifiedTx(ctx context.Context, tx *sql.Tx, id int64, hash string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE destination_files SET verified_at = ?, hash = COALESCE(hash, ?) WHERE id = ?`,
		db.FormatTime(at), nullStr(hash), id)
	if err != nil {
		return fmt.Errorf("record the verification of %d: %w", id, err)
	}
	return nil
}

// RestorePresentTx sets a missing record back to present: a later verify found its recorded
// version intact where it is (§6.6: a later verify tries a missing record again first).
func (s *Store) RestorePresentTx(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET state = 'present' WHERE id = ? AND state = 'missing' AND link_of IS NULL`, id); err != nil {
		return fmt.Errorf("restore record %d: %w", id, err)
	}
	return nil
}

// VerifyCandidates returns up to limit live records of a destination for a verify's content
// sample (§6.6, §7.6), in the order the sample takes them: missing records with content of their
// own first when withMissing (a later verify tries them again first), then present ones, the
// least recently verified first (never verified before verified), then by id. baseOnly keeps the
// records whose version is in their source's base (restic: engine_ref NULL). Records of deleted
// sources are left out.
func (s *Store) VerifyCandidates(ctx context.Context, destinationID int64, baseOnly, withMissing bool, limit int) ([]Record, error) {
	states := `('present')`
	if withMissing {
		states = `('present', 'missing')`
	}
	q := `WHERE destination_id = ? AND source_id IS NOT NULL AND link_of IS NULL AND state IN ` + states
	if baseOnly {
		q += ` AND engine_ref IS NULL`
	}
	q += ` ORDER BY CASE state WHEN 'missing' THEN 0 ELSE 1 END, verified_at IS NOT NULL, verified_at, id LIMIT ?`
	return s.query(ctx, q, destinationID, limit)
}

// CountPresent counts a destination's present records with content of their own (baseOnly: whose
// version is in the base) and their bytes: the population a verify's samplePercent is taken of.
func (s *Store) CountPresent(ctx context.Context, destinationID int64, baseOnly bool) (files, bytes int64, err error) {
	q := `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM destination_files
		WHERE destination_id = ? AND source_id IS NOT NULL AND link_of IS NULL AND state = 'present'`
	if baseOnly {
		q += ` AND engine_ref IS NULL`
	}
	if err := s.reader(q, []any{destinationID}).QueryRowContext(ctx, q, destinationID).Scan(&files, &bytes); err != nil {
		return 0, 0, fmt.Errorf("count the records of destination %d: %w", destinationID, err)
	}
	return files, bytes, nil
}

// LiveBySourcePath returns the live record of a source path at a destination, if any (a link's
// primary, a retain's reappeared check).
func (s *Store) LiveBySourcePath(ctx context.Context, destinationID, sourceID int64, sourceRel string) (Record, bool, error) {
	return s.liveForSourcePath(ctx, destinationID, sourceID, sourceRel)
}

// RetainedBySourcePath returns the retained rows of a source path at a destination (the rclone
// reconciliation and the retention-directory settle look for an existing row).
func (s *Store) RetainedBySourcePath(ctx context.Context, destinationID, sourceID int64, sourceRel string) ([]Record, error) {
	return s.query(ctx, `WHERE destination_id = ? AND source_id IS ? AND source_rel_path = ? AND state = 'retained' ORDER BY id`,
		destinationID, nullInt(sourceID), sourceRel)
}
