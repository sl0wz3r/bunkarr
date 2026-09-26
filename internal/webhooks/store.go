package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Retention of stored events (S13): pruned at start, hourly, and once an insert takes the
// payloads over their budget (the processor prunes then, off the intake's request path).
const (
	// KeepFor is how long an event is kept.
	KeepFor = 30 * 24 * time.Hour
	// MaxRows is the most events kept (the oldest go first).
	MaxRows = 50000
	// MaxPayloadBytes is the most payload bytes kept (the oldest events go first).
	MaxPayloadBytes = 256 << 20
	// budgetLowWater is the share of MaxPayloadBytes, in percent, that a budget prune brings the
	// payloads down to: the next one comes only after a tenth of the budget arrived again, not on
	// every insert.
	budgetLowWater = 90
)

// ErrNotFound means the event does not exist.
var ErrNotFound = errors.New("webhook event not found")

// errJobCancelled means MarkProcessed was asked to link events to a refresh that is cancelled
// already: its OnFinish hook ran before the events named it, so they would never be re-armed.
var errJobCancelled = errors.New("the refresh job is cancelled")

// Record is a stored event (API: WebhookEvent). Payload is set only by Store.Get.
type Record struct {
	ID int64 `json:"id"`
	// IntegrationID is nil once the integration was deleted.
	IntegrationID *int64            `json:"integrationId"`
	Source        integrations.Type `json:"source"`
	EventType     string            `json:"eventType"`
	Class         Class             `json:"class"`
	// Targets are the item ids the event names.
	Targets     []int64    `json:"-"`
	ReceivedAt  time.Time  `json:"receivedAt"`
	ProcessedAt *time.Time `json:"processedAt"`
	// Outcome is nil while the event is not processed.
	Outcome   *Outcome `json:"outcome"`
	JobID     *int64   `json:"jobId"`
	Truncated bool     `json:"truncated"`
	// Summary is decoded again from the payload (Summarize).
	Summary Summary         `json:"summary"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Pending is an unprocessed event as the processor schedules it: from its class and targets
// only, never from its payload.
type Pending struct {
	ID int64
	// IntegrationID is 0 when the integration was deleted.
	IntegrationID int64
	Class         Class
	Targets       []int64
	ReceivedAt    time.Time
}

// Store records webhook events (the webhook_events table). It is safe for concurrent use.
type Store struct {
	db  *db.DB
	now func() time.Time

	keepFor  time.Duration
	maxRows  int
	maxBytes int64

	// mu serialises the writes that add or delete payloads (Insert, Prune, pruneBudget) with
	// the payload total they keep.
	mu sync.Mutex
	// bytes is the payload bytes stored: measured once (the first prune), then kept up to date
	// by every insert and delete, so no prune reads every payload again.
	bytes      int64
	bytesKnown bool
	// over is overBudget's answer, updated with bytes under mu and read without it, so neither
	// the intake's Notify nor the processor loop waits behind an insert queued for the writer.
	over atomic.Bool
	// measures counts the measurements of every payload (tests).
	measures int
}

// NewStore returns a store over d.
func NewStore(d *db.DB) *Store {
	s := &Store{db: d, now: time.Now, keepFor: KeepFor, maxRows: MaxRows, maxBytes: MaxPayloadBytes}
	s.over.Store(true) // the payloads are not measured yet
	return s
}

const recordColumns = `id, integration_id, source, event_type, class, targets, truncated, received_at, processed_at, outcome, job_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanRecord(r rowScanner, withPayload bool) (Record, error) {
	var (
		rec                 Record
		integ, job          sql.NullInt64
		source, targets     string
		received            string
		processed, outcome  sql.NullString
		payload             string
		dest                = []any{&rec.ID, &integ, &source, &rec.EventType, &rec.Class, &targets, &rec.Truncated, &received, &processed, &outcome, &job, &payload}
		n                   = len(dest)
		err                 error
		receivedAt, procdAt time.Time
	)
	if !withPayload {
		n--
	}
	if err = r.Scan(dest[:n]...); err != nil {
		return Record{}, err
	}
	rec.Source = integrations.Type(source)
	if integ.Valid {
		rec.IntegrationID = &integ.Int64
	}
	if job.Valid {
		rec.JobID = &job.Int64
	}
	if err := json.Unmarshal([]byte(targets), &rec.Targets); err != nil {
		return Record{}, fmt.Errorf("webhook event %d: targets: %w", rec.ID, err)
	}
	if receivedAt, err = db.ParseTime(received); err != nil {
		return Record{}, fmt.Errorf("webhook event %d: received_at: %w", rec.ID, err)
	}
	rec.ReceivedAt = receivedAt
	if processed.Valid {
		if procdAt, err = db.ParseTime(processed.String); err != nil {
			return Record{}, fmt.Errorf("webhook event %d: processed_at: %w", rec.ID, err)
		}
		rec.ProcessedAt = &procdAt
	}
	if outcome.Valid {
		o := Outcome(outcome.String)
		rec.Outcome = &o
	}
	if withPayload {
		rec.Payload = json.RawMessage(payload)
	}
	return rec, nil
}

// pending returns the processor's view of a record.
func (r Record) pending() Pending {
	p := Pending{ID: r.ID, Class: r.Class, Targets: r.Targets, ReceivedAt: r.ReceivedAt}
	if r.IntegrationID != nil {
		p.IntegrationID = *r.IntegrationID
	}
	return p
}

// Insert stores an event of integration integrationID received at at. A Test event is recorded
// processed with outcome test, an ignored one with outcome ignored; every other event waits for
// the processor. It adds the payload to the stored total but prunes nothing: once the total
// exceeds MaxPayloadBytes (overBudget), the processor, told of the event, prunes. So an error
// always means the event was not stored.
func (s *Store) Insert(ctx context.Context, integrationID int64, app integrations.Type, ev Event, at time.Time) (Record, error) {
	if !ev.Class.Valid() {
		return Record{}, fmt.Errorf("store a webhook event: unknown class %q", ev.Class)
	}
	if len(ev.Payload) > MaxPayload {
		return Record{}, fmt.Errorf("store a webhook event: payload of %d bytes", len(ev.Payload))
	}
	targets, err := json.Marshal(nonNil(ev.Targets))
	if err != nil {
		return Record{}, fmt.Errorf("store a webhook event: %w", err)
	}
	stamp := db.FormatTime(at)
	var processed, outcome any
	switch ev.Class {
	case ClassTest:
		processed, outcome = stamp, string(OutcomeTest)
	case ClassIgnored:
		processed, outcome = stamp, string(OutcomeIgnored)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var id int64
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO webhook_events (integration_id, source, event_type, class, targets, payload,
			truncated, received_at, processed_at, outcome) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			integrationID, string(app), ev.EventType, string(ev.Class), string(targets), string(ev.Payload), ev.Truncated, stamp,
			processed, outcome).Scan(&id)
	})
	if err != nil {
		return Record{}, fmt.Errorf("store a webhook event: %w", err)
	}
	if s.bytesKnown {
		s.bytes += int64(len(ev.Payload))
	}
	s.updateOverLocked()
	rec := Record{ID: id, IntegrationID: &integrationID, Source: app, EventType: ev.EventType, Class: ev.Class,
		Targets: nonNil(ev.Targets), ReceivedAt: at.UTC(), Truncated: ev.Truncated, Summary: ev.Summary}
	if outcome != nil {
		o, t := Outcome(outcome.(string)), at.UTC()
		rec.Outcome, rec.ProcessedAt = &o, &t
	}
	return rec, nil
}

// measureLocked reads the stored payload total once; s.mu is held. octet_length reads the size
// from the record header, not the payload's overflow pages.
func (s *Store) measureLocked(ctx context.Context) error {
	if s.bytesKnown {
		return nil
	}
	s.measures++
	var n int64
	if err := s.db.Reader().QueryRowContext(ctx, `SELECT COALESCE(SUM(octet_length(payload)), 0) FROM webhook_events`).Scan(&n); err != nil {
		return fmt.Errorf("measure webhook payloads: %w", err)
	}
	s.bytes, s.bytesKnown = n, true
	s.updateOverLocked()
	return nil
}

// updateOverLocked sets the answer of overBudget from the payload total; s.mu is held.
func (s *Store) updateOverLocked() { s.over.Store(!s.bytesKnown || s.bytes > s.maxBytes) }

// overBudget reports whether a budget prune is due (pruneBudget): the stored payloads exceed
// MaxPayloadBytes, or their total is not known yet. It neither blocks nor does I/O.
func (s *Store) overBudget() bool { return s.over.Load() }

// lowWater is the payload total a budget prune brings the table down to.
func (s *Store) lowWater() int64 { return s.maxBytes / 100 * budgetLowWater }

// Prune deletes the events older than KeepFor, then the oldest beyond MaxRows, then, when the
// payloads exceed MaxPayloadBytes, the oldest until they fit in 90% of it. It returns the number
// of events deleted. When the payload total cannot be measured, the first two steps still run
// and the error is returned with their count.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	measureErr := s.measureLocked(ctx)
	cutoff := db.FormatTime(s.now().Add(-s.keepFor))
	var deleted, freed int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		deleted, freed = 0, 0
		n, b, err := deleteSizes(ctx, tx, `DELETE FROM webhook_events WHERE received_at < ? RETURNING octet_length(payload)`, cutoff)
		if err != nil {
			return err
		}
		deleted, freed = deleted+n, freed+b
		n, b, err = deleteSizes(ctx, tx, `DELETE FROM webhook_events WHERE id IN
			(SELECT id FROM webhook_events ORDER BY id DESC LIMIT -1 OFFSET ?) RETURNING octet_length(payload)`, s.maxRows)
		if err != nil {
			return err
		}
		deleted, freed = deleted+n, freed+b
		if s.bytesKnown && s.bytes-freed > s.maxBytes {
			n, b, err = trimOldest(ctx, tx, s.bytes-freed-s.lowWater())
			if err != nil {
				return err
			}
			deleted, freed = deleted+n, freed+b
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("prune webhook events: %w", err)
	}
	if s.bytesKnown {
		s.bytes -= freed
	}
	s.updateOverLocked()
	return deleted, measureErr
}

// pruneBudget deletes the oldest events until the payloads fit in 90% of MaxPayloadBytes, when
// they exceed it (measuring them first if their total is not known). It reads only the events it
// deletes. It returns the number of events deleted.
func (s *Store) pruneBudget(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.measureLocked(ctx); err != nil {
		return 0, err
	}
	if s.bytes <= s.maxBytes {
		return 0, nil
	}
	var deleted, freed int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		deleted, freed, err = trimOldest(ctx, tx, s.bytes-s.lowWater())
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("prune webhook events to their payload budget: %w", err)
	}
	s.bytes -= freed
	s.updateOverLocked()
	return deleted, nil
}

// deleteSizes runs a DELETE ... RETURNING <payload size> and returns the rows deleted and the
// payload bytes they held.
func deleteSizes(ctx context.Context, tx *sql.Tx, query string, args ...any) (n, bytes int64, err error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var b int64
		if err := rows.Scan(&b); err != nil {
			return 0, 0, err
		}
		n, bytes = n+1, bytes+b
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	return n, bytes, nil
}

// trimOldest deletes the oldest events that hold at least need payload bytes, reading the sizes
// of those events only, and returns the rows deleted and the bytes they held.
func trimOldest(ctx context.Context, tx *sql.Tx, need int64) (n, freed int64, err error) {
	if need <= 0 {
		return 0, 0, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, octet_length(payload) FROM webhook_events ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}
	var last int64
	for freed < need && rows.Next() {
		var id, b int64
		if err := rows.Scan(&id, &b); err != nil {
			_ = rows.Close()
			return 0, 0, err
		}
		last, freed = id, freed+b
	}
	err = rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	if err != nil || last == 0 {
		return 0, 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM webhook_events WHERE id <= ?`, last)
	if err != nil {
		return 0, 0, err
	}
	n, _ = res.RowsAffected()
	return n, freed, nil
}

// Unprocessed returns up to limit unprocessed events with an id greater than afterID, in id order.
func (s *Store) Unprocessed(ctx context.Context, afterID int64, limit int) ([]Pending, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT `+recordColumns+` FROM webhook_events
		WHERE processed_at IS NULL AND id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("read unprocessed webhook events: %w", err)
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		rec, err := scanRecord(rows, false)
		if err != nil {
			return nil, fmt.Errorf("read unprocessed webhook events: %w", err)
		}
		out = append(out, rec.pending())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read unprocessed webhook events: %w", err)
	}
	return out, nil
}

// MarkProcessed records the outcome of the unprocessed events ids (jobID 0: none), in one write,
// and returns how many it changed (events already processed, or pruned, are left alone). When job
// jobID is cancelled already, nothing is marked and the error wraps errJobCancelled: the job's
// OnFinish hook has run (or is running) without these events, so they would never be re-armed.
// The check is in the same write, and the job's cancel commits before its hook runs, so either
// the hook finds the events or this write sees the cancel.
func (s *Store) MarkProcessed(ctx context.Context, ids []int64, outcome Outcome, jobID int64, at time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var job any
	if jobID != 0 {
		job = jobID
	}
	var changed int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		changed = 0
		if jobID != 0 {
			var status string
			err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return err
			case jobs.Status(status) == jobs.StatusCancelled:
				return errJobCancelled
			}
		}
		for len(ids) > 0 {
			chunk := ids[:min(len(ids), 500)]
			ids = ids[len(chunk):]
			args := []any{db.FormatTime(at), string(outcome), job}
			for _, id := range chunk {
				args = append(args, id)
			}
			res, err := tx.ExecContext(ctx, `UPDATE webhook_events SET processed_at = ?, outcome = ?, job_id = ?
				WHERE processed_at IS NULL AND id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`, args...)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			changed += n
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("mark webhook events processed: %w", err)
	}
	return changed, nil
}

// Rearm makes the events that were queued into (or coalesced with) job jobID unprocessed again
// (the job was cancelled, design §7.3), and returns them.
func (s *Store) Rearm(ctx context.Context, jobID int64) ([]Pending, error) {
	var out []Pending
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `UPDATE webhook_events SET processed_at = NULL, outcome = NULL, job_id = NULL
			WHERE job_id = ? AND outcome IN ('queued', 'coalesced') RETURNING `+recordColumns, jobID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			rec, err := scanRecord(rows, false)
			if err != nil {
				return err
			}
			out = append(out, rec.pending())
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("re-arm the webhook events of job %d: %w", jobID, err)
	}
	return out, nil
}

// rearmCancelled makes the events queued into (or coalesced with) a refresh that is cancelled
// unprocessed again, and returns how many: the refresh's OnFinish hook re-arms them (Rearm), but
// the process may have stopped between the cancel and the hook. Processor.Start calls it before
// it reads the unprocessed events.
func (s *Store) rearmCancelled(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE webhook_events SET processed_at = NULL, outcome = NULL, job_id = NULL
			WHERE job_id IS NOT NULL AND outcome IN ('queued', 'coalesced')
			AND job_id IN (SELECT id FROM jobs WHERE status = 'cancelled')`)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("re-arm the webhook events of cancelled refreshes: %w", err)
	}
	return n, nil
}

// Query selects events for List. Zero values mean "any"; Outcome "pending" selects unprocessed
// events. Page is 1-based, PageSize 1-500 (default 50).
type Query struct {
	IntegrationID int64
	EventType     string
	Outcome       string
	Page          int
	PageSize      int
}

// Page is one page of List, newest first.
type Page struct {
	Page         int      `json:"page"`
	PageSize     int      `json:"pageSize"`
	TotalRecords int64    `json:"totalRecords"`
	Records      []Record `json:"records"`
}

// ValidationError is an invalid query (API: 400).
type ValidationError string

// Error implements error.
func (e ValidationError) Error() string { return string(e) }

// List returns a page of events, newest first, each with its summary.
func (s *Store) List(ctx context.Context, q Query) (Page, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	switch {
	case q.PageSize < 1:
		q.PageSize = 50
	case q.PageSize > 500:
		q.PageSize = 500
	}
	where, args := []string{"1 = 1"}, []any{}
	if q.IntegrationID != 0 {
		where, args = append(where, "integration_id = ?"), append(args, q.IntegrationID)
	}
	if q.EventType != "" {
		where, args = append(where, "event_type = ?"), append(args, q.EventType)
	}
	switch Outcome(q.Outcome) {
	case "":
	case "pending":
		where = append(where, "processed_at IS NULL")
	case OutcomeTest, OutcomeIgnored, OutcomeQueued, OutcomeCoalesced, OutcomeFailed:
		where, args = append(where, "outcome = ?"), append(args, q.Outcome)
	default:
		return Page{}, ValidationError(fmt.Sprintf("outcome must be pending, test, ignored, queued, coalesced or failed, got %q", q.Outcome))
	}
	cond := strings.Join(where, " AND ")
	page := Page{Page: q.Page, PageSize: q.PageSize, Records: []Record{}}
	if err := s.db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM webhook_events WHERE `+cond, args...).Scan(&page.TotalRecords); err != nil {
		return Page{}, fmt.Errorf("count webhook events: %w", err)
	}
	offset := int64(q.Page-1) * int64(q.PageSize)
	if offset >= page.TotalRecords {
		return page, nil
	}
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT `+recordColumns+`, payload FROM webhook_events WHERE `+cond+
		` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, q.PageSize, offset)...)
	if err != nil {
		return Page{}, fmt.Errorf("list webhook events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		rec, err := scanRecord(rows, true)
		if err != nil {
			return Page{}, fmt.Errorf("list webhook events: %w", err)
		}
		rec.Summary = Summarize(rec.Source, rec.Payload, rec.Truncated)
		rec.Payload = nil
		page.Records = append(page.Records, rec)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("list webhook events: %w", err)
	}
	return page, nil
}

// Get returns one event with its payload and summary, or an error wrapping ErrNotFound.
func (s *Store) Get(ctx context.Context, id int64) (Record, error) {
	rec, err := scanRecord(s.db.Reader().QueryRowContext(ctx, `SELECT `+recordColumns+`, payload FROM webhook_events WHERE id = ?`, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("webhook event %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Record{}, fmt.Errorf("read webhook event %d: %w", id, err)
	}
	rec.Summary = Summarize(rec.Source, rec.Payload, rec.Truncated)
	return rec, nil
}

// Activity is an integration's recent webhook activity (GET /integrations/{id}/webhook).
type Activity struct {
	LastEventAt *time.Time `json:"lastEventAt"`
	LastTestAt  *time.Time `json:"lastTestAt"`
	// Last24h counts the events received in the last 24 hours.
	Last24h int64 `json:"last24h"`
	// Recent are the newest events (at most 10), with their summaries.
	Recent []Record `json:"recent"`
}

// Activity returns the webhook activity of integration id at now.
func (s *Store) Activity(ctx context.Context, id int64, now time.Time) (Activity, error) {
	var (
		a          Activity
		last, test sql.NullString
	)
	err := s.db.Reader().QueryRowContext(ctx, `SELECT MAX(received_at), MAX(CASE WHEN class = 'test' THEN received_at END),
		COUNT(CASE WHEN received_at >= ? THEN 1 END) FROM webhook_events WHERE integration_id = ?`,
		db.FormatTime(now.Add(-24*time.Hour)), id).Scan(&last, &test, &a.Last24h)
	if err != nil {
		return Activity{}, fmt.Errorf("read the webhook activity of integration %d: %w", id, err)
	}
	for _, p := range []struct {
		s   sql.NullString
		dst **time.Time
	}{{last, &a.LastEventAt}, {test, &a.LastTestAt}} {
		if p.s.Valid {
			t, err := db.ParseTime(p.s.String)
			if err != nil {
				return Activity{}, fmt.Errorf("read the webhook activity of integration %d: %w", id, err)
			}
			*p.dst = &t
		}
	}
	page, err := s.List(ctx, Query{IntegrationID: id, PageSize: 10})
	if err != nil {
		return Activity{}, err
	}
	a.Recent = page.Records
	return a, nil
}
