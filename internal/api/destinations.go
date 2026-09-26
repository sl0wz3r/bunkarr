package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Destinations (design §7). Creating, updating and deleting a destination also manages its sync
// and verify schedules; deleting one never touches the backup data at its target.

func (s *Server) destinationRoutes(r chi.Router) {
	r.Get("/destinations", s.listDestinations)
	r.Post("/destinations", s.createDestination)
	r.Post("/destinations/test", s.testTarget)
	r.Get("/destinations/{id}", s.getDestination)
	r.Put("/destinations/{id}", s.updateDestination)
	r.Delete("/destinations/{id}", s.deleteDestination)
	r.Post("/destinations/{id}/test", s.testDestination)
	r.Post("/destinations/{id}/sync", s.syncDestination)
	r.Post("/destinations/{id}/verify", s.verifyDestination)
	r.Get("/destinations/{id}/snapshots", s.destinationSnapshots)
}

// destinationView is the API's Destination: the stored destination plus its schedules, the
// most recent job that worked on it (any type) and its most recent sync.
type destinationView struct {
	destinations.Destination
	Schedule       cronSchedule `json:"schedule"`
	VerifySchedule cronSchedule `json:"verifySchedule"`
	LastJob        *jobs.Job    `json:"lastJob"`
	// LastSync is the newest sync job (queued, running or finished; previews excluded): the
	// "last sync status" of design §10, which later verify, retention and Plex DB backup jobs
	// must not hide.
	LastSync *jobs.Job `json:"lastSync"`
}

// lastSyncPageSize is how many sync jobs lastSync reads per page while it skips previews.
const lastSyncPageSize = 20

func (s *Server) destinationView(ctx context.Context, scheds []jobqueue.Schedule, d destinations.Destination) (destinationView, error) {
	v := destinationView{Destination: d, Schedule: scheduleOf(scheds, jobs.TypeSync, d.ID), VerifySchedule: scheduleOf(scheds, jobs.TypeVerify, d.ID)}
	page, err := s.app.Jobs.List(ctx, jobqueue.JobQuery{DestinationID: d.ID, PageSize: 1})
	if err != nil {
		return v, err
	}
	if len(page.Records) > 0 {
		j := page.Records[0]
		v.LastJob = &j
	}
	if v.LastSync, err = s.lastSync(ctx, d.ID); err != nil {
		return v, err
	}
	return v, nil
}

// lastSync returns destination destID's newest sync job that is not a preview (dry run), or nil.
func (s *Server) lastSync(ctx context.Context, destID int64) (*jobs.Job, error) {
	for p := 1; ; p++ {
		page, err := s.app.Jobs.List(ctx, jobqueue.JobQuery{DestinationID: destID, Type: jobs.TypeSync, Page: p, PageSize: lastSyncPageSize})
		if err != nil {
			return nil, err
		}
		for _, j := range page.Records {
			if !j.DryRun {
				return &j, nil
			}
		}
		if len(page.Records) < lastSyncPageSize || int64(p*lastSyncPageSize) >= page.TotalRecords {
			return nil, nil
		}
	}
}

func (s *Server) writeDestination(w http.ResponseWriter, r *http.Request, status int, d destinations.Destination) {
	scheds, err := s.schedules(r.Context())
	if err != nil {
		s.fail(w, r, "read schedules", err)
		return
	}
	v, err := s.destinationView(r.Context(), scheds, d)
	if err != nil {
		s.fail(w, r, "read the last job", err)
		return
	}
	writeJSON(w, status, v)
}

// destination loads the destination named by the {id} URL parameter.
func (s *Server) destination(r *http.Request) (destinations.Destination, error) {
	id, err := pathID(r)
	if err != nil {
		return destinations.Destination{}, err
	}
	return s.app.Destinations.Get(r.Context(), id)
}

func (s *Server) listDestinations(w http.ResponseWriter, r *http.Request) {
	list, err := s.app.Destinations.List(r.Context())
	if err != nil {
		s.fail(w, r, "list destinations", err)
		return
	}
	scheds, err := s.schedules(r.Context())
	if err != nil {
		s.fail(w, r, "list destinations", err)
		return
	}
	out := make([]destinationView, 0, len(list))
	for _, d := range list {
		v, err := s.destinationView(r.Context(), scheds, d)
		if err != nil {
			s.fail(w, r, "list destinations", err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getDestination(w http.ResponseWriter, r *http.Request) {
	d, err := s.destination(r)
	if err != nil {
		s.fail(w, r, "get destination", err)
		return
	}
	s.writeDestination(w, r, http.StatusOK, d)
}

// destinationBody is the body of POST and PUT /destinations: destinations.Input plus the
// schedules and, on create only, the confirmations of safety rule S3.
type destinationBody struct {
	destinations.Input
	// Schedule is the sync schedule. Create: nil means none (syncs run when started, until one
	// is chosen). Update: nil keeps it. {cron: "", enabled: false} removes it.
	Schedule *cronSchedule `json:"schedule"`
	// VerifySchedule is the verify schedule. Create: nil means DefaultVerifyCron, enabled.
	// Update: nil keeps it. {cron: "", enabled: false} removes it.
	VerifySchedule *cronSchedule `json:"verifySchedule"`
	// Attach and AllowLocal are destinations.CreateOptions (create only).
	Attach     bool `json:"attach"`
	AllowLocal bool `json:"allowLocal"`
}

func (b *destinationBody) validateSchedules() error {
	if err := validateSchedule("schedule", b.Schedule); err != nil {
		return err
	}
	return validateSchedule("verifySchedule", b.VerifySchedule)
}

func (s *Server) createDestination(w http.ResponseWriter, r *http.Request) {
	var body destinationBody
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, "create destination", err)
		return
	}
	if err := body.validateSchedules(); err != nil {
		s.fail(w, r, "create destination", err)
		return
	}
	if strings.TrimSpace(body.Target) == "" {
		s.fail(w, r, "create destination", errorf(http.StatusBadRequest, "target is required: the absolute path of the mounted share"))
		return
	}
	if body.VerifySchedule == nil {
		body.VerifySchedule = &cronSchedule{Cron: DefaultVerifyCron, Enabled: true}
	}
	ctx := r.Context()
	d, err := s.app.Destinations.Create(ctx, body.Input, destinations.CreateOptions{Attach: body.Attach, AllowLocal: body.AllowLocal})
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			err = errorf(http.StatusBadRequest, "%v (check PUID/PGID and the share's permissions)", err)
		}
		s.fail(w, r, "create destination", err)
		return
	}
	s.log.Info("Destination created", "id", d.ID, "name", d.Name, "target", d.Target, "fsType", d.FSType, "attach", body.Attach, "allowLocal", body.AllowLocal)
	changed := false
	for _, sc := range []struct {
		t  jobs.Type
		sc *cronSchedule
	}{{jobs.TypeSync, body.Schedule}, {jobs.TypeVerify, body.VerifySchedule}} {
		c, err := s.applyDestinationSchedule(ctx, sc.t, d.ID, sc.sc)
		if err != nil {
			s.fail(w, r, "create destination", errScheduleSync("the destination", err))
			return
		}
		changed = changed || c
	}
	if changed {
		s.reloadSchedules(ctx)
	}
	s.writeDestination(w, r, http.StatusCreated, d)
}

func (s *Server) updateDestination(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "update destination", err)
		return
	}
	var body destinationBody
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, "update destination", err)
		return
	}
	if body.Attach || body.AllowLocal {
		s.fail(w, r, "update destination", errorf(http.StatusBadRequest, "attach and allowLocal only apply when a destination is created"))
		return
	}
	if err := body.validateSchedules(); err != nil {
		s.fail(w, r, "update destination", err)
		return
	}
	ctx := r.Context()
	d, err := s.app.Destinations.Update(ctx, id, body.Input)
	if err != nil {
		s.fail(w, r, "update destination", err)
		return
	}
	s.log.Info("Destination updated", "id", d.ID, "name", d.Name, "enabled", d.Enabled)
	changed := false
	for _, sc := range []struct {
		t  jobs.Type
		sc *cronSchedule
	}{{jobs.TypeSync, body.Schedule}, {jobs.TypeVerify, body.VerifySchedule}} {
		c, err := s.applyDestinationSchedule(ctx, sc.t, d.ID, sc.sc)
		if err != nil {
			s.fail(w, r, "update destination", errScheduleSync("the destination", err))
			return
		}
		changed = changed || c
	}
	if changed {
		s.reloadSchedules(ctx)
	}
	s.writeDestination(w, r, http.StatusOK, d)
}

// deleteDestination forgets a destination: its row, file records, snapshot records and
// schedules (including Plex DB backups to it, whose integrations are left without a backup
// destination). Nothing at the target is touched. A destination with queued or running jobs
// cannot be deleted.
func (s *Server) deleteDestination(w http.ResponseWriter, r *http.Request) {
	d, err := s.destination(r)
	if err != nil {
		s.fail(w, r, "delete destination", err)
		return
	}
	ctx := r.Context()
	active, err := s.app.Jobs.List(ctx, jobqueue.JobQuery{State: jobqueue.StateActive, DestinationID: d.ID, PageSize: 1})
	if err != nil {
		s.fail(w, r, "delete destination", err)
		return
	}
	if active.TotalRecords > 0 {
		s.fail(w, r, "delete destination", errorf(http.StatusConflict,
			"destination %q has queued or running jobs (job %d); cancel them or wait until they finish", d.Name, active.Records[0].ID))
		return
	}
	if err := s.app.Destinations.Delete(ctx, d.ID); err != nil {
		s.fail(w, r, "delete destination", err)
		return
	}
	// Its id leaves every tier rule's "Applies at" (an emptied list applies nowhere, never
	// everywhere; phase2-3.md §8.7).
	if err := s.app.Tiers.Store().RemoveDestination(ctx, d.ID); err != nil {
		s.fail(w, r, "delete destination", err)
		return
	}
	n, err := s.app.Jobs.Store().DeleteSchedulesFor(ctx, jobs.Params{DestinationID: d.ID})
	if err != nil {
		s.fail(w, r, "delete destination", errScheduleSync("the deletion", err))
		return
	}
	if n > 0 {
		s.reloadSchedules(ctx)
	}
	if err := s.clearPlexBackups(ctx, d.ID); err != nil {
		s.fail(w, r, "delete destination", err)
		return
	}
	s.log.Info("Destination deleted (its backup data was not touched)", "id", d.ID, "name", d.Name, "target", d.Target, "schedulesRemoved", n)
	w.WriteHeader(http.StatusNoContent)
}

// clearPlexBackups turns off the Plex DB backup of every integration that backed up to
// destination destID (just deleted), so its settings do not name a destination that is gone.
func (s *Server) clearPlexBackups(ctx context.Context, destID int64) error {
	list, err := s.app.Integrations.List(ctx)
	if err != nil {
		return err
	}
	for _, it := range list {
		if it.Type != integrations.TypePlex {
			continue
		}
		ps, err := it.PlexSettings()
		if err != nil || ps.Backup.DestinationID != destID {
			continue
		}
		ps.Backup.DestinationID, ps.Backup.Enabled = 0, false
		raw, err := jsonMarshal(ps)
		if err != nil {
			return err
		}
		if _, err := s.app.Integrations.Update(ctx, it.ID, integrations.Input{Name: it.Name, URL: it.URL, Settings: raw}); err != nil {
			return errScheduleSync("the deletion", err)
		}
		s.log.Info("Plex DB backup turned off: its destination was deleted", "integration", it.Name, "destinationId", destID)
	}
	return nil
}

func (s *Server) testTarget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target string `json:"target"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, "test destination target", err)
		return
	}
	if strings.TrimSpace(body.Target) == "" {
		s.fail(w, r, "test destination target", errorf(http.StatusBadRequest, "target is required"))
		return
	}
	res, err := s.app.Destinations.Test(r.Context(), body.Target, 0)
	if err != nil {
		s.fail(w, r, "test destination target", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) testDestination(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "test destination", err)
		return
	}
	var body struct{}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "test destination", err)
		return
	}
	res, err := s.app.Destinations.Test(r.Context(), "", id)
	if err != nil {
		s.fail(w, r, "test destination", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// enabledDestination loads the {id} destination and refuses a disabled one (409).
func (s *Server) enabledDestination(r *http.Request) (destinations.Destination, error) {
	d, err := s.destination(r)
	if err != nil {
		return d, err
	}
	if !d.Enabled {
		return d, errorf(http.StatusConflict, "destination %q is disabled", d.Name)
	}
	return d, nil
}

func (s *Server) syncDestination(w http.ResponseWriter, r *http.Request) {
	d, err := s.enabledDestination(r)
	if err != nil {
		s.fail(w, r, "start sync", err)
		return
	}
	var body struct {
		DryRun       bool `json:"dryRun"`
		AllowChanges bool `json:"allowChanges"`
		releaseParams
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "start sync", err)
		return
	}
	// A release (phase2-3.md S15) must apply the preview the user confirmed (409 otherwise).
	if err := s.checkRelease(r.Context(), d.ID, body.DryRun, body.releaseParams); err != nil {
		s.fail(w, r, "start sync", err)
		return
	}
	job, err := s.app.Jobs.Enqueue(r.Context(), jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerManual, DryRun: body.DryRun,
		Params: jobs.Params{DestinationID: d.ID, AllowChanges: body.AllowChanges, ReleaseDemoted: body.ReleaseDemoted,
			ReleaseOf: body.ReleaseOf, ReleaseRevision: body.ReleaseRevision}})
	if err != nil {
		s.fail(w, r, "start sync", err)
		return
	}
	writeAccepted(w, job.ID, job)
}

func (s *Server) verifyDestination(w http.ResponseWriter, r *http.Request) {
	d, err := s.enabledDestination(r)
	if err != nil {
		s.fail(w, r, "start verify", err)
		return
	}
	var body struct {
		DryRun bool `json:"dryRun"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "start verify", err)
		return
	}
	job, err := s.app.Jobs.Enqueue(r.Context(), jobs.Spec{Type: jobs.TypeVerify, Trigger: jobs.TriggerManual, DryRun: body.DryRun,
		Params: jobs.Params{DestinationID: d.ID}})
	if err != nil {
		s.fail(w, r, "start verify", err)
		return
	}
	writeAccepted(w, job.ID, job)
}

func (s *Server) destinationSnapshots(w http.ResponseWriter, r *http.Request) {
	d, err := s.destination(r)
	if err != nil {
		s.fail(w, r, "list snapshots", err)
		return
	}
	list, err := s.app.Snapshots.List(r.Context(), d.ID)
	if err != nil {
		s.fail(w, r, "list snapshots", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
