// Package jobqueue is Bunkarr's persistent job queue and scheduler. It implements the contract in
// internal/jobs (docs/design/phase1.md §6):
//
//   - Store persists jobs, job items (it is the jobs.ItemStore handed to runners), job logs and
//     schedules (tables jobs, job_items, job_logs, schedules).
//   - Manager runs queued jobs on a bounded number of workers, serializes jobs that share a lock
//     key (one destination, one Plex server, one source), recovers jobs a crash left running,
//     re-queues jobs interrupted by a graceful shutdown, and reports progress, logs and final
//     states (OnFinish hooks).
//   - Scheduler enqueues jobs from the schedules table on their cron expressions.
//
// Every write goes through db.Write; every time is stored with db.FormatTime. Error texts, log
// messages and log fields that reach the database are passed through logging.RedactSecrets.
package jobqueue

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

var (
	// ErrNotFound means the job, item or schedule does not exist.
	ErrNotFound = errors.New("not found")
	// ErrNotActive means a job cannot be cancelled because it is not queued or running (in this
	// process).
	ErrNotActive = errors.New("job is not active")
)

// ValidationError is a user-facing input error (an invalid spec, filter or cron expression). The
// API maps it to 400.
type ValidationError string

// Error returns the message.
func (e ValidationError) Error() string { return string(e) }

// Paging defaults.
const (
	// DefaultPageSize is the page size when a query does not set one.
	DefaultPageSize = 50
	// MaxPageSize is the largest page a query may ask for.
	MaxPageSize = 500
)

// Page is one page of a list, in the paging shape of the *arr APIs.
type Page[T any] struct {
	Page         int   `json:"page"`
	PageSize     int   `json:"pageSize"`
	TotalRecords int64 `json:"totalRecords"`
	Records      []T   `json:"records"`
}

// normalizePage applies the paging defaults and bounds.
func normalizePage(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = DefaultPageSize
	}
	if size > MaxPageSize {
		size = MaxPageSize
	}
	return page, size
}

// validType reports whether t is a known job type.
func validType(t jobs.Type) bool {
	switch t {
	case jobs.TypeScan, jobs.TypeSync, jobs.TypePlexDBBackup, jobs.TypeRetention, jobs.TypeVerify:
		return true
	}
	return false
}

// validStatus reports whether s is a known job status.
func validStatus(s jobs.Status) bool {
	switch s {
	case jobs.StatusQueued, jobs.StatusRunning, jobs.StatusCompleted, jobs.StatusCompletedWithWarnings,
		jobs.StatusFailed, jobs.StatusCancelled:
		return true
	}
	return false
}

// validTrigger reports whether t is a known trigger.
func validTrigger(t jobs.Trigger) bool {
	switch t {
	case jobs.TriggerSchedule, jobs.TriggerManual, jobs.TriggerWebhook, jobs.TriggerResume, jobs.TriggerStartup:
		return true
	}
	return false
}

// validAction reports whether a is a known item action.
func validAction(a jobs.ItemAction) bool {
	switch a {
	case jobs.ActionCopy, jobs.ActionUpdate, jobs.ActionMove, jobs.ActionAdopt, jobs.ActionLink,
		jobs.ActionPromote, jobs.ActionRetain, jobs.ActionExpire, jobs.ActionVerify, jobs.ActionBackup,
		jobs.ActionSkip:
		return true
	}
	return false
}

// validItemStatus reports whether s is a known item status.
func validItemStatus(s jobs.ItemStatus) bool {
	switch s {
	case jobs.ItemPending, jobs.ItemDone, jobs.ItemFailed, jobs.ItemSkipped, jobs.ItemHeld:
		return true
	}
	return false
}

// normalizeParams returns p with SourceIDs sorted and de-duplicated (nil when empty), so equal
// selections compare and serialize equally.
func normalizeParams(p jobs.Params) jobs.Params {
	if len(p.SourceIDs) == 0 {
		p.SourceIDs = nil
		return p
	}
	ids := slices.Clone(p.SourceIDs)
	slices.Sort(ids)
	p.SourceIDs = slices.Compact(ids)
	return p
}

// canonicalParams is the stored form of p: normalized, then JSON with the struct's fixed field
// order and zero values omitted. Two specs select the same work iff their canonical params are
// equal; the jobs dedupe and the schedules (job_type, params) key rely on it.
func canonicalParams(p jobs.Params) (string, error) {
	b, err := json.Marshal(normalizeParams(p))
	if err != nil {
		return "", fmt.Errorf("encode job params: %w", err)
	}
	return string(b), nil
}

// parseParams reads a stored params column.
func parseParams(s string) (jobs.Params, error) {
	var p jobs.Params
	if s == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return p, fmt.Errorf("decode job params %q: %w", s, err)
	}
	return p, nil
}

// validateParams checks the parameters a job type needs: the lock keys (and so the
// per-destination serialization) depend on them.
func validateParams(t jobs.Type, p jobs.Params) error {
	if !validType(t) {
		return ValidationError(fmt.Sprintf("unknown job type %q", t))
	}
	if p.DestinationID < 0 || p.IntegrationID < 0 {
		return ValidationError("job params: ids must be positive")
	}
	for _, id := range p.SourceIDs {
		if id <= 0 {
			return ValidationError("job params: source ids must be positive")
		}
	}
	switch t {
	case jobs.TypeSync, jobs.TypeVerify:
		if p.DestinationID == 0 {
			return ValidationError(fmt.Sprintf("a %s job needs a destinationId", t))
		}
	case jobs.TypePlexDBBackup:
		if p.IntegrationID == 0 {
			return ValidationError("a plexdb_backup job needs an integrationId")
		}
	case jobs.TypeScan:
		if len(p.SourceIDs) == 0 {
			return ValidationError("a scan job needs at least one sourceId")
		}
	}
	return nil
}

// normalizeSpec validates spec and fills in defaults (trigger manual, normalized params).
func normalizeSpec(spec jobs.Spec) (jobs.Spec, error) {
	if spec.Trigger == "" {
		spec.Trigger = jobs.TriggerManual
	}
	if !validTrigger(spec.Trigger) {
		return spec, ValidationError(fmt.Sprintf("unknown job trigger %q", spec.Trigger))
	}
	if err := validateParams(spec.Type, spec.Params); err != nil {
		return spec, err
	}
	spec.Params = normalizeParams(spec.Params)
	return spec, nil
}

// LockKeys returns the keys a job holds while it runs (design §6.2): sync, verify and retention
// hold "dest:<id>" (a retention job without a destination holds none), plexdb_backup holds
// "plexdb:<integrationId>", scan holds "source:<id>" for each source. Two jobs sharing a key never
// run at the same time.
func LockKeys(t jobs.Type, p jobs.Params) []string {
	switch t {
	case jobs.TypeSync, jobs.TypeVerify, jobs.TypeRetention:
		if p.DestinationID != 0 {
			return []string{"dest:" + strconv.FormatInt(p.DestinationID, 10)}
		}
	case jobs.TypePlexDBBackup:
		if p.IntegrationID != 0 {
			return []string{"plexdb:" + strconv.FormatInt(p.IntegrationID, 10)}
		}
	case jobs.TypeScan:
		ids := normalizeParams(p).SourceIDs
		keys := make([]string, 0, len(ids))
		for _, id := range ids {
			keys = append(keys, "source:"+strconv.FormatInt(id, 10))
		}
		return keys
	}
	return nil
}
