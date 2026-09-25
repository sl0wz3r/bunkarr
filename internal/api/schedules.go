package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// cronSchedule is a destination's (or Plex backup's) schedule in the API: {cron, enabled}. A
// destination without a schedule of that type shows {"cron": "", "enabled": false}.
type cronSchedule struct {
	Cron    string `json:"cron"`
	Enabled bool   `json:"enabled"`
}

// validateSchedule checks a schedule sent by a client: an enabled schedule needs a cron
// expression, and a non-empty one must be valid. nil (not sent) is valid.
func validateSchedule(field string, sc *cronSchedule) error {
	if sc == nil {
		return nil
	}
	sc.Cron = strings.TrimSpace(sc.Cron)
	if sc.Cron == "" {
		if sc.Enabled {
			return errorf(http.StatusBadRequest, "%s.cron is required when the schedule is enabled", field)
		}
		return nil
	}
	if err := jobqueue.ValidateCron(sc.Cron); err != nil {
		return errorf(http.StatusBadRequest, "%s: %v", field, err)
	}
	return nil
}

// destinationSchedule finds the schedule of job type t for destination destID (params exactly
// {destinationId}) in list.
func destinationSchedule(list []jobqueue.Schedule, t jobs.Type, destID int64) (jobqueue.Schedule, bool) {
	for _, sc := range list {
		p := sc.Params
		if sc.JobType == t && p.DestinationID == destID && p.IntegrationID == 0 && len(p.SourceIDs) == 0 && !p.AllowChanges {
			return sc, true
		}
	}
	return jobqueue.Schedule{}, false
}

// scheduleOf returns the API view of the schedule of type t for destination destID.
func scheduleOf(list []jobqueue.Schedule, t jobs.Type, destID int64) cronSchedule {
	if sc, ok := destinationSchedule(list, t, destID); ok {
		return cronSchedule{Cron: sc.Cron, Enabled: sc.Enabled}
	}
	return cronSchedule{}
}

// applyDestinationSchedule makes the stored schedule of type t for destination destID match sc
// (already validated): nil keeps it; an empty, disabled schedule removes it; anything else
// creates or updates it. It reports whether the schedules changed.
func (s *Server) applyDestinationSchedule(ctx context.Context, t jobs.Type, destID int64, sc *cronSchedule) (bool, error) {
	if sc == nil {
		return false, nil
	}
	st := s.app.Jobs.Store()
	if sc.Cron == "" {
		list, err := st.ListSchedules(ctx)
		if err != nil {
			return false, err
		}
		cur, ok := destinationSchedule(list, t, destID)
		if !ok {
			return false, nil
		}
		if err := st.DeleteSchedule(ctx, cur.ID); err != nil && !errors.Is(err, jobqueue.ErrNotFound) {
			return false, err
		}
		return true, nil
	}
	if _, err := st.UpsertSchedule(ctx, t, jobs.Params{DestinationID: destID}, sc.Cron, sc.Enabled); err != nil {
		return false, err
	}
	return true, nil
}

// reloadSchedules tells the scheduler the schedules changed. A failure is logged, not returned:
// the change is stored and the next reload (or restart) picks it up.
func (s *Server) reloadSchedules(ctx context.Context) {
	if err := s.app.Scheduler.Reload(ctx); err != nil {
		s.log.Error("Could not reload the schedules", "error", err)
	}
}

// preparePlexSettings validates the backup part of a Plex integration's settings before they are
// stored: an enabled backup without a cron expression gets DefaultPlexBackupCron, a cron
// expression must be valid, and the backup destination must exist. Other types and absent
// settings are returned unchanged.
func (s *Server) preparePlexSettings(ctx context.Context, typ integrations.Type, raw []byte) ([]byte, error) {
	t := strings.TrimSpace(string(raw))
	if typ != integrations.TypePlex || t == "" || t == "null" {
		return raw, nil
	}
	ps, err := integrations.ParsePlexSettings(raw)
	if err != nil {
		return nil, err
	}
	if ps.Backup.Enabled && ps.Backup.Cron == "" {
		ps.Backup.Cron = DefaultPlexBackupCron
	}
	if ps.Backup.Cron != "" {
		if err := jobqueue.ValidateCron(ps.Backup.Cron); err != nil {
			return nil, errorf(http.StatusBadRequest, "backup.cron: %v", err)
		}
	}
	if id := ps.Backup.DestinationID; id > 0 {
		if _, err := s.app.Destinations.Get(ctx, id); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return nil, errorf(http.StatusBadRequest, "backup.destinationId: destination %d does not exist", id)
			}
			return nil, err
		}
	}
	return jsonMarshal(ps)
}

// plexBackupParams is the params of an integration's scheduled Plex DB backup.
func plexBackupParams(integrationID, destinationID int64) jobs.Params {
	return jobs.Params{IntegrationID: integrationID, DestinationID: destinationID}
}

// syncPlexSchedule makes the plexdb_backup schedule of a Plex integration match its settings: a
// backup destination and a cron expression give a schedule (enabled as the settings say); no
// destination, or a disabled backup without a cron expression, gives none. Schedules of the
// integration with other params (an earlier destination) are removed.
func (s *Server) syncPlexSchedule(ctx context.Context, it integrations.Integration) error {
	if it.Type != integrations.TypePlex {
		return nil
	}
	ps, err := it.PlexSettings()
	if err != nil {
		return err
	}
	want := ps.Backup.DestinationID > 0 && ps.Backup.Cron != ""
	params := plexBackupParams(it.ID, ps.Backup.DestinationID)
	st := s.app.Jobs.Store()
	list, err := st.ListSchedules(ctx)
	if err != nil {
		return err
	}
	changed := false
	for _, sc := range list {
		if sc.JobType != jobs.TypePlexDBBackup || sc.Params.IntegrationID != it.ID {
			continue
		}
		if want && sameParams(sc.Params, params) {
			continue
		}
		if err := st.DeleteSchedule(ctx, sc.ID); err != nil && !errors.Is(err, jobqueue.ErrNotFound) {
			return err
		}
		changed = true
	}
	if want {
		if _, err := st.UpsertSchedule(ctx, jobs.TypePlexDBBackup, params, ps.Backup.Cron, ps.Backup.Enabled); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		s.reloadSchedules(ctx)
	}
	return nil
}

// sameParams reports whether two job params select the same work.
func sameParams(a, b jobs.Params) bool {
	as, bs := slices.Clone(a.SourceIDs), slices.Clone(b.SourceIDs)
	slices.Sort(as)
	slices.Sort(bs)
	return a.DestinationID == b.DestinationID && a.IntegrationID == b.IntegrationID &&
		a.AllowChanges == b.AllowChanges && slices.Equal(slices.Compact(as), slices.Compact(bs))
}

// scheduleView is GET /schedules' item (design §7).
type scheduleView struct {
	ID          int64       `json:"id"`
	JobType     jobs.Type   `json:"jobType"`
	Params      jobs.Params `json:"params"`
	Description string      `json:"description"`
	Cron        string      `json:"cron"`
	Enabled     bool        `json:"enabled"`
	LastRunAt   *time.Time  `json:"lastRunAt"`
	// NextRunAt is when the schedule queues its job next: nil when it is disabled or blocked.
	NextRunAt *time.Time `json:"nextRunAt"`
	// BlockedReason says why the scheduler refuses the schedule's jobs (its destination or Plex
	// server is disabled); "" when they can run.
	BlockedReason string `json:"blockedReason"`
}

func (s *Server) scheduleView(ctx context.Context, sc jobqueue.Schedule) scheduleView {
	v := scheduleView{ID: sc.ID, JobType: sc.JobType, Params: sc.Params, Description: s.app.describe(ctx, sc.JobType, sc.Params),
		Cron: sc.Cron, Enabled: sc.Enabled, LastRunAt: sc.LastRunAt, BlockedReason: s.blockedReason(ctx, sc.Params)}
	if next := s.app.Scheduler.NextRun(sc.ID); !next.IsZero() && v.BlockedReason == "" {
		n := next.UTC()
		v.NextRunAt = &n
	}
	return v
}

// blockedReason says why jobs with params p cannot run: the conditions of scheduleGate
// (App.checkEnabled), under which the scheduler refuses them and the runners fail them. ""
// when they can run (a destination or integration that does not exist is not reported here).
func (s *Server) blockedReason(ctx context.Context, p jobs.Params) string {
	if p.DestinationID != 0 {
		if d, err := s.app.Destinations.Get(ctx, p.DestinationID); err == nil && !d.Enabled {
			return fmt.Sprintf("destination %q is disabled", d.Name)
		}
	}
	if p.IntegrationID != 0 {
		if it, err := s.app.Integrations.Get(ctx, p.IntegrationID); err == nil && !it.Enabled {
			if it.Type == integrations.TypePlex {
				return fmt.Sprintf("Plex server %q is disabled", it.Name)
			}
			return fmt.Sprintf("integration %q is disabled", it.Name)
		}
	}
	return ""
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	list, err := s.app.Jobs.Store().ListSchedules(r.Context())
	if err != nil {
		s.fail(w, r, "list schedules", err)
		return
	}
	out := make([]scheduleView, 0, len(list))
	for _, sc := range list {
		out = append(out, s.scheduleView(r.Context(), sc))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "update schedule", err)
		return
	}
	var body struct {
		Cron    string `json:"cron"`
		Enabled *bool  `json:"enabled"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, "update schedule", err)
		return
	}
	st := s.app.Jobs.Store()
	cur, err := st.GetSchedule(r.Context(), id)
	if err != nil {
		s.fail(w, r, "update schedule", err)
		return
	}
	enabled := cur.Enabled
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	cron := strings.TrimSpace(body.Cron)
	if cron == "" {
		cron = cur.Cron
	}
	sc, err := st.UpdateSchedule(r.Context(), id, cron, enabled)
	if err != nil {
		s.fail(w, r, "update schedule", err)
		return
	}
	s.reloadSchedules(r.Context())
	s.log.Info("Schedule changed", "scheduleId", id, "jobType", string(sc.JobType), "cron", sc.Cron, "enabled", sc.Enabled)
	writeJSON(w, http.StatusOK, s.scheduleView(r.Context(), sc))
}

// runSchedule queues a schedule's job now (trigger manual). The optional body {dryRun} queues a
// dry run instead (the Preview of System → Tasks, e.g. what retention would expire), which is not
// recorded as a run of the schedule. A blocked schedule is a 409, as a manual sync of a disabled
// destination is: the job would only fail.
func (s *Server) runSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "run schedule", err)
		return
	}
	var body struct {
		DryRun bool `json:"dryRun"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "run schedule", err)
		return
	}
	sc, err := s.app.Jobs.Store().GetSchedule(r.Context(), id)
	if err != nil {
		s.fail(w, r, "run schedule", err)
		return
	}
	if reason := s.blockedReason(r.Context(), sc.Params); reason != "" {
		s.fail(w, r, "run schedule", errorf(http.StatusConflict, "%s; enable it to run this task", reason))
		return
	}
	job, err := s.app.Scheduler.RunNow(r.Context(), id, body.DryRun)
	if err != nil {
		s.fail(w, r, "run schedule", err)
		return
	}
	writeAccepted(w, job.ID, job)
}

// plexScheduleOverlay returns ps with the backup's cron and enabled flag taken from the stored
// plexdb_backup schedule of integration id, when one exists for the configured destination (the
// schedule can be edited on System → Tasks; it is the source of truth).
func plexScheduleOverlay(list []jobqueue.Schedule, id int64, ps integrations.PlexSettings) integrations.PlexSettings {
	for _, sc := range list {
		if sc.JobType == jobs.TypePlexDBBackup && sameParams(sc.Params, plexBackupParams(id, ps.Backup.DestinationID)) {
			ps.Backup.Cron, ps.Backup.Enabled = sc.Cron, sc.Enabled
			return ps
		}
	}
	return ps
}

// errScheduleSync wraps a failure to update schedules after the main change was saved.
func errScheduleSync(what string, err error) error {
	return fmt.Errorf("%s was saved, but its schedules could not be updated: %w", what, err)
}
