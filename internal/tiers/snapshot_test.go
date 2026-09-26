package tiers

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
)

// TestDecisionsThroughASnapshotReadNoPool: decisions made through a Snapshot (the manifest
// builder's read transaction) read nothing through the read pool, facts of every provider and the
// stale-reference checks included. With the pool cut to the one connection the transaction holds,
// a pool read would wait until the deadline. The decisions are those made through the pool.
func TestDecisionsThroughASnapshotReadNoPool(t *testing.T) {
	e := newLibEnv(t)
	movies := e.srcs["movies"]
	dest := e.destination("d", movies.ID)
	e.saveRules(
		RuleInput{Name: "Maintainerr deletes", Conditions: []Condition{cnd(FieldMaintainerrPending, OpIs, true)}, Action: Skip},
		RuleInput{Name: "Watched", Conditions: []Condition{cnd(FieldTautulliPlayCount, OpGte, 1)}, Action: Manifest},
		RuleInput{Name: "Requested", Match: MatchAny, Conditions: []Condition{cnd(FieldSeerrRequested, OpIs, true),
			cnd(FieldSeerrRequestedBy, OpIn, []int{1})}, Action: Full},
		RuleInput{Name: "Movies section", Conditions: []Condition{cnd(FieldPlexSection, OpIs, itoa64(e.plexIt.ID)+":1")}, Action: Manifest},
	)
	want, _ := e.decisions(dest, movies)
	byRule := map[string]bool{}
	for _, d := range want {
		byRule[d.RuleName] = true
	}
	if len(byRule) < 3 {
		t.Fatalf("the rules decide too little to compare: %v", byRule)
	}

	srcs, err := e.cat.List(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	ints, err := e.ints.List(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.db.Reader().SetMaxOpenConns(1)
	t.Cleanup(func() { e.db.Reader().SetMaxOpenConns(4) })
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	defer cancel()
	tx, err := e.db.Reader().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var ids map[string]int64
	if ids, err = liveIDs(ctx, tx, movies.ID); err != nil {
		t.Fatal(err)
	}
	for _, q := range []Queryer{Snapshot{Queryer: tx, Sources: srcs, Integrations: ints}, &Snapshot{Queryer: tx, Sources: srcs, Integrations: ints}} {
		sd, err := e.eng.DecisionsByID(ctx, q, dest, movies.ID)
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%T: a decision waited for a second connection of the read pool: %v", q, err)
		}
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Decision{}
		for rel, id := range ids {
			got[rel] = sd.Decide(id)
		}
		if !maps.EqualFunc(got, want, decisionsEqual) {
			t.Fatalf("%T: through the snapshot %+v, through the pool %+v", q, got, want)
		}
	}
	// A source the snapshot does not list is not found (not read from the pool).
	if _, err := e.eng.DecisionsByID(ctx, Snapshot{Queryer: tx}, dest, movies.ID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("a source missing from the snapshot: %v", err)
	}
}

func liveIDs(ctx context.Context, q Queryer, sourceID int64) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, rel_path FROM catalog_files WHERE source_id = ? AND deleted_at IS NULL`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var (
			id  int64
			rel string
		)
		if err := rows.Scan(&id, &rel); err != nil {
			return nil, err
		}
		out[rel] = id
	}
	return out, rows.Err()
}

func decisionsEqual(a, b Decision) bool {
	return a.Tier == b.Tier && a.RuleID == b.RuleID && a.RuleName == b.RuleName && a.Follows == b.Follows &&
		a.UnknownPromoted == b.UnknownPromoted && len(a.Reasons) == len(b.Reasons) && len(a.Unknown) == len(b.Unknown)
}
