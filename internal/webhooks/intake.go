package webhooks

import (
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
)

// Limits of the intake (S13).
const (
	// DefaultRate is how many events per second one integration may send...
	DefaultRate = 20
	// DefaultBurst ...with bursts of this many (429 beyond).
	DefaultBurst = 2000
	// MaxBacklog is the number of unprocessed events at which the intake answers 503.
	MaxBacklog = 10000
	// DefaultSlots is how many bodies are read at a time...
	DefaultSlots = 4
	// DefaultSlotWait ...and how long a request waits for a slot (503 after).
	DefaultSlotWait = 10 * time.Second
	// genericKeep is how long a use of the generic route is remembered.
	genericKeep = 7 * 24 * time.Hour
)

// IntakeOptions configures an Intake. Zero values take the defaults above.
type IntakeOptions struct {
	// Store records the events (required).
	Store *Store
	// Processor is told about every stored event and reports the backlog (required).
	Processor *Processor
	// Log receives the intake's log lines; nil discards them.
	Log *slog.Logger
	// Now is the clock of received_at and of the rate limits; nil means time.Now.
	Now func() time.Time

	Rate       float64
	Burst      int
	MaxBacklog int
	Slots      int
	SlotWait   time.Duration
}

// Intake is the handler of the webhook routes behind the credential and integration checks of
// internal/api (design §7.1 checks 3-5): the rate and the backlog, a body-read slot, the content
// type, the size and the JSON shape; then the event is stored, the processor is told, and the
// answer is 200 {}. It makes no outbound call and runs no job logic.
type Intake struct {
	o     IntakeOptions
	log   *slog.Logger
	now   func() time.Time
	slots chan struct{}

	mu      sync.Mutex
	buckets map[int64]*bucket
	// generic remembers when each integration last received an event on the generic route (in
	// memory: a hint for the webhook panel, lost at restart).
	generic map[int64]time.Time
}

// NewIntake returns an intake.
func NewIntake(o IntakeOptions) *Intake {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rate <= 0 {
		o.Rate = DefaultRate
	}
	if o.Burst <= 0 {
		o.Burst = DefaultBurst
	}
	if o.MaxBacklog <= 0 {
		o.MaxBacklog = MaxBacklog
	}
	if o.Slots <= 0 {
		o.Slots = DefaultSlots
	}
	if o.SlotWait <= 0 {
		o.SlotWait = DefaultSlotWait
	}
	return &Intake{o: o, log: o.Log, now: o.Now, slots: make(chan struct{}, o.Slots), buckets: map[int64]*bucket{},
		generic: map[int64]time.Time{}}
}

// bucket is a token bucket of one integration.
type bucket struct {
	tokens float64
	last   time.Time
}

// allow takes a token from integration id's bucket.
func (in *Intake) allow(id int64, now time.Time) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	b := in.buckets[id]
	if b == nil {
		b = &bucket{tokens: float64(in.o.Burst), last: now}
		in.buckets[id] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(float64(in.o.Burst), b.tokens+elapsed.Seconds()*in.o.Rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// NoteGenericRoute records that integration id received an event on the generic route
// /webhook/{app} (the webhook panel warns about the other integrations of the app: one of them
// may be sending with the wrong key).
func (in *Intake) NoteGenericRoute(id int64) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.generic[id] = in.now()
}

// GenericRouteUses returns the integrations that received events on the generic route in the
// last 7 days, with the time of the last one.
func (in *Intake) GenericRouteUses() map[int64]time.Time {
	in.mu.Lock()
	defer in.mu.Unlock()
	cutoff := in.now().Add(-genericKeep)
	out := map[int64]time.Time{}
	for id, t := range in.generic {
		if t.After(cutoff) {
			out[id] = t
		} else {
			delete(in.generic, id)
		}
	}
	return out
}

// Serve handles one webhook call of integration id, of type app, whose credential and
// integration the caller has checked.
func (in *Intake) Serve(w http.ResponseWriter, r *http.Request, id int64, app integrations.Type) {
	now := in.now()
	if !in.allow(id, now) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "too many webhook events from this integration; slow down")
		return
	}
	if n := in.o.Processor.Backlog(); n >= in.o.MaxBacklog {
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusServiceUnavailable, "too many webhook events are waiting to be processed; try again later")
		return
	}
	t := time.NewTimer(in.o.SlotWait)
	select {
	case in.slots <- struct{}{}:
		t.Stop()
	case <-t.C:
		writeError(w, http.StatusServiceUnavailable, "the webhook intake is busy; try again later")
		return
	case <-r.Context().Done():
		t.Stop()
		return
	}
	defer func() { <-in.slots }()
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "a webhook body must be application/json")
		return
	}
	if r.ContentLength > MaxBody {
		writeError(w, http.StatusRequestEntityTooLarge, ErrTooLarge.Error())
		return
	}
	ev, err := Parse(app, r.Body)
	var ie *InvalidError
	switch {
	case errors.Is(err, ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case errors.As(err, &ie):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "cannot read the webhook body")
		return
	}
	rec, err := in.o.Store.Insert(r.Context(), id, app, ev, now)
	if err != nil {
		in.log.Error("Could not store a webhook event", "integrationId", id, "eventType", ev.EventType, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	faultinject.Point(PointAfterStore)
	in.o.Processor.Notify(rec)
	in.log.Debug("Webhook event received", "integrationId", id, "eventType", ev.EventType, "class", string(ev.Class),
		"items", rec.Targets, "eventId", rec.ID)
	writeJSON(w, http.StatusOK, struct{}{})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes {"message": msg}, the shape of every API error.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}
