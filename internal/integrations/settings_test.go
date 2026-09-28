package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// testDest is a destination as the target lookup reports it.
type testDest struct {
	kind  engines.DestKind
	enc   engines.EncryptionMode
	modes bool
}

// The destinations of these tests: 1 the UNAS (filecopy on SMB without POSIX extensions), 2 a
// restic repository on B2, 3 plain rclone on S3, 4 rclone crypt on S3, 5 a local disk that keeps
// modes.
var testDests = map[int64]testDest{
	1: {engines.Local, engines.EncryptionNone, false},
	2: {engines.B2, engines.EncryptionRestic, true},
	3: {engines.S3, engines.EncryptionNone, false},
	4: {engines.S3, engines.EncryptionCrypt, true},
	5: {engines.Local, engines.EncryptionNone, true},
}

func testLookup() ValidateOptions {
	return ValidateOptions{Destination: func(ctx context.Context, id int64) (engines.DestKind, engines.EncryptionMode, bool, error) {
		d, ok := testDests[id]
		if !ok {
			return "", "", false, fmt.Errorf("destination %d: %w", id, ErrNoDestination)
		}
		return d.kind, d.enc, d.modes, nil
	}}
}

func TestBackupTargetsParse(t *testing.T) {
	// The single form stays as it is and is the one target.
	ps, err := ParsePlexSettings(json.RawMessage(`{"backup":{"destinationId":5,"cron":"0 6 * * *","enabled":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []BackupTarget{{DestinationID: 5, Cron: "0 6 * * *", Enabled: true}}
	if ps.Backup.Targets != nil || !reflect.DeepEqual(ps.Backup.EffectiveTargets(), want) {
		t.Fatalf("single form = %+v, effective %+v", ps.Backup, ps.Backup.EffectiveTargets())
	}
	if tg, ok := ps.Backup.TargetFor(0); !ok || tg.DestinationID != 5 {
		t.Fatalf("TargetFor(0) = %+v, %v", tg, ok)
	}
	if _, ok := ps.Backup.TargetFor(9); ok {
		t.Fatal("TargetFor(9) found a target")
	}
	// The targets form: the single form mirrors targets[0]; both are written.
	ps, err = ParsePlexSettings(json.RawMessage(`{"dataPath":"/plex","backup":{"destinationId":9,"cron":"x","targets":[
		{"destinationId":1,"cron":" 0 3 * * * ","enabled":true,"acceptInsecureModes":true},
		{"destinationId":2,"cron":"0 4 * * 0","enabled":true}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ps.Backup.DestinationID != 1 || ps.Backup.Cron != "0 3 * * *" || !ps.Backup.Enabled || len(ps.Backup.Targets) != 2 {
		t.Fatalf("targets form = %+v", ps.Backup)
	}
	if err := ps.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(ps)
	if !strings.Contains(string(b), `"backup":{"destinationId":1,"cron":"0 3 * * *","enabled":true,"targets":[{"destinationId":1,`) {
		t.Fatalf("encoded = %s", b)
	}
	again, err := ParsePlexSettings(b)
	if err != nil || !reflect.DeepEqual(again, ps) {
		t.Fatalf("round trip = %+v, %v", again, err)
	}
	// *arr: the single form's acceptInsecureModes is the target's; an enabled target without a
	// cron expression is weekly.
	as, err := ParseArrSettings(json.RawMessage(`{"backup":{"destinationId":1,"acceptInsecureModes":true}}`))
	if err != nil || !reflect.DeepEqual(as.Backup.EffectiveTargets(), []BackupTarget{{DestinationID: 1, AcceptInsecureModes: true}}) {
		t.Fatalf("arr single form = %+v, %v", as.Backup, err)
	}
	as, err = ParseArrSettings(json.RawMessage(`{"backup":{"maxScheduledAgeDays":3,"targets":[{"destinationId":2,"enabled":true}]}}`))
	if err != nil || as.Backup.Targets[0].Cron != DefaultArrBackupCron || as.Backup.Cron != DefaultArrBackupCron ||
		as.Backup.MaxScheduledAgeDays != 3 || as.Backup.DestinationID != 2 {
		t.Fatalf("arr targets = %+v, %v", as.Backup, err)
	}
	// An explicit empty list clears the single form too.
	as, err = ParseArrSettings(json.RawMessage(`{"backup":{"destinationId":1,"enabled":true,"targets":[]}}`))
	if err != nil || as.Backup.DestinationID != 0 || as.Backup.Enabled || len(as.Backup.EffectiveTargets()) != 0 {
		t.Fatalf("empty targets = %+v, %v", as.Backup, err)
	}
}

func TestBackupTargetsValidate(t *testing.T) {
	target := func(id int64) BackupTarget { return BackupTarget{DestinationID: id, Cron: "0 3 * * *", Enabled: true} }
	tests := []struct {
		name    string
		targets []BackupTarget
		wantErr string
	}{
		{"four", []BackupTarget{target(1), target(2), target(3), target(4)}, ""},
		{"five", []BackupTarget{target(1), target(2), target(3), target(4), target(5)}, "at most 4 targets"},
		{"twice", []BackupTarget{target(1), target(1)}, "is a target already"},
		{"no destination", []BackupTarget{target(1), target(0)}, "backup.targets[1].destinationId must be a destination id"},
		{"bad cron", []BackupTarget{target(1), {DestinationID: 2, Cron: "61 * * * *"}}, "backup.targets[1].cron"},
		{"enabled without cron", []BackupTarget{target(1), {DestinationID: 2, Enabled: true}}, "need a schedule"},
		{"manual only", []BackupTarget{{DestinationID: 2}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := PlexSettings{DataPath: "/plex", PathMappings: []PathMapping{}}
			ps.Backup.setTargets(slices.Clone(tt.targets))
			err := ps.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var verr ValidationError
			if !errors.As(err, &verr) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
	ps := PlexSettings{PathMappings: []PathMapping{}}
	ps.Backup.setTargets([]BackupTarget{{DestinationID: 2}, target(3)})
	if err := ps.Validate(); err == nil || !strings.Contains(err.Error(), "dataPath") {
		t.Fatalf("an enabled second target without a data path: %v", err)
	}
}

// TestValidateBackupTargetsS17: Plex and *arr targets must be encrypted off the machine;
// acceptInsecureModes is per target and only counts for local destinations.
func TestValidateBackupTargetsS17(t *testing.T) {
	ctx := context.Background()
	o := testLookup()
	accept := func(id int64) BackupTarget { return BackupTarget{DestinationID: id, AcceptInsecureModes: true} }
	plain := func(id int64) BackupTarget { return BackupTarget{DestinationID: id} }
	tests := []struct {
		name    string
		targets []BackupTarget
		wantErr string
	}{
		{"encrypted restic", []BackupTarget{plain(2)}, ""},
		{"crypt", []BackupTarget{plain(4)}, ""},
		{"local keeping modes", []BackupTarget{plain(5)}, ""},
		{"plain rclone on s3", []BackupTarget{plain(3)}, "this backup holds Radarr's credentials; use an encrypted destination"},
		{"plain rclone even with the flag", []BackupTarget{accept(3)}, "use an encrypted destination"},
		{"UNAS without the flag", []BackupTarget{plain(1)}, "acceptInsecureModes"},
		{"UNAS with the flag", []BackupTarget{accept(1)}, ""},
		{"the UNAS flag does not cover a second target on plain rclone", []BackupTarget{accept(1), plain(3)}, "use an encrypted destination"},
		{"missing destination", []BackupTarget{plain(8)}, "destination 8 does not exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBackupTargets(ctx, "Radarr", tt.targets, o)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var verr ValidationError
			if !errors.As(err, &verr) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want a ValidationError with %q", err, tt.wantErr)
			}
		})
	}
	if err := ValidateBackupTargets(ctx, "Plex", []BackupTarget{plain(3)}, ValidateOptions{}); err != nil {
		t.Fatalf("without a lookup nothing is checked: %v", err)
	}
	if err := CheckBackupDestination("Plex", engines.S3, engines.EncryptionNone, true, true); !errors.Is(err, ErrUnencrypted) {
		t.Fatalf("CheckBackupDestination = %v", err)
	}
	for _, enc := range []engines.EncryptionMode{engines.EncryptionRestic, engines.EncryptionCrypt} {
		if err := CheckBackupDestination("Plex", engines.Local, enc, false, false); err != nil {
			t.Fatalf("an encrypted destination counts as keeping modes: %v", err)
		}
	}
	if err := CheckBackupDestination("Plex", "", "", false, false); !errors.Is(err, ErrModes) {
		t.Fatalf("CheckBackupDestination(Phase 3 SMB) = %v", err)
	}
}

// TestStoreBackupTargets: saving checks the targets' destinations; an update with only the single
// form keeps the other targets; deleting a destination removes it from every target list; output
// carries both forms.
func TestStoreBackupTargets(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	s.SetValidateOptions(testLookup())
	radarr := func(settings string) Input {
		return Input{Type: TypeRadarr, Name: "Radarr", URL: "http://radarr:7878", APIKey: "radarr-key-targets-0001", Settings: json.RawMessage(settings)}
	}
	if _, err := s.Create(ctx, radarr(`{"backup":{"targets":[{"destinationId":1,"acceptInsecureModes":true},{"destinationId":3}]}}`)); err == nil ||
		!strings.Contains(err.Error(), "use an encrypted destination") {
		t.Fatalf("Create with plain rclone = %v", err)
	}
	it, err := s.Create(ctx, radarr(`{"backup":{"targets":[{"destinationId":1,"cron":"0 3 * * *","enabled":true,"acceptInsecureModes":true},
		{"destinationId":2,"cron":"0 4 * * 0","enabled":true},{"destinationId":4}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	// A client that knows only the single form changes targets[0]; the others stay.
	upd, err := s.Update(ctx, it.ID, Input{Name: "Radarr", URL: "http://radarr:7878",
		Settings: json.RawMessage(`{"backup":{"destinationId":5,"cron":"0 5 * * *","enabled":true}}`)})
	if err != nil {
		t.Fatal(err)
	}
	as, _ := upd.ArrSettings()
	var ids []int64
	for _, tg := range as.Backup.EffectiveTargets() {
		ids = append(ids, tg.DestinationID)
	}
	if !slices.Equal(ids, []int64{5, 2, 4}) || as.Backup.DestinationID != 5 || as.Backup.AcceptInsecureModes {
		t.Fatalf("merged targets = %v (%+v)", ids, as.Backup)
	}
	// Schedules: one per target with a cron expression.
	specs := BackupSchedules(it.ID, as.Backup.EffectiveTargets())
	if len(specs) != 2 || !reflect.DeepEqual(specs[0].Params, jobs.Params{IntegrationID: it.ID, DestinationID: 5}) || specs[1].Cron != "0 4 * * 0" {
		t.Fatalf("schedules = %+v", specs)
	}
	jq := jobqueue.NewStore(s.db)
	got, err := jq.SyncSchedules(ctx, jobs.TypeArrBackup, jobs.Params{IntegrationID: it.ID}, specs)
	if err != nil || len(got) != 2 || got[0].Params.DestinationID != 5 || got[1].Params.DestinationID != 2 {
		t.Fatalf("SyncSchedules = %+v, %v", got, err)
	}
	// Deleting destination 5 drops it: targets[1] becomes the first.
	plex, err := s.Create(ctx, Input{Type: TypePlex, Name: "Plex", URL: "http://plex:32400", Settings: json.RawMessage(
		`{"dataPath":"/plex","backup":{"destinationId":5,"cron":"0 1 * * *","enabled":true}}`)})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := s.RemoveDestination(ctx, 5)
	if err != nil || !slices.Equal(changed, []int64{it.ID, plex.ID}) {
		t.Fatalf("RemoveDestination = %v, %v", changed, err)
	}
	after, _ := s.Get(ctx, it.ID)
	as, _ = after.ArrSettings()
	if as.Backup.DestinationID != 2 || len(as.Backup.Targets) != 2 {
		t.Fatalf("after the delete = %+v", as.Backup)
	}
	pAfter, _ := s.Get(ctx, plex.ID)
	ps, _ := pAfter.PlexSettings()
	if ps.Backup.DestinationID != 0 || ps.Backup.Enabled || len(ps.Backup.EffectiveTargets()) != 0 {
		t.Fatalf("Plex after the delete = %+v", ps.Backup)
	}
	if changed, err := s.RemoveDestination(ctx, 5); err != nil || len(changed) != 0 {
		t.Fatalf("second RemoveDestination = %v, %v", changed, err)
	}
	// Output: both forms, also for a stored single form and for none.
	for _, id := range []int64{it.ID, plex.ID} {
		cur, _ := s.Get(ctx, id)
		out := string(cur.WithBackupTargets().Settings)
		if !strings.Contains(out, `"targets":[`) || !strings.Contains(out, `"destinationId":`) {
			t.Fatalf("output settings = %s", out)
		}
	}
	legacy, err := s.Create(ctx, Input{Type: TypePlex, Name: "Plex 2", URL: "http://plex2:32400", Settings: json.RawMessage(
		`{"dataPath":"/plex","backup":{"destinationId":2,"cron":"0 1 * * *","enabled":true}}`)})
	if err != nil {
		t.Fatal(err)
	}
	out := string(legacy.WithBackupTargets().Settings)
	if !strings.Contains(out, `"backup":{"destinationId":2,"cron":"0 1 * * *","enabled":true,"targets":[{"destinationId":2,"cron":"0 1 * * *","enabled":true,"acceptInsecureModes":false}]}`) {
		t.Fatalf("legacy output = %s", out)
	}
}

// TestNormalizeStoredMigratesSingleBackupForm: a stored single form with a destination becomes
// the targets form at start-up; a second run changes nothing.
func TestNormalizeStoredMigratesSingleBackupForm(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	p, err := s.Create(ctx, Input{Type: TypePlex, Name: "Plex", URL: "http://plex:32400", Settings: json.RawMessage(
		`{"dataPath":"/plex","backup":{"destinationId":3,"cron":"0 1 * * *","enabled":true}}`)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(ctx, Input{Type: TypeRadarr, Name: "Radarr", URL: "http://radarr:7878", Settings: json.RawMessage(
		`{"backup":{"destinationId":4,"acceptInsecureModes":true}}`)})
	if err != nil {
		t.Fatal(err)
	}
	none, err := s.Create(ctx, Input{Type: TypeSonarr, Name: "Sonarr", URL: "http://sonarr:8989"})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.NormalizeStored(ctx)
	if err != nil || !slices.Equal(rep.Migrated, []int64{p.ID, r.ID}) || len(rep.Normalized) != 0 {
		t.Fatalf("NormalizeStored = %+v, %v", rep, err)
	}
	got, _ := s.Get(ctx, r.ID)
	as, _ := got.ArrSettings()
	if !reflect.DeepEqual(as.Backup.Targets, []BackupTarget{{DestinationID: 4, AcceptInsecureModes: true}}) || !as.Backup.AcceptInsecureModes {
		t.Fatalf("migrated arr = %+v", as.Backup)
	}
	gp, _ := s.Get(ctx, p.ID)
	ps, _ := gp.PlexSettings()
	if !reflect.DeepEqual(ps.Backup.Targets, []BackupTarget{{DestinationID: 3, Cron: "0 1 * * *", Enabled: true}}) {
		t.Fatalf("migrated plex = %+v", ps.Backup)
	}
	if gn, _ := s.Get(ctx, none.ID); strings.Contains(string(gn.Settings), "targets") {
		t.Fatalf("an integration without a backup destination got targets: %s", gn.Settings)
	}
	rep, err = s.NormalizeStored(ctx)
	if err != nil || len(rep.Migrated)+len(rep.Normalized) != 0 {
		t.Fatalf("second run = %+v, %v", rep, err)
	}
}

// TestSingleFormWithdrawsPlexInsecureModes: the Plex single form has no acceptInsecureModes, so
// an update in it withdraws the stored flag of targets[0] (the web UI sends it when the box is
// unticked) instead of carrying the consent over (S17).
func TestSingleFormWithdrawsPlexInsecureModes(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	s.SetValidateOptions(testLookup())
	plex := func(settings string) Input {
		return Input{Type: TypePlex, Name: "Plex", URL: "http://plex:32400", Settings: json.RawMessage(settings)}
	}
	unas, err := s.Create(ctx, plex(`{"dataPath":"/plex","backup":{"targets":[{"destinationId":1,"cron":"0 1 * * *","enabled":true,"acceptInsecureModes":true}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	// Unticked on the UNAS (enforcesModes false): refused, not saved with the old consent.
	_, err = s.Update(ctx, unas.ID, plex(`{"dataPath":"/plex","backup":{"destinationId":1,"cron":"0 1 * * *","enabled":true}}`))
	var verr ValidationError
	if !errors.As(err, &verr) || !strings.Contains(err.Error(), "acceptInsecureModes") {
		t.Fatalf("single-form update on the UNAS = %v, want the acceptInsecureModes ValidationError", err)
	}
	// The targets form still keeps it.
	if _, err := s.Update(ctx, unas.ID, plex(`{"dataPath":"/plex","backup":{"targets":[{"destinationId":1,"cron":"0 2 * * *","enabled":true,"acceptInsecureModes":true}]}}`)); err != nil {
		t.Fatalf("targets-form update with the flag = %v", err)
	}
	// On a destination that does not need it, the single form clears the flag and keeps targets[1:].
	disk, err := s.Create(ctx, Input{Type: TypePlex, Name: "Plex 2", URL: "http://plex2:32400", Settings: json.RawMessage(
		`{"dataPath":"/plex","backup":{"targets":[{"destinationId":5,"acceptInsecureModes":true},{"destinationId":2}]}}`)})
	if err != nil {
		t.Fatal(err)
	}
	upd, err := s.Update(ctx, disk.ID, Input{Name: "Plex 2", URL: "http://plex2:32400", Settings: json.RawMessage(
		`{"dataPath":"/plex","backup":{"destinationId":5,"cron":"0 1 * * *","enabled":true}}`)})
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := upd.PlexSettings()
	want := []BackupTarget{{DestinationID: 5, Cron: "0 1 * * *", Enabled: true}, {DestinationID: 2}}
	if !reflect.DeepEqual(ps.Backup.Targets, want) {
		t.Fatalf("merged targets = %+v, want %+v", ps.Backup.Targets, want)
	}
}
