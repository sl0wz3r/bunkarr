package tiers

import (
	"context"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
)

// fakeProvider supplies Maintainerr facts: pending files by source path, or unknown with stale.
type fakeProvider struct {
	fields        []string
	integrationID int64
	pending       map[string]bool
	stale         string
}

func (p *fakeProvider) Fields() []string { return p.fields }

func (p *fakeProvider) Load(_ context.Context, _ Queryer, _ catalog.Source, files []*Facts, _ time.Time) error {
	if !slices.Contains(p.fields, FieldMaintainerrPending) {
		return nil
	}
	for _, f := range files {
		m := &MaintainerrFacts{IntegrationID: p.integrationID, Pending: resultOf(p.pending[f.RelPath])}
		if p.stale != "" {
			m.Pending, m.Why = Unknown, p.stale
		}
		f.Maintainerr = m
	}
	return nil
}

func (p *fakeProvider) Unknown(context.Context, Queryer, time.Time) ([]UnknownSource, error) {
	if p.stale == "" {
		return nil, nil
	}
	return []UnknownSource{{IntegrationID: p.integrationID, Name: "Maintainerr", Reason: p.stale}}, nil
}

func (p *fakeProvider) Suggestions(context.Context, string) ([]Suggestion, error) { return nil, nil }

func (p *fakeProvider) Known(context.Context, Queryer, string, string, []int64, time.Time) (bool, error) {
	return true, nil
}
