package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/arrbackup"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// *arr backups (docs/design/phase2-3.md §10, §13): "Back up now" and the list of an *arr's
// versions. The backup schedule of an *arr integration is a schedules row mirrored from its
// settings (backup.destinationId, backup.cron, backup.enabled), like the Plex DB backup's. The
// versions themselves are never served (S17): only their snapshot rows (names, sizes, hashes).

func (s *Server) arrBackupRoutes(r chi.Router) {
	r.Post("/integrations/{id}/arr/backup", s.arrBackup)
	r.Get("/integrations/{id}/arr/snapshots", s.arrSnapshots)
}

// arrBackup is POST /integrations/{id}/arr/backup {destinationId?, dryRun} → 202 Job. The
// destination defaults to the integration's backup destination. A disabled integration or
// destination, and a destination that does not keep the zip private without
// backup.acceptInsecureModes (S17), are 409: the job would only fail.
func (s *Server) arrBackup(w http.ResponseWriter, r *http.Request) {
	it, err := s.arrIntegration(r)
	if err != nil {
		s.fail(w, r, "start an *arr backup", err)
		return
	}
	var body struct {
		DestinationID int64 `json:"destinationId"`
		DryRun        bool  `json:"dryRun"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "start an *arr backup", err)
		return
	}
	as, err := it.ArrSettings()
	if err != nil {
		s.fail(w, r, "start an *arr backup", err)
		return
	}
	app := it.Type.AppName()
	destID := body.DestinationID
	if destID == 0 {
		destID = as.Backup.DestinationID
	}
	if destID <= 0 {
		s.fail(w, r, "start an *arr backup", errorf(http.StatusBadRequest, "choose a destination for the %s backup (destinationId)", app))
		return
	}
	d, err := s.app.Destinations.Get(r.Context(), destID)
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			err = errorf(http.StatusBadRequest, "destination %d does not exist", destID)
		}
		s.fail(w, r, "start an *arr backup", err)
		return
	}
	switch {
	case !it.Enabled:
		s.fail(w, r, "start an *arr backup", errorf(http.StatusConflict, "%s %q is disabled", app, it.Name))
		return
	case !d.Enabled:
		s.fail(w, r, "start an *arr backup", errorf(http.StatusConflict, "destination %q is disabled", d.Name))
		return
	}
	if err := arrbackup.CheckModes(d.Capabilities, as.Backup.AcceptInsecureModes, d.Name, app); err != nil {
		s.fail(w, r, "start an *arr backup", errorf(http.StatusConflict, "%v", err))
		return
	}
	job, err := s.app.Jobs.Enqueue(r.Context(), jobs.Spec{Type: jobs.TypeArrBackup, Trigger: jobs.TriggerManual, DryRun: body.DryRun,
		Params: arrBackupParams(it.ID, destID)})
	if err != nil {
		s.fail(w, r, "start an *arr backup", err)
		return
	}
	writeAccepted(w, job.ID, job)
}

// arrSnapshots is GET /integrations/{id}/arr/snapshots: the integration's *arr backup versions at
// every destination, newest first (Snapshot rows of kind arr; nothing of the zips is served).
func (s *Server) arrSnapshots(w http.ResponseWriter, r *http.Request) {
	it, err := s.arrIntegration(r)
	if err != nil {
		s.fail(w, r, "list *arr backups", err)
		return
	}
	dests, err := s.app.Destinations.List(r.Context())
	if err != nil {
		s.fail(w, r, "list *arr backups", err)
		return
	}
	out := []snapshots.Snapshot{}
	for _, d := range dests {
		list, err := s.app.Snapshots.ListFor(r.Context(), snapshots.KindArr, d.ID, it.ID)
		if err != nil {
			s.fail(w, r, "list *arr backups", err)
			return
		}
		out = append(out, list...)
	}
	sortSnapshotsNewestFirst(out)
	writeJSON(w, http.StatusOK, out)
}

// sortSnapshotsNewestFirst orders versions by creation time, newest first (the higher id first at
// the same instant), as snapshots.Store lists them.
func sortSnapshotsNewestFirst(list []snapshots.Snapshot) {
	slices.SortFunc(list, func(a, b snapshots.Snapshot) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
}

// arrBackupParams is the params of an integration's *arr backup.
func arrBackupParams(integrationID, destinationID int64) jobs.Params {
	return jobs.Params{IntegrationID: integrationID, DestinationID: destinationID}
}

// prepareArrSettings checks the backup part of an *arr integration's settings before they are
// stored: the backup destination must exist, and it must keep the zips private
// (capabilities.enforcesModes) unless backup.acceptInsecureModes is set (S17; the 400 names the
// flag). Other types, absent settings and settings the store will refuse anyway are returned
// unchanged (the store validates and normalizes them).
func (s *Server) prepareArrSettings(ctx context.Context, typ integrations.Type, raw []byte) ([]byte, error) {
	t := strings.TrimSpace(string(raw))
	if !typ.IsArr() || t == "" || t == "null" {
		return raw, nil
	}
	as, err := integrations.ParseArrSettings(raw)
	if err != nil || as.Backup.DestinationID <= 0 {
		return raw, nil
	}
	d, err := s.app.Destinations.Get(ctx, as.Backup.DestinationID)
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			return nil, errorf(http.StatusBadRequest, "backup.destinationId: destination %d does not exist", as.Backup.DestinationID)
		}
		return nil, err
	}
	if err := arrbackup.CheckModes(d.Capabilities, as.Backup.AcceptInsecureModes, d.Name, typ.AppName()); err != nil {
		return nil, errorf(http.StatusBadRequest, "%v", err)
	}
	return raw, nil
}

// syncArrBackupSchedule makes the arr_backup schedule of an *arr integration match its settings:
// a backup destination and a cron expression give a schedule (enabled as the settings say; an
// enabled backup without a cron expression gets the weekly default when its settings are
// parsed); no destination, or a disabled backup without a cron expression, gives none.
// Schedules of the integration with other params (an earlier destination) are removed.
func (s *Server) syncArrBackupSchedule(ctx context.Context, it integrations.Integration) error {
	if !it.Type.IsArr() {
		return nil
	}
	as, err := it.ArrSettings()
	if err != nil {
		return err
	}
	want := as.Backup.DestinationID > 0 && as.Backup.Cron != ""
	params := arrBackupParams(it.ID, as.Backup.DestinationID)
	st := s.app.Jobs.Store()
	list, err := st.ListSchedules(ctx)
	if err != nil {
		return err
	}
	changed, current := false, false
	for _, sc := range list {
		if sc.JobType != jobs.TypeArrBackup || sc.Params.IntegrationID != it.ID {
			continue
		}
		if want && sameParams(sc.Params, params) {
			current = sc.Cron == as.Backup.Cron && sc.Enabled == as.Backup.Enabled
			continue
		}
		if err := st.DeleteSchedule(ctx, sc.ID); err != nil && !errors.Is(err, jobqueue.ErrNotFound) {
			return err
		}
		changed = true
	}
	if want && !current {
		if _, err := st.UpsertSchedule(ctx, jobs.TypeArrBackup, params, as.Backup.Cron, as.Backup.Enabled); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		s.reloadSchedules(ctx)
	}
	return nil
}

// arrBackupOverlay returns an *arr integration with backup.cron and backup.enabled taken from its
// stored arr_backup schedule for the configured destination, when there is one (System → Tasks
// edits it; it is the source of truth).
func arrBackupOverlay(list []jobqueue.Schedule, it integrations.Integration) integrations.Integration {
	as, err := it.ArrSettings()
	if err != nil || as.Backup.DestinationID <= 0 {
		return it
	}
	for _, sc := range list {
		if sc.JobType == jobs.TypeArrBackup && sameParams(sc.Params, arrBackupParams(it.ID, as.Backup.DestinationID)) {
			as.Backup.Cron, as.Backup.Enabled = sc.Cron, sc.Enabled
			if b, err := jsonMarshal(as); err == nil {
				it.Settings = b
			}
			return it
		}
	}
	return it
}

// describeArrBackup names an arr_backup job ("Backup of Radarr 4K" for an integration named so,
// "Backup of Radarr" when its name is the application's), design §12.5.
func (a *App) describeArrBackup(ctx context.Context, p jobs.Params) string {
	name, app := "*arr #"+strconv.FormatInt(p.IntegrationID, 10), "*arr"
	if it, err := a.Integrations.Get(ctx, p.IntegrationID); err == nil {
		name, app = it.Name, it.Type.AppName()
	}
	s := "Backup of " + app
	if !strings.EqualFold(strings.TrimSpace(name), app) {
		s += " " + name
	}
	if p.DestinationID != 0 {
		if d, err := a.Destinations.Get(ctx, p.DestinationID); err == nil {
			s += " to " + d.Name
		} else {
			s += " to destination #" + strconv.FormatInt(p.DestinationID, 10)
		}
	}
	return s
}

// newArrBackupRunner builds the arr_backup runner.
func (a *App) newArrBackupRunner(o AppOptions, configDir string) (*arrbackup.Runner, error) {
	return arrbackup.NewRunner(arrbackup.Options{
		DB:           o.DB,
		Integrations: a.Integrations,
		Destinations: a.Destinations,
		ConfigDir:    configDir,
		Log:          a.log.With("component", "arrbackup"),
		Location:     o.Location,
	})
}
