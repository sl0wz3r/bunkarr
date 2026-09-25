package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// Integrations (design §7). The API key (Plex token) is write-only: responses carry hasApiKey.

func (s *Server) integrationRoutes(r chi.Router) {
	r.Get("/integrations", s.listIntegrations)
	r.Post("/integrations", s.createIntegration)
	r.Post("/integrations/test", s.testIntegration)
	r.Get("/integrations/{id}", s.getIntegration)
	r.Put("/integrations/{id}", s.updateIntegration)
	r.Delete("/integrations/{id}", s.deleteIntegration)
	r.Get("/integrations/{id}/plex/sections", s.plexSections)
	r.Post("/integrations/{id}/plex/backup", s.plexBackup)
}

// integrationView returns it as the API shows it: a Plex integration's backup cron and enabled
// flag come from its stored schedule.
func integrationView(list []jobqueue.Schedule, it integrations.Integration) integrations.Integration {
	if it.Type != integrations.TypePlex {
		return it
	}
	ps, err := it.PlexSettings()
	if err != nil {
		return it
	}
	if b, err := json.Marshal(plexScheduleOverlay(list, it.ID, ps)); err == nil {
		it.Settings = b
	}
	return it
}

func (s *Server) schedules(ctx context.Context) ([]jobqueue.Schedule, error) {
	return s.app.Jobs.Store().ListSchedules(ctx)
}

func (s *Server) listIntegrations(w http.ResponseWriter, r *http.Request) {
	list, err := s.app.Integrations.List(r.Context())
	if err != nil {
		s.fail(w, r, "list integrations", err)
		return
	}
	scheds, err := s.schedules(r.Context())
	if err != nil {
		s.fail(w, r, "list integrations", err)
		return
	}
	for i := range list {
		list[i] = integrationView(scheds, list[i])
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getIntegration(w http.ResponseWriter, r *http.Request) {
	it, err := s.integration(r)
	if err != nil {
		s.fail(w, r, "get integration", err)
		return
	}
	s.writeIntegration(w, r, http.StatusOK, it)
}

func (s *Server) writeIntegration(w http.ResponseWriter, r *http.Request, status int, it integrations.Integration) {
	scheds, err := s.schedules(r.Context())
	if err != nil {
		s.fail(w, r, "read schedules", err)
		return
	}
	writeJSON(w, status, integrationView(scheds, it))
}

// integration loads the integration named by the {id} URL parameter.
func (s *Server) integration(r *http.Request) (integrations.Integration, error) {
	id, err := pathID(r)
	if err != nil {
		return integrations.Integration{}, err
	}
	return s.app.Integrations.Get(r.Context(), id)
}

func (s *Server) createIntegration(w http.ResponseWriter, r *http.Request) {
	var in integrations.Input
	if err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, "create integration", err)
		return
	}
	settings, err := s.preparePlexSettings(r.Context(), in.Type, in.Settings)
	if err != nil {
		s.fail(w, r, "create integration", err)
		return
	}
	in.Settings = settings
	it, err := s.app.Integrations.Create(r.Context(), in)
	if err != nil {
		s.fail(w, r, "create integration", err)
		return
	}
	s.log.Info("Integration created", "id", it.ID, "type", string(it.Type), "name", it.Name)
	if err := s.syncPlexSchedule(r.Context(), it); err != nil {
		s.fail(w, r, "create integration", errScheduleSync("the integration", err))
		return
	}
	s.writeIntegration(w, r, http.StatusCreated, it)
}

// updateIntegration replaces an integration (PUT semantics, see integrations.Input). The stored API
// key (Plex token) is bound to the stored URL: a request that changes the URL of an integration
// that has a key must send the key again (or clearApiKey). Such a request is refused with a 400
// that asks for the token, rather than silently clearing it, so the key is never sent to a host
// chosen by a caller who does not know it (S8) and a saved integration never loses its token
// without the user seeing why.
func (s *Server) updateIntegration(w http.ResponseWriter, r *http.Request) {
	cur, err := s.integration(r)
	if err != nil {
		s.fail(w, r, "update integration", err)
		return
	}
	var in integrations.Input
	if err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, "update integration", err)
		return
	}
	settings, err := s.preparePlexSettings(r.Context(), cur.Type, in.Settings)
	if err != nil {
		s.fail(w, r, "update integration", err)
		return
	}
	in.Settings = settings
	it, err := s.app.Integrations.Update(r.Context(), cur.ID, in)
	if err != nil {
		s.fail(w, r, "update integration", err)
		return
	}
	s.log.Info("Integration updated", "id", it.ID, "type", string(it.Type), "name", it.Name)
	if err := s.syncPlexSchedule(r.Context(), it); err != nil {
		s.fail(w, r, "update integration", errScheduleSync("the integration", err))
		return
	}
	s.writeIntegration(w, r, http.StatusOK, it)
}

// deleteIntegration removes an integration and its schedules. Backups it made stay at their
// destinations (their snapshot rows lose the integration link). It is refused (409) while a job of
// the integration (a Plex database backup) is queued or running, so the user cancels or waits for
// it. A backup that starts between this check and the delete is not left unrecorded: the runner
// records its version without the integration link (plexdb.Runner), as it does the integration's
// earlier versions.
func (s *Server) deleteIntegration(w http.ResponseWriter, r *http.Request) {
	it, err := s.integration(r)
	if err != nil {
		s.fail(w, r, "delete integration", err)
		return
	}
	ctx := r.Context()
	active, err := s.app.Jobs.Store().ActiveForIntegration(ctx, it.ID)
	if err != nil {
		s.fail(w, r, "delete integration", err)
		return
	}
	if active {
		s.fail(w, r, "delete integration", errorf(http.StatusConflict,
			"integration %q has queued or running jobs (a Plex database backup); cancel them or wait until they finish", it.Name))
		return
	}
	n, err := s.app.Jobs.Store().DeleteSchedulesFor(ctx, jobs.Params{IntegrationID: it.ID})
	if err != nil {
		s.fail(w, r, "delete integration", err)
		return
	}
	if err := s.app.Integrations.Delete(ctx, it.ID); err != nil {
		if n > 0 {
			// Put the schedule back: the integration is still there.
			if serr := s.syncPlexSchedule(ctx, it); serr != nil {
				s.log.Error("Could not restore the schedule of an integration that was not deleted", "id", it.ID, "error", serr)
			}
		}
		s.fail(w, r, "delete integration", err)
		return
	}
	if n > 0 {
		s.reloadSchedules(ctx)
	}
	s.log.Info("Integration deleted", "id", it.ID, "type", string(it.Type), "name", it.Name, "schedulesRemoved", n)
	w.WriteHeader(http.StatusNoContent)
}

// integrationTestResult is POST /integrations/test's answer: plex.TestResult plus Plex's butler
// window when the server reports it (the UI warns when a backup schedule falls inside it).
type integrationTestResult struct {
	plex.TestResult
	ButlerStartHour *int `json:"butlerStartHour,omitempty"`
	ButlerEndHour   *int `json:"butlerEndHour,omitempty"`
}

func (s *Server) testIntegration(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type   integrations.Type `json:"type"`
		URL    string            `json:"url"`
		APIKey string            `json:"apiKey"`
		// ID lets the edit form test with the stored token when apiKey is empty, only against the
		// stored URL.
		ID int64 `json:"id"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, "test integration", err)
		return
	}
	ctx := r.Context()
	if body.ID < 0 {
		s.fail(w, r, "test integration", errorf(http.StatusBadRequest, "id must be an integration id"))
		return
	}
	var cur *integrations.Integration
	if body.ID > 0 {
		it, err := s.app.Integrations.Get(ctx, body.ID)
		if err != nil {
			s.fail(w, r, "test integration", err)
			return
		}
		cur = &it
		if body.Type == "" {
			body.Type = it.Type
		}
	}
	switch {
	case body.Type == "":
		s.fail(w, r, "test integration", errorf(http.StatusBadRequest, "type is required"))
		return
	case !body.Type.Valid():
		s.fail(w, r, "test integration", errorf(http.StatusBadRequest, "unknown integration type %q", body.Type))
		return
	case body.Type != integrations.TypePlex:
		writeJSON(w, http.StatusOK, integrationTestResult{TestResult: plex.TestResult{
			Message: fmt.Sprintf("Testing %s integrations is not available yet.", body.Type)}})
		return
	}
	u, err := integrations.NormalizeURL(body.URL)
	if err != nil {
		s.fail(w, r, "test integration", err)
		return
	}
	// A typed token is not registered as a secret (it is not stored): it is redacted from the
	// answer below. The stored token is only sent to the stored URL (S8), so a caller who does
	// not know it cannot point it at a server of its choice. TokenFor compares u with the URL of
	// the row it reads the token from, so a URL change during this request cannot pair them.
	token := strings.TrimSpace(body.APIKey)
	if token == "" && cur != nil && cur.HasAPIKey {
		token, err = s.app.Integrations.TokenFor(ctx, cur.ID, u)
		if errors.Is(err, integrations.ErrURLChanged) {
			err = errorf(http.StatusBadRequest,
				"enter the Plex token to test a different URL: the saved token is only sent to the URL it was saved with")
		}
		if err != nil {
			s.fail(w, r, "test integration", err)
			return
		}
	}
	c, err := plex.New(u, token, s.app.plexOpts)
	if err != nil {
		s.fail(w, r, "test integration", errorf(http.StatusBadRequest, "%v", err))
		return
	}
	res := integrationTestResult{TestResult: c.Test(ctx)}
	if res.OK {
		if p, err := c.Prefs(ctx); err == nil {
			start, end := p.ButlerStartHour, p.ButlerEndHour
			res.ButlerStartHour, res.ButlerEndHour = &start, &end
		}
	}
	res.Message = logging.RedactValues(res.Message, token)
	writeJSON(w, http.StatusOK, res)
}

// plexClient returns a client for a stored Plex integration, with its token. The token is only
// returned while the stored URL is still it.URL (S8): when the URL changed after it was read, the
// request fails with 409 instead of sending the new token to the old URL.
func (s *Server) plexClient(ctx context.Context, it integrations.Integration) (*plex.Client, integrations.PlexSettings, error) {
	if it.Type != integrations.TypePlex {
		return nil, integrations.PlexSettings{}, errorf(http.StatusBadRequest, "integration %q is a %s integration, not Plex", it.Name, it.Type)
	}
	ps, err := it.PlexSettings()
	if err != nil {
		return nil, integrations.PlexSettings{}, err
	}
	token, err := s.app.Integrations.TokenFor(ctx, it.ID, it.URL)
	if errors.Is(err, integrations.ErrURLChanged) {
		err = errorf(http.StatusConflict, "the URL of Plex %q changed during the request; try again", it.Name)
	}
	if err != nil {
		return nil, integrations.PlexSettings{}, err
	}
	c, err := plex.New(it.URL, token, s.app.plexOpts)
	if err != nil {
		return nil, integrations.PlexSettings{}, fmt.Errorf("plex %q: %w", it.Name, err)
	}
	return c, ps, nil
}

// plexLocation is one folder of a Plex section: as Plex sees it, as Bunkarr sees it through the
// path mappings ("" when no mapping applies) and whether that directory exists.
type plexLocation struct {
	Path      string `json:"path"`
	LocalPath string `json:"localPath"`
	Exists    bool   `json:"exists"`
}

// plexSection is a Plex library section with its locations.
type plexSection struct {
	Key       string         `json:"key"`
	Title     string         `json:"title"`
	Type      string         `json:"type"`
	Locations []plexLocation `json:"locations"`
}

func (s *Server) plexSections(w http.ResponseWriter, r *http.Request) {
	it, err := s.integration(r)
	if err != nil {
		s.fail(w, r, "list plex sections", err)
		return
	}
	c, ps, err := s.plexClient(r.Context(), it)
	if err != nil {
		s.fail(w, r, "list plex sections", err)
		return
	}
	sections, err := c.Sections(r.Context())
	if err != nil {
		s.fail(w, r, "list plex sections", err)
		return
	}
	out := make([]plexSection, 0, len(sections))
	for _, sec := range sections {
		v := plexSection{Key: sec.Key, Title: sec.Title, Type: sec.Type, Locations: make([]plexLocation, 0, len(sec.Locations))}
		for _, loc := range sec.Locations {
			l := plexLocation{Path: loc.Path}
			if local, ok := ps.MapPath(loc.Path); ok {
				l.LocalPath = local
				if fi, err := os.Stat(local); err == nil && fi.IsDir() {
					l.Exists = true
				}
			}
			v.Locations = append(v.Locations, l)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) plexBackup(w http.ResponseWriter, r *http.Request) {
	it, err := s.integration(r)
	if err != nil {
		s.fail(w, r, "start plex backup", err)
		return
	}
	var body struct {
		DestinationID int64 `json:"destinationId"`
		DryRun        bool  `json:"dryRun"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "start plex backup", err)
		return
	}
	if it.Type != integrations.TypePlex {
		s.fail(w, r, "start plex backup", errorf(http.StatusBadRequest, "integration %q is a %s integration, not Plex", it.Name, it.Type))
		return
	}
	ps, err := it.PlexSettings()
	if err != nil {
		s.fail(w, r, "start plex backup", err)
		return
	}
	destID := body.DestinationID
	if destID == 0 {
		destID = ps.Backup.DestinationID
	}
	if destID <= 0 {
		s.fail(w, r, "start plex backup", errorf(http.StatusBadRequest, "choose a destination for the Plex database backup (destinationId)"))
		return
	}
	d, err := s.app.Destinations.Get(r.Context(), destID)
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			err = errorf(http.StatusBadRequest, "destination %d does not exist", destID)
		}
		s.fail(w, r, "start plex backup", err)
		return
	}
	if !it.Enabled {
		s.fail(w, r, "start plex backup", errorf(http.StatusConflict, "Plex %q is disabled", it.Name))
		return
	}
	if !d.Enabled {
		s.fail(w, r, "start plex backup", errorf(http.StatusConflict, "destination %q is disabled", d.Name))
		return
	}
	job, err := s.app.Jobs.Enqueue(r.Context(), jobs.Spec{Type: jobs.TypePlexDBBackup, Trigger: jobs.TriggerManual, DryRun: body.DryRun,
		Params: plexBackupParams(it.ID, destID)})
	if err != nil {
		s.fail(w, r, "start plex backup", err)
		return
	}
	writeAccepted(w, job.ID, job)
}
