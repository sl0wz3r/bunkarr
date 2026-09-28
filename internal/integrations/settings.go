package integrations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// validateCron checks the syntax of a cron expression, as the scheduler parses it.
var validateCron = jobqueue.ValidateCron

// AppName is the product name of an integration type, for messages ("Radarr", "Plex").
func (t Type) AppName() string {
	switch t {
	case TypePlex:
		return "Plex"
	case TypeSonarr:
		return "Sonarr"
	case TypeRadarr:
		return "Radarr"
	case TypeLidarr:
		return "Lidarr"
	case TypeTautulli:
		return "Tautulli"
	case TypeSeerr:
		return "Seerr"
	case TypeMaintainerr:
		return "Maintainerr"
	}
	return string(t)
}

// normalizeOtherSettings validates the settings document of a type other than Plex and returns
// its stored form: parsed (unknown fields dropped, missing fields defaulted) and encoded again.
// References to other integrations are checked by checkLinks, inside the write.
func normalizeOtherSettings(typ Type, raw json.RawMessage) (string, error) {
	var (
		v   any
		err error
	)
	switch {
	case typ.IsArr():
		var s ArrSettings
		if s, err = ParseArrSettings(raw); err == nil {
			err = s.Validate(typ.AppName())
		}
		v = s
	case typ == TypeTautulli:
		var s TautulliSettings
		if s, err = ParseTautulliSettings(raw); err == nil {
			err = s.Validate()
		}
		v = s
	case typ == TypeSeerr:
		var s SeerrSettings
		if s, err = ParseSeerrSettings(raw); err == nil {
			err = s.Validate()
		}
		v = s
	case typ == TypeMaintainerr:
		var s MaintainerrSettings
		if s, err = ParseMaintainerrSettings(raw); err == nil {
			err = s.Validate()
		}
		v = s
	default:
		return "", ValidationError(fmt.Sprintf("unknown integration type %q", typ))
	}
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode %s settings: %w", typ, err)
	}
	return string(b), nil
}

// linkedPlex returns the plexIntegrationId of stored Tautulli, Seerr or Maintainerr settings (0
// for other types or none).
func linkedPlex(typ Type, settings string) int64 {
	var s struct {
		PlexIntegrationID int64 `json:"plexIntegrationId"`
	}
	switch typ {
	case TypeTautulli, TypeSeerr, TypeMaintainerr:
		if json.Unmarshal([]byte(settings), &s) == nil {
			return s.PlexIntegrationID
		}
	}
	return 0
}

// checkLinks checks, inside a write, what a row's settings and key may refer to: the
// plexIntegrationId of Tautulli, Seerr and Maintainerr must name a Plex integration, and a
// Maintainerr integration has no API key (it has no API authentication, S8). hasKey is whether
// the row will have a key after the write.
func checkLinks(ctx context.Context, tx *sql.Tx, typ Type, settings string, hasKey bool) error {
	if typ == TypeMaintainerr && hasKey {
		return ValidationError("Maintainerr has no API authentication: leave the API key empty (clearApiKey removes a saved one)")
	}
	id := linkedPlex(typ, settings)
	if id == 0 {
		return nil
	}
	var linked string
	err := tx.QueryRowContext(ctx, `SELECT type FROM integrations WHERE id = ?`, id).Scan(&linked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ValidationError(fmt.Sprintf("plexIntegrationId: integration %d does not exist", id))
	case err != nil:
		return fmt.Errorf("check plexIntegrationId: %w", err)
	case Type(linked) != TypePlex:
		return ValidationError(fmt.Sprintf("plexIntegrationId: integration %d is a %s integration, not Plex", id, linked))
	}
	return nil
}

// CheckArrIntegration checks that id names a Sonarr, Radarr or Lidarr integration, for
// sources.arr_integration_id (design §4.1): ErrNotFound for an unknown id, a ValidationError for
// another type.
func (s *Store) CheckArrIntegration(ctx context.Context, id int64) (Type, error) {
	it, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if !it.Type.IsArr() {
		return it.Type, ValidationError(fmt.Sprintf("integration %q is a %s integration, not Sonarr, Radarr or Lidarr", it.Name, it.Type.AppName()))
	}
	return it.Type, nil
}

// Backup targets (docs/design/phase4.md §8.5, D25). A Plex DB or *arr backup can go to up to
// MaxBackupTargets destinations, each with its own schedule: backup.targets. The Phase 1-2 single
// form {destinationId, cron, enabled} (and the *arr's acceptInsecureModes) is still accepted and
// stays a valid stored form: it is the one target it names (EffectiveTargets). Settings that carry
// backup.targets also carry the single form, mirroring targets[0], so a client that knows only the
// single form keeps reading the first target; WithBackupTargets adds targets to a stored single
// form for output, and NormalizeStored rewrites stored single forms with a destination into the
// targets form.

// MaxBackupTargets is the most backup targets a Plex or *arr integration may have.
const MaxBackupTargets = 4

// BackupTarget is one destination a Plex DB or *arr backup is written to, with its own schedule
// (one plexdb_backup or arr_backup schedule per target, params {integrationId, destinationId}).
type BackupTarget struct {
	DestinationID int64 `json:"destinationId"`
	// Cron is a 5-field cron expression ("" with enabled false: manual backups only).
	Cron    string `json:"cron"`
	Enabled bool   `json:"enabled"`
	// AcceptInsecureModes allows this target's destination not to enforce file modes. It counts
	// only for a local destination whose probe reports enforcesModes false (an SMB share without
	// POSIX extensions) and never for another target (S17 for engines, §8.4 step 6).
	AcceptInsecureModes bool `json:"acceptInsecureModes"`
}

// targetsPresent reports whether a settings document has a non-null backup.targets.
func targetsPresent(raw []byte) bool {
	var probe struct {
		Backup struct {
			Targets json.RawMessage `json:"targets"`
		} `json:"backup"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return false
	}
	t := bytes.TrimSpace(probe.Backup.Targets)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

// normalizeTargets trims the cron expressions of ts (a nil ts stays nil: the single form).
func normalizeTargets(ts []BackupTarget, defaultCron string) []BackupTarget {
	if ts == nil {
		return nil
	}
	out := make([]BackupTarget, len(ts))
	for i, t := range ts {
		t.Cron = strings.TrimSpace(t.Cron)
		if t.Enabled && t.Cron == "" {
			t.Cron = defaultCron
		}
		out[i] = t
	}
	return out
}

// singleTarget is the target the single form names (none without a destination).
func singleTarget(destinationID int64, cron string, enabled, accept bool) []BackupTarget {
	if destinationID <= 0 {
		return nil
	}
	return []BackupTarget{{DestinationID: destinationID, Cron: cron, Enabled: enabled, AcceptInsecureModes: accept}}
}

// targetFor returns the target of destinationID in ts (0: the first one).
func targetFor(ts []BackupTarget, destinationID int64) (BackupTarget, bool) {
	for _, t := range ts {
		if destinationID == 0 || t.DestinationID == destinationID {
			return t, true
		}
	}
	return BackupTarget{}, false
}

// validateTargets checks the structure of ts: at most MaxBackupTargets, each with a destination
// and a valid cron expression (required when enabled), no destination twice. app names the
// application in messages.
func validateTargets(ts []BackupTarget, app string) error {
	if len(ts) > MaxBackupTargets {
		return ValidationError(fmt.Sprintf("a %s backup has at most %d targets", app, MaxBackupTargets))
	}
	seen := map[int64]bool{}
	for i, t := range ts {
		field := fmt.Sprintf("backup.targets[%d]", i)
		switch {
		case t.DestinationID <= 0:
			return ValidationError(field + ".destinationId must be a destination id")
		case seen[t.DestinationID]:
			return ValidationError(fmt.Sprintf("%s: destination %d is a target already", field, t.DestinationID))
		case t.Enabled && t.Cron == "":
			return ValidationError(fmt.Sprintf("%s: scheduled %s backups need a schedule (cron)", field, app))
		}
		seen[t.DestinationID] = true
		if t.Cron != "" {
			if err := validateCron(t.Cron); err != nil {
				return ValidationError(field + ".cron: " + err.Error())
			}
		}
	}
	return nil
}

// BackupSchedules returns the schedules that mirror an integration's backup targets: one per
// target with a cron expression, params {integrationId, destinationId} (phase4.md §8.5). The
// caller passes them to jobqueue.Store.SyncSchedules with match {integrationId} for the
// integration's job type (plexdb_backup or arr_backup), then reloads the scheduler.
func BackupSchedules(integrationID int64, targets []BackupTarget) []jobqueue.ScheduleSpec {
	out := []jobqueue.ScheduleSpec{}
	for _, t := range targets {
		if t.DestinationID > 0 && t.Cron != "" {
			out = append(out, jobqueue.ScheduleSpec{Params: jobs.Params{IntegrationID: integrationID, DestinationID: t.DestinationID},
				Cron: t.Cron, Enabled: t.Enabled})
		}
	}
	return out
}

// ErrNoDestination is what a ValidateOptions.Destination lookup wraps for a destination that does
// not exist.
var ErrNoDestination = errors.New("destination not found")

// ErrUnencrypted means a Plex DB or *arr backup target is a destination off the machine without
// encryption: the backup holds the application's credentials (S17 for engines).
var ErrUnencrypted = errors.New("the backup holds the application's credentials")

// ErrModes means a backup target does not keep file modes and the target does not accept that.
var ErrModes = errors.New("the destination does not keep files private")

// ValidateOptions configures the destination checks of backup targets (Store.SetValidateOptions,
// ValidateBackupTargets). The api wires Destination to the destinations store.
type ValidateOptions struct {
	// Destination returns a target's destination: its kind (local, sftp, s3, b2), its encryption
	// (none, restic, crypt) and whether its files keep their modes (capabilities.enforcesModes).
	// An unknown id is an error wrapping ErrNoDestination.
	Destination func(ctx context.Context, id int64) (kind engines.DestKind, encryption engines.EncryptionMode, enforcesModes bool, err error)
}

// CheckBackupDestination is S17 for a Plex DB or *arr backup at one destination (phase4.md §8.4
// step 6): the backup holds app's credentials (Plex's PlexOnlineToken in Preferences.xml; the
// *arr's API key and passwords), so a destination whose kind is not local must be encrypted
// ("this backup holds <app>'s credentials; use an encrypted destination", wrapping
// ErrUnencrypted); an encrypted destination counts as enforcing file modes; otherwise a
// destination that does not enforce modes needs the target's acceptInsecureModes (ErrModes). An
// empty kind is local (Phase 1-3 destinations).
func CheckBackupDestination(app string, kind engines.DestKind, encryption engines.EncryptionMode, enforcesModes, acceptInsecure bool) error {
	encrypted := encryption != "" && encryption != engines.EncryptionNone
	if kind != "" && kind != engines.Local && !encrypted {
		return fmt.Errorf("%w: this backup holds %s's credentials; use an encrypted destination", ErrUnencrypted, app)
	}
	if encrypted || enforcesModes || acceptInsecure {
		return nil
	}
	return fmt.Errorf("%w (an SMB share without POSIX extensions, or not tested since Bunkarr checks this): the %s backup holds its "+
		"credentials; set acceptInsecureModes on this backup target to write it there anyway", ErrModes, app)
}

// ValidateBackupTargets checks each target's destination with o.Destination
// (CheckBackupDestination); a missing destination, an unencrypted remote one and one that does not
// keep modes are ValidationErrors naming the target. Without o.Destination it checks nothing.
func ValidateBackupTargets(ctx context.Context, app string, targets []BackupTarget, o ValidateOptions) error {
	if o.Destination == nil {
		return nil
	}
	for i, t := range targets {
		kind, enc, modes, err := o.Destination(ctx, t.DestinationID)
		if errors.Is(err, ErrNoDestination) {
			return ValidationError(fmt.Sprintf("backup.targets[%d]: destination %d does not exist", i, t.DestinationID))
		}
		if err != nil {
			return fmt.Errorf("check backup target %d: %w", t.DestinationID, err)
		}
		if err := CheckBackupDestination(app, kind, enc, modes, t.AcceptInsecureModes); err != nil {
			if errors.Is(err, ErrUnencrypted) {
				return ValidationError(fmt.Sprintf("this backup holds %s's credentials; use an encrypted destination (backup.targets[%d], destination %d)",
					app, i, t.DestinationID))
			}
			return ValidationError(fmt.Sprintf("backup.targets[%d]: destination %d %s", i, t.DestinationID, strings.TrimPrefix(err.Error(), "the destination ")))
		}
	}
	return nil
}

// SetValidateOptions makes Create and Update check the destinations of backup targets with o
// (ValidateBackupTargets): the api wires it after the destinations store exists. Without it only
// the targets' structure is checked.
func (s *Store) SetValidateOptions(o ValidateOptions) {
	s.validate.Store(&o)
}

// validateOptions returns what SetValidateOptions set (the zero options before).
func (s *Store) validateOptions() ValidateOptions {
	if o := s.validate.Load(); o != nil {
		return *o
	}
	return ValidateOptions{}
}

// backupTargets returns the effective backup targets of stored settings of typ (none for types
// without backups and settings that do not parse).
func backupTargets(typ Type, settings string) []BackupTarget {
	switch {
	case typ == TypePlex:
		if ps, err := ParsePlexSettings(json.RawMessage(settings)); err == nil {
			return ps.Backup.EffectiveTargets()
		}
	case typ.IsArr():
		if as, err := ParseArrSettings(json.RawMessage(settings)); err == nil {
			return as.Backup.EffectiveTargets()
		}
	}
	return nil
}

// checkTargets checks the destinations of the backup targets in normalized settings of typ.
func (s *Store) checkTargets(ctx context.Context, typ Type, settings string) error {
	if typ != TypePlex && !typ.IsArr() {
		return nil
	}
	app := typ.AppName()
	if typ == TypePlex {
		app = "Plex"
	}
	return ValidateBackupTargets(ctx, app, backupTargets(typ, settings), s.validateOptions())
}

// mergeSingleForm keeps the other targets of stored settings when an update sends the single
// form only (a client that knows only it): the single form replaces targets[0] (or removes it
// when it names no destination) and the stored targets[1:] stay. targets[0] gets exactly the
// single form's acceptInsecureModes, never the stored one: the Plex single form has none, so it
// withdraws a stored flag (S17: the web UI sends the single form when the box is unticked, and a
// consent carried over would keep writing Preferences.xml to a share that does not keep files
// private); a destination that needs the flag then fails checkTargets. raw is the update's
// settings document, norm its normalized form. Settings stored in the single form are replaced as
// they are.
func mergeSingleForm(typ Type, stored string, raw json.RawMessage, norm string) (string, error) {
	if (typ != TypePlex && !typ.IsArr()) || targetsPresent(bytes.TrimSpace(raw)) {
		return norm, nil
	}
	merge := func(old []BackupTarget, first []BackupTarget) []BackupTarget {
		out := slices.Clone(first)
		for i, t := range old {
			if i == 0 {
				continue
			}
			if len(out) == 0 || t.DestinationID != out[0].DestinationID {
				out = append(out, t)
			}
		}
		return out
	}
	var v any
	switch typ {
	case TypePlex:
		old, perr := ParsePlexSettings(json.RawMessage(stored))
		if perr != nil || old.Backup.Targets == nil {
			return norm, nil
		}
		ps, err := ParsePlexSettings(json.RawMessage(norm))
		if err != nil {
			return "", err
		}
		ps.Backup.setTargets(merge(old.Backup.Targets, ps.Backup.EffectiveTargets()))
		if err := ps.Validate(); err != nil {
			return "", err
		}
		v = ps
	default:
		old, perr := ParseArrSettings(json.RawMessage(stored))
		if perr != nil || old.Backup.Targets == nil {
			return norm, nil
		}
		as, err := ParseArrSettings(json.RawMessage(norm))
		if err != nil {
			return "", err
		}
		as.Backup.setTargets(merge(old.Backup.Targets, as.Backup.EffectiveTargets()))
		if err := as.Validate(typ.AppName()); err != nil {
			return "", err
		}
		v = as
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode %s settings: %w", typ, err)
	}
	return string(b), nil
}

// withTargetsForm returns stored settings of typ in the targets form when they hold the single
// form with a destination (NormalizeStored's migration); changed is false when nothing changes.
func withTargetsForm(typ Type, settings string) (out string, changed bool, err error) {
	var v any
	switch {
	case typ == TypePlex:
		ps, err := ParsePlexSettings(json.RawMessage(settings))
		if err != nil || ps.Backup.Targets != nil || ps.Backup.DestinationID <= 0 {
			return settings, false, nil
		}
		ps.Backup.setTargets(ps.Backup.EffectiveTargets())
		v = ps
	case typ.IsArr():
		as, err := ParseArrSettings(json.RawMessage(settings))
		if err != nil || as.Backup.Targets != nil || as.Backup.DestinationID <= 0 {
			return settings, false, nil
		}
		as.Backup.setTargets(as.Backup.EffectiveTargets())
		v = as
	default:
		return settings, false, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", false, fmt.Errorf("encode %s settings: %w", typ, err)
	}
	return string(b), string(b) != settings, nil
}

// withoutTarget returns stored settings of typ without the backup target of destinationID
// (targets form); changed is false when it had none.
func withoutTarget(typ Type, settings string, destinationID int64) (out string, changed bool, err error) {
	drop := func(ts []BackupTarget) ([]BackupTarget, bool) {
		out := slices.DeleteFunc(slices.Clone(ts), func(t BackupTarget) bool { return t.DestinationID == destinationID })
		return out, len(out) != len(ts)
	}
	var v any
	switch {
	case typ == TypePlex:
		ps, err := ParsePlexSettings(json.RawMessage(settings))
		if err != nil {
			return settings, false, nil
		}
		ts, ok := drop(ps.Backup.EffectiveTargets())
		if !ok {
			return settings, false, nil
		}
		ps.Backup.setTargets(ts)
		v = ps
	case typ.IsArr():
		as, err := ParseArrSettings(json.RawMessage(settings))
		if err != nil {
			return settings, false, nil
		}
		ts, ok := drop(as.Backup.EffectiveTargets())
		if !ok {
			return settings, false, nil
		}
		as.Backup.setTargets(ts)
		v = as
	default:
		return settings, false, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", false, fmt.Errorf("encode %s settings: %w", typ, err)
	}
	return string(b), true, nil
}

// RemoveDestination removes destinationID from the backup targets of every Plex and *arr
// integration (a deleted destination, phase4.md §8.5), in one transaction, and returns the ids of
// the integrations it changed (their backup schedules are then the caller's to sync).
func (s *Store) RemoveDestination(ctx context.Context, destinationID int64) ([]int64, error) {
	var changed []int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		changed = nil
		rows, err := tx.QueryContext(ctx, `SELECT id, type, settings FROM integrations
			WHERE type IN ('plex', 'sonarr', 'radarr', 'lidarr') ORDER BY id`)
		if err != nil {
			return err
		}
		type row struct {
			id       int64
			typ      Type
			settings string
		}
		var list []row
		for rows.Next() {
			var r row
			var typ string
			if err := rows.Scan(&r.id, &typ, &r.settings); err != nil {
				_ = rows.Close()
				return err
			}
			r.typ = Type(typ)
			list = append(list, r)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		now := db.FormatTime(s.now())
		for _, r := range list {
			out, ok, err := withoutTarget(r.typ, r.settings, destinationID)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE integrations SET settings = ?, updated_at = ? WHERE id = ?`, out, now, r.id); err != nil {
				return err
			}
			changed = append(changed, r.id)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("remove destination %d from the backup targets: %w", destinationID, err)
	}
	return changed, nil
}

// plexBackupOut and arrBackupOut are the output forms of the backup settings: targets always
// present.
type plexBackupOut struct {
	PlexBackup
	Targets []BackupTarget `json:"targets"`
}

type arrBackupOut struct {
	ArrBackup
	Targets []BackupTarget `json:"targets"`
}

// WithBackupTargets returns it with backup.targets in its settings (the effective targets; [] when
// there are none) next to the single form, for output: GET returns both forms (phase4.md §8.5).
// Other types, and settings that do not parse, are returned as they are.
func (i Integration) WithBackupTargets() Integration {
	var v any
	switch {
	case i.Type == TypePlex:
		ps, err := ParsePlexSettings(i.Settings)
		if err != nil {
			return i
		}
		ts := ps.Backup.EffectiveTargets()
		if ts == nil {
			ts = []BackupTarget{}
		}
		v = struct {
			PlexSettings
			Backup plexBackupOut `json:"backup"`
		}{ps, plexBackupOut{ps.Backup, ts}}
	case i.Type.IsArr():
		as, err := ParseArrSettings(i.Settings)
		if err != nil {
			return i
		}
		ts := as.Backup.EffectiveTargets()
		if ts == nil {
			ts = []BackupTarget{}
		}
		v = struct {
			ArrSettings
			Backup arrBackupOut `json:"backup"`
		}{as, arrBackupOut{as.Backup, ts}}
	default:
		return i
	}
	b, err := json.Marshal(v)
	if err != nil {
		return i
	}
	i.Settings = b
	return i
}
