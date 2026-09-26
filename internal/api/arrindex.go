package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

// The *arr index and connections (docs/design/phase2-3.md §6, §13): the index status, refresh,
// the indexed metadata, the live root folders, the webhook key and the unmapped files. The
// refresh schedule of an *arr integration is a schedules row mirrored from its settings, like
// the Plex DB backup's.

func (s *Server) arrIndexRoutes(r chi.Router) {
	r.Get("/integrations/{id}/index", s.integrationIndex)
	r.Post("/integrations/{id}/refresh", s.refreshIntegration)
	r.Get("/integrations/{id}/arr/metadata", s.arrMetadata)
	r.Get("/integrations/{id}/arr/rootfolders", s.arrRootFolders)
	r.Post("/integrations/{id}/webhook/key", s.webhookKey)
	r.Get("/catalog/unmapped", s.unmappedFiles)
}

func (s *Server) integrationIndex(w http.ResponseWriter, r *http.Request) {
	it, err := s.integration(r)
	if err != nil {
		s.fail(w, r, "read the index state", err)
		return
	}
	v, err := s.app.Index.IndexStatus(r.Context(), it)
	if err != nil {
		s.fail(w, r, "read the index state", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) refreshIntegration(w http.ResponseWriter, r *http.Request) {
	it, err := s.integration(r)
	if err != nil {
		s.fail(w, r, "start a refresh", err)
		return
	}
	var body struct {
		DryRun       bool `json:"dryRun"`
		AllowChanges bool `json:"allowChanges"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "start a refresh", err)
		return
	}
	switch {
	case it.Type == integrations.TypePlex && !mediaindex.Refreshes(it):
		s.fail(w, r, "start a refresh", errorf(http.StatusBadRequest, "the library index of %[1]q is turned off: turn it on in Settings → Connect (Library index · %[1]s)", it.Name))
		return
	case !mediaindex.Refreshes(it):
		s.fail(w, r, "start a refresh", errorf(http.StatusBadRequest, "refreshing %s integrations is not available", it.Type.AppName()))
		return
	case !it.Enabled:
		s.fail(w, r, "start a refresh", errorf(http.StatusConflict, "%s %q is disabled", it.Type.AppName(), it.Name))
		return
	}
	job, err := s.app.Jobs.Enqueue(r.Context(), mediaindex.RefreshSpec(it.ID, jobs.TriggerManual, body.DryRun, body.AllowChanges))
	if err != nil {
		s.fail(w, r, "start a refresh", err)
		return
	}
	writeAccepted(w, job.ID, job)
}

// arrIntegration loads the {id} integration and requires Sonarr, Radarr or Lidarr.
func (s *Server) arrIntegration(r *http.Request) (integrations.Integration, error) {
	it, err := s.integration(r)
	if err != nil {
		return it, err
	}
	if !it.Type.IsArr() {
		return it, errorf(http.StatusBadRequest, "integration %q is a %s integration, not Sonarr, Radarr or Lidarr", it.Name, it.Type.AppName())
	}
	return it, nil
}

func (s *Server) arrMetadata(w http.ResponseWriter, r *http.Request) {
	it, err := s.arrIntegration(r)
	if err != nil {
		s.fail(w, r, "read the *arr metadata", err)
		return
	}
	m, err := s.app.Index.Meta(r.Context(), nil, it.ID)
	if err != nil {
		s.fail(w, r, "read the *arr metadata", err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// arrRootFolderView is an element of GET /integrations/{id}/arr/rootfolders: a root folder as the
// *arr reports it now, mapped through the stored path mappings and located among the sources.
type arrRootFolderView struct {
	ID         int64   `json:"id"`
	Path       string  `json:"path"`
	Accessible bool    `json:"accessible"`
	LocalPath  *string `json:"localPath"`
	SourceID   *int64  `json:"sourceId"`
	Exists     bool    `json:"exists"`
}

func (s *Server) arrRootFolders(w http.ResponseWriter, r *http.Request) {
	it, err := s.arrIntegration(r)
	if err != nil {
		s.fail(w, r, "list *arr root folders", err)
		return
	}
	ctx := r.Context()
	settings, err := it.ArrSettings()
	if err != nil {
		s.fail(w, r, "list *arr root folders", err)
		return
	}
	token, err := s.app.Integrations.TokenFor(ctx, it.ID, it.URL)
	if errors.Is(err, integrations.ErrURLChanged) {
		err = errorf(http.StatusConflict, "the URL of %s %q changed during the request; try again", it.Type.AppName(), it.Name)
	}
	if err != nil {
		s.fail(w, r, "list *arr root folders", err)
		return
	}
	c, err := arr.New(arr.Kind(it.Type), it.URL, token, arr.Options{})
	if err != nil {
		s.fail(w, r, "list *arr root folders", errorf(http.StatusBadRequest, "%s %q: %v", it.Type.AppName(), it.Name, err))
		return
	}
	roots, err := c.RootFolders(ctx)
	if err != nil {
		s.fail(w, r, "list *arr root folders", errorf(http.StatusBadGateway, "%s %q: %v", it.Type.AppName(), it.Name, err))
		return
	}
	loc, err := s.app.Catalog.Locator(ctx)
	if err != nil {
		s.fail(w, r, "list *arr root folders", err)
		return
	}
	out := make([]arrRootFolderView, 0, len(roots))
	for _, rf := range roots {
		v := arrRootFolderView{ID: rf.ID, Path: rf.Path, Accessible: rf.Accessible}
		if local, ok := settings.MapPath(rf.Path); ok {
			v.LocalPath = &local
			if fi, err := os.Stat(local); err == nil && fi.IsDir() {
				v.Exists = true
			}
			if locs := loc.Locate(local); len(locs) > 0 {
				id := locs[0].SourceID
				v.SourceID = &id
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// webhookKey is POST /integrations/{id}/webhook/key {rotate}: the only response that contains an
// integration's webhook key (design S8, D7). rotate: false reveals it, rotate: true replaces it
// (the old key stops working at once).
func (s *Server) webhookKey(w http.ResponseWriter, r *http.Request) {
	it, err := s.arrIntegration(r)
	if err != nil {
		s.fail(w, r, "read the webhook key", err)
		return
	}
	var body struct {
		Rotate bool `json:"rotate"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "read the webhook key", err)
		return
	}
	var key string
	if body.Rotate {
		key, err = s.app.Integrations.RotateWebhookKey(r.Context(), it.ID)
	} else {
		key, err = s.app.Integrations.WebhookKey(r.Context(), it.ID)
	}
	if err != nil {
		s.fail(w, r, "read the webhook key", err)
		return
	}
	if body.Rotate {
		s.log.Info("Webhook key regenerated", "id", it.ID, "type", string(it.Type), "name", it.Name)
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

func (s *Server) unmappedFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, err := int64Param(q, "integrationId")
	if err != nil {
		s.fail(w, r, "list unmapped files", err)
		return
	}
	page, size, err := paging(q)
	if err != nil {
		s.fail(w, r, "list unmapped files", err)
		return
	}
	if id > 0 {
		it, err := s.app.Integrations.Get(r.Context(), id)
		if err != nil {
			s.fail(w, r, "list unmapped files", err)
			return
		}
		if !it.Type.IsArr() {
			// The Plex library index is a later slice (design §18): nothing is indexed for Plex yet.
			writeJSON(w, http.StatusOK, mediaindex.UnmappedPage{Page: page, PageSize: size, Records: []mediaindex.UnmappedFile{}})
			return
		}
	}
	p, err := s.app.Index.Unmapped(r.Context(), id, page, size)
	if err != nil {
		s.fail(w, r, "list unmapped files", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// arrRefreshParams is the params of an integration's scheduled full refresh.
func arrRefreshParams(integrationID int64) jobs.Params {
	return jobs.Params{IntegrationID: integrationID}
}

// syncArrRefreshSchedule makes the refresh schedule of an *arr integration match its settings
// (refresh.cron and refresh.enabled). It reports whether the schedules changed.
func (s *Server) syncArrRefreshSchedule(ctx context.Context, it integrations.Integration) (bool, error) {
	return syncArrRefreshScheduleIn(ctx, s.app.Jobs.Store(), it, true)
}

// syncArrRefreshScheduleIn upserts the refresh schedule of an *arr integration from its settings;
// with overwrite false an existing schedule is kept as it is (start-up: the schedule may have
// been edited on System → Tasks, which is its source of truth). It reports whether it wrote.
func syncArrRefreshScheduleIn(ctx context.Context, st *jobqueue.Store, it integrations.Integration, overwrite bool) (bool, error) {
	if !mediaindex.Supports(it.Type) {
		return false, nil
	}
	as, err := it.ArrSettings()
	if err != nil {
		return false, err
	}
	params := arrRefreshParams(it.ID)
	list, err := st.ListSchedules(ctx)
	if err != nil {
		return false, err
	}
	for _, sc := range list {
		if sc.JobType == jobs.TypeRefresh && sameParams(sc.Params, params) {
			if !overwrite || sc.Cron == as.Refresh.Cron && sc.Enabled == as.Refresh.Enabled {
				return false, nil
			}
		}
	}
	if _, err := st.UpsertSchedule(ctx, jobs.TypeRefresh, params, as.Refresh.Cron, as.Refresh.Enabled); err != nil {
		return false, err
	}
	return true, nil
}

// arrRefreshOverlay returns an *arr integration with refresh.cron and refresh.enabled taken from
// its stored refresh schedule, when there is one (System → Tasks edits it).
func arrRefreshOverlay(list []jobqueue.Schedule, it integrations.Integration) integrations.Integration {
	as, err := it.ArrSettings()
	if err != nil {
		return it
	}
	for _, sc := range list {
		if sc.JobType == jobs.TypeRefresh && sameParams(sc.Params, arrRefreshParams(it.ID)) {
			as.Refresh.Cron, as.Refresh.Enabled = sc.Cron, sc.Enabled
			if b, err := jsonMarshal(as); err == nil {
				it.Settings = b
			}
			return it
		}
	}
	return it
}

// afterArrSave runs after an integration was created (before nil) or updated: an *arr's refresh
// schedule follows its settings, and a new integration, or one whose URL, key or path mappings
// changed, gets a full refresh (design §4.1). A failed enqueue is logged: the save stands, and
// the scheduled and start-up refreshes follow.
func (s *Server) afterArrSave(ctx context.Context, before *integrations.Integration, it integrations.Integration, keyChanged bool) error {
	if !it.Type.IsArr() {
		return nil
	}
	changed, err := s.syncArrRefreshSchedule(ctx, it)
	if err != nil {
		return errScheduleSync("the integration", err)
	}
	if changed {
		s.reloadSchedules(ctx)
	}
	if mediaindex.NeedsRefresh(before, it, keyChanged) {
		if job, err := s.app.Jobs.Enqueue(ctx, mediaindex.RefreshSpec(it.ID, jobs.TriggerManual, false, false)); err != nil {
			s.log.Warn("Could not queue the refresh of a saved integration", "id", it.ID, "error", err)
		} else {
			s.log.Info("Refresh queued for a saved integration", "id", it.ID, "jobId", job.ID)
		}
	}
	return nil
}

// ensureArrRefreshSchedules gives every *arr integration whose settings are valid its refresh
// schedule when it has none (rows created before 0003, design §4.1). Existing schedules are kept.
func (a *App) ensureArrRefreshSchedules(ctx context.Context) error {
	list, err := a.Integrations.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range list {
		if !it.Type.IsArr() {
			continue
		}
		if _, err := syncArrRefreshScheduleIn(ctx, a.Jobs.Store(), it, false); err != nil {
			errs = append(errs, fmt.Errorf("integration %q: %w", it.Name, err))
		}
	}
	return errors.Join(errs...)
}

// queueStartupRefreshes queues a full refresh of every enabled *arr integration (design §6.1).
func (a *App) queueStartupRefreshes(ctx context.Context) {
	list, err := a.Integrations.List(ctx)
	if err != nil {
		a.log.Warn("Could not list the integrations to refresh at start-up", "error", err)
		return
	}
	queued, err := mediaindex.QueueStartup(ctx, list, a.Jobs)
	if err != nil {
		a.log.Warn("Could not queue every start-up refresh", "error", err)
	}
	if len(queued) > 0 {
		a.log.Info("Queued the start-up refreshes of the *arr indexes", "jobs", len(queued))
	}
}

// followUpDestinations lists the destinations a refresh's follow-up syncs may go to.
// syncOnArrChange is on for every destination until destinations store that setting (design §13:
// its default is on).
func (a *App) followUpDestinations(ctx context.Context) ([]mediaindex.FollowUpDestination, error) {
	list, err := a.Destinations.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mediaindex.FollowUpDestination, 0, len(list))
	for _, d := range list {
		out = append(out, mediaindex.FollowUpDestination{ID: d.ID, Enabled: d.Enabled, SourceIDs: d.SourceIDs, SyncOnArrChange: true})
	}
	return out, nil
}

// newRefreshRunner builds the refresh runner (job type refresh).
func (a *App) newRefreshRunner(o AppOptions) (*mediaindex.Runner, error) {
	return mediaindex.NewRunner(mediaindex.RefreshOptions{
		DB:           o.DB,
		Integrations: a.Integrations,
		Catalog:      a.Catalog,
		Enqueuer:     a.Jobs,
		Destinations: a.followUpDestinations,
		Log:          a.log.With("component", "mediaindex"),
		Plex:         a.plexOpts,
	})
}
