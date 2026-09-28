package notify

import (
	"fmt"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// Warnings that belong to no job (docs/design/phase4.md §5.2, §11.5): a recovery kit was exported,
// a destination's recovery kit custody is still not confirmed. They go to every enabled target that
// subscribed to warnings (OnWarning), like a job's warning, and are never limited by the §12.5
// rules (their callers decide how often to send them).

// Warn queues a warning that belongs to no job, titled "Bunkarr: <title>", for every enabled
// target with OnWarning, and returns at once; it never blocks on the database or the network.
// Title and body are redacted (registered secrets) and bounded like a job's summary. When the
// queue is full, or the dispatcher is closed, the warning is dropped with a log line.
func (d *Dispatcher) Warn(title, body string) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "warning"
	}
	msg := &Message{
		Title: "Bunkarr: " + truncate(logging.RedactSecrets(title), maxDescribeLen),
		Body:  truncate(logging.RedactSecrets(strings.TrimSpace(body)), maxTextLen),
		Type:  TypeWarning,
	}
	if msg.Body == "" {
		// Apprise requires a body.
		msg.Body = msg.Title
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		d.log.Warn("Notification dropped: notifications are shutting down", "title", msg.Title)
		return
	}
	d.pending.Add(1)
	select {
	case d.queue <- queued{msg: msg}:
	default:
		d.pending.Done()
		d.log.Warn("Notification dropped: too many notifications waiting", "title", msg.Title)
	}
}

// sendWarning sends a Warn message to every target that subscribed to warnings.
func (d *Dispatcher) sendWarning(msg Message) {
	defer func() {
		if p := recover(); p != nil {
			d.log.Error("Notification failed: internal error", "title", msg.Title, "panic", logging.RedactSecrets(fmt.Sprint(p)))
		}
	}()
	rows, err := d.store.subscribers(d.ctx, TypeWarning)
	if err != nil {
		d.log.Error("Notification failed: cannot read notification settings", "title", msg.Title, "error", logging.RedactSecrets(err.Error()))
		return
	}
	for _, r := range rows {
		t, err := d.store.openTarget(r)
		if err != nil {
			d.log.Error("Notification failed", "notification", r.Name, "title", msg.Title, "error", logging.RedactSecrets(err.Error()))
			continue
		}
		if err := d.client.Send(d.ctx, t, msg); err != nil {
			d.log.Warn("Notification failed", "notification", t.Name, "title", msg.Title, "error", logging.RedactSecrets(err.Error()))
			continue
		}
		d.log.Info("Notification sent", "notification", t.Name, "title", msg.Title, "type", string(msg.Type))
	}
}
