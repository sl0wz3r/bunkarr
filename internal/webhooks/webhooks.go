// Package webhooks receives the webhooks of Sonarr, Radarr and Lidarr (docs/design/phase2-3.md
// §7). It owns the webhook_events table.
//
//   - Parse decodes a body as a stream and keeps only what Bunkarr uses: the event type, the
//     *arr item it names (movie.id, series.id, artist.id), the fields that decide its scheduling
//     class, and a summary for the event list (§7.2).
//   - Store records every authenticated event and prunes the table.
//   - Intake is the HTTP handler of the webhook routes once the caller (internal/api's
//     webhookAuth) has checked the credential and the integration: it bounds the rate, the
//     backlog, the concurrent body reads and the body size, then stores the event and returns.
//   - Processor turns the stored events into targeted refresh jobs of their items, one due time
//     per item (§7.3), with trigger webhook and syncAfter set, so the refresh queues the targeted
//     syncs of the items' folders.
//
// Safety rules that live here:
//   - S12 (webhooks are hints, never truth): a payload only selects which *arr items to refresh.
//     No path it carries is opened or trusted, and no event deletes, retains or marks anything.
//     A Test event is answered before any lookup; an unknown event type is stored and ignored.
//   - S13 (the intake is bounded): 20 events per second per integration with bursts of 2000, 503
//     while 10,000 events are unprocessed, 4 bodies read at a time (10 s wait for a slot),
//     application/json of at most 16 MiB decoded as a stream, at most 64 KiB stored per event,
//     and the table pruned to 30 days, 50,000 rows and 256 MiB of payloads. Headers, the query
//     (where the key may be) and the client address are never stored.
//
// Fault points of this package: webhook.afterStore (an event is stored, the processor was not
// told) and webhook.afterEnqueue (a refresh is queued, its events are not marked processed).
// Both are recovered at start-up: the processor re-reads the unprocessed events, and the
// coalescing of jobqueue.Enqueue merges them into the job already queued.
package webhooks

// Fault-injection points of this package (internal/faultinject).
const (
	// PointAfterStore: an event is stored, the processor was not notified and nothing was
	// answered.
	PointAfterStore = "webhook.afterStore"
	// PointAfterEnqueue: a refresh was queued for due items, their events are not marked
	// processed yet.
	PointAfterEnqueue = "webhook.afterEnqueue"
)

// Class is how the processor schedules an event (webhook_events.class, design §7.2).
type Class string

// Event classes.
const (
	// ClassTest is a Test event: answered at once, nothing looked up.
	ClassTest Class = "test"
	// ClassIgnored is an event type Bunkarr does not act on (Grab, Health, ...).
	ClassIgnored Class = "ignored"
	// ClassChange is an item change without a new file of its own (rename, add, a manual file
	// delete, a delete without files): refreshed 5 s after the last event, at most 20 s after
	// the first.
	ClassChange Class = "change"
	// ClassDownload is an import (new, upgrade, same-path replacement); it also releases an
	// upgrade delete's hold.
	ClassDownload Class = "download"
	// ClassDelete is an item deleted with its files: refreshed 60 s later, because the folder
	// is removed after the event is sent.
	ClassDelete Class = "delete"
	// ClassUpgradeDelete is the delete that precedes an upgrade's Download: the item waits for
	// that Download, or 30 minutes.
	ClassUpgradeDelete Class = "upgrade_delete"
)

// Valid reports whether c is a known class.
func (c Class) Valid() bool {
	switch c {
	case ClassTest, ClassIgnored, ClassChange, ClassDownload, ClassDelete, ClassUpgradeDelete:
		return true
	}
	return false
}

// schedules reports whether events of class c are turned into refresh jobs.
func (c Class) schedules() bool {
	return c == ClassChange || c == ClassDownload || c == ClassDelete || c == ClassUpgradeDelete
}

// Outcome is what became of a processed event (webhook_events.outcome).
type Outcome string

// Event outcomes.
const (
	// OutcomeTest: a Test event.
	OutcomeTest Outcome = "test"
	// OutcomeIgnored: an event type Bunkarr does not act on.
	OutcomeIgnored Outcome = "ignored"
	// OutcomeQueued: a new refresh job was queued for the event's item.
	OutcomeQueued Outcome = "queued"
	// OutcomeCoalesced: a job that was already queued absorbed the event's item.
	OutcomeCoalesced Outcome = "coalesced"
	// OutcomeFailed: the event named no item, its integration is gone or disabled, or the
	// enqueue failed three times.
	OutcomeFailed Outcome = "failed"
)
