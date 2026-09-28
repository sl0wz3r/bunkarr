package api

import (
	"context"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Backup targets of Plex and *arr integrations (docs/design/phase4.md §8.5, D25): up to four
// destinations per backup, each mirrored by one plexdb_backup or arr_backup schedule with params
// {integrationId, destinationId}. The Phase 1-2 single form {destinationId, cron, enabled} is
// still accepted and is the one target it names; GET returns both forms (the single form
// mirrors targets[0]).

// backupJobType is the job type of an integration's backups ("" for types without backups).
func backupJobType(typ integrations.Type) jobs.Type {
	switch {
	case typ == integrations.TypePlex:
		return jobs.TypePlexDBBackup
	case typ.IsArr():
		return jobs.TypeArrBackup
	}
	return ""
}

// syncBackupSchedules makes the backup schedules of a Plex or *arr integration match its targets
// (integrations.BackupSchedules): one schedule per target with a cron expression, enabled as the
// target says; the integration's other backup schedules (a removed target, an earlier
// destination) are deleted. Other types have none.
func (s *Server) syncBackupSchedules(ctx context.Context, it integrations.Integration) error {
	jt := backupJobType(it.Type)
	if jt == "" {
		return nil
	}
	var targets []integrations.BackupTarget
	if it.Type == integrations.TypePlex {
		ps, err := it.PlexSettings()
		if err != nil {
			return err
		}
		targets = ps.Backup.EffectiveTargets()
	} else {
		as, err := it.ArrSettings()
		if err != nil {
			return err
		}
		targets = as.Backup.EffectiveTargets()
	}
	if _, err := s.app.Jobs.Store().SyncSchedules(ctx, jt, jobs.Params{IntegrationID: it.ID},
		integrations.BackupSchedules(it.ID, targets)); err != nil {
		return err
	}
	s.reloadSchedules(ctx)
	return nil
}

// overlayTargets gives each backup target of integration id the cron and enabled flag of its
// stored schedule of type jt (System → Tasks edits it; it is the source of truth).
func overlayTargets(list []jobqueue.Schedule, jt jobs.Type, id int64, ts []integrations.BackupTarget) []integrations.BackupTarget {
	for i, t := range ts {
		for _, sc := range list {
			if sc.JobType == jt && sameParams(sc.Params, jobs.Params{IntegrationID: id, DestinationID: t.DestinationID}) {
				ts[i].Cron, ts[i].Enabled = sc.Cron, sc.Enabled
				break
			}
		}
	}
	return ts
}

// backupOverlay returns a Plex or *arr integration whose backup targets carry the cron and enabled
// flag of their stored schedules, in the targets form with the single form mirroring targets[0].
// Other types, and settings that do not parse, are returned as they are.
func backupOverlay(list []jobqueue.Schedule, it integrations.Integration) integrations.Integration {
	var v any
	switch {
	case it.Type == integrations.TypePlex:
		ps, err := it.PlexSettings()
		if err != nil {
			return it
		}
		ts := overlayTargets(list, jobs.TypePlexDBBackup, it.ID, ps.Backup.EffectiveTargets())
		ps.Backup.Targets = ts
		if len(ts) > 0 {
			ps.Backup.DestinationID, ps.Backup.Cron, ps.Backup.Enabled = ts[0].DestinationID, ts[0].Cron, ts[0].Enabled
		}
		v = ps
	case it.Type.IsArr():
		as, err := it.ArrSettings()
		if err != nil {
			return it
		}
		ts := overlayTargets(list, jobs.TypeArrBackup, it.ID, as.Backup.EffectiveTargets())
		as.Backup.Targets = ts
		if len(ts) > 0 {
			as.Backup.DestinationID, as.Backup.Cron, as.Backup.Enabled = ts[0].DestinationID, ts[0].Cron, ts[0].Enabled
			as.Backup.AcceptInsecureModes = ts[0].AcceptInsecureModes
		}
		v = as
	default:
		return it
	}
	if b, err := jsonMarshal(v); err == nil {
		it.Settings = b
	}
	return it
}

// saveTargetSchedule writes a backup schedule edited on System → Tasks back into its target in
// the integration's settings, so a later save of the integration that does not send that target
// (a client that knows only the single form keeps targets[1:]) mirrors the edited schedule, not
// the one it replaced. A failure is logged: the schedule is saved, and GET shows it anyway
// (backupOverlay).
func (s *Server) saveTargetSchedule(ctx context.Context, sc jobqueue.Schedule) {
	if (sc.JobType != jobs.TypePlexDBBackup && sc.JobType != jobs.TypeArrBackup) || sc.Params.IntegrationID == 0 {
		return
	}
	it, err := s.app.Integrations.Get(ctx, sc.Params.IntegrationID)
	if err != nil || backupJobType(it.Type) != sc.JobType {
		return
	}
	it = backupOverlay([]jobqueue.Schedule{sc}, it)
	if _, err := s.app.Integrations.Update(ctx, it.ID, integrations.Input{Name: it.Name, URL: it.URL, Settings: it.Settings}); err != nil {
		s.log.Warn("Could not save an edited backup schedule into its integration's settings", "integration", it.Name,
			"scheduleId", sc.ID, "error", err)
	}
}
