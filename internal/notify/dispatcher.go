package notify

import (
	"bytes"
	"context"
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
	queue  chan jobs.Job
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
		queue:    make(chan jobs.Job, opts.QueueSize),
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
// and unfinished jobs → none. Whether a target receives it depends on its OnFailure, OnWarning
// and OnSuccess.
func eventFor(job jobs.Job) (event, bool) {
	switch job.Status {
	case jobs.StatusFailed:
		return event{TypeFailure, "failed"}, true
	case jobs.StatusCompletedWithWarnings:
		if !job.DryRun {
			return event{TypeWarning, "completed with warnings"}, true
		}
	case jobs.StatusCompleted:
		if !job.DryRun {
			return event{TypeSuccess, "completed"}, true
		}
	}
	return event{}, false
}

// Handle queues the notifications for a finished job and returns at once; it never blocks on the
// database or the network. When the queue is full (or the dispatcher is closed) the job's
// notifications are dropped with a log line.
func (d *Dispatcher) Handle(job jobs.Job) {
	if _, ok := eventFor(job); !ok {
		return
	}
	job = snapshot(job)
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		d.log.Warn("Notification dropped: notifications are shutting down", "job", job.ID, "status", string(job.Status))
		return
	}
	select {
	case d.queue <- job:
	default:
		d.log.Warn("Notification dropped: too many notifications waiting", "job", job.ID, "status", string(job.Status))
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
	for job := range d.queue {
		if d.ctx.Err() != nil {
			d.log.Warn("Notification dropped: notifications are shutting down", "job", job.ID, "status", string(job.Status))
			continue
		}
		d.process(job)
	}
}

// process sends job's notification to every subscribed target.
func (d *Dispatcher) process(job jobs.Job) {
	defer func() {
		// A bug in Describe must not take the server down with it.
		if p := recover(); p != nil {
			d.log.Error("Notification failed: internal error", "job", job.ID, "panic", logging.RedactSecrets(fmt.Sprint(p)))
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
			d.log.Warn("Notification failed", "notification", t.Name, "job", job.ID, "error", logging.RedactSecrets(err.Error()))
			continue
		}
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
