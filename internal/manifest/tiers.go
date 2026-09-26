package manifest

import (
	"context"

	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

// TierEngine plugs the tier engine (internal/tiers, design §8) into the manifest builder: each
// source's decisions are made through the builder's read transaction, with the sources and
// integrations the builder listed (a tiers.Snapshot), so the tiers and the rest of a manifest
// describe one state of the database (§11.2) and a build never asks the read pool for a second
// connection. With no rules every file is full.
type TierEngine struct {
	Engine *tiers.Engine
}

var _ Tiers = TierEngine{}

// Decide implements Tiers.
func (t TierEngine) Decide(ctx context.Context, r TierRead, destinationID, sourceID int64) (func(int64) Decision, error) {
	snap := tiers.Snapshot{Queryer: r.Q, Sources: r.Sources, Integrations: r.Integrations}
	d, err := t.Engine.DecisionsByID(ctx, snap, destinationID, sourceID)
	if err != nil {
		return nil, err
	}
	return func(fileID int64) Decision {
		dec := d.Decide(fileID)
		return Decision{Tier: Tier(dec.Tier), RuleID: dec.RuleID, RuleName: dec.RuleName}
	}, nil
}
