package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/tautulli"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

// Tautulli, Seerr, Maintainerr and the Plex library index (docs/design/phase2-3.md §4.4-4.6,
// §6.2-6.3, §13): Test, the live Seerr users, the refresh schedules mirrored from the settings
// (like the *arr refresh's, arrindex.go), the refresh queued after a save, and the tier provider
// that reads their caches. Every client dials through the outbound guard (S16); keys are read only
// through TokenFor and never returned (S8).

func (s *Server) providerRoutes(r chi.Router) {
	r.Get("/integrations/{id}/seerr/users", s.seerrUsers)
}

// providerTestResult is POST /integrations/test's answer for Tautulli, Seerr and Maintainerr.
type providerTestResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Version string `json:"version,omitempty"`
	AppName string `json:"appName,omitempty"`
	// PlexMatches (Tautulli, Maintainerr): whether the application works with the linked Plex
	// server; null without a plexIntegrationId, or when it cannot be told.
	PlexMatches *bool `json:"plexMatches"`
}

// providerLink reads the plexIntegrationId a Test uses: the body's settings (validated like a
// create), else the stored ones, else none. Unlike a create, a Test does not need the link: the
// form tests before a Plex server is chosen (none connected yet, or several and none picked) and
// sends plexIntegrationId 0, which means "no link" (plexMatches null, §13); the rest of the
// settings are still validated.
func providerLink(typ integrations.Type, raw json.RawMessage, cur *integrations.Integration) (int64, error) {
	var link int64
	if t := strings.TrimSpace(string(raw)); t != "" && t != "null" {
		switch typ {
		case integrations.TypeTautulli:
			st, err := integrations.ParseTautulliSettings(raw)
			if err == nil {
				link = st.PlexIntegrationID
				st.PlexIntegrationID = linkForTest(link)
				err = st.Validate()
			}
			if err != nil {
				return 0, errorf(http.StatusBadRequest, "%v", err)
			}
		case integrations.TypeSeerr:
			st, err := integrations.ParseSeerrSettings(raw)
			if err == nil {
				link = st.PlexIntegrationID
				st.PlexIntegrationID = linkForTest(link)
				err = st.Validate()
			}
			if err != nil {
				return 0, errorf(http.StatusBadRequest, "%v", err)
			}
		case integrations.TypeMaintainerr:
			st, err := integrations.ParseMaintainerrSettings(raw)
			if err == nil {
				link = st.PlexIntegrationID
				st.PlexIntegrationID = linkForTest(link)
				err = st.Validate()
			}
			if err != nil {
				return 0, errorf(http.StatusBadRequest, "%v", err)
			}
		}
		return link, nil
	}
	if cur != nil && cur.Type == typ {
		var v struct {
			PlexIntegrationID int64 `json:"plexIntegrationId"`
		}
		_ = json.Unmarshal(cur.Settings, &v)
		link = v.PlexIntegrationID
	}
	return link, nil
}

// linkForTest is the plexIntegrationId a Test validates the settings with: an unset (0) link
// passes as a placeholder id, so only the link's own checks are skipped; a negative one still fails.
func linkForTest(link int64) int64 {
	if link == 0 {
		return 1
	}
	return link
}

// testProviderIntegration tests a Tautulli, Seerr or Maintainerr URL (and key) with unsaved
// settings (§4.1). A typed key is used as typed; the stored key only for the stored URL (S8).
func (s *Server) testProviderIntegration(w http.ResponseWriter, r *http.Request, typ integrations.Type, rawURL, apiKey string,
	settings json.RawMessage, cur *integrations.Integration) {
	ctx := r.Context()
	u, err := integrations.NormalizeURL(rawURL)
	if err != nil {
		s.fail(w, r, "test integration", err)
		return
	}
	key := strings.TrimSpace(apiKey)
	if typ == integrations.TypeMaintainerr && key != "" {
		s.fail(w, r, "test integration", errorf(http.StatusBadRequest, "Maintainerr has no API authentication: leave the API key empty"))
		return
	}
	if key == "" && cur != nil && cur.HasAPIKey && typ != integrations.TypeMaintainerr {
		key, err = s.app.Integrations.TokenFor(ctx, cur.ID, u)
		if errors.Is(err, integrations.ErrURLChanged) {
			err = errorf(http.StatusBadRequest, "enter the API key to test a different URL: the saved key is only sent to the URL it was saved with")
		}
		if err != nil {
			s.fail(w, r, "test integration", err)
			return
		}
	}
	link, err := providerLink(typ, settings, cur)
	if err != nil {
		s.fail(w, r, "test integration", err)
		return
	}
	var res providerTestResult
	switch typ {
	case integrations.TypeTautulli:
		res = s.testTautulli(ctx, u, key, link)
	case integrations.TypeSeerr:
		res = s.testSeerr(ctx, u, key)
	case integrations.TypeMaintainerr:
		res = s.testMaintainerr(ctx, u, link)
	}
	res.Message = logging.RedactValues(res.Message, key)
	writeJSON(w, http.StatusOK, res)
}

// describeProviderError turns a client error into a user-facing sentence (the errors carry no
// URL, key or body).
func describeProviderError(app string, err error) string {
	switch {
	case errors.Is(err, tautulli.ErrUnauthorized), errors.Is(err, seerr.ErrUnauthorized):
		return app + " rejected the API key."
	case errors.Is(err, tautulli.ErrAPIDisabled):
		return "The Tautulli API is turned off: enable it in Tautulli (Settings → Web Interface → Enable API)."
	case errors.Is(err, tautulli.ErrTooOld), errors.Is(err, maintainerr.ErrTooOld):
		return err.Error() + "."
	case errors.Is(err, context.DeadlineExceeded):
		return app + " did not answer in time."
	}
	return fmt.Sprintf("Could not reach %s: %v", app, err)
}

// linkedMachine reads the machineIdentifier of a linked Plex integration's server (/identity,
// without the token).
func (s *Server) linkedMachine(ctx context.Context, plexID int64) (string, string, error) {
	p, err := s.app.Integrations.Get(ctx, plexID)
	if err != nil {
		return "", "", err
	}
	if p.Type != integrations.TypePlex {
		return "", "", fmt.Errorf("integration %d is not a Plex integration", plexID)
	}
	c, err := plex.New(p.URL, "", s.app.plexOpts)
	if err != nil {
		return "", p.Name, err
	}
	id, err := c.Identity(ctx)
	if err != nil {
		return "", p.Name, err
	}
	return id.MachineIdentifier, p.Name, nil
}

func (s *Server) testTautulli(ctx context.Context, u, key string, link int64) providerTestResult {
	res := providerTestResult{AppName: "Tautulli"}
	c, err := tautulli.New(u, key, tautulli.Options{})
	if err != nil {
		res.Message = err.Error()
		return res
	}
	info, err := c.Info(ctx)
	res.Version = info.Version
	if err != nil {
		res.Message = describeProviderError("Tautulli", err)
		return res
	}
	si, err := c.ServerInfo(ctx)
	if err != nil {
		res.Message = describeProviderError("Tautulli", err)
		return res
	}
	res.OK = true
	res.Message = fmt.Sprintf("Connected to Tautulli %s.", info.Version)
	if link == 0 {
		return res
	}
	mid, name, err := s.linkedMachine(ctx, link)
	if err != nil {
		res.Message += fmt.Sprintf(" Could not read the linked Plex server's identity: %v", err)
		return res
	}
	match := si.PMSIdentifier == mid
	res.PlexMatches = &match
	if match {
		res.Message += fmt.Sprintf(" It watches %s.", name)
	} else {
		res.OK = false
		res.Message += fmt.Sprintf(" It watches another Plex server than %s: link it to the right Plex server.", name)
	}
	return res
}

func (s *Server) testSeerr(ctx context.Context, u, key string) providerTestResult {
	res := providerTestResult{AppName: "Seerr"}
	c, err := seerr.New(u, key, seerr.Options{})
	if err != nil {
		res.Message = err.Error()
		return res
	}
	st, err := c.Status(ctx)
	if err != nil {
		res.Message = describeProviderError("Seerr", err)
		return res
	}
	res.Version = st.Version
	if _, err := c.Me(ctx); err != nil {
		res.Message = describeProviderError("Seerr", err)
		return res
	}
	res.OK = true
	res.Message = fmt.Sprintf("Connected to Seerr %s.", st.Version)
	return res
}

// testMaintainerr checks the version gate and, with a linked Plex server whose library index is
// built, whether Maintainerr's collections live in that server's libraries: plexMatches is true
// when every collection's library is a section of the index and at least one member's rating key
// is in it.
func (s *Server) testMaintainerr(ctx context.Context, u string, link int64) providerTestResult {
	res := providerTestResult{AppName: "Maintainerr"}
	c, err := maintainerr.New(u, maintainerr.Options{})
	if err != nil {
		res.Message = err.Error()
		return res
	}
	st, err := c.Status(ctx)
	res.Version = st.Version
	if err != nil {
		res.Message = describeProviderError("Maintainerr", err)
		return res
	}
	res.OK = true
	res.Message = fmt.Sprintf("Connected to Maintainerr %s. It has no API authentication; Bunkarr only reads from it.", st.Version)
	if link == 0 {
		return res
	}
	secs, err := s.app.Index.PlexSections(ctx, nil, link)
	if err != nil || len(secs) == 0 {
		res.Message += " Turn on and refresh the linked Plex server's library index to check that Maintainerr manages it."
		return res
	}
	cols, err := c.OverlayData(ctx)
	if err != nil {
		res.Message += " " + describeProviderError("Maintainerr", err)
		return res
	}
	keys := map[string]bool{}
	if err := s.app.Index.EachPlexItem(ctx, nil, link, func(it mediaindex.PlexItem) error {
		keys[it.RatingKey] = true
		return nil
	}); err != nil {
		return res
	}
	match, members, found := true, 0, 0
	for _, col := range cols {
		known := false
		for _, sec := range secs {
			known = known || sec.Key == col.LibraryID
		}
		match = match && known
		for _, m := range col.Media {
			members++
			if keys[m.RatingKey] {
				found++
			}
		}
	}
	if members > 0 && found == 0 {
		match = false
	}
	res.PlexMatches = &match
	if !match {
		res.Message += " Its collections do not match the linked Plex server's libraries: link it to the Plex server Maintainerr manages."
	}
	return res
}

// seerrUsers is GET /integrations/{id}/seerr/users: the users, read live, with labels built from
// user names only (never an e-mail, SEC4).
func (s *Server) seerrUsers(w http.ResponseWriter, r *http.Request) {
	it, err := s.integration(r)
	if err != nil {
		s.fail(w, r, "list Seerr users", err)
		return
	}
	if it.Type != integrations.TypeSeerr {
		s.fail(w, r, "list Seerr users", errorf(http.StatusBadRequest, "integration %q is a %s integration, not Seerr", it.Name, it.Type.AppName()))
		return
	}
	users, err := s.app.fetchSeerrUsers(r.Context(), it)
	if err != nil {
		var ve integrations.ValidationError
		if errors.As(err, &ve) || errors.Is(err, integrations.ErrURLChanged) {
			s.fail(w, r, "list Seerr users", errorf(http.StatusConflict, "%v", err))
			return
		}
		s.fail(w, r, "list Seerr users", errorf(http.StatusBadGateway, "%s", describeProviderError("Seerr", err)))
		return
	}
	writeJSON(w, http.StatusOK, users)
}

// fetchSeerrUsers reads a Seerr integration's users with its stored key, sent only to its stored
// URL (S8).
func (a *App) fetchSeerrUsers(ctx context.Context, it integrations.Integration) ([]seerr.User, error) {
	key, err := a.Integrations.TokenFor(ctx, it.ID, it.URL)
	if err != nil {
		return nil, err
	}
	c, err := seerr.New(it.URL, key, seerr.Options{})
	if err != nil {
		return nil, err
	}
	return c.Users(ctx)
}

// indexProvider is the tier engine's provider of the Plex index, Tautulli, Seerr and Maintainerr
// facts; the rule editor's Seerr user labels are read live.
func (a *App) indexProvider(idx *mediaindex.Store) tiers.Provider {
	return tiers.NewIndexProvider(idx, a.Integrations, func(ctx context.Context, it integrations.Integration) ([]tiers.Suggestion, error) {
		users, err := a.fetchSeerrUsers(ctx, it)
		if err != nil {
			return nil, err
		}
		out := make([]tiers.Suggestion, 0, len(users))
		for _, u := range users {
			out = append(out, tiers.Suggestion{Value: u.ID, Label: fmt.Sprintf("%s (%s)", u.Label, it.Name)})
		}
		return out, nil
	})
}

// providerRefresh returns the refresh schedule an integration's settings ask for: Tautulli, Seerr
// and Maintainerr's refresh, and a Plex integration's library index. ok is false for other types
// (and for a Plex integration whose index was never turned on: no schedule is created for it).
func providerRefresh(it integrations.Integration) (cron string, enabled, ok bool) {
	switch it.Type {
	case integrations.TypeTautulli:
		if st, err := it.TautulliSettings(); err == nil {
			return st.Refresh.Cron, st.Refresh.Enabled, true
		}
	case integrations.TypeSeerr:
		if st, err := it.SeerrSettings(); err == nil {
			return st.Refresh.Cron, st.Refresh.Enabled, true
		}
	case integrations.TypeMaintainerr:
		if st, err := it.MaintainerrSettings(); err == nil {
			return st.Refresh.Cron, st.Refresh.Enabled, true
		}
	case integrations.TypePlex:
		if ps, err := it.PlexSettings(); err == nil && ps.Index != nil {
			ix := ps.IndexSettings()
			cron := ix.Cron
			if cron == "" {
				cron = integrations.DefaultPlexIndexCron
			}
			return cron, ix.Enabled, true
		}
	}
	return "", false, false
}

// syncProviderRefreshScheduleIn upserts the refresh schedule of a Tautulli, Seerr or Maintainerr
// integration, or of a Plex library index, from its settings; with overwrite false an existing
// schedule is kept (start-up: System → Tasks is its source of truth). A Plex index that is off
// and has no schedule gets none. It reports whether it wrote.
func syncProviderRefreshScheduleIn(ctx context.Context, st *jobqueue.Store, it integrations.Integration, overwrite bool) (bool, error) {
	cron, enabled, ok := providerRefresh(it)
	if !ok {
		return false, nil
	}
	params := arrRefreshParams(it.ID)
	list, err := st.ListSchedules(ctx)
	if err != nil {
		return false, err
	}
	exists := false
	for _, sc := range list {
		if sc.JobType == jobs.TypeRefresh && sameParams(sc.Params, params) {
			exists = true
			if !overwrite || sc.Cron == cron && sc.Enabled == enabled {
				return false, nil
			}
		}
	}
	if !exists && !enabled && it.Type == integrations.TypePlex {
		return false, nil
	}
	if _, err := st.UpsertSchedule(ctx, jobs.TypeRefresh, params, cron, enabled); err != nil {
		return false, err
	}
	return true, nil
}

// providerRefreshOverlay returns a Tautulli, Seerr or Maintainerr integration with refresh.cron
// and refresh.enabled taken from its stored schedule (System → Tasks edits it), and a Plex one
// with index.cron and index.enabled likewise.
func providerRefreshOverlay(list []jobqueue.Schedule, it integrations.Integration) integrations.Integration {
	var sched *jobqueue.Schedule
	for i, sc := range list {
		if sc.JobType == jobs.TypeRefresh && sameParams(sc.Params, arrRefreshParams(it.ID)) {
			sched = &list[i]
			break
		}
	}
	if sched == nil {
		return it
	}
	var (
		v   any
		err error
	)
	switch it.Type {
	case integrations.TypeTautulli:
		var st integrations.TautulliSettings
		if st, err = it.TautulliSettings(); err == nil {
			st.Refresh.Cron, st.Refresh.Enabled = sched.Cron, sched.Enabled
			v = st
		}
	case integrations.TypeSeerr:
		var st integrations.SeerrSettings
		if st, err = it.SeerrSettings(); err == nil {
			st.Refresh.Cron, st.Refresh.Enabled = sched.Cron, sched.Enabled
			v = st
		}
	case integrations.TypeMaintainerr:
		var st integrations.MaintainerrSettings
		if st, err = it.MaintainerrSettings(); err == nil {
			st.Refresh.Cron, st.Refresh.Enabled = sched.Cron, sched.Enabled
			v = st
		}
	case integrations.TypePlex:
		var ps integrations.PlexSettings
		if ps, err = it.PlexSettings(); err == nil {
			ix := ps.IndexSettings()
			ix.Cron, ix.Enabled = sched.Cron, sched.Enabled
			ps.Index = &ix
			v = ps
		}
	default:
		return it
	}
	if err != nil {
		return it
	}
	if b, err := jsonMarshal(v); err == nil {
		it.Settings = b
	}
	return it
}

// afterProviderSave runs after a Tautulli, Seerr, Maintainerr or Plex integration was created
// (before nil) or updated: its refresh schedule follows its settings, and a new integration, or
// one whose URL, key, linked Plex server (or, for Plex, index or mappings) changed, gets a full
// refresh (design §4.1). A failed enqueue is logged: the save stands, and the schedule follows.
func (s *Server) afterProviderSave(ctx context.Context, before *integrations.Integration, it integrations.Integration, keyChanged bool) error {
	if it.Type.IsArr() {
		return nil
	}
	changed, err := syncProviderRefreshScheduleIn(ctx, s.app.Jobs.Store(), it, true)
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

// ensureProviderRefreshSchedules gives every Tautulli, Seerr and Maintainerr integration (and
// Plex library index) whose settings are valid its refresh schedule when it has none. Existing
// schedules are kept.
func (a *App) ensureProviderRefreshSchedules(ctx context.Context) error {
	list, err := a.Integrations.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range list {
		if _, err := syncProviderRefreshScheduleIn(ctx, a.Jobs.Store(), it, false); err != nil {
			errs = append(errs, fmt.Errorf("integration %q: %w", it.Name, err))
		}
	}
	return errors.Join(errs...)
}
