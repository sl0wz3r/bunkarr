// Package notify sends Bunkarr's job notifications through an Apprise API server
// (docs/design/phase1.md §7): the notifications table (Store), a small Apprise API client (Client)
// and the Dispatcher that turns finished jobs into messages (hooked to the job manager's OnFinish).
//
// Apprise URLs carry the credentials of the services they notify, so they are treated as secrets
// (safety rule S8): sealed at rest with the AAD "notification:<id>:urls" (ADR 0003), never
// returned (Notification.HasURLs instead), sent only in request bodies to the Apprise API they were
// saved with (redirects are not followed; another API URL needs the URLs sent again), held in
// the redaction registry while stored (logging.SetSecrets, owner "notification:<id>"; released
// when they are replaced or deleted), and never included in errors or log lines.
package notify

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Kind is a notification service type. Phase 1 supports Apprise only.
type Kind string

// KindApprise sends through an Apprise API server (https://github.com/caronc/apprise-api).
const KindApprise Kind = "apprise"

// Limits on stored values.
const (
	// MaxNameLen is the longest accepted name, in characters.
	MaxNameLen = 64
	// MaxAPIURLLen is the longest accepted Apprise API URL, in bytes.
	MaxAPIURLLen = 2048
	// MaxURLsLen is the longest accepted Apprise URL list, in bytes.
	MaxURLsLen = 16 << 10
)

// ErrNotFound means no notification has the requested id.
var ErrNotFound = errors.New("notification not found")

// ValidationError is a user-facing input error. Its message never contains the Apprise URLs or
// the API URL that was rejected (either may hold credentials).
type ValidationError string

// Error returns the user-facing message.
func (e ValidationError) Error() string { return string(e) }

// Notification is a saved notification target as the API returns it. The Apprise URLs are
// write-only: HasURLs says whether some are stored.
type Notification struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Kind    Kind   `json:"kind"`
	Enabled bool   `json:"enabled"`
	// APIURL is the Apprise API base URL, e.g. "http://apprise:8000" (no trailing slash).
	APIURL string `json:"apiUrl"`
	// ConfigKey selects Apprise's stateful mode (POST {apiUrl}/notify/{configKey}); "" means
	// stateless mode with the stored URLs.
	ConfigKey string `json:"configKey"`
	HasURLs   bool   `json:"hasUrls"`
	// OnFailure, OnWarning and OnSuccess choose the job events that are sent (§7).
	OnFailure bool      `json:"onFailure"`
	OnWarning bool      `json:"onWarning"`
	OnSuccess bool      `json:"onSuccess"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Input creates or updates a notification (and describes an unsaved one for Dispatcher.Test).
//
// On create, nil booleans take the defaults enabled, onFailure and onWarning = true, onSuccess =
// false. On update, nil booleans and an empty URLs keep the stored values; the other fields are
// replaced. An empty URLs is refused when APIURL changes (stateless mode): the stored URLs are
// only sent to the API URL they were saved with. Setting ConfigKey (stateful mode) removes stored
// URLs, which that mode does not use.
type Input struct {
	Name string `json:"name"`
	// Kind is optional; "" means KindApprise.
	Kind      Kind   `json:"kind,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
	APIURL    string `json:"apiUrl"`
	ConfigKey string `json:"configKey,omitempty"`
	// URLs is the write-only Apprise URL list (comma and/or whitespace separated), required in
	// stateless mode.
	URLs      string `json:"urls,omitempty"`
	OnFailure *bool  `json:"onFailure,omitempty"`
	OnWarning *bool  `json:"onWarning,omitempty"`
	OnSuccess *bool  `json:"onSuccess,omitempty"`
}

// MessageType is Apprise's notification type.
type MessageType string

// Apprise notification types.
const (
	TypeInfo    MessageType = "info"
	TypeSuccess MessageType = "success"
	TypeWarning MessageType = "warning"
	TypeFailure MessageType = "failure"
)

// Valid reports whether t is one of Apprise's notification types.
func (t MessageType) Valid() bool {
	switch t {
	case TypeInfo, TypeSuccess, TypeWarning, TypeFailure:
		return true
	}
	return false
}

// Message is one notification.
type Message struct {
	Title string
	// Body is required by Apprise.
	Body string
	Type MessageType
}

// Target is where Client.Send delivers: a saved notification with its URLs opened, or an unsaved
// one being tested. It holds secrets: String, GoString and LogValue leave the URLs out, and it
// does not marshal them to JSON.
type Target struct {
	// ID is 0 for an unsaved target.
	ID     int64
	Name   string
	APIURL string
	// ConfigKey selects stateful mode; URLs is then unused.
	ConfigKey string
	URLs      string `json:"-"`
}

// Stateful reports whether t sends through a configuration stored in Apprise (ConfigKey) rather
// than its own URLs.
func (t Target) Stateful() bool { return t.ConfigKey != "" }

// String describes t without its URLs.
func (t Target) String() string {
	name := t.Name
	if name == "" {
		name = "unsaved"
	}
	return fmt.Sprintf("notification %q (%s)", name, t.APIURL)
}

// GoString describes t without its URLs (for %#v).
func (t Target) GoString() string { return "notify.Target{" + t.String() + "}" }

// LogValue describes t without its URLs (for slog).
func (t Target) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int64("id", t.ID),
		slog.String("name", t.Name),
		slog.String("apiUrl", t.APIURL),
		slog.Bool("stateful", t.Stateful()),
	)
}
