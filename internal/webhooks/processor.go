package webhooks

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/testhooks"
)

// Windows of the processor (design §7.3). A binary built with -tags e2e can shorten them
// (internal/testhooks); a production build always uses these.
const (
	// DefaultQuiet: a change or download is refreshed this long after the item's last event...
	DefaultQuiet = 5 * time.Second
	// DefaultCap: ...but at most this long after its first pending event.
	DefaultCap = 20 * time.Second
	// DefaultDeleteDelay: a delete with files is refreshed this long after the event (the *arr
	// removes the folder after sending it).
	DefaultDeleteDelay = 60 * time.Second
	// DefaultUpgradeHold: an upgrade's file delete holds its item until the upgrade's Download
	// arrives, or this long.
	DefaultUpgradeHold = 30 * time.Minute
)

// Other constants of the processor.
const (
	// defaultRetryDelay is the wait before a failed enqueue is tried again.
	defaultRetryDelay = 5 * time.Second
	// maxEnqueueAttempts is how often an item's enqueue is tried before its events fail.
	maxEnqueueAttempts = 3
	// defaultPruneEvery is how often the table is pruned.
	defaultPruneEvery = time.Hour
	// loadPage is how many unprocessed events are read at a time at start.
	loadPage = 500
)

// ProcessorOptions configures a Processor. Zero durations take the defaults above.
type ProcessorOptions struct {
	// Store is the event store (required).
	Store *Store
	// Enqueuer queues the refresh jobs (required): the job manager.
	Enqueuer jobs.Enqueuer
	// Enabled reports whether an integration still exists and is enabled; the events of one
	// that is not are marked failed. nil means always.
	Enabled func(ctx context.Context, integrationID int64) (bool, error)
	// Log receives the processor's log lines; nil discards them.
	Log *slog.Logger
	// Now is the clock of the due times; nil means time.Now.
	Now func() time.Time

	Quiet, Cap, DeleteDelay, UpgradeHold time.Duration
	RetryDelay, PruneEvery               time.Duration
}

// itemKey names one *arr item of one integration.
type itemKey struct{ integration, item int64 }

// item is an item with pending events.
type item struct {
	// first is when the item's first pending event (or the Download that released a hold)
	// arrived: the quiet window of change and download events is capped at first + Cap.
	first time.Time
	// due is when the change and download events want the item refreshed; deleteDue when the
	// delete events do.
	due, deleteDue time.Time
	// hold is when an upgrade delete stops waiting for its Download (zero: no hold).
	hold time.Time
	// retryAt is when a failed enqueue may be tried again; attempts counts the failures.
	retryAt  time.Time
	attempts int
	events   []int64
}

// dueAt is when the item is flushed.
func (it *item) dueAt() time.Time {
	return later(later(later(it.due, it.deleteDue), it.hold), it.retryAt)
}

// joins reports whether the item may go with a flush of its integration at now although it is
// not due yet: it waits only for its quiet window, which ends within one more quiet window. So a
// burst of events for many items becomes one refresh, while a delete's delay and an upgrade's
// hold are never cut short.
func (it *item) joins(now time.Time, quiet time.Duration) bool {
	return it.deleteDue.IsZero() && it.hold.IsZero() && !it.retryAt.After(now) && !it.due.After(now.Add(quiet))
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Processor turns stored webhook events into targeted refresh jobs (design §7.3): one due time
// per (integration, item), and a flush that queues one refresh {integrationId, arrItemIds,
// syncAfter} with trigger webhook per integration and marks its events processed. A flush also
// takes the integration's items that only wait for a quiet window ending within one more quiet
// window, so a burst of events for many items is one refresh. It is safe for
// concurrent use; Notify and Backlog never block on I/O.
type Processor struct {
	o    ProcessorOptions
	log  *slog.Logger
	now  func() time.Time
	wall func() time.Time // the clock compared with a job's queued_at

	mu    sync.Mutex
	inbox []Pending
	// known holds the ids of the unprocessed events in memory (the inbox and the items): the
	// backlog.
	known map[int64]bool

	// items is owned by the loop (and by Start before the loop runs).
	items map[itemKey]*item
	// budgetRetryAt is when a failed budget prune may be tried again (loop-owned).
	budgetRetryAt time.Time

	kick    chan struct{}
	stopCh  chan struct{}
	done    chan struct{}
	started bool
	stopped bool
}

// NewProcessor returns a processor. Start it once, after the job manager.
func NewProcessor(o ProcessorOptions) *Processor {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Quiet <= 0 {
		o.Quiet = testhooks.WebhookQuiet(DefaultQuiet)
	}
	if o.Cap <= 0 {
		o.Cap = testhooks.WebhookCap(DefaultCap)
	}
	if o.DeleteDelay <= 0 {
		o.DeleteDelay = testhooks.WebhookDeleteDelay(DefaultDeleteDelay)
	}
	if o.UpgradeHold <= 0 {
		o.UpgradeHold = testhooks.WebhookUpgradeHold(DefaultUpgradeHold)
	}
	if o.RetryDelay <= 0 {
		o.RetryDelay = defaultRetryDelay
	}
	if o.PruneEvery <= 0 {
		o.PruneEvery = defaultPruneEvery
	}
	return &Processor{o: o, log: o.Log, now: o.Now, wall: time.Now, known: map[int64]bool{}, items: map[itemKey]*item{},
		kick: make(chan struct{}, 1), stopCh: make(chan struct{}), done: make(chan struct{})}
}

// Notify hands a stored event to the processor (the intake calls it after Store.Insert). Test
// and ignored events are processed already and are not taken, but any event wakes the loop when
// the insert took the payloads over their budget: the loop prunes them (S13).
func (p *Processor) Notify(rec Record) {
	if rec.Class.schedules() && rec.ProcessedAt == nil {
		p.push([]Pending{rec.pending()})
		return
	}
	if p.o.Store != nil && p.o.Store.overBudget() {
		p.wake()
	}
}

// push adds events to the inbox and wakes the loop.
func (p *Processor) push(evs []Pending) {
	p.mu.Lock()
	for _, ev := range evs {
		if !p.known[ev.ID] {
			p.known[ev.ID] = true
			p.inbox = append(p.inbox, ev)
		}
	}
	p.mu.Unlock()
	p.wake()
}

// wake makes the loop run a step.
func (p *Processor) wake() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Backlog returns the number of unprocessed events the processor holds (the intake answers 503
// while it reaches MaxBacklog).
func (p *Processor) Backlog() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.known)
}

// forget drops processed events from the backlog.
func (p *Processor) forget(ids []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range ids {
		delete(p.known, id)
	}
}

// Start prunes the table, re-arms the events of refreshes cancelled before their OnFinish hook
// could, reads the unprocessed events (in id order, from their class and targets only) and
// starts the loop. It runs until Stop, not until ctx ends. When it fails the loop never runs:
// Stop then returns at once, and Start cannot be called again.
func (p *Processor) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return errors.New("webhook processor: already started")
	}
	p.started = true
	p.mu.Unlock()
	if n, err := p.o.Store.Prune(ctx); err != nil {
		p.log.Warn("Could not prune the webhook events", "events", n, "error", err)
	} else if n > 0 {
		p.log.Info("Pruned old webhook events", "events", n)
	}
	if n, err := p.o.Store.rearmCancelled(ctx); err != nil {
		p.log.Warn("Could not re-arm the webhook events of cancelled refreshes", "error", err)
	} else if n > 0 {
		p.log.Info("Webhook events of refreshes cancelled before a restart are queued again", "events", n)
	}
	var after int64
	for {
		page, err := p.o.Store.Unprocessed(ctx, after, loadPage)
		if err != nil {
			// The loop never runs: close done so that Stop does not wait for it until its
			// deadline (the job manager, stopped next, would then get no grace at all).
			close(p.done)
			return err
		}
		p.push(page)
		if len(page) < loadPage {
			break
		}
		after = page[len(page)-1].ID
	}
	if n := p.Backlog(); n > 0 {
		p.log.Info("Processing the webhook events received before the restart", "events", n)
	}
	go p.loop(context.WithoutCancel(ctx))
	return nil
}

// Stop stops the loop and waits for it (or for ctx). Pending items stay in the database and are
// read again at the next start.
func (p *Processor) Stop(ctx context.Context) error {
	p.mu.Lock()
	started, stopped := p.started, p.stopped
	p.stopped = true
	p.mu.Unlock()
	if !started || stopped {
		return nil
	}
	close(p.stopCh)
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// OnJobFinish is the job manager's OnFinish hook: when a refresh is cancelled, the events it
// carried become unprocessed again and their items are flushed anew (design §7.3). A cancelled
// sync is not re-armed: the user cancelled it.
func (p *Processor) OnJobFinish(job jobs.Job) {
	if job.Type != jobs.TypeRefresh || job.Status != jobs.StatusCancelled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	evs, err := p.o.Store.Rearm(ctx, job.ID)
	if err != nil {
		p.log.Warn("Could not re-arm the webhook events of a cancelled refresh", "jobId", job.ID, "error", err)
		return
	}
	if len(evs) > 0 {
		p.log.Info("A refresh with webhook events was cancelled: its items are queued again", "jobId", job.ID, "events", len(evs))
		p.push(evs)
	}
}

func (p *Processor) loop(ctx context.Context) {
	defer close(p.done)
	prune := time.NewTicker(p.o.PruneEvery)
	defer prune.Stop()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		next := p.step(ctx, p.now())
		wait := time.Hour
		if !next.IsZero() {
			wait = max(next.Sub(p.now()), time.Millisecond)
		}
		timer.Reset(wait)
		select {
		case <-p.stopCh:
			return
		case <-p.kick:
		case <-timer.C:
		case <-prune.C:
			if _, err := p.o.Store.Prune(ctx); err != nil {
				p.log.Warn("Could not prune the webhook events", "error", err)
			}
		}
	}
}

// step prunes the table when an insert took the payloads over their budget, takes the new
// events, flushes the items due at now and returns the next due time (zero: nothing pending).
func (p *Processor) step(ctx context.Context, now time.Time) time.Time {
	p.pruneBudget(ctx, now)
	p.mu.Lock()
	inbox := p.inbox
	p.inbox = nil
	p.mu.Unlock()
	var failed []int64
	for _, ev := range inbox {
		if !p.add(ev) {
			failed = append(failed, ev.ID)
		}
	}
	if len(failed) > 0 {
		p.log.Warn("Webhook events name no item (or their integration was deleted); they are not processed", "events", len(failed))
		p.markFailed(ctx, failed, now)
	}
	p.flush(ctx, now)
	var next time.Time
	for _, it := range p.items {
		if d := it.dueAt(); next.IsZero() || d.Before(next) {
			next = d
		}
	}
	// A failed budget prune is retried at budgetRetryAt even when no item is pending (the loop
	// would otherwise sleep until the next event or the hourly prune).
	if r := p.budgetRetryAt; !r.IsZero() && p.o.Store.overBudget() && (next.IsZero() || r.Before(next)) {
		next = r
	}
	return next
}

// pruneBudget deletes the oldest events down to 90% of the payload budget once the inserts took
// the payloads over it (S13). It runs here, not in the intake's request, so an insert never
// waits for it and never fails because of it; a failure is retried after RetryDelay.
func (p *Processor) pruneBudget(ctx context.Context, now time.Time) {
	if p.budgetRetryAt.After(now) || !p.o.Store.overBudget() {
		return
	}
	n, err := p.o.Store.pruneBudget(ctx)
	if err != nil {
		p.log.Warn("Could not prune the webhook events to their payload budget; retrying", "error", err)
		p.budgetRetryAt = now.Add(p.o.RetryDelay)
		return
	}
	p.budgetRetryAt = time.Time{}
	if n > 0 {
		p.log.Info("Pruned the oldest webhook events: their payloads exceeded the budget", "events", n)
	}
}

// add schedules an event; false means it cannot be (no item, no integration).
func (p *Processor) add(ev Pending) bool {
	if ev.IntegrationID == 0 || len(ev.Targets) == 0 || !ev.Class.schedules() {
		return false
	}
	at := ev.ReceivedAt
	for _, target := range ev.Targets {
		k := itemKey{ev.IntegrationID, target}
		it := p.items[k]
		if it == nil {
			it = &item{first: at}
			p.items[k] = it
		}
		it.events = append(it.events, ev.ID)
		switch ev.Class {
		case ClassDownload, ClassChange:
			if ev.Class == ClassDownload && !it.hold.IsZero() {
				// The upgrade's Download: the hold ends, the quiet window starts now.
				it.hold, it.first = time.Time{}, at
			}
			due := at.Add(p.o.Quiet)
			if limit := it.first.Add(p.o.Cap); due.After(limit) {
				due = limit
			}
			it.due = later(it.due, due)
		case ClassDelete:
			it.deleteDue = later(it.deleteDue, at.Add(p.o.DeleteDelay))
		case ClassUpgradeDelete:
			it.hold = later(it.hold, at.Add(p.o.UpgradeHold))
		}
	}
	return true
}

// flush queues a refresh for the items due at now, per integration (with the items that join
// them), and marks their events.
func (p *Processor) flush(ctx context.Context, now time.Time) {
	due := map[int64]bool{}
	for k, it := range p.items {
		if !it.dueAt().After(now) {
			due[k.integration] = true
		}
	}
	byIntegration := map[int64][]itemKey{}
	for k, it := range p.items {
		if due[k.integration] && (!it.dueAt().After(now) || it.joins(now, p.o.Quiet)) {
			byIntegration[k.integration] = append(byIntegration[k.integration], k)
		}
	}
	for _, integ := range slices.Sorted(maps.Keys(byIntegration)) {
		keys := byIntegration[integ]
		slices.SortFunc(keys, func(a, b itemKey) int { return cmp.Compare(a.item, b.item) })
		if p.o.Enabled != nil {
			ok, err := p.o.Enabled(ctx, integ)
			if err != nil {
				p.log.Warn("Could not read an integration for its webhook events", "integrationId", integ, "error", err)
				p.retry(keys, now)
				continue
			}
			if !ok {
				p.log.Warn("Webhook events of an integration that was deleted or disabled are not processed", "integrationId", integ)
				p.drop(ctx, keys, now)
				continue
			}
		}
		for len(keys) > 0 {
			chunk := keys[:min(len(keys), jobs.MaxTargetItems)]
			keys = keys[len(chunk):]
			p.flushChunk(ctx, integ, chunk, now)
		}
	}
}

// flushChunk queues one refresh for the items chunk of integration integ and marks their events.
func (p *Processor) flushChunk(ctx context.Context, integ int64, chunk []itemKey, now time.Time) {
	ids := make([]int64, len(chunk))
	var events []int64
	for i, k := range chunk {
		ids[i] = k.item
		events = append(events, p.items[k].events...)
	}
	spec := jobs.Spec{Type: jobs.TypeRefresh, Trigger: jobs.TriggerWebhook,
		Params: jobs.Params{IntegrationID: integ, ArrItemIDs: ids, SyncAfter: true}}
	flushAt := p.wall()
	job, err := p.o.Enqueuer.Enqueue(ctx, spec)
	if err != nil {
		p.log.Warn("Could not queue the refresh of webhook items; retrying", "integrationId", integ, "items", ids, "error", err)
		var failed []int64
		for _, k := range chunk {
			it := p.items[k]
			it.attempts++
			if it.attempts >= maxEnqueueAttempts {
				failed = append(failed, it.events...)
				delete(p.items, k)
				continue
			}
			it.retryAt = now.Add(p.o.RetryDelay)
		}
		if len(failed) > 0 {
			p.log.Error("Gave up queuing the refresh of webhook items; the next full refresh reconciles them", "integrationId", integ,
				"events", len(failed))
			p.markFailed(ctx, failed, now)
		}
		return
	}
	faultinject.Point(PointAfterEnqueue)
	outcome := OutcomeQueued
	if job.QueuedAt.Before(flushAt) {
		outcome = OutcomeCoalesced
	}
	if _, err := p.o.Store.MarkProcessed(ctx, events, outcome, job.ID, now); err != nil {
		if errors.Is(err, errJobCancelled) {
			// The refresh was cancelled after the enqueue returned it, and its OnFinish hook
			// found none of these events to re-arm: they stay unprocessed and a later flush
			// queues a new refresh.
			p.log.Info("A refresh was cancelled as webhook items were queued into it; they are queued again", "jobId", job.ID,
				"integrationId", integ, "items", ids)
			p.retry(chunk, now)
			return
		}
		// The job is queued; a later flush enqueues again and coalesces into it.
		p.log.Warn("Could not mark webhook events processed; retrying", "jobId", job.ID, "error", err)
		p.retry(chunk, now)
		return
	}
	for _, k := range chunk {
		delete(p.items, k)
	}
	p.forget(events)
	p.log.Info("Queued the refresh of webhook items", "integrationId", integ, "items", ids, "jobId", job.ID,
		"outcome", string(outcome), "events", len(events))
}

// retry postpones items after a failure that is not the enqueue's.
func (p *Processor) retry(keys []itemKey, now time.Time) {
	for _, k := range keys {
		p.items[k].retryAt = now.Add(p.o.RetryDelay)
	}
}

// drop marks the events of items failed and forgets the items.
func (p *Processor) drop(ctx context.Context, keys []itemKey, now time.Time) {
	var events []int64
	for _, k := range keys {
		events = append(events, p.items[k].events...)
		delete(p.items, k)
	}
	p.markFailed(ctx, events, now)
}

// markFailed records outcome failed. On a database error the events stay unprocessed in the
// database and are read again at the next start.
func (p *Processor) markFailed(ctx context.Context, ids []int64, now time.Time) {
	if _, err := p.o.Store.MarkProcessed(ctx, ids, OutcomeFailed, 0, now); err != nil {
		p.log.Warn("Could not mark webhook events failed", "events", len(ids), "error", err)
	}
	p.forget(ids)
}
