// Package jobqueue is Bunkarr's persistent job queue and scheduler. It implements the contract in
// internal/jobs (docs/design/phase1.md §6, phase2-3.md §12):
//
//   - Store persists jobs, job items (it is the jobs.ItemStore handed to runners), job logs and
//     schedules (tables jobs, job_items, job_logs, schedules). Its CreateJob validates and
//     canonicalizes a spec, and in one transaction returns an identical queued job, coalesces a
//     targeted spec with queued jobs (phase2-3.md §12.2) or inserts a new job.
//   - Manager runs queued jobs on a bounded number of workers, plus a separate pool for refresh
//     jobs, serializes jobs that share a lock key (one destination, one Plex server, one source,
//     one integration's index, one integration's *arr backups, one destination's manifests),
//     recovers jobs a crash left running, re-queues jobs interrupted by a graceful shutdown, and
//     reports progress, logs and final states (OnFinish hooks).
//   - Scheduler enqueues jobs from the schedules table on their cron expressions.
//
// Every write goes through db.Write; every time is stored with db.FormatTime. Error texts, log
// messages and log fields that reach the database are passed through logging.RedactSecrets.
package jobqueue

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
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
	case jobs.TypeScan, jobs.TypeSync, jobs.TypePlexDBBackup, jobs.TypeRetention, jobs.TypeVerify,
		jobs.TypeRefresh, jobs.TypeArrBackup, jobs.TypeManifestExport:
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

// normalizeParams returns p in canonical form, so equal selections compare and serialize
// equally: SourceIDs and ArrItemIDs sorted and de-duplicated, Paths sorted and de-duplicated with
// every path inside another listed path dropped (phase2-3.md §12.1); empty lists become nil.
func normalizeParams(p jobs.Params) jobs.Params {
	p.SourceIDs = sortedIDs(p.SourceIDs)
	p.ArrItemIDs = sortedIDs(p.ArrItemIDs)
	p.Paths = canonicalPaths(p.Paths)
	return p
}

// sortedIDs returns ids sorted and de-duplicated, nil when empty.
func sortedIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// canonicalPaths returns ps sorted and de-duplicated, without the paths that lie inside another
// listed path ("Show/Season 1" is dropped next to "Show"), nil when empty. The paths are expected
// to satisfy jobs.ValidTargetPath.
func canonicalPaths(ps []string) []string {
	if len(ps) == 0 {
		return nil
	}
	set := make(map[string]bool, len(ps))
	for _, p := range ps {
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		if !insideAny(p, set) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// insideAny reports whether a proper ancestor directory of p is in set.
func insideAny(p string, set map[string]bool) bool {
	for d := path.Dir(p); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if set[d] {
			return true
		}
	}
	return false
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

// validateParams checks the parameters a job type needs (phase1.md §6.2, phase2-3.md §12.1): the
// lock keys (and so the serialization) depend on them. dryRun is the spec's (a schedule is a real
// run). The targeting and release params are refused on the types they do not apply to.
func validateParams(t jobs.Type, p jobs.Params, dryRun bool) error {
	if !validType(t) {
		return ValidationError(fmt.Sprintf("unknown job type %q", t))
	}
	if p.DestinationID < 0 || p.IntegrationID < 0 || p.ReleaseOf < 0 || p.ReleaseRevision < 0 {
		return ValidationError("job params: ids must be positive")
	}
	for _, id := range p.SourceIDs {
		if id <= 0 {
			return ValidationError("job params: source ids must be positive")
		}
	}
	if t != jobs.TypeSync && (len(p.Paths) > 0 || p.ReleaseDemoted || p.ReleaseOf != 0 || p.ReleaseRevision != 0) {
		return ValidationError(fmt.Sprintf("a %s job takes no paths or release params", t))
	}
	if t != jobs.TypeRefresh && (len(p.ArrItemIDs) > 0 || p.SyncAfter) {
		return ValidationError(fmt.Sprintf("a %s job takes no arrItemIds or syncAfter", t))
	}
	switch t {
	case jobs.TypeSync:
		if p.DestinationID == 0 {
			return ValidationError("a sync job needs a destinationId")
		}
		return validateSyncTargets(p, dryRun)
	case jobs.TypeVerify, jobs.TypeManifestExport:
		if p.DestinationID == 0 {
			return ValidationError(fmt.Sprintf("a %s job needs a destinationId", t))
		}
	case jobs.TypePlexDBBackup, jobs.TypeArrBackup:
		if p.IntegrationID == 0 {
			return ValidationError(fmt.Sprintf("a %s job needs an integrationId", t))
		}
	case jobs.TypeRefresh:
		if p.IntegrationID == 0 {
			return ValidationError("a refresh job needs an integrationId")
		}
		for _, id := range p.ArrItemIDs {
			if id <= 0 {
				return ValidationError("job params: arrItemIds must be positive")
			}
		}
		if n := len(sortedIDs(p.ArrItemIDs)); n > jobs.MaxTargetItems {
			return ValidationError(fmt.Sprintf("a refresh job takes at most %d arrItemIds (%d given)", jobs.MaxTargetItems, n))
		}
		if p.SyncAfter && len(p.ArrItemIDs) == 0 {
			// Only an overflow merge makes an untargeted refresh with syncAfter (phase2-3.md §12.2).
			return ValidationError("syncAfter needs arrItemIds")
		}
	case jobs.TypeScan:
		if len(p.SourceIDs) == 0 {
			return ValidationError("a scan job needs at least one sourceId")
		}
	}
	return nil
}

// validateSyncTargets checks a sync's paths and release params (phase2-3.md §12.1).
func validateSyncTargets(p jobs.Params, dryRun bool) error {
	if len(p.Paths) > 0 {
		if len(sortedIDs(p.SourceIDs)) != 1 {
			return ValidationError("a sync with paths needs exactly one sourceId")
		}
		for _, tp := range p.Paths {
			if !jobs.ValidTargetPath(tp) {
				return ValidationError(fmt.Sprintf("invalid path %q: a clean, relative path inside the source is required", tp))
			}
		}
		if n := len(canonicalPaths(p.Paths)); n > jobs.MaxTargetPaths {
			return ValidationError(fmt.Sprintf("a sync takes at most %d paths (%d given)", jobs.MaxTargetPaths, n))
		}
		if p.ReleaseDemoted {
			return ValidationError("releaseDemoted cannot be combined with paths")
		}
	}
	if !p.ReleaseDemoted && (p.ReleaseOf != 0 || p.ReleaseRevision != 0) {
		return ValidationError("releaseOf and releaseRevision need releaseDemoted")
	}
	if p.ReleaseDemoted && !dryRun && (p.ReleaseOf == 0 || p.ReleaseRevision == 0) {
		return ValidationError("a release needs releaseOf (its dry run) and releaseRevision")
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
	if err := validateParams(spec.Type, spec.Params, spec.DryRun); err != nil {
		return spec, err
	}
	spec.Params = normalizeParams(spec.Params)
	return spec, nil
}

// resumesStoredPlan reports whether a resumed job of type t executes the plan it stored
// (jobs.planned_at set) without planning again (phase1.md §4.1 step 3: internal/syncer sync, verify
// and retention). plexdb_backup and arr_backup also set planned_at when they record items, but a
// resumed one starts over or follows its recorded Backup command, so it is not stale.
func resumesStoredPlan(t jobs.Type) bool {
	switch t {
	case jobs.TypeSync, jobs.TypeVerify, jobs.TypeRetention:
		return true
	}
	return false
}

// ManifestBuildKey is held by every manifest_export job, so manifest builds of all destinations
// take turns (a build holds the whole library in memory, internal/manifest). A second export
// waits in the queue for it rather than parked in a general worker.
const ManifestBuildKey = "manifest-build"

// LockKeys returns the keys a job holds while it runs (phase1.md §6.2, phase2-3.md §12.1): sync,
// verify and retention hold "dest:<id>" (a retention job without a destination holds none),
// plexdb_backup holds "plexdb:<integrationId>", scan holds "source:<id>" for each source, refresh
// holds "integration:<id>", arr_backup "arrbackup:<integrationId>" and manifest_export
// "manifest:<destinationId>" plus ManifestBuildKey. Two jobs sharing a key never run at the same
// time. (A sync also takes "source:<id>" while it scans; the syncer does that itself,
// internal/catalog.)
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
	case jobs.TypeRefresh:
		if p.IntegrationID != 0 {
			return []string{"integration:" + strconv.FormatInt(p.IntegrationID, 10)}
		}
	case jobs.TypeArrBackup:
		if p.IntegrationID != 0 {
			return []string{"arrbackup:" + strconv.FormatInt(p.IntegrationID, 10)}
		}
	case jobs.TypeManifestExport:
		if p.DestinationID != 0 {
			return []string{"manifest:" + strconv.FormatInt(p.DestinationID, 10), ManifestBuildKey}
		}
	case jobs.TypeScan:
		ids := sortedIDs(p.SourceIDs)
		keys := make([]string, 0, len(ids))
		for _, id := range ids {
			keys = append(keys, "source:"+strconv.FormatInt(id, 10))
		}
		return keys
	}
	return nil
}
