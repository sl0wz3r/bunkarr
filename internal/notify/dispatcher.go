package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

// Dispatcher defaults.
const (
	// DefaultWorkers is the number of goroutines sending notifications.
	DefaultWorkers = 2
	// DefaultQueueSize is how many finished jobs may wait for a worker before Handle drops them.
	DefaultQueueSize = 100
	// describeTimeout bounds Options.Describe (it may query the database).
	describeTimeout = 10 * time.Second
	// maxDescribeLen and maxTextLen bound the title's description and the body's summary and
	// error, in characters.
	maxDescribeLen = 200
	maxTextLen     = 2000
	// quietInterval is how often a limited job (see limitKey) may notify a warning, and a failure,
	// for one integration or destination while its problem lasts (docs/design/phase2-3.md §12.5).
	quietInterval = 24 * time.Hour
	// clockSlack is how far a job's finish time may precede the last notified one of its key and
	// still be a hook that ran out of order; further back, the clock was set back (see admit).
	clockSlack = 5 * time.Minute
)

// Options configures a Dispatcher.
type Options struct {
	// Describe names what a job worked on for the title, e.g. "Sync to UNAS". Optional: nil or
	// "" falls back to the job type ("Sync"). It runs on a worker goroutine, never in Handle.
	Describe func(ctx context.Context, job jobs.Job) string
	// BaseURL is the UI's external URL (e.g. "http://tower:8484"); when set, messages link to
	// the job's page.
	BaseURL string
	// Workers and QueueSize bound the background work (defaults DefaultWorkers and
	// DefaultQueueSize).
	Workers   int
	QueueSize int
	// Client sends the messages (default NewClient()).
	Client *Client
}

// Dispatcher turns finished jobs into Apprise notifications. Register Handle with the job
// manager's OnFinish; call Close on shutdown.
type Dispatcher struct {
	store    *Store
	log      *slog.Logger
	client   *Client
	describe func(ctx context.Context, job jobs.Job) string
	baseURL  string

	ctx    context.Context // cancelled by Close when its deadline passes
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.RWMutex // guards closed and sending on queue
	closed bool
	queue  chan queued

	now      func() time.Time // the clock for jobs without FinishedAt (a variable for tests)
	limitMu  sync.Mutex       // guards notified and lastSlotID
	notified map[slotKey]slot // the §12.5 limits: the last problem notified per key and type
	// lastSlotID numbers the slots admit takes, so a release only undoes its own.
	lastSlotID uint64
	// pending counts the queued jobs a worker has not finished; tests wait on it.
	pending sync.WaitGroup
}

// queued is a job waiting for a worker, with the §12.5 slot admit took for it (nil: none).
type queued struct {
	job jobs.Job
	res *reservation
}

// New starts a Dispatcher's workers.
func New(store *Store, log *slog.Logger, opts Options) *Dispatcher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.Client == nil {
		opts.Client = NewClient()
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{
		store:    store,
		log:      log,
		client:   opts.Client,
		describe: opts.Describe,
		baseURL:  strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		ctx:      ctx,
		cancel:   cancel,
		queue:    make(chan queued, opts.QueueSize),
		now:      time.Now,
		notified: make(map[slotKey]slot),
	}
	for range opts.Workers {
		d.wg.Go(d.worker)
	}
	return d
}

// event is what a finished job notifies.
type event struct {
	typ  MessageType
	verb string
}

// eventFor maps a finished job to its notification (design §7): failed → failure, also for dry
// runs; completed with warnings → warning and completed → success, not for dry runs; cancelled
// and unfinished jobs → none. Jobs that limitKey limits never send success
// (docs/design/phase2-3.md §12.5). Whether a target receives it depends on its OnFailure,
// OnWarning and OnSuccess.
func eventFor(job jobs.Job) (event, bool) {
	switch job.Status {
	case jobs.StatusFailed:
		return event{TypeFailure, "failed"}, true
	case jobs.StatusCompletedWithWarnings:
		if !job.DryRun {
			return event{TypeWarning, "completed with warnings"}, true
		}
	case jobs.StatusCompleted:
		if _, limited := limitKey(job); !job.DryRun && !limited {
			return event{TypeSuccess, "completed"}, true
		}
	}
	return event{}, false
}

// problemKey names the integration or destination a job's §12.5 limits are per: a refresh per
// integration, a manifest export and a sync per destination. Every sync has one (not only the
// limited follow-up syncs), so that a clean untargeted sync ends a follow-up sync's problem. ok is
// false for the other job types.
func problemKey(job jobs.Job) (key string, ok bool) {
	switch job.Type {
	case jobs.TypeRefresh:
		return "refresh:" + strconv.FormatInt(job.Params.IntegrationID, 10), true
	case jobs.TypeManifestExport:
		return "manifest:" + strconv.FormatInt(job.Params.DestinationID, 10), true
	case jobs.TypeSync:
		return "follow-up-sync:" + strconv.FormatInt(job.Params.DestinationID, 10), true
	}
	return "", false
}

// limitKey names what a job's warnings and failures are limited per (docs/design/phase2-3.md
// §12.5): a refresh per integration, a manifest export per destination, and a sync queued by a
// webhook or a refresh per destination. Those jobs never send success, and notify a warning or a
// failure at most once per quietInterval while the problem lasts. ok is false for every other job,
// which notifies as in Phase 1.
func limitKey(job jobs.Job) (key string, ok bool) {
	if job.Type == jobs.TypeSync && !followUpSync(job) {
		return "", false
	}
	return problemKey(job)
}

// followUpSync reports whether a sync was queued by a refresh (which queues the syncs of a
// webhook too): trigger webhook, or it names its sources. Only a refresh's follow-up queues a
// sync with sourceIds (always exactly one source; with paths when targeted, without when the
// folder must be synced whole; mediaindex queueSyncs), whatever its trigger (schedule, manual or
// startup like its refresh, or resume). The sync API and destination schedules never set
// sourceIds.
func followUpSync(job jobs.Job) bool {
	return job.Trigger == jobs.TriggerWebhook || len(job.Params.Paths) > 0 || len(job.Params.SourceIDs) > 0
}

// heldChanges reports whether a sync held changes (S10): those always notify, as in Phase 1.
func heldChanges(job jobs.Job) bool {
	if job.Type != jobs.TypeSync || len(job.Stats) == 0 {
		return false
	}
	var st struct {
		FilesHeld int64 `json:"filesHeld"`
	}
	return json.Unmarshal(job.Stats, &st) == nil && st.FilesHeld > 0
}

// scope is what a job worked on, so that only a job that did all of it ends its problem.
type scope struct {
	sources []int64  // sync: its sources; nil: every source of the destination
	paths   []string // sync: the paths inside its one source; nil: the whole sources
	items   []int64  // refresh: the *arr item ids; nil: the whole integration
}

// scopeOf returns what job worked on. Its slices are job's (admit gets a snapshot).
func scopeOf(job jobs.Job) scope {
	switch job.Type {
	case jobs.TypeSync:
		return scope{sources: job.Params.SourceIDs, paths: job.Params.Paths}
	case jobs.TypeRefresh:
		return scope{items: job.Params.ArrItemIDs}
	}
	return scope{} // a manifest export always covers its whole destination
}

// covers reports whether a job with scope s did everything a job with scope o did.
func (s scope) covers(o scope) bool {
	if len(s.sources) > 0 && (len(o.sources) == 0 || !subset(o.sources, s.sources)) {
		return false
	}
	if len(s.paths) > 0 && (len(o.paths) == 0 || !pathsCover(s.paths, o.paths)) {
		return false
	}
	if len(s.items) > 0 && (len(o.items) == 0 || !subset(o.items, s.items)) {
		return false
	}
	return true
}

// subset reports whether every id in a is in b.
func subset(a, b []int64) bool {
	for _, id := range a {
		if !slices.Contains(b, id) {
			return false
		}
	}
	return true
}

// pathsCover reports whether every path in o is one of ps or inside one of them.
func pathsCover(ps, o []string) bool {
	for _, q := range o {
		if !slices.ContainsFunc(ps, func(p string) bool { return q == p || strings.HasPrefix(q, p+"/") }) {
			return false
		}
	}
	return true
}

// slotKey is one §12.5 limit: warnings and failures of a key are limited separately, so a failure
// is never held back by a warning (or the reverse), e.g. for a target that only wants failures.
type slotKey struct {
	key string
	typ MessageType
}

// slot is the last problem notified for a slotKey.
type slot struct {
	at    time.Time // its job's finish time
	scope scope     // what its job worked on: a clean job that covers it ends the problem
	id    uint64    // numbers the slot for release
}

// reservation is a slot admit took, with what it replaced so release can restore it.
type reservation struct {
	k    slotKey
	id   uint64
	prev slot
	had  bool
}

// admit applies the §12.5 limits to a finished job and reports whether it notifies, with the slot
// it took (nil when none): the caller releases it when the message reaches no target.
//
// A clean job (completed, not a dry run) ends the problems of its key that it covers: those a job
// with the same or a narrower scope notified (a full refresh ends a targeted refresh's problem,
// never the reverse; a sync of source A never ends source B's). Their next warning or failure then
// notifies at once. A limited warning or failure notifies unless one of the same key and type did
// within quietInterval of it. Times are the jobs' finish times, so hooks that run out of order
// still decide alike; a gap of quietInterval or more either way (e.g. the clock was set back)
// starts a new window. A job that finished between clockSlack and quietInterval before a slot's
// job (a smaller set-back) moves the slot to its own finish time: the window then lasts about
// quietInterval of real time, and clean jobs after the set-back end the problem.
func (d *Dispatcher) admit(job jobs.Job) (bool, *reservation) {
	ev, notify := eventFor(job)
	key, ok := problemKey(job)
	if !ok {
		return notify, nil
	}
	at := d.now()
	if job.FinishedAt != nil {
		at = *job.FinishedAt
	}
	sc := scopeOf(job)
	clean := job.Status == jobs.StatusCompleted && !job.DryRun
	d.limitMu.Lock()
	defer d.limitMu.Unlock()
	for _, typ := range []MessageType{TypeWarning, TypeFailure} {
		k := slotKey{key, typ}
		s, seen := d.notified[k]
		if !seen {
			continue
		}
		// Hooks run moments after a job finishes: a job of the key that finished more than
		// clockSlack before the slot's job means the clock was set back. Measure the slot on the
		// new clock from this job on (a set-back of quietInterval or more starts a new window
		// below instead).
		if back := s.at.Sub(at); back > clockSlack && back < quietInterval {
			s.at = at
			d.notified[k] = s
		}
		if clean && !at.Before(s.at) && sc.covers(s.scope) {
			delete(d.notified, k)
		}
	}
	if _, limited := limitKey(job); !notify || !limited {
		return notify, nil
	}
	if heldChanges(job) {
		return true, nil
	}
	k := slotKey{key, ev.typ}
	prev, seen := d.notified[k]
	if seen && at.Sub(prev.at).Abs() < quietInterval {
		d.log.Info("Notification skipped: one was sent for the same problem in the last 24 hours", "job", job.ID,
			"type", string(job.Type), "status", string(job.Status))
		return false, nil
	}
	d.lastSlotID++
	res := &reservation{k: k, id: d.lastSlotID, prev: prev, had: seen}
	d.notified[k] = slot{at: at, scope: sc, id: res.id}
	return true, res
}

// release gives back the slot res took when its message reached no target (nobody subscribed, the
// sends failed before anything was sent, or the job was dropped), so the next warning or failure
// of the key notifies. A slot a later job replaced or a clean job ended stays as it is. A job of
// the same key and type that admit held back while res was being sent is not retried.
func (d *Dispatcher) release(res *reservation) {
	if res == nil {
		return
	}
	d.limitMu.Lock()
	defer d.limitMu.Unlock()
	if cur, ok := d.notified[res.k]; ok && cur.id == res.id {
		if res.had {
			d.notified[res.k] = res.prev
		} else {
			delete(d.notified, res.k)
		}
	}
}

// Handle queues the notifications for a finished job and returns at once; it never blocks on the
// database or the network. The §12.5 limits (admit) are applied here, in finish order. When the
// queue is full (or the dispatcher is closed) the job's notifications are dropped with a log line.
func (d *Dispatcher) Handle(job jobs.Job) {
	job = snapshot(job)
	ok, res := d.admit(job)
	if !ok {
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		d.log.Warn("Notification dropped: notifications are shutting down", "job", job.ID, "status", string(job.Status))
		d.release(res)
		return
	}
	d.pending.Add(1)
	select {
	case d.queue <- queued{job: job, res: res}:
	default:
		d.pending.Done()
		d.log.Warn("Notification dropped: too many notifications waiting", "job", job.ID, "status", string(job.Status))
		d.release(res)
	}
}

// Test sends a test message and returns the delivery error, if any (the Test button). With in ==
// nil it tests saved notification id; otherwise it tests the unsaved settings in (for an edit form,
// id > 0 lets an empty URLs field use the stored URLs). Disabled notifications are tested too.
// Errors: ValidationError, an error wrapping ErrNotFound, or *SendError.
func (d *Dispatcher) Test(ctx context.Context, id int64, in *Input) error {
	var (
		t   Target
		err error
	)
	switch {
	case in != nil:
		t, err = d.store.testTarget(ctx, id, *in)
	case id > 0:
		t, err = d.store.target(ctx, id)
	default:
		return ValidationError("a notification id or settings are required")
	}
	if err != nil {
		return err
	}
	body := "This is a test notification from Bunkarr " + version.Version + "."
	if d.baseURL != "" {
		body += "\nSettings: " + d.baseURL + "/settings/connect"
	}
	err = d.client.Send(ctx, t, Message{Title: "Bunkarr: test notification", Body: body, Type: TypeInfo})
	// The URLs of an unsaved form are not registered secrets: redact them here (a SendError never
	// quotes them, but its message reaches the API).
	var se *SendError
	if errors.As(err, &se) {
		se.msg = redactURLs(se.msg, t.URLs)
	}
	return err
}

// Close stops accepting jobs and waits for the queued notifications to be sent. When ctx ends
// first, the sends in progress are cancelled, the rest are dropped and ctx's error is returned.
// Close is idempotent.
func (d *Dispatcher) Close(ctx context.Context) error {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		d.cancel()
		return nil
	case <-ctx.Done():
		d.cancel()
		<-done // every send and query observes d.ctx, so this is prompt
		return fmt.Errorf("notifications: shutdown: %w", ctx.Err())
	}
}

func (d *Dispatcher) worker() {
	for q := range d.queue {
		if d.ctx.Err() != nil {
			d.log.Warn("Notification dropped: notifications are shutting down", "job", q.job.ID, "status", string(q.job.Status))
			d.release(q.res)
			d.pending.Done()
			continue
		}
		d.process(q.job, q.res)
		d.pending.Done()
	}
}

// process sends job's notification to every subscribed target. When it reached no target (no
// subscriber, or every send failed in a way that proves nothing was sent: see maybeDelivered), it
// releases the §12.5 slot res (see admit).
func (d *Dispatcher) process(job jobs.Job, res *reservation) {
	delivered := false
	defer func() {
		// A bug in Describe must not take the server down with it.
		if p := recover(); p != nil {
			d.log.Error("Notification failed: internal error", "job", job.ID, "panic", logging.RedactSecrets(fmt.Sprint(p)))
		}
		if !delivered {
			d.release(res)
		}
	}()
	ev, ok := eventFor(job)
	if !ok {
		return
	}
	rows, err := d.store.subscribers(d.ctx, ev.typ)
	if err != nil {
		d.log.Error("Notification failed: cannot read notification settings", "job", job.ID, "error", logging.RedactSecrets(err.Error()))
		return
	}
	if len(rows) == 0 {
		return
	}
	msg := d.message(job, ev)
	for _, r := range rows {
		t, err := d.store.openTarget(r)
		if err != nil {
			d.log.Error("Notification failed", "notification", r.Name, "job", job.ID, "error", logging.RedactSecrets(err.Error()))
			continue
		}
		if err := d.client.Send(d.ctx, t, msg); err != nil {
			// A 424 (the key's other services got it) or a failure after the request went out
			// (e.g. no answer in time) may have been delivered: keep the slot, or the services
			// that work would get every repeat of a lasting problem.
			if maybeDelivered(err) {
				delivered = true
			}
			d.log.Warn("Notification failed", "notification", t.Name, "job", job.ID, "error", logging.RedactSecrets(err.Error()))
			continue
		}
		delivered = true
		d.log.Info("Notification sent", "notification", t.Name, "job", job.ID, "type", string(ev.typ))
	}
}

// message renders job's notification: title "Bunkarr: <description> <verb>"; body with the
// summary, the (redacted) error, the warning count, the duration, the job id and a link.
func (d *Dispatcher) message(job jobs.Job, ev event) Message {
	desc := d.description(job)
	if job.DryRun {
		desc += " (dry run)"
	}
	var b strings.Builder
	line := func(s string) {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}
	if s := strings.TrimSpace(job.Summary); s != "" {
		line(truncate(logging.RedactSecrets(s), maxTextLen))
	}
	if e := strings.TrimSpace(job.Error); e != "" {
		line("Error: " + truncate(logging.RedactSecrets(e), maxTextLen))
	}
	if job.Warnings > 0 {
		line("Warnings: " + strconv.Itoa(job.Warnings))
	}
	if job.StartedAt != nil && job.FinishedAt != nil {
		line("Duration: " + formatDuration(job.FinishedAt.Sub(*job.StartedAt)))
	}
	line("Job: #" + strconv.FormatInt(job.ID, 10))
	if d.baseURL != "" {
		line("Details: " + d.baseURL + "/activity/jobs/" + strconv.FormatInt(job.ID, 10))
	}
	return Message{Title: "Bunkarr: " + desc + " " + ev.verb, Body: b.String(), Type: ev.typ}
}

// description returns Options.Describe's text for job as one bounded, redacted line, or the job
// type's name.
func (d *Dispatcher) description(job jobs.Job) string {
	var desc string
	if d.describe != nil {
		ctx, cancel := context.WithTimeout(d.ctx, describeTimeout)
		desc = d.describe(ctx, job)
		cancel()
	}
	desc = strings.Join(strings.Fields(desc), " ")
	if desc == "" {
		return typeName(job.Type)
	}
	return truncate(logging.RedactSecrets(desc), maxDescribeLen)
}

// typeName is the fallback description of a job.
func typeName(t jobs.Type) string {
	switch t {
	case jobs.TypeSync:
		return "Sync"
	case jobs.TypeScan:
		return "Scan"
	case jobs.TypePlexDBBackup:
		return "Plex database backup"
	case jobs.TypeRetention:
		return "Retention"
	case jobs.TypeVerify:
		return "Verify"
	case jobs.TypeRefresh:
		return "Refresh"
	case jobs.TypeArrBackup:
		return "Backup"
	case jobs.TypeManifestExport:
		return "Manifest export"
	case "":
		return "Job"
	}
	return string(t)
}

// snapshot copies the parts of job that alias the caller's memory, so the manager may reuse them
// after Handle returns.
func snapshot(job jobs.Job) jobs.Job {
	if job.StartedAt != nil {
		t := *job.StartedAt
		job.StartedAt = &t
	}
	if job.FinishedAt != nil {
		t := *job.FinishedAt
		job.FinishedAt = &t
	}
	job.Stats = bytes.Clone(job.Stats)
	job.Params.SourceIDs = slices.Clone(job.Params.SourceIDs)
	job.Params.Paths = slices.Clone(job.Params.Paths)
	job.Params.ArrItemIDs = slices.Clone(job.Params.ArrItemIDs)
	return job
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// truncate shortens s to at most n characters, marking the cut with "...".
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-3]) + "..."
}
