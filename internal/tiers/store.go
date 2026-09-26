package tiers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// SettingRevision is the settings key of the rule set's revision, bumped by every save (§8.7).
const SettingRevision = "tiers.revision"

// Store reads and writes tier_rules, item_flags and the setting tiers.revision.
type Store struct {
	db  *db.DB
	now func() time.Time
}

// NewStore returns a store over d; now is the clock (nil: time.Now).
func NewStore(d *db.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: d, now: now}
}

func (s *Store) reader(q Queryer) Queryer {
	if q == nil {
		return s.db.Reader()
	}
	return q
}

// Revision returns tiers.revision (0 before the first save). q may be nil (the read pool).
func (s *Store) Revision(ctx context.Context, q Queryer) (int64, error) {
	return revisionQ(ctx, s.reader(q))
}

func revisionQ(ctx context.Context, q Queryer) (int64, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, SettingRevision).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the tier rules' revision: %w", err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("read the tier rules' revision: %q is not a number", v)
	}
	return n, nil
}

const ruleColumns = `id, priority, name, enabled, match, conditions, action, destination_ids, created_at, updated_at`

func scanRule(r interface{ Scan(...any) error }) (Rule, error) {
	var (
		rule              Rule
		enabled           int64
		conds, match, act string
		dests             sql.NullString
		created, updated  string
	)
	if err := r.Scan(&rule.ID, &rule.Priority, &rule.Name, &enabled, &match, &conds, &act, &dests, &created, &updated); err != nil {
		return Rule{}, err
	}
	rule.Enabled, rule.Match, rule.Action = enabled != 0, Match(match), Tier(act)
	if err := json.Unmarshal([]byte(conds), &rule.Conditions); err != nil {
		return Rule{}, fmt.Errorf("rule %d: conditions: %w", rule.ID, err)
	}
	if rule.Conditions == nil {
		rule.Conditions = []Condition{}
	}
	if dests.Valid {
		rule.DestinationIDs = []int64{}
		if err := json.Unmarshal([]byte(dests.String), &rule.DestinationIDs); err != nil {
			return Rule{}, fmt.Errorf("rule %d: destination_ids: %w", rule.ID, err)
		}
		if rule.DestinationIDs == nil {
			rule.DestinationIDs = []int64{}
		}
	}
	var err error
	if rule.CreatedAt, err = db.ParseTime(created); err != nil {
		return Rule{}, fmt.Errorf("rule %d: created_at: %w", rule.ID, err)
	}
	if rule.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return Rule{}, fmt.Errorf("rule %d: updated_at: %w", rule.ID, err)
	}
	return rule, nil
}

// Rules returns the rule set in priority order with its revision. q may be nil (the read pool).
// On the pool the two reads can run on different connections, so a save committed between them
// would pair the old revision with the new rules (and a release checked against that revision
// would pass after a rule change): the revision is read again after the rules, and the read is
// retried until both agree. In a transaction they always do.
func (s *Store) Rules(ctx context.Context, q Queryer) (RuleSet, error) {
	q = s.reader(q)
	for attempt := 0; ; attempt++ {
		rev, err := revisionQ(ctx, q)
		if err != nil {
			return RuleSet{}, err
		}
		rules, err := rulesQ(ctx, q)
		if err != nil {
			return RuleSet{}, err
		}
		after, err := revisionQ(ctx, q)
		if err != nil {
			return RuleSet{}, err
		}
		if after == rev {
			return RuleSet{Revision: rev, Rules: rules}, nil
		}
		if attempt >= 9 {
			return RuleSet{}, fmt.Errorf("read the tier rules: they kept changing while being read (revision %d, then %d)", rev, after)
		}
	}
}

func rulesQ(ctx context.Context, q Queryer) ([]Rule, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+ruleColumns+` FROM tier_rules ORDER BY priority, id`)
	if err != nil {
		return nil, fmt.Errorf("read the tier rules: %w", err)
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("read the tier rules: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the tier rules: %w", err)
	}
	return out, nil
}

// Replace replaces the whole ordered rule set in one transaction (PUT /tiers/rules, §8.7). base
// must be the revision the caller loaded (ErrRevisionMismatch otherwise). The array order is the
// priority. A rule sent with an id keeps it (the id must be one of the stored rules); a new rule
// gets a new id (never reused). Every destination id must exist. The revision is bumped by one.
// The error of an invalid rule is a *ValidationError.
func (s *Store) Replace(ctx context.Context, base int64, in []RuleInput) (RuleSet, error) {
	norm, err := Validate(in)
	if err != nil {
		return RuleSet{}, err
	}
	now := db.FormatTime(s.now())
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		rev, err := revisionQ(ctx, tx)
		if err != nil {
			return err
		}
		if rev != base {
			return ErrRevisionMismatch
		}
		old, err := rulesQ(ctx, tx)
		if err != nil {
			return err
		}
		byID := make(map[int64]Rule, len(old))
		for _, r := range old {
			byID[r.ID] = r
		}
		dests, err := destinationIDs(ctx, tx)
		if err != nil {
			return err
		}
		keep := map[int64]bool{}
		for i, r := range norm {
			if r.ID != 0 {
				if _, ok := byID[r.ID]; !ok {
					return invalid(i, -1, "rule id %d does not exist (reload the rules)", r.ID)
				}
				keep[r.ID] = true
			}
			for _, id := range r.DestinationIDs {
				if !dests[id] {
					return invalid(i, -1, "destination %d does not exist", id)
				}
			}
		}
		for _, r := range old {
			if !keep[r.ID] {
				if _, err := tx.ExecContext(ctx, `DELETE FROM tier_rules WHERE id = ?`, r.ID); err != nil {
					return fmt.Errorf("delete tier rule %d: %w", r.ID, err)
				}
			}
		}
		for i, r := range norm {
			conds, err := json.Marshal(r.Conditions)
			if err != nil {
				return err
			}
			var destIDs any
			if r.DestinationIDs != nil {
				b, err := json.Marshal(r.DestinationIDs)
				if err != nil {
					return err
				}
				destIDs = string(b)
			}
			enabled := 0
			if r.enabled() {
				enabled = 1
			}
			if r.ID == 0 {
				_, err = tx.ExecContext(ctx, `INSERT INTO tier_rules (priority, name, enabled, match, conditions, action, destination_ids,
					created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					i+1, r.Name, enabled, string(r.Match), string(conds), string(r.Action), destIDs, now, now)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE tier_rules SET priority = ?, name = ?, enabled = ?, match = ?, conditions = ?,
					action = ?, destination_ids = ?, updated_at = CASE WHEN name = ? AND enabled = ? AND match = ? AND conditions = ?
					AND action = ? AND destination_ids IS ? THEN updated_at ELSE ? END WHERE id = ?`,
					i+1, r.Name, enabled, string(r.Match), string(conds), string(r.Action), destIDs,
					r.Name, enabled, string(r.Match), string(conds), string(r.Action), destIDs, now, r.ID)
			}
			if err != nil {
				return fmt.Errorf("save tier rule %d: %w", i+1, err)
			}
		}
		return setRevisionTx(ctx, tx, rev+1, now)
	})
	if err != nil {
		return RuleSet{}, err
	}
	return s.Rules(ctx, nil)
}

func setRevisionTx(ctx context.Context, tx *sql.Tx, rev int64, now string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, encrypted, updated_at) VALUES (?, ?, 0, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, encrypted = 0, updated_at = excluded.updated_at`,
		SettingRevision, strconv.FormatInt(rev, 10), now)
	if err != nil {
		return fmt.Errorf("save the tier rules' revision: %w", err)
	}
	return nil
}

func destinationIDs(ctx context.Context, q Queryer) (map[int64]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM destinations`)
	if err != nil {
		return nil, fmt.Errorf("read destinations: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// RemoveDestination removes a deleted destination's id from every rule's destination_ids
// (§8.7). An array left empty makes the rule apply nowhere, never everywhere. The revision is not
// bumped: no other destination's decisions change.
func (s *Store) RemoveDestination(ctx context.Context, destID int64) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		rules, err := rulesQ(ctx, tx)
		if err != nil {
			return err
		}
		for _, r := range rules {
			if r.DestinationIDs == nil || !slices.Contains(r.DestinationIDs, destID) {
				continue
			}
			ids := slices.DeleteFunc(slices.Clone(r.DestinationIDs), func(id int64) bool { return id == destID })
			b, err := json.Marshal(ids)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tier_rules SET destination_ids = ?, updated_at = ? WHERE id = ?`,
				string(b), db.FormatTime(s.now()), r.ID); err != nil {
				return fmt.Errorf("update tier rule %d: %w", r.ID, err)
			}
		}
		return nil
	})
}
