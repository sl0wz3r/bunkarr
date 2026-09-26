package mediaindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// The provider refreshes (design §6.2, §6.3): the Plex library index and the Tautulli, Seerr and
// Maintainerr caches. Each one is all-or-nothing: it fetches everything first, then replaces the
// integration's rows and records the complete refresh in one transaction, only if every request
// succeeded. A failure records the attempt and leaves the rows and refreshed_at alone, so the
// cache ages into unknown (S14). A dry run fetches and counts, and writes nothing (S9).

// Fault points of the provider refreshes (the crash matrix).
const (
	// PointProviderFetched: everything is fetched; nothing is written yet.
	PointProviderFetched = "refresh.providerFetched"
	// PointProviderReplaced: the rows are replaced and the refresh recorded; the job has not
	// finished.
	PointProviderReplaced = "refresh.providerReplaced"
)

// Refreshes reports whether refresh jobs run for it: every Sonarr, Radarr, Lidarr, Tautulli, Seerr
// and Maintainerr integration, and a Plex integration whose library index is turned on.
func Refreshes(it integrations.Integration) bool {
	switch it.Type {
	case integrations.TypeTautulli, integrations.TypeSeerr, integrations.TypeMaintainerr:
		return true
	case integrations.TypePlex:
		ps, err := it.PlexSettings()
		return err == nil && ps.IndexSettings().Enabled
	}
	return Supports(it.Type)
}

// noCacheReason says why an integration has no cache that counts.
func noCacheReason(it integrations.Integration) string {
	if it.Type == integrations.TypePlex {
		return fmt.Sprintf("the Plex library index of %s is turned off", it.Name)
	}
	return fmt.Sprintf("%s has no metadata cache", it.Type.AppName())
}

// PlexInstanceID is the instance id of a Plex library index: "<url>#<machineIdentifier>".
func PlexInstanceID(url, machineIdentifier string) string {
	return url + "#" + machineIdentifier
}

// InstanceMatches reports whether a cache's instance id names the integration's current URL (for
// the Plex index, the URL part of "<url>#<machineIdentifier>").
func InstanceMatches(instanceID string, it integrations.Integration) bool {
	if instanceID == "" {
		return false
	}
	if it.Type == integrations.TypePlex {
		u, _, ok := strings.Cut(instanceID, "#")
		return ok && u == it.URL
	}
	return instanceID == it.URL
}

// ProviderStats are the stats of a Plex index, Tautulli, Seerr or Maintainerr refresh (design
// §12.4; the keys shared with the *arr refresh have the same names).
type ProviderStats struct {
	IntegrationID   int64             `json:"integrationId"`
	IntegrationType integrations.Type `json:"integrationType"`
	DryRun          bool              `json:"dryRun"`
	Targeted        bool              `json:"targeted"`
	AppVersion      string            `json:"appVersion,omitempty"`
	// Items is how many rows the cache holds after the refresh (a dry run or a held refresh: how
	// many it would hold); ItemsBefore how many it held before.
	Items       int64 `json:"items"`
	ItemsBefore int64 `json:"itemsBefore"`
	// Files, FilesMapped and FilesUnmapped: the Plex index's files and whether they locate in a
	// source.
	Files         int64 `json:"files"`
	FilesMapped   int64 `json:"filesMapped"`
	FilesUnmapped int64 `json:"filesUnmapped"`
	// Requests is how many requests were sent.
	Requests int64 `json:"requests"`
	// GuardHeld is set when the refresh guard (S10) kept the old rows; GuardReason says why.
	GuardHeld    bool    `json:"guardHeld"`
	GuardReason  string  `json:"guardReason,omitempty"`
	FollowUpJobs []int64 `json:"followUpJobs"`
	// Cache holds the type's own counts (the same object index_state.stats keeps).
	Cache      any   `json:"cache,omitempty"`
	DurationMs int64 `json:"durationMs"`
}

// providerRun is one provider refresh attempt.
type providerRun struct {
	r        *Runner
	job      jobs.Job
	env      jobs.Env
	it       integrations.Integration
	runAt    time.Time
	dry      bool
	stats    ProviderStats
	warnings int
	summary  string
}

func (pr *providerRun) warn(msg string, args ...any) {
	pr.warnings++
	pr.env.Reporter.Log(slog.LevelWarn, msg, args...)
}

func (pr *providerRun) info(msg string, args ...any) {
	pr.env.Reporter.Log(slog.LevelInfo, msg, args...)
}

// runProvider runs a refresh of a Plex index, Tautulli, Seerr or Maintainerr integration.
func (r *Runner) runProvider(ctx context.Context, job jobs.Job, env jobs.Env, it integrations.Integration) (jobs.Result, error) {
	start := r.o.Now()
	pr := &providerRun{r: r, job: job, env: env, it: it, runAt: start.UTC(), dry: job.DryRun,
		stats: ProviderStats{IntegrationID: it.ID, IntegrationType: it.Type, DryRun: job.DryRun, FollowUpJobs: []int64{}}}
	var err error
	switch {
	case len(job.Params.ArrItemIDs) > 0 || job.Params.SyncAfter:
		err = fmt.Errorf("%q is a %s integration: arrItemIds and syncAfter are only for Sonarr, Radarr and Lidarr", it.Name, it.Type.AppName())
	case !it.Enabled:
		err = fmt.Errorf("%s %q is disabled", it.Type.AppName(), it.Name)
	default:
		switch it.Type {
		case integrations.TypePlex:
			err = pr.plexIndex(ctx)
		case integrations.TypeTautulli:
			err = pr.refreshTautulli(ctx)
		case integrations.TypeSeerr:
			err = pr.refreshSeerr(ctx)
		case integrations.TypeMaintainerr:
			err = pr.refreshMaintainerr(ctx)
		default:
			err = fmt.Errorf("refreshing %s integrations is not available", it.Type.AppName())
		}
	}
	pr.stats.DurationMs = r.o.Now().Sub(start).Milliseconds()
	if err != nil {
		if !pr.dry && ctx.Err() == nil {
			if serr := r.store.recordFailure(context.WithoutCancel(ctx), it.ID, pr.runAt, err); serr != nil {
				r.log.Error("Could not record a failed refresh", "integrationId", it.ID, "error", serr)
			}
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return jobs.Result{Stats: pr.stats, Warnings: pr.warnings}, err
	}
	return jobs.Result{Stats: pr.stats, Warnings: pr.warnings, Summary: pr.summary}, nil
}

// linkedPlex returns the Plex integration a Tautulli, Seerr or Maintainerr integration names.
func (pr *providerRun) linkedPlex(ctx context.Context, id int64) (integrations.Integration, error) {
	p, err := pr.r.o.Integrations.Get(ctx, id)
	if errors.Is(err, integrations.ErrNotFound) {
		return p, fmt.Errorf("the linked Plex integration %d no longer exists: link %q to a Plex server again", id, pr.it.Name)
	}
	if err != nil {
		return p, err
	}
	if p.Type != integrations.TypePlex {
		return p, fmt.Errorf("integration %d is not a Plex integration", id)
	}
	return p, nil
}

// plexIdentity reads the linked Plex server's machineIdentifier from /identity (sent without the
// token, as Plex answers it to anyone).
func (pr *providerRun) plexIdentity(ctx context.Context, p integrations.Integration) (string, error) {
	c, err := plex.New(p.URL, "", pr.r.o.Plex)
	if err != nil {
		return "", fmt.Errorf("Plex %q: %w", p.Name, err)
	}
	id, err := c.Identity(ctx)
	if err != nil {
		return "", err
	}
	return id.MachineIdentifier, nil
}

// rowCount counts an integration's rows of a cache table.
func (pr *providerRun) rowCount(ctx context.Context, table string) (int64, error) {
	var n int64
	if err := pr.r.o.DB.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE integration_id = ?`, pr.it.ID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the cached rows: %w", err)
	}
	return n, nil
}

// shrinkReason is the refresh guard of Tautulli and Seerr (S10): a refresh that would remove more
// than half of the cache's rows (and more than GuardMinimum), or that got none while rows exist,
// is held. It returns why ("" when the refresh may apply).
func shrinkReason(before, after int64) string {
	switch {
	case before > 0 && after == 0:
		return fmt.Sprintf("the application answered no rows while the cache has %d", before)
	case before-after > GuardMinimum && after*2 < before:
		return fmt.Sprintf("the cache would shrink from %d rows to %d", before, after)
	}
	return ""
}

// guard applies the shrink guard before a replacement; it returns true when the refresh is held
// (the old rows stay, refreshed_at does not move, the job ends with a warning). A cache of another
// instance is never guarded: it is replaced.
func (pr *providerRun) guard(ctx context.Context, before, after int64, instanceChanged bool) (bool, error) {
	reason := shrinkReason(before, after)
	if reason == "" || instanceChanged {
		return false, nil
	}
	if pr.job.Params.AllowChanges {
		pr.info("The refresh guard would have held this refresh; applied because the job allows changes", "reason", reason)
		return false, nil
	}
	pr.stats.GuardHeld, pr.stats.GuardReason = true, reason
	pr.warn("Refresh guard held the new rows: the old ones are kept and the cache ages into unknown; run the refresh with "+
		"allowChanges (Apply held changes) if the change is real", "reason", reason)
	if pr.dry {
		return true, nil
	}
	return true, pr.r.o.DB.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO index_state (integration_id, status, attempted_at, error, app_version) VALUES (?, 'ok', ?, ?, ?)
			ON CONFLICT (integration_id) DO UPDATE SET status = 'ok', attempted_at = excluded.attempted_at, error = excluded.error,
				app_version = CASE WHEN excluded.app_version <> '' THEN excluded.app_version ELSE index_state.app_version END`,
			pr.it.ID, db.FormatTime(pr.runAt), "Refresh guard held the new rows: "+reason, pr.stats.AppVersion)
		if err != nil {
			return fmt.Errorf("record the held refresh: %w", err)
		}
		return nil
	})
}

// replace makes the integration's rows of tables the new rows and records the complete refresh
// (status ok, refreshed_at = the attempt's start, the instance, the version and the stored
// stats), all in one transaction. The rows are compared with the cached ones first, outside the
// writer (providerdiff.go), so the transaction writes only the rows that changed.
func (pr *providerRun) replace(ctx context.Context, tables []*cacheTable, instance string, stored any) error {
	faultinject.Point(PointProviderFetched)
	stats, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	diffs := make([]tableDiff, 0, len(tables))
	for _, t := range tables {
		d, err := diffTable(ctx, pr.r.o.DB.Reader(), pr.it.ID, t)
		if err != nil {
			return err
		}
		diffs = append(diffs, d)
	}
	err = pr.r.o.DB.Write(ctx, func(tx *sql.Tx) error {
		for _, d := range diffs {
			if err := d.apply(ctx, tx, pr.it.ID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO index_state (integration_id, status, refreshed_at, attempted_at, error, instance_id, app_version, stats)
			VALUES (?, 'ok', ?, ?, NULL, ?, ?, ?)
			ON CONFLICT (integration_id) DO UPDATE SET status = 'ok', refreshed_at = excluded.refreshed_at, attempted_at = excluded.attempted_at,
				error = NULL, instance_id = excluded.instance_id,
				app_version = CASE WHEN excluded.app_version <> '' THEN excluded.app_version ELSE index_state.app_version END,
				stats = excluded.stats`,
			pr.it.ID, db.FormatTime(pr.runAt), db.FormatTime(pr.runAt), instance, pr.stats.AppVersion, string(stats))
		if err != nil {
			return fmt.Errorf("record the refresh: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	faultinject.Point(PointProviderReplaced)
	return nil
}

// instanceChanged reports whether the integration's cache was built from another instance.
func (pr *providerRun) instanceChanged(ctx context.Context, instance string) (bool, error) {
	st, err := pr.r.store.State(ctx, nil, pr.it.ID)
	if err != nil {
		return false, err
	}
	return st.InstanceID != "" && st.InstanceID != instance, nil
}

// nullInt stores 0 as NULL.
func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// nullIntPtr stores nil as NULL.
func nullIntPtr(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}
