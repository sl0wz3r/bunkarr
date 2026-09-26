package jobqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// DefaultPendingLimit is the batch size Pending uses when limit <= 0.
const DefaultPendingLimit = 500

const itemColumns = `id, job_id, file_id, rel_path, action, status, bytes, error, detail`

func scanItem(sc rowScanner) (jobs.Item, error) {
	var (
		it             jobs.Item
		fileID         sql.NullInt64
		action, status string
		errText        sql.NullString
		detail         string
	)
	if err := sc.Scan(&it.ID, &it.JobID, &fileID, &it.RelPath, &action, &status, &it.Bytes, &errText, &detail); err != nil {
		return it, err
	}
	it.FileID = fileID.Int64
	it.Action, it.Status = jobs.ItemAction(action), jobs.ItemStatus(status)
	it.Error = errText.String
	if detail != "" {
		it.Detail = json.RawMessage(detail)
	}
	return it, nil
}

// detailText validates an item detail and returns its stored form ("{}" when empty).
func detailText(d json.RawMessage) (string, error) {
	if len(d) == 0 {
		return "{}", nil
	}
	if !json.Valid(d) {
		return "", ValidationError("item detail is not valid JSON")
	}
	return string(d), nil
}

// Planned implements jobs.ItemStore: whether jobs.planned_at is set.
func (s *Store) Planned(ctx context.Context, jobID int64) (bool, error) {
	var planned sql.NullString
	err := s.db.Reader().QueryRowContext(ctx, `SELECT planned_at FROM jobs WHERE id = ?`, jobID).Scan(&planned)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("job %d: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return false, fmt.Errorf("read plan state of job %d: %w", jobID, err)
	}
	return planned.Valid, nil
}

// AddItems implements jobs.ItemStore. The batch is inserted in one transaction; with final=true
// the same transaction sets jobs.planned_at, so a plan is either complete or recognisably
// interrupted. An item without a status is pending; error texts are redacted.
func (s *Store) AddItems(ctx context.Context, jobID int64, items []jobs.Item, final bool) error {
	type row struct {
		it     jobs.Item
		detail string
	}
	rows := make([]row, len(items))
	for i, it := range items {
		if it.Status == "" {
			it.Status = jobs.ItemPending
		}
		if !validAction(it.Action) {
			return ValidationError(fmt.Sprintf("item %q: unknown action %q", it.RelPath, it.Action))
		}
		if !validItemStatus(it.Status) {
			return ValidationError(fmt.Sprintf("item %q: unknown status %q", it.RelPath, it.Status))
		}
		d, err := detailText(it.Detail)
		if err != nil {
			return fmt.Errorf("item %q: %w", it.RelPath, err)
		}
		rows[i] = row{it: it, detail: d}
	}
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		ok, err := jobExists(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("job %d: %w", jobID, ErrNotFound)
		}
		if len(rows) > 0 {
			stmt, err := tx.PrepareContext(ctx, `INSERT INTO job_items (job_id, file_id, rel_path, action, status, bytes, error, detail)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, r := range rows {
				if _, err := stmt.ExecContext(ctx, jobID, nullInt(r.it.FileID), r.it.RelPath, string(r.it.Action),
					string(r.it.Status), r.it.Bytes, nullString(redactText(r.it.Error)), r.detail); err != nil {
					return fmt.Errorf("insert item %q: %w", r.it.RelPath, err)
				}
			}
		}
		if final {
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET planned_at = ? WHERE id = ?`, db.FormatTime(s.now()), jobID); err != nil {
				return fmt.Errorf("mark plan complete: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("add items to job %d: %w", jobID, err)
	}
	return nil
}

// DeleteItems implements jobs.ItemStore: removes all of the job's items and clears planned_at, in
// one transaction.
func (s *Store) DeleteItems(ctx context.Context, jobID int64) error {
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET planned_at = NULL WHERE id = ?`, jobID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("job %d: %w", jobID, ErrNotFound)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM job_items WHERE job_id = ?`, jobID)
		return err
	})
	if err != nil {
		return fmt.Errorf("delete items of job %d: %w", jobID, err)
	}
	return nil
}

// Pending implements jobs.ItemStore: up to limit pending items with id > afterID, in id order
// (limit <= 0 means DefaultPendingLimit).
func (s *Store) Pending(ctx context.Context, jobID, afterID int64, limit int) ([]jobs.Item, error) {
	if limit <= 0 {
		limit = DefaultPendingLimit
	}
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT `+itemColumns+` FROM job_items
		WHERE job_id = ? AND status = 'pending' AND id > ? ORDER BY id LIMIT ?`, jobID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("read pending items of job %d: %w", jobID, err)
	}
	defer rows.Close()
	out := []jobs.Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("read pending items of job %d: %w", jobID, err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending items of job %d: %w", jobID, err)
	}
	return out, nil
}

// SetDetail implements jobs.ItemStore: replaces an item's detail (valid JSON; empty means {}).
func (s *Store) SetDetail(ctx context.Context, itemID int64, detail json.RawMessage) error {
	d, err := detailText(detail)
	if err != nil {
		return fmt.Errorf("item %d: %w", itemID, err)
	}
	return s.updateItem(ctx, itemID, `UPDATE job_items SET detail = ? WHERE id = ?`, d, itemID)
}

// Finish implements jobs.ItemStore: records an item's outcome (done, failed, skipped or held),
// its bytes and error text (redacted; "" clears it).
func (s *Store) Finish(ctx context.Context, itemID int64, status jobs.ItemStatus, bytes int64, errMsg string) error {
	if !validItemStatus(status) || status == jobs.ItemPending {
		return ValidationError(fmt.Sprintf("item %d: %q is not a final item status", itemID, status))
	}
	return s.updateItem(ctx, itemID, `UPDATE job_items SET status = ?, bytes = ?, error = ? WHERE id = ?`,
		string(status), bytes, nullString(redactText(errMsg)), itemID)
}

func (s *Store) updateItem(ctx context.Context, itemID int64, stmt string, args ...any) error {
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, stmt, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("item %d: %w", itemID, ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("update item %d: %w", itemID, err)
	}
	return nil
}

// Counts implements jobs.ItemStore: the number of items and their bytes by action and status,
// ordered by action then status.
func (s *Store) Counts(ctx context.Context, jobID int64) ([]jobs.ItemCount, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT action, status, COUNT(*), COALESCE(SUM(bytes), 0) FROM job_items
		WHERE job_id = ? GROUP BY action, status ORDER BY action, status`, jobID)
	if err != nil {
		return nil, fmt.Errorf("count items of job %d: %w", jobID, err)
	}
	defer rows.Close()
	out := []jobs.ItemCount{}
	for rows.Next() {
		var c jobs.ItemCount
		var action, status string
		if err := rows.Scan(&action, &status, &c.Files, &c.Bytes); err != nil {
			return nil, fmt.Errorf("count items of job %d: %w", jobID, err)
		}
		c.Action, c.Status = jobs.ItemAction(action), jobs.ItemStatus(status)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count items of job %d: %w", jobID, err)
	}
	return out, nil
}

// TierItemCount is an ItemCount with the tier dimension (phase2-3.md §13): Tier is the tier
// decision an item's detail records (detail.tier.tier), full for a file copy that records none.
type TierItemCount struct {
	jobs.ItemCount
	Tier string `json:"tier"`
}

// itemTierExpr is an item's tier: the decision its detail records. Without one, a file copy
// (copy, update, move, link, adopt) is full: a sync without tier rules copies every file in
// full. Any other item without one (a vanished file's retain, a promote, an expire, a verify, a
// job type that does not tier) has no tier (NULL): it is not a tier decision, so neither a tier
// filter nor the tier dimension counts it. Details are valid JSON (detailText).
const itemTierExpr = `COALESCE(json_extract(detail, '$.tier.tier'),
	CASE WHEN action IN ('copy', 'update', 'move', 'link', 'adopt') THEN 'full' END)`

// validItemTier reports whether t is a tier (full, manifest or skip).
func validItemTier(t string) bool { return t == "full" || t == "manifest" || t == "skip" }

// TierCounts is Counts with the tier dimension: items and bytes by action, status and tier,
// ordered by action, status, then tier. Items without a tier (itemTierExpr) are left out.
func (s *Store) TierCounts(ctx context.Context, jobID int64) ([]TierItemCount, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT action, status, tier, COUNT(*), COALESCE(SUM(bytes), 0)
		FROM (SELECT action, status, bytes, `+itemTierExpr+` AS tier FROM job_items WHERE job_id = ?)
		WHERE tier IS NOT NULL GROUP BY action, status, tier ORDER BY action, status, tier`, jobID)
	if err != nil {
		return nil, fmt.Errorf("count items of job %d by tier: %w", jobID, err)
	}
	defer rows.Close()
	out := []TierItemCount{}
	for rows.Next() {
		var c TierItemCount
		var action, status string
		if err := rows.Scan(&action, &status, &c.Tier, &c.Files, &c.Bytes); err != nil {
			return nil, fmt.Errorf("count items of job %d by tier: %w", jobID, err)
		}
		c.Action, c.Status = jobs.ItemAction(action), jobs.ItemStatus(status)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count items of job %d by tier: %w", jobID, err)
	}
	return out, nil
}

// ItemQuery filters ListItems. Zero values do not filter.
type ItemQuery struct {
	Action jobs.ItemAction
	Status jobs.ItemStatus
	// Tier filters by the tier decision the item's detail records (full, manifest or skip; a
	// file copy that records none is full, and any other item that records none matches no
	// tier: itemTierExpr).
	Tier string
	// RuleID filters by the deciding rule the item's detail records (detail.tier.ruleId; 0 is a
	// built-in: no rule matched, or the irreplaceable override); nil does not filter.
	RuleID *int64
	// Page is 1-based; PageSize defaults to DefaultPageSize and is capped at MaxPageSize.
	Page     int
	PageSize int
}

// ListItems returns one page of a job's items in id (plan) order. A job that does not exist is
// ErrNotFound.
func (s *Store) ListItems(ctx context.Context, jobID int64, q ItemQuery) (Page[jobs.Item], error) {
	where := []string{"job_id = ?"}
	args := []any{jobID}
	if q.Action != "" {
		if !validAction(q.Action) {
			return Page[jobs.Item]{}, ValidationError(fmt.Sprintf("unknown item action %q", q.Action))
		}
		where = append(where, "action = ?")
		args = append(args, string(q.Action))
	}
	if q.Status != "" {
		if !validItemStatus(q.Status) {
			return Page[jobs.Item]{}, ValidationError(fmt.Sprintf("unknown item status %q", q.Status))
		}
		where = append(where, "status = ?")
		args = append(args, string(q.Status))
	}
	if q.Tier != "" {
		if !validItemTier(q.Tier) {
			return Page[jobs.Item]{}, ValidationError(fmt.Sprintf("unknown tier %q (full, manifest or skip)", q.Tier))
		}
		where = append(where, itemTierExpr+" = ?")
		args = append(args, q.Tier)
	}
	if q.RuleID != nil {
		where = append(where, "json_extract(detail, '$.tier.ruleId') = ?")
		args = append(args, *q.RuleID)
	}
	cond := " WHERE " + strings.Join(where, " AND ")
	page, size := normalizePage(q.Page, q.PageSize)
	out := Page[jobs.Item]{Page: page, PageSize: size, Records: []jobs.Item{}}
	err := s.readTx(ctx, func(tx *sql.Tx) error {
		ok, err := jobExists(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("job %d: %w", jobID, ErrNotFound)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM job_items`+cond, args...).Scan(&out.TotalRecords); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+itemColumns+` FROM job_items`+cond+` ORDER BY id LIMIT ? OFFSET ?`,
			append(args, size, (page-1)*size)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				return err
			}
			out.Records = append(out.Records, it)
		}
		return rows.Err()
	})
	if err != nil {
		return Page[jobs.Item]{}, fmt.Errorf("list items of job %d: %w", jobID, err)
	}
	return out, nil
}
