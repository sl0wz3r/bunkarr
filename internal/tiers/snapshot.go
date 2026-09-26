package tiers

import (
	"context"
	"fmt"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
)

// Snapshot is a read transaction together with the configuration its caller listed before it
// began: the sources and the integrations (configuration, not state, design §11.2). Passed as the
// Queryer of Decisions or DecisionsByID, every read goes through the transaction and the
// configuration comes from these lists, so nothing is read through the read pool while the
// transaction holds one of its connections. A pool read there would wait for a second connection
// (builds holding every connection of the pool would wait for each other for ever), and it would
// not see the transaction's snapshot (S20). The manifest builder passes one; the syncer and the
// API read through the pool (a nil Queryer).
type Snapshot struct {
	Queryer
	Sources      []catalog.Source
	Integrations []integrations.Integration
}

// snapshotOf returns q as a Snapshot.
func snapshotOf(q Queryer) (*Snapshot, bool) {
	switch s := q.(type) {
	case Snapshot:
		return &s, true
	case *Snapshot:
		return s, s != nil
	}
	return nil, false
}

// integrationsOf lists the integrations: q's when it is a Snapshot, the store's otherwise.
func integrationsOf(ctx context.Context, store *integrations.Store, q Queryer) ([]integrations.Integration, error) {
	if s, ok := snapshotOf(q); ok {
		return s.Integrations, nil
	}
	return store.List(ctx)
}

// sourcesOf lists the sources: q's when it is a Snapshot, the store's otherwise.
func sourcesOf(ctx context.Context, store *catalog.Store, q Queryer) ([]catalog.Source, error) {
	if s, ok := snapshotOf(q); ok {
		return s.Sources, nil
	}
	return store.List(ctx)
}

// sourceOf returns a source: from q's list when it is a Snapshot, from the store otherwise. An
// error wraps catalog.ErrNotFound when there is no such source.
func sourceOf(ctx context.Context, store *catalog.Store, q Queryer, id int64) (catalog.Source, error) {
	if s, ok := snapshotOf(q); ok {
		for _, src := range s.Sources {
			if src.ID == id {
				return src, nil
			}
		}
		return catalog.Source{}, fmt.Errorf("source %d: %w", id, catalog.ErrNotFound)
	}
	return store.Get(ctx, id)
}
