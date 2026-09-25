// Package logging builds Bunkarr's slog logger: JSON lines to a size-rotated file in
// /config/logs plus stdout (text or JSON), with secrets redacted from both.
package logging

import (
	"cmp"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
)

// Options configures New.
type Options struct {
	// Dir is the log directory; "" disables the file log (tests, one-shot commands).
	Dir string
	// Level is debug, info, warn or error.
	Level string
	// StdoutFormat is text or json.
	StdoutFormat string
	// Stdout defaults to os.Stdout.
	Stdout io.Writer
	// MaxBytes is the size at which the file rotates (default 10 MiB).
	MaxBytes int64
	// Keep is the number of rotated files kept (default 5).
	Keep int
}

// New returns the logger and a closer for the log file.
func New(o Options) (*slog.Logger, io.Closer, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(o.Level)); err != nil {
		return nil, nil, fmt.Errorf("log level %q: %w", o.Level, err)
	}
	hopts := &slog.HandlerOptions{Level: level, ReplaceAttr: redactAttr}
	stdout := o.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	var handlers []slog.Handler
	if o.StdoutFormat == "json" {
		handlers = append(handlers, slog.NewJSONHandler(stdout, hopts))
	} else {
		handlers = append(handlers, slog.NewTextHandler(stdout, hopts))
	}

	var closer io.Closer = io.NopCloser(nil)
	if o.Dir != "" {
		rw, err := NewRotatingWriter(o.Dir, "bunkarr", o.MaxBytes, o.Keep)
		if err != nil {
			return nil, nil, err
		}
		handlers = append(handlers, slog.NewJSONHandler(rw, hopts))
		closer = rw
	}
	return slog.New(slog.NewMultiHandler(handlers...)), closer, nil
}

// Redacted replaces secret values in logs, API responses and diagnostics.
const Redacted = "[REDACTED]"

// sensitiveKey reports whether an attribute, header or query parameter name holds a secret.
func sensitiveKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"password", "passwd", "apikey", "api_key", "api-key", "token", "secret", "authorization", "cookie", "credential"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	if sensitiveKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		if s := a.Value.String(); ContainsSecret(s) {
			return slog.String(a.Key, RedactSecrets(s))
		}
	case slog.KindAny:
		// Errors and Stringers are rendered to text, so a secret inside (e.g. a token in a URL
		// of a *url.Error) cannot slip through.
		switch v := a.Value.Any().(type) {
		case error:
			if s := v.Error(); ContainsSecret(s) {
				return slog.String(a.Key, RedactSecrets(s))
			}
		case fmt.Stringer:
			if s := v.String(); ContainsSecret(s) {
				return slog.String(a.Key, RedactSecrets(s))
			}
		}
	}
	return a
}

// minSecretLen keeps short, common strings out of the registry (they would redact normal text).
const minSecretLen = 8

// maxReleased bounds how many released values (see SetSecrets) stay redacted. When more are
// kept, the oldest are dropped down to maxReleased/2, so the cost of a drop is amortized.
const maxReleased = 4096

// secretRegistry is the set of secret values that are redacted. A value is redacted while it is
// held (registered with RegisterSecret, or held by an owner through SetSecrets) and for a while
// after its last holder released it.
type secretRegistry struct {
	// sorted is every redacted value (held or released), longest first, so a secret that contains
	// another registered secret (a Plex token inside an Apprise URL) is replaced whole before its
	// shorter part is.
	sorted []string
	// holders counts the holders of each held value: its owners, plus one when it is pinned.
	holders map[string]int
	// pinned is the set of values registered with RegisterSecret (held for good).
	pinned map[string]bool
	// owned is the values each owner holds.
	owned map[string][]string
	// released maps each released value that is still redacted to its release number (n).
	released map[string]uint64
	n        uint64
}

var (
	secretsMu sync.RWMutex
	secrets   secretRegistry
)

// hold adds a holder to v and reports whether v must be added to sorted (it was not redacted).
func (r *secretRegistry) hold(v string) bool {
	if r.holders == nil {
		r.holders = make(map[string]int)
	}
	r.holders[v]++
	if r.holders[v] > 1 {
		return false
	}
	if _, ok := r.released[v]; ok {
		delete(r.released, v)
		return false
	}
	return true
}

// release removes a holder of v; a value without holders stays redacted as a released value.
func (r *secretRegistry) release(v string) {
	if r.holders[v] > 1 {
		r.holders[v]--
		return
	}
	delete(r.holders, v)
	if r.released == nil {
		r.released = make(map[string]uint64)
	}
	r.n++
	r.released[v] = r.n
}

// add merges values (none of them in sorted yet) into sorted.
func (r *secretRegistry) add(values []string) {
	if len(values) == 0 {
		return
	}
	slices.SortFunc(values, compareSecrets)
	merged := make([]string, 0, len(r.sorted)+len(values))
	i, j := 0, 0
	for i < len(r.sorted) || j < len(values) {
		if j == len(values) || (i < len(r.sorted) && compareSecrets(r.sorted[i], values[j]) <= 0) {
			merged, i = append(merged, r.sorted[i]), i+1
		} else {
			merged, j = append(merged, values[j]), j+1
		}
	}
	r.sorted = merged
}

// trim drops the oldest released values when more than maxReleased are kept. Held values are
// never dropped.
func (r *secretRegistry) trim() {
	if len(r.released) <= maxReleased {
		return
	}
	order := make([]string, 0, len(r.released))
	for v := range r.released {
		order = append(order, v)
	}
	slices.SortFunc(order, func(a, b string) int { return cmp.Compare(r.released[a], r.released[b]) })
	drop := make(map[string]bool, len(order)-maxReleased/2)
	for _, v := range order[:len(order)-maxReleased/2] {
		drop[v] = true
		delete(r.released, v)
	}
	r.sorted = slices.DeleteFunc(r.sorted, func(v string) bool { return drop[v] })
}

// RegisterSecret adds a secret VALUE (a Plex token, an Apprise URL, an API key) to the set that is
// replaced by [REDACTED] wherever it appears in a log message or attribute, in addition to the
// key-based redaction, for good. Values shorter than 8 characters are ignored. Safe for concurrent
// use.
//
// Use it for a secret Bunkarr reads but does not store (a PlexOnlineToken in Plex's
// Preferences.xml). A secret stored in a row goes through SetSecrets, so it is released when the
// row changes. A value that is only held for one request (a token typed into a Test form) goes
// through RedactValues: request input never enters the process-wide registry.
func RegisterSecret(value string) {
	if len(value) < minSecretLen {
		return
	}
	secretsMu.Lock()
	defer secretsMu.Unlock()
	if secrets.pinned[value] {
		return
	}
	if secrets.pinned == nil {
		secrets.pinned = make(map[string]bool)
	}
	secrets.pinned[value] = true
	if secrets.hold(value) {
		secrets.add([]string{value})
	}
}

// SetSecrets makes values the secret values owner holds, in place of the ones it held before.
// owner names one stored row ("integration:3"); SetSecrets(owner) with no values releases what
// it held (the row was deleted or its secret removed). Held values are redacted like registered
// ones and are never dropped. A released value that nothing else holds stays redacted among the
// most recently released values (maxReleased at most), so a request that still uses a replaced
// token cannot log it, while the registry stays bounded by the secrets that are stored. Values
// shorter than 8 characters are ignored. Safe for concurrent use.
func SetSecrets(owner string, values ...string) {
	next := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if len(v) >= minSecretLen && !seen[v] {
			seen[v] = true
			next = append(next, v)
		}
	}
	secretsMu.Lock()
	defer secretsMu.Unlock()
	var added []string
	for _, v := range next {
		if secrets.hold(v) {
			added = append(added, v)
		}
	}
	for _, v := range secrets.owned[owner] {
		secrets.release(v)
	}
	if len(next) == 0 {
		delete(secrets.owned, owner)
	} else {
		if secrets.owned == nil {
			secrets.owned = make(map[string][]string)
		}
		secrets.owned[owner] = next
	}
	secrets.add(added)
	secrets.trim()
}

// compareSecrets orders longer values first, then lexically.
func compareSecrets(a, b string) int {
	if len(a) != len(b) {
		return len(b) - len(a)
	}
	return strings.Compare(a, b)
}

// SensitiveKey reports whether an attribute, header, field or query parameter NAME holds a
// secret (password, token, API key, cookie, ...). Other packages that persist logs use it so
// they redact the same names as the process log.
func SensitiveKey(name string) bool { return sensitiveKey(name) }

// ContainsSecret reports whether s contains a registered secret value.
func ContainsSecret(s string) bool {
	if len(s) < minSecretLen {
		return false
	}
	secretsMu.RLock()
	defer secretsMu.RUnlock()
	for _, v := range secrets.sorted {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// RedactSecrets returns s with every registered secret value replaced by [REDACTED]. Use it on
// any text that leaves the process (job logs, API error messages, notifications).
func RedactSecrets(s string) string {
	return RedactValues(s)
}

// RedactValues is RedactSecrets that also replaces values (those of at least 8 bytes) without
// registering them: use it for a secret that is only held for one request, such as a token typed
// into a Test form, so request input never enters the process-wide registry.
func RedactValues(s string, values ...string) string {
	if len(s) < minSecretLen {
		return s
	}
	local := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) >= minSecretLen {
			local = append(local, v)
		}
	}
	slices.SortFunc(local, compareSecrets)
	secretsMu.RLock()
	defer secretsMu.RUnlock()
	// Merge both lists longest first, so a value that contains another is replaced whole.
	i, j := 0, 0
	for i < len(secrets.sorted) || j < len(local) {
		var v string
		if j == len(local) || (i < len(secrets.sorted) && compareSecrets(secrets.sorted[i], local[j]) <= 0) {
			v, i = secrets.sorted[i], i+1
		} else {
			v, j = local[j], j+1
		}
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, Redacted)
		}
	}
	return s
}

// RedactURL renders u with the values of secret-looking query parameters and any userinfo
// password replaced, for logging.
func RedactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	if c.User != nil {
		if _, has := c.User.Password(); has {
			c.User = url.UserPassword(c.User.Username(), Redacted)
		}
	}
	if c.RawQuery != "" {
		q := c.Query()
		changed := false
		for k := range q {
			if sensitiveKey(k) {
				q[k] = []string{Redacted}
				changed = true
			}
		}
		if changed {
			c.RawQuery = q.Encode()
		}
	}
	return c.String()
}
