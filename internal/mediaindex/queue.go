package mediaindex

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// RefreshSpec is the job spec of a full refresh of an integration.
func RefreshSpec(integrationID int64, trigger jobs.Trigger, dryRun, allowChanges bool) jobs.Spec {
	return jobs.Spec{Type: jobs.TypeRefresh, Trigger: trigger, DryRun: dryRun,
		Params: jobs.Params{IntegrationID: integrationID, AllowChanges: allowChanges}}
}

// QueueStartup queues a full refresh (trigger startup; the queue deduplicates it against one
// already queued) of every enabled integration whose refresh Supports, so the events missed while
// Bunkarr was down are reconciled (design §6.1, D8). An install without such an integration
// queues nothing. It returns the queued jobs and every enqueue error.
func QueueStartup(ctx context.Context, list []integrations.Integration, enq jobs.Enqueuer) ([]jobs.Job, error) {
	var (
		out  []jobs.Job
		errs []error
	)
	for _, it := range list {
		if !it.Enabled || !Supports(it.Type) {
			continue
		}
		job, err := enq.Enqueue(ctx, RefreshSpec(it.ID, jobs.TriggerStartup, false, false))
		if err != nil {
			errs = append(errs, fmt.Errorf("queue the start-up refresh of %q: %w", it.Name, err))
			continue
		}
		out = append(out, job)
	}
	return out, errors.Join(errs...)
}

// NeedsRefresh reports whether a saved integration needs a full refresh (design §4.1): it was
// created (before is nil), or an update changed its URL, its key (keyChanged) or its path
// mappings. Only enabled integrations whose refresh Supports need one.
func NeedsRefresh(before *integrations.Integration, after integrations.Integration, keyChanged bool) bool {
	if !after.Enabled || !Supports(after.Type) {
		return false
	}
	if before == nil || !before.Enabled || before.URL != after.URL || keyChanged {
		return true
	}
	if after.Type.IsArr() {
		a, errA := before.ArrSettings()
		b, errB := after.ArrSettings()
		if errA != nil || errB != nil {
			return true
		}
		return !slices.Equal(a.PathMappings, b.PathMappings)
	}
	return false
}
