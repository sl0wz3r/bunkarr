package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
	"github.com/sl0wz3r/bunkarr/internal/webhooks"
)

// Webhooks (docs/design/phase2-3.md §7, D7, S12, S13). The *arrs post to
// /api/v1/webhook/{app}/{integrationId} (or the generic /api/v1/webhook/{app}). These routes sit
// outside the session group and authenticate with nothing but the integration's webhook key
// (webhookAuth): Bunkarr's master API key, a session cookie and the "disabled for local
// addresses" mode never do. The webhook key, in turn, is refused by every other route (the
// session group's auth.Require knows only the master key and sessions).

// Failed-authentication limit of the webhook routes (S13): per client address, separate from the
// login limiter, so a stale key in one *arr cannot lock out the UI.
const (
	webhookFailMax    = 10
	webhookFailWindow = time.Minute
)

// webhookService is the webhook intake with its store and processor, and the failed-
// authentication limiter of the webhook routes.
type webhookService struct {
	store  *webhooks.Store
	proc   *webhooks.Processor
	intake *webhooks.Intake
	fails  *auth.Limiter
}

// newWebhooks builds the webhook service over the job manager (a.Jobs must be set).
func (a *App) newWebhooks(o AppOptions) *webhookService {
	log := a.log.With("component", "webhooks")
	st := webhooks.NewStore(o.DB)
	proc := webhooks.NewProcessor(webhooks.ProcessorOptions{Store: st, Enqueuer: a.Jobs, Enabled: a.webhookIntegrationEnabled, Log: log})
	return &webhookService{
		store:  st,
		proc:   proc,
		intake: webhooks.NewIntake(webhooks.IntakeOptions{Store: st, Processor: proc, Log: log}),
		fails:  auth.NewLimiter(webhookFailMax, webhookFailWindow),
	}
}

// WebhookEvents returns the store of received webhook events.
func (a *App) WebhookEvents() *webhooks.Store { return a.webhooks.store }

// webhookIntegrationEnabled is the processor's check that an integration still takes webhooks.
func (a *App) webhookIntegrationEnabled(ctx context.Context, id int64) (bool, error) {
	it, err := a.Integrations.Get(ctx, id)
	if errors.Is(err, integrations.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return it.Enabled && it.Type.IsArr(), nil
}

// manifestAfterSync is the sync runner's §9.2 decision, the destination's manifest.afterSync
// "auto": a full sync is followed by a manifest export when an enabled *arr integration exists.
// (Destinations do not store an explicit on or off yet.)
func (a *App) manifestAfterSync(ctx context.Context, _ int64) (bool, error) {
	list, err := a.Integrations.List(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(list, func(it integrations.Integration) bool { return it.Enabled && it.Type.IsArr() }), nil
}

// expectedFiles returns the files the *arr index expects under the paths of a webhook sync of src
// (design §9.1): the indexed files of non-deleted items whose mapped local path lies under one of
// them, except those the source excludes (the catalog never lists them, so waiting for them would
// only hold the locks and end in a false warning).
//
// No other query runs while a FilesUnder cursor is open: the cursor holds a read-pool connection,
// and syncs waiting on a nested query for a further connection could exhaust the pool.
func (a *App) expectedFiles(ctx context.Context, src catalog.Source, paths []string) ([]syncer.ExpectedFile, error) {
	if a.Index == nil {
		return nil, nil
	}
	ex := newSourceExcludes(src.Exclude)
	var (
		out []syncer.ExpectedFile
		ids []int64 // the integration of each out entry
	)
	for _, p := range paths {
		err := a.Index.FilesUnder(ctx, nil, path.Join(src.Path, p), func(f mediaindex.File) error {
			rel, ok := strings.CutPrefix(f.LocalPath, src.Path+"/")
			if !ok || f.Size <= 0 || ex.excluded(rel) {
				return nil
			}
			out = append(out, syncer.ExpectedFile{RelPath: rel, Size: f.Size})
			ids = append(ids, f.IntegrationID)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	// The cursors are closed: name the integrations with one query.
	names := map[int64]string{}
	if list, err := a.Integrations.List(ctx); err == nil {
		for _, it := range list {
			names[it.ID] = it.Name
		}
	}
	for i, id := range ids {
		name, ok := names[id]
		if !ok {
			name = "integration #" + strconv.FormatInt(id, 10)
		}
		out[i].App = name
	}
	return out, nil
}

// sourceExcludes applies a source's exclude patterns to paths relative to it the way the catalog's
// scanner does (internal/catalog/exclude.go: the default patterns case-insensitively, the source's
// own as written; a trailing "/" matches folders only, a leading "/" the relative path only; a
// pattern matches the base name or the relative path). The scanner never enters an excluded
// folder, so a file is excluded when it or one of its folders is.
type sourceExcludes struct {
	patterns []excludePattern
}

type excludePattern struct {
	glob     string
	dirOnly  bool
	anchored bool
	fold     bool
}

func newSourceExcludes(user []string) sourceExcludes {
	var ex sourceExcludes
	add := func(raw string, fold bool) {
		p := excludePattern{glob: raw, fold: fold}
		if strings.HasSuffix(p.glob, "/") {
			p.dirOnly = true
			p.glob = strings.TrimRight(p.glob, "/")
		}
		if strings.HasPrefix(p.glob, "/") {
			p.anchored = true
			p.glob = strings.TrimLeft(p.glob, "/")
		}
		if fold {
			p.glob = strings.ToLower(p.glob)
		}
		ex.patterns = append(ex.patterns, p)
	}
	for _, d := range catalog.DefaultExcludes() {
		add(d, true)
	}
	for _, u := range user {
		add(u, false)
	}
	return ex
}

// excluded reports whether the file at rel (slash-separated, relative to the source root) is
// excluded, by its own name or by one of its folders.
func (ex sourceExcludes) excluded(rel string) bool {
	for i := 0; ; {
		j := strings.IndexByte(rel[i:], '/')
		if j < 0 {
			return ex.match(rel, rel[i:], false)
		}
		if ex.match(rel[:i+j], rel[i:i+j], true) {
			return true
		}
		i += j + 1
	}
}

// match reports whether the entry at rel with base name base is excluded.
func (ex sourceExcludes) match(rel, base string, isDir bool) bool {
	lrel, lbase := strings.ToLower(rel), strings.ToLower(base)
	for _, p := range ex.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		r, b := rel, base
		if p.fold {
			r, b = lrel, lbase
		}
		if !p.anchored {
			if ok, _ := path.Match(p.glob, b); ok {
				return true
			}
		}
		if ok, _ := path.Match(p.glob, r); ok {
			return true
		}
	}
	return false
}

// webhookRoutes are the routes the *arrs call (outside the session group).
func (s *Server) webhookRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireApp)
		r.Post("/webhook/{app}", s.webhook)
		r.Post("/webhook/{app}/{integrationId}", s.webhook)
	})
}

// webhookInfoRoutes are the authenticated routes about the received webhooks.
func (s *Server) webhookInfoRoutes(r chi.Router) {
	r.Get("/integrations/{id}/webhook", s.webhookInfo)
	r.Get("/webhooks/events", s.webhookEvents)
	r.Get("/webhooks/events/{id}", s.webhookEvent)
}

// webhookCredentials returns the candidate webhook keys of a request, in order: the Basic auth
// password (the user name is ignored), the X-Api-Key header and the apikey query parameter.
func webhookCredentials(r *http.Request) []string {
	var out []string
	if _, pw, ok := r.BasicAuth(); ok && pw != "" {
		out = append(out, pw)
	}
	if k := r.Header.Get("X-Api-Key"); k != "" {
		out = append(out, k)
	}
	if k := r.URL.Query().Get("apikey"); k != "" {
		out = append(out, k)
	}
	return out
}

// webhook is POST /webhook/{app}[/{integrationId}] (design §7.1): webhookAuth's credential and
// integration checks, then the intake.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	svc := s.app.webhooks
	app := integrations.Type(chi.URLParam(r, "app"))
	// 1. The credential, compared in constant time with the in-memory key map, before any I/O.
	var (
		ident integrations.WebhookIdentity
		ok    bool
	)
	for _, k := range webhookCredentials(r) {
		if ident, ok = s.app.Integrations.MatchWebhookKey(k); ok {
			break
		}
	}
	client := auth.ClientIP(r).String()
	if !ok {
		if blocked, wait := svc.fails.Blocked(client); blocked {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "too many webhook requests without a valid key; try again later")
			return
		}
		svc.fails.Fail(client)
		s.log.Warn("Webhook refused: wrong or missing webhook key", "app", string(app), "remote", client)
		writeError(w, http.StatusUnauthorized, "a valid webhook key is required (Settings → Connect shows it)")
		return
	}
	// 2. The integration.
	if !app.IsArr() {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var it integrations.Integration
	if raw := chi.URLParam(r, "integrationId"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		it, err = s.app.Integrations.Get(r.Context(), id)
		if errors.Is(err, integrations.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such integration")
			return
		}
		if err != nil {
			s.fail(w, r, "receive a webhook", err)
			return
		}
		if ident.IntegrationID != id {
			s.log.Warn("Webhook refused: the key belongs to another integration", "integrationId", id, "remote", client)
			writeError(w, http.StatusUnauthorized, "this is not the webhook key of this integration")
			return
		}
	} else {
		if ident.Type != app {
			s.log.Warn("Webhook refused: the key belongs to another app", "app", string(app), "remote", client)
			writeError(w, http.StatusUnauthorized, fmt.Sprintf("this is not the webhook key of a %s integration", app.AppName()))
			return
		}
		var err error
		it, err = s.app.Integrations.Get(r.Context(), ident.IntegrationID)
		if errors.Is(err, integrations.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "a valid webhook key is required")
			return
		}
		if err != nil {
			s.fail(w, r, "receive a webhook", err)
			return
		}
		svc.intake.NoteGenericRoute(it.ID)
	}
	switch {
	case it.Type != app:
		writeError(w, http.StatusConflict, fmt.Sprintf("%q is a %s integration, not %s", it.Name, it.Type.AppName(), app.AppName()))
		return
	case !it.Enabled:
		writeError(w, http.StatusConflict, fmt.Sprintf("%s %q is disabled", it.Type.AppName(), it.Name))
		return
	}
	// 3-5 and the event: the intake.
	svc.intake.Serve(w, r, it.ID, app)
}

// webhookInfoView is GET /integrations/{id}/webhook.
type webhookInfoView struct {
	// Path is the integration's own webhook route, GenericPath the app's generic one.
	Path        string     `json:"path"`
	GenericPath string     `json:"genericPath"`
	HasKey      bool       `json:"hasKey"`
	LastEventAt *time.Time `json:"lastEventAt"`
	LastTestAt  *time.Time `json:"lastTestAt"`
	Last24h     int64      `json:"last24h"`
	// Warnings: a linked destination without a sync schedule; another integration of the app that
	// received events on the generic route in the last 7 days.
	Warnings []string          `json:"warnings"`
	Recent   []webhooks.Record `json:"recent"`
}

func (s *Server) webhookInfo(w http.ResponseWriter, r *http.Request) {
	it, err := s.arrIntegration(r)
	if err != nil {
		s.fail(w, r, "read the webhook activity", err)
		return
	}
	ctx := r.Context()
	hasKey, err := s.app.Integrations.HasWebhookKey(ctx, it.ID)
	if err != nil {
		s.fail(w, r, "read the webhook activity", err)
		return
	}
	act, err := s.app.webhooks.store.Activity(ctx, it.ID, time.Now())
	if err != nil {
		s.fail(w, r, "read the webhook activity", err)
		return
	}
	warnings, err := s.app.webhookWarnings(ctx, it)
	if err != nil {
		s.fail(w, r, "read the webhook activity", err)
		return
	}
	writeJSON(w, http.StatusOK, webhookInfoView{
		Path:        fmt.Sprintf("/api/v1/webhook/%s/%d", it.Type, it.ID),
		GenericPath: "/api/v1/webhook/" + string(it.Type),
		HasKey:      hasKey,
		LastEventAt: act.LastEventAt,
		LastTestAt:  act.LastTestAt,
		Last24h:     act.Last24h,
		Warnings:    warnings,
		Recent:      act.Recent,
	})
}

// webhookWarnings are the webhook panel's warnings for integration it (design §13).
func (a *App) webhookWarnings(ctx context.Context, it integrations.Integration) ([]string, error) {
	warnings := []string{}
	// The sources the integration's items live in: its root folders as indexed, and the sources
	// imported from it.
	sources := map[int64]bool{}
	if m, err := a.Index.Meta(ctx, nil, it.ID); err == nil {
		for _, rf := range m.RootFolders {
			if rf.SourceID != nil {
				sources[*rf.SourceID] = true
			}
		}
	} else {
		return nil, err
	}
	srcs, err := a.Catalog.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, src := range srcs {
		if src.ArrIntegrationID != nil && *src.ArrIntegrationID == it.ID {
			sources[src.ID] = true
		}
	}
	dests, err := a.Destinations.List(ctx)
	if err != nil {
		return nil, err
	}
	scheds, err := a.Jobs.Store().ListSchedules(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range dests {
		if !d.Enabled || !slices.ContainsFunc(d.SourceIDs, func(id int64) bool { return sources[id] }) {
			continue
		}
		scheduled := slices.ContainsFunc(scheds, func(sc jobqueue.Schedule) bool {
			return sc.JobType == jobs.TypeSync && sc.Enabled && sc.Params.DestinationID == d.ID
		})
		if !scheduled {
			warnings = append(warnings, fmt.Sprintf("Destination %q has no sync schedule: a webhook that %s never sends (they are not retried) is backed up only by the next full refresh or a manual sync",
				d.Name, it.Type.AppName()))
		}
	}
	others, err := a.Integrations.List(ctx)
	if err != nil {
		return nil, err
	}
	uses := a.webhooks.intake.GenericRouteUses()
	for _, o := range others {
		if o.ID == it.ID || o.Type != it.Type {
			continue
		}
		if at, ok := uses[o.ID]; ok {
			warnings = append(warnings, fmt.Sprintf("%q received events on the generic route /api/v1/webhook/%s (last %s): if this %s posts there too, it may be using %q's key",
				o.Name, it.Type, at.UTC().Format(time.RFC3339), it.Type.AppName(), o.Name))
		}
	}
	return warnings, nil
}

func (s *Server) webhookEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, size, err := paging(q)
	if err != nil {
		s.fail(w, r, "list webhook events", err)
		return
	}
	integ, err := int64Param(q, "integrationId")
	if err != nil {
		s.fail(w, r, "list webhook events", err)
		return
	}
	res, err := s.app.webhooks.store.List(r.Context(), webhooks.Query{IntegrationID: integ, EventType: q.Get("eventType"),
		Outcome: q.Get("outcome"), Page: page, PageSize: size})
	var ve webhooks.ValidationError
	if errors.As(err, &ve) {
		err = errorf(http.StatusBadRequest, "%s", ve.Error())
	}
	if err != nil {
		s.fail(w, r, "list webhook events", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) webhookEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "read a webhook event", err)
		return
	}
	ev, err := s.app.webhooks.store.Get(r.Context(), id)
	if errors.Is(err, webhooks.ErrNotFound) {
		err = errorf(http.StatusNotFound, "webhook event %d not found", id)
	}
	if err != nil {
		s.fail(w, r, "read a webhook event", err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}
