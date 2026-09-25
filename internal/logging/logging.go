// Package logging builds Bunkarr's slog logger: JSON lines to a size-rotated file in
// /config/logs plus stdout (text or JSON), with secrets redacted from both.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
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
	if a.Value.Kind() != slog.KindGroup && sensitiveKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	return a
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
