package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/enginerun"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// Destinations (design §7; docs/design/phase4.md §12). Creating, updating and deleting a
// destination also manages its sync and verify schedules (and, for restic and rclone
// destinations, its retention schedule); deleting one never touches the backup data at its
// target. Off-site destinations follow S29 (offsite.go): the routes that choose where data goes
// need a UI session and the user's password. Their jobs, except dry runs, run only once the
// recovery kit custody is confirmed and the engine is available (S21, §11.1).

func (s *Server) destinationRoutes(r chi.Router) {
	r.Get("/destinations", s.listDestinations)
	r.Post("/destinations", s.createDestination)
	r.Post("/destinations/test", s.testTarget)
	r.Post("/destinations/sftp/hostkeys", s.sftpHostKeys)
	r.Get("/destinations/{id}", s.getDestination)
	r.Put("/destinations/{id}", s.updateDestination)
	r.Delete("/destinations/{id}", s.deleteDestination)
	r.Post("/destinations/{id}/test", s.testDestination)
	r.Post("/destinations/{id}/sync", s.syncDestination)
	r.Post("/destinations/{id}/verify", s.verifyDestination)
	r.Post("/destinations/{id}/retention", s.retentionDestination)
	r.Post("/destinations/{id}/unlock", s.unlockDestination)
	r.Post("/destinations/{id}/recovery-kit", s.exportRecoveryKit)
	r.Post("/destinations/{id}/recovery-kit/confirm", s.confirmRecoveryKit)
	r.Get("/destinations/{id}/snapshots", s.destinationSnapshots)
}

// destinationView is the API's Destination: the stored destination plus its schedules, the
// most recent job that worked on it (any type) and its most recent sync, and for restic and
// rclone destinations their engine's version and state.
type destinationView struct {
	destinations.Destination
	Schedule       cronSchedule `json:"schedule"`
	VerifySchedule cronSchedule `json:"verifySchedule"`
	// RetentionSchedule is a restic or rclone destination's own retention schedule (created with
	// it, listed in System → Tasks); nil for filecopy destinations, whose retention the global
	// retention job queues.
	RetentionSchedule *cronSchedule `json:"retentionSchedule,omitempty"`
	LastJob           *jobs.Job     `json:"lastJob"`
	// LastSync is the newest sync job (queued, running or finished; previews excluded): the
	// "last sync status" of design §10, which later verify, retention and Plex DB backup jobs
	// must not hide.
	LastSync *jobs.Job `json:"lastSync"`
	// EngineVersion is the installed version of a restic or rclone destination's engine ("" for
	// filecopy, or when the engine is not available).
	EngineVersion string `json:"engineVersion,omitempty"`
	// EngineState is a restic or rclone destination's engine state: repository or remote figures,
	// the last prune, check and cleanup, the measured throughput (nil for filecopy).
	EngineState *enginerun.EngineState `json:"engineState,omitempty"`
	// WaitingUntil is when a job of the destination that waits for its transfer window starts
	// again (the earliest not_before of its deferred jobs); nil when none waits.
	WaitingUntil *time.Time `json:"waitingUntil"`
}

// lastSyncPageSize is how many sync jobs lastSync reads per page while it skips previews.
const lastSyncPageSize = 20

func (s *Server) destinationView(ctx context.Context, scheds []jobqueue.Schedule, d destinations.Destination) (destinationView, error) {
	v := destinationView{Destination: d, Schedule: scheduleOf(scheds, jobs.TypeSync, d.ID), VerifySchedule: scheduleOf(scheds, jobs.TypeVerify, d.ID)}
	// blockedReason also says when the engine is not available on this server (§11.1).
	v.BlockedReason = s.app.engineBlock(d)
	if d.IsEngine() {
		rs := scheduleOf(scheds, jobs.TypeRetention, d.ID)
		v.RetentionSchedule = &rs
		v.EngineVersion = s.app.engines.availability.Of(engines.Kind(d.Engine)).Version
		st, err := s.app.Engine.State(ctx, d.ID)
		if err != nil {
			return v, err
		}
		v.EngineState = &st
	}
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
	if v.WaitingUntil, err = s.waitingUntil(ctx, d.ID); err != nil {
		return v, err
	}
	return v, nil
}

// waitingUntil returns the earliest not_before of the queued jobs of destination destID that a
// transfer window deferred, or nil (phase4.md §9.2, §15: "Waiting for the window until 01:00").
func (s *Server) waitingUntil(ctx context.Context, destID int64) (*time.Time, error) {
	var out *time.Time
	for p := 1; ; p++ {
		page, err := s.app.Jobs.List(ctx, jobqueue.JobQuery{State: jobqueue.StateActive, Status: jobs.StatusQueued, DestinationID: destID,
			Page: p, PageSize: jobqueue.MaxPageSize})
		if err != nil {
			return nil, err
		}
		for _, j := range page.Records {
			if j.NotBefore != nil && (out == nil || j.NotBefore.Before(*out)) {
				t := j.NotBefore.UTC()
				out = &t
			}
		}
		if len(page.Records) < jobqueue.MaxPageSize || int64(p*jobqueue.MaxPageSize) >= page.TotalRecords {
			return out, nil
		}
	}
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
// schedules, on create only the confirmations of safety rule S3, and the user's password for the
// changes of S29.
type destinationBody struct {
	destinations.Input
	// Schedule is the sync schedule. Create: nil means none (syncs run when started, until one
	// is chosen). Update: nil keeps it. {cron: "", enabled: false} removes it.
	Schedule *cronSchedule `json:"schedule"`
	// VerifySchedule is the verify schedule. Create: nil means DefaultVerifyCron, enabled.
	// Update: nil keeps it. {cron: "", enabled: false} removes it.
	VerifySchedule *cronSchedule `json:"verifySchedule"`
	// RetentionSchedule is a restic or rclone destination's retention schedule. Create: nil means
	// DefaultEngineRetentionCron, enabled. Update: nil keeps it. {cron: "", enabled: false}
	// removes it. Filecopy destinations have none (the global retention job covers them).
	RetentionSchedule *cronSchedule `json:"retentionSchedule"`
	// Attach and AllowLocal are destinations.CreateOptions (create only).
	Attach     bool `json:"attach"`
	AllowLocal bool `json:"allowLocal"`
	// CurrentPassword is the user's password, required (with a UI session) by the changes of S29.
	CurrentPassword string `json:"currentPassword"`
}

func (b *destinationBody) validateSchedules() error {
	if err := validateSchedule("schedule", b.Schedule); err != nil {
		return err
	}
	if err := validateSchedule("verifySchedule", b.VerifySchedule); err != nil {
		return err
	}
	return validateSchedule("retentionSchedule", b.RetentionSchedule)
}

// scheduleChanges are the schedules a create or update applies, in order.
func (b *destinationBody) scheduleChanges(engine bool) []struct {
	t  jobs.Type
	sc *cronSchedule
} {
	out := []struct {
		t  jobs.Type
		sc *cronSchedule
	}{{jobs.TypeSync, b.Schedule}, {jobs.TypeVerify, b.VerifySchedule}}
	if engine {
		out = append(out, struct {
			t  jobs.Type
			sc *cronSchedule
		}{jobs.TypeRetention, b.RetentionSchedule})
	}
	return out
}

// isEngineInput reports whether a create body describes a restic or rclone destination.
func isEngineInput(in destinations.Input) bool {
	return (in.Kind != "" && in.Kind != engines.Local) || in.Engine == destinations.EngineRestic || in.Engine == destinations.EngineRclone
}

func (s *Server) createDestination(w http.ResponseWriter, r *http.Request) {
	const action = "create destination"
	var body destinationBody
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, action, err)
		return
	}
	if err := body.validateSchedules(); err != nil {
		s.fail(w, r, action, err)
		return
	}
	engine := isEngineInput(body.Input)
	if !engine && body.RetentionSchedule != nil {
		s.fail(w, r, action, errorf(http.StatusBadRequest,
			"retentionSchedule applies to restic and rclone destinations only (the global retention job covers filecopy destinations)"))
		return
	}
	local := body.Kind == "" || body.Kind == engines.Local
	if local && strings.TrimSpace(body.Target) == "" {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "target is required: the absolute path of the mounted share"))
		return
	}
	// S29: an off-site kind, or accepting no encryption, needs a session and the password; checked
	// before anything reaches an engine.
	if !s.offsite(w, r, createNeedsFreshPassword(body.Input), body.CurrentPassword, action) {
		return
	}
	if reason := s.app.resticRcloneBlock(body.Engine, body.Kind); reason != "" {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "%s", reason))
		return
	}
	if body.VerifySchedule == nil {
		body.VerifySchedule = &cronSchedule{Cron: DefaultVerifyCron, Enabled: true}
	}
	if engine && body.RetentionSchedule == nil {
		body.RetentionSchedule = &cronSchedule{Cron: DefaultEngineRetentionCron, Enabled: true}
	}
	ctx := r.Context()
	d, err := s.app.Destinations.Create(ctx, body.Input, destinations.CreateOptions{Attach: body.Attach, AllowLocal: body.AllowLocal})
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			err = errorf(http.StatusBadRequest, "%v (check PUID/PGID and the share's permissions)", err)
		}
		s.fail(w, r, action, err)
		return
	}
	s.log.Info("Destination created", "id", d.ID, "name", d.Name, "kind", string(d.Kind), "engine", d.Engine, "target", d.Target,
		"fsType", d.FSType, "attach", body.Attach, "allowLocal", body.AllowLocal, "encryption", string(d.Encryption.Mode))
	changed := false
	for _, sc := range body.scheduleChanges(d.IsEngine()) {
		c, err := s.applyDestinationSchedule(ctx, sc.t, d.ID, sc.sc)
		if err != nil {
			s.fail(w, r, action, errScheduleSync("the destination", err))
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
	const action = "update destination"
	cur, err := s.destination(r)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	var body destinationBody
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, action, err)
		return
	}
	if body.Attach || body.AllowLocal {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "attach and allowLocal only apply when a destination is created"))
		return
	}
	if err := body.validateSchedules(); err != nil {
		s.fail(w, r, action, err)
		return
	}
	if !cur.IsEngine() && body.RetentionSchedule != nil {
		s.fail(w, r, action, errorf(http.StatusBadRequest,
			"retentionSchedule applies to restic and rclone destinations only (the global retention job covers filecopy destinations)"))
		return
	}
	// S29: new credentials or pinned keys, a new source on an off-site destination.
	if !s.offsite(w, r, updateNeedsFreshPassword(cur, body.Input), body.CurrentPassword, action) {
		return
	}
	ctx := r.Context()
	d, err := s.app.Destinations.Update(ctx, cur.ID, body.Input)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	s.log.Info("Destination updated", "id", d.ID, "name", d.Name, "enabled", d.Enabled, "credentialsChanged", body.Credentials != nil)
	changed := false
	for _, sc := range body.scheduleChanges(d.IsEngine()) {
		c, err := s.applyDestinationSchedule(ctx, sc.t, d.ID, sc.sc)
		if err != nil {
			s.fail(w, r, action, errScheduleSync("the destination", err))
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
// schedules, and removes it from the backup targets of every Plex and *arr integration. Nothing
// at the target is touched. A destination with queued or running jobs cannot be deleted, nor one
// whose encryption secret's recovery kit custody was never confirmed without
// ?confirmLoseSecret=true (409, S21: the secret is deleted with the row).
func (s *Server) deleteDestination(w http.ResponseWriter, r *http.Request) {
	const action = "delete destination"
	d, err := s.destination(r)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	confirm, err := boolParam(r, "confirmLoseSecret")
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	ctx := r.Context()
	active, err := s.app.Jobs.List(ctx, jobqueue.JobQuery{State: jobqueue.StateActive, DestinationID: d.ID, PageSize: 1})
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	if active.TotalRecords > 0 {
		s.fail(w, r, action, errorf(http.StatusConflict,
			"destination %q has queued or running jobs (job %d); cancel them or wait until they finish", d.Name, active.Records[0].ID))
		return
	}
	if err := s.app.Destinations.Delete(ctx, d.ID, destinations.DeleteOptions{ConfirmLoseSecret: confirm}); err != nil {
		if errors.Is(err, destinations.ErrSecretNotConfirmed) {
			err = errorf(http.StatusConflict, "%v", err)
		}
		s.fail(w, r, action, err)
		return
	}
	// Its restic cache is Bunkarr's own copy of repository metadata, not backup data.
	s.app.removeResticCache(d)
	// Its id leaves every tier rule's "Applies at" (an emptied list applies nowhere, never
	// everywhere; phase2-3.md §8.7).
	if err := s.app.Tiers.Store().RemoveDestination(ctx, d.ID); err != nil {
		s.fail(w, r, action, err)
		return
	}
	n, err := s.app.Jobs.Store().DeleteSchedulesFor(ctx, jobs.Params{DestinationID: d.ID})
	if err != nil {
		s.fail(w, r, action, errScheduleSync("the deletion", err))
		return
	}
	if n > 0 {
		s.reloadSchedules(ctx)
	}
	if err := s.removeBackupTargets(ctx, d.ID); err != nil {
		s.fail(w, r, action, err)
		return
	}
	s.log.Info("Destination deleted (its backup data was not touched)", "id", d.ID, "name", d.Name, "target", d.Target, "schedulesRemoved", n,
		"secretDeleted", d.HasSecret())
	w.WriteHeader(http.StatusNoContent)
}

// boolParam reads an optional boolean query parameter (false when absent).
func boolParam(r *http.Request, name string) (bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errorf(http.StatusBadRequest, "%s must be true or false", name)
	}
	return v, nil
}

// removeBackupTargets removes destination destID (just deleted) from the backup targets of every
// Plex and *arr integration (phase4.md §8.5), so their settings do not name a destination that is
// gone (a Plex or *arr backup whose only target it was is left without one, turned off), and
// mirrors the changed integrations' schedules.
func (s *Server) removeBackupTargets(ctx context.Context, destID int64) error {
	ids, err := s.app.Integrations.RemoveDestination(ctx, destID)
	if err != nil {
		return errScheduleSync("the deletion", err)
	}
	for _, id := range ids {
		it, err := s.app.Integrations.Get(ctx, id)
		if err != nil {
			return errScheduleSync("the deletion", err)
		}
		if err := s.syncBackupSchedules(ctx, it); err != nil {
			return errScheduleSync("the deletion", err)
		}
		s.log.Info("Backup target removed: its destination was deleted", "integration", it.Name, "type", string(it.Type), "destinationId", destID)
	}
	return nil
}

// testTarget is POST /destinations/test: a filecopy target ({target}), or the connection of a
// restic or rclone destination to create ({kind, engine, target, remote, credentials,
// encryption}). It never takes a destination id and never uses a stored secret (§4.5).
func (s *Server) testTarget(w http.ResponseWriter, r *http.Request) {
	const action = "test destination target"
	var body destinations.TestInput
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, action, err)
		return
	}
	if isEngineInput(destinations.Input{Kind: body.Kind, Engine: body.Engine}) {
		if reason := s.app.resticRcloneBlock(body.Engine, body.Kind); reason != "" {
			s.fail(w, r, action, errorf(http.StatusBadRequest, "%s", reason))
			return
		}
		res, err := s.app.Destinations.TestRemote(r.Context(), body)
		if err != nil {
			s.fail(w, r, action, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	if len(bytes.TrimSpace(body.Remote)) > 0 || body.Credentials != nil || body.Encryption != nil {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "a filecopy target is tested with its target only"))
		return
	}
	if body.Engine != "" && body.Engine != destinations.EngineFilecopy {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "unknown engine %q: use filecopy, restic or rclone", body.Engine))
		return
	}
	if strings.TrimSpace(body.Target) == "" {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "target is required"))
		return
	}
	res, err := s.app.Destinations.Test(r.Context(), body.Target, 0)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// connectionFields are the fields POST /destinations/{id}/test refuses for a restic or rclone
// destination: it tests the stored location with the stored secrets only (§4.5).
var connectionFields = []string{"kind", "engine", "target", "remote", "credentials", "encryption"}

// msgTestWithoutConnection is the 400 of POST /destinations/{id}/test with connection fields.
const msgTestWithoutConnection = "test an engine destination without connection fields"

func (s *Server) testDestination(w http.ResponseWriter, r *http.Request) {
	const action = "test destination"
	d, err := s.destination(r)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	if !d.IsEngine() {
		var body struct{}
		if err := decodeOptionalBody(w, r, &body); err != nil {
			s.fail(w, r, action, err)
			return
		}
		res, err := s.app.Destinations.Test(r.Context(), "", d.ID)
		if err != nil {
			s.fail(w, r, action, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	var body map[string]json.RawMessage
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, action, err)
		return
	}
	for k := range body {
		if slices.Contains(connectionFields, k) {
			s.fail(w, r, action, errorf(http.StatusBadRequest, "%s", msgTestWithoutConnection))
			return
		}
		s.fail(w, r, action, errorf(http.StatusBadRequest, "invalid JSON body: json: unknown field %q", k))
		return
	}
	if reason := s.app.resticRcloneBlock(d.Engine, d.Kind); reason != "" {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "%s", reason))
		return
	}
	res, err := s.app.Destinations.TestStored(r.Context(), d.ID)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// sftpHostKeys is POST /destinations/sftp/hostkeys {host, port} → [{type, fingerprint, key}]: the
// keys an SFTP server presents, for the user to confirm and pin (§4.6). It connects through the
// outbound guard and never authenticates.
func (s *Server) sftpHostKeys(w http.ResponseWriter, r *http.Request) {
	const action = "read SFTP host keys"
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, action, err)
		return
	}
	host := strings.TrimSpace(body.Host)
	if host == "" {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "host is required"))
		return
	}
	if body.Port == 0 {
		body.Port = 22
	}
	if body.Port < 1 || body.Port > 65535 {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "port must be from 1 to 65535"))
		return
	}
	keys, err := s.app.Destinations.ScanHostKeys(r.Context(), host, body.Port)
	if err != nil {
		if statusOf(err) == http.StatusInternalServerError {
			err = errorf(http.StatusBadGateway, "could not read the host keys of %s: %v", host, err)
		}
		s.fail(w, r, action, err)
		return
	}
	if keys == nil {
		keys = []engines.HostKeyInfo{}
	}
	writeJSON(w, http.StatusOK, keys)
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
	var body struct {
		DryRun       bool `json:"dryRun"`
		AllowChanges bool `json:"allowChanges"`
		releaseParams
	}
	d, err := s.enabledDestination(r)
	if err != nil {
		s.fail(w, r, "start sync", err)
		return
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "start sync", err)
		return
	}
	if err := s.app.checkRunnable(d, body.DryRun); err != nil {
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

// verifyDestination is POST /destinations/{id}/verify {dryRun, readData} → 202 Job. readData
// reads everything once (phase4.md §6.6, §7.6).
func (s *Server) verifyDestination(w http.ResponseWriter, r *http.Request) {
	d, err := s.enabledDestination(r)
	if err != nil {
		s.fail(w, r, "start verify", err)
		return
	}
	var body struct {
		DryRun   bool `json:"dryRun"`
		ReadData bool `json:"readData"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "start verify", err)
		return
	}
	if err := s.app.checkRunnable(d, body.DryRun); err != nil {
		s.fail(w, r, "start verify", err)
		return
	}
	job, err := s.app.Jobs.Enqueue(r.Context(), jobs.Spec{Type: jobs.TypeVerify, Trigger: jobs.TriggerManual, DryRun: body.DryRun,
		Params: jobs.Params{DestinationID: d.ID, ReadData: body.ReadData}})
	if err != nil {
		s.fail(w, r, "start verify", err)
		return
	}
	writeAccepted(w, job.ID, job)
}

// retentionDestination is POST /destinations/{id}/retention {dryRun, prune} → 202 Job: the
// destination's retention now (phase4.md §6.5, §7.5, §11.1); prune makes a restic destination
// prune its repository in this run.
func (s *Server) retentionDestination(w http.ResponseWriter, r *http.Request) {
	const action = "start retention"
	d, err := s.enabledDestination(r)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	var body struct {
		DryRun bool `json:"dryRun"`
		Prune  bool `json:"prune"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, action, err)
		return
	}
	if err := s.app.checkRunnable(d, body.DryRun); err != nil {
		s.fail(w, r, action, err)
		return
	}
	if body.Prune && d.Engine != destinations.EngineRestic {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "prune applies to restic destinations only"))
		return
	}
	job, err := s.app.Jobs.Enqueue(r.Context(), jobs.Spec{Type: jobs.TypeRetention, Trigger: jobs.TriggerManual, DryRun: body.DryRun,
		Params: jobs.Params{DestinationID: d.ID, Prune: body.Prune}})
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	writeAccepted(w, job.ID, job)
}

// unlockDestination is POST /destinations/{id}/unlock {removeAll} → 204: removes the stale locks
// of a restic repository (§6.7), or with removeAll every lock (restic unlock --remove-all, the
// user's explicit decision). It is refused (409) while any job of the destination is running or
// deferred, of any type (the Plex DB, *arr and manifest jobs hold no dest:<id> key but run restic
// there), and holds dest:<id> itself while it runs.
func (s *Server) unlockDestination(w http.ResponseWriter, r *http.Request) {
	const action = "unlock destination"
	d, err := s.destination(r)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	var body struct {
		RemoveAll bool `json:"removeAll"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, action, err)
		return
	}
	if d.Engine != destinations.EngineRestic {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "only restic destinations have locks"))
		return
	}
	ctx := r.Context()
	busy := func() error {
		active, err := s.app.Jobs.Store().ActiveForDestinationAnyType(ctx, d.ID)
		if err != nil {
			return err
		}
		if active {
			return errorf(http.StatusConflict, "a job of destination %q is running or waiting for its transfer window; remove locks when it has finished", d.Name)
		}
		return nil
	}
	if err := busy(); err != nil {
		s.fail(w, r, action, err)
		return
	}
	release, err := s.app.Jobs.HoldKeys(ctx, "dest:"+strconv.FormatInt(d.ID, 10))
	if errors.Is(err, jobqueue.ErrBusy) {
		err = errorf(http.StatusConflict, "a job of destination %q is running; remove locks when it has finished", d.Name)
	}
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	defer release()
	// The Plex DB, *arr and manifest jobs hold no dest:<id> key: their version stores wait at the
	// destination's version gate until the unlock has finished, so none of their restic commands
	// takes a lock that --remove-all would remove (versionGates).
	unlockDone, ok := s.app.engines.gates.lockOut(d.ID)
	if !ok {
		s.fail(w, r, action, errorf(http.StatusConflict, "a backup of destination %q is using its repository; remove locks when it has finished", d.Name))
		return
	}
	defer unlockDone()
	// A config version job that started in between still runs restic here.
	if err := busy(); err != nil {
		s.fail(w, r, action, err)
		return
	}
	if err := s.app.Engine.Unlock(ctx, d.ID, body.RemoveAll); err != nil {
		if statusOf(err) == http.StatusInternalServerError && ctx.Err() == nil {
			err = errorf(http.StatusBadGateway, "restic unlock failed: %v", err)
		}
		s.fail(w, r, action, err)
		return
	}
	s.log.Warn("Restic locks removed", "destinationId", d.ID, "destination", d.Name, "removeAll", body.RemoveAll)
	w.WriteHeader(http.StatusNoContent)
}

// snapshotView is GET /destinations/{id}/snapshots' item: a Plex DB or *arr version (a snapshots
// row, as in Phases 1-3), or on a restic destination a media snapshot of one source and batch
// (kind "media", phase4.md §12), whose extra fields are set only for it.
type snapshotView struct {
	snapshots.Snapshot
	SourceID  int64  `json:"sourceId,omitempty"`
	Batch     int    `json:"batch,omitempty"`
	Complete  *bool  `json:"complete,omitempty"`
	Files     *int64 `json:"files,omitempty"`
	DataAdded *int64 `json:"dataAdded,omitempty"`
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
	out := make([]snapshotView, 0, len(list))
	for _, sn := range list {
		out = append(out, snapshotView{Snapshot: sn})
	}
	if d.IsEngine() {
		media, err := s.app.Engine.List(r.Context(), d.ID)
		if err != nil {
			s.fail(w, r, "list snapshots", err)
			return
		}
		for _, v := range media {
			complete, files, added := v.Complete, v.Files, v.DataAdded
			out = append(out, snapshotView{Snapshot: snapshots.Snapshot{DestinationID: d.ID, Kind: snapshots.Kind(engines.VersionMedia),
				JobID: v.JobID, CreatedAt: v.Time, Size: v.Bytes, EngineRef: string(v.Ref)},
				SourceID: v.SourceID, Batch: v.Batch, Complete: &complete, Files: &files, DataAdded: &added})
		}
		// Newest first, as the snapshots store lists its rows.
		slices.SortStableFunc(out, func(a, b snapshotView) int {
			if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
				return c
			}
			return cmp.Compare(b.ID, a.ID)
		})
	}
	writeJSON(w, http.StatusOK, out)
}
