package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/arrbackup"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
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
// destination defaults to the integration's first backup target (phase4.md §8.5). A disabled
// integration or destination, a destination that does not keep the zip private without the
// target's acceptInsecureModes (S17), an unencrypted remote destination (§8.4 step 6), and a
// destination whose recovery kit custody is not confirmed or whose engine is unavailable (S21,
// dry runs excepted) are 409: the job would only fail.
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
		if t, ok := as.Backup.TargetFor(0); ok {
			destID = t.DestinationID
		}
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
	target, _ := as.Backup.TargetFor(destID)
	accept := target.DestinationID == destID && target.AcceptInsecureModes
	if err := checkArrDestination(d, accept, app); err != nil {
		s.fail(w, r, "start an *arr backup", errorf(http.StatusConflict, "%v", err))
		return
	}
	if err := s.app.checkRunnable(d, body.DryRun); err != nil {
		s.fail(w, r, "start an *arr backup", err)
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

// checkArrDestination is S17 for an *arr backup at destination d: a filecopy destination must keep
// file modes unless the target accepts insecure modes (arrbackup.CheckModes, whose message names
// backup.acceptInsecureModes); a restic or rclone destination must be encrypted when it is not
// local (phase4.md §8.4 step 6; an encrypted one counts as keeping the zip private).
func checkArrDestination(d destinations.Destination, acceptInsecure bool, app string) error {
	if d.IsEngine() {
		return integrations.CheckBackupDestination(app, d.Kind, d.Encryption.Mode, d.Capabilities.EnforcesModes, acceptInsecure)
	}
	return arrbackup.CheckModes(d.Capabilities, acceptInsecure, d.Name, app)
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

// prepareArrSettings checks the backup targets of an *arr integration's settings before they are
// stored: each target's destination must exist and keep the zips private (checkArrDestination:
// capabilities.enforcesModes unless the target's acceptInsecureModes is set, S17, the 400 naming
// the flag; an encrypted destination when it is not local). Other types, absent settings and
// settings the store will refuse anyway are returned unchanged (the store validates and
// normalizes them).
func (s *Server) prepareArrSettings(ctx context.Context, typ integrations.Type, raw []byte) ([]byte, error) {
	t := strings.TrimSpace(string(raw))
	if !typ.IsArr() || t == "" || t == "null" {
		return raw, nil
	}
	as, err := integrations.ParseArrSettings(raw)
	if err != nil {
		return raw, nil
	}
	for i, target := range as.Backup.EffectiveTargets() {
		field := "backup.destinationId"
		if as.Backup.Targets != nil {
			field = fmt.Sprintf("backup.targets[%d].destinationId", i)
		}
		if target.DestinationID <= 0 {
			continue
		}
		d, err := s.app.Destinations.Get(ctx, target.DestinationID)
		if err != nil {
			if statusOf(err) == http.StatusNotFound {
				return nil, errorf(http.StatusBadRequest, "%s: destination %d does not exist", field, target.DestinationID)
			}
			return nil, err
		}
		if err := checkArrDestination(d, target.AcceptInsecureModes, typ.AppName()); err != nil {
			return nil, errorf(http.StatusBadRequest, "%v", err)
		}
	}
	return raw, nil
}

// syncArrBackupSchedule makes the arr_backup schedules of an *arr integration match its backup
// targets (syncBackupSchedules): one per target with a cron expression (an enabled target without
// one gets the weekly default when its settings are parsed); a disabled target without a cron
// expression gives none. Schedules of the integration with other params are removed.
func (s *Server) syncArrBackupSchedule(ctx context.Context, it integrations.Integration) error {
	if !it.Type.IsArr() {
		return nil
	}
	return s.syncBackupSchedules(ctx, it)
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
		OpenVersions: a.versionOpener(o),
	})
}
