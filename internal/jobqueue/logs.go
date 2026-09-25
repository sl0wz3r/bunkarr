package jobqueue

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// Log list limits.
const (
	// DefaultLogLimit is the number of log lines ListLogs returns when limit <= 0.
	DefaultLogLimit = 200
	// MaxLogLimit caps ListLogs.
	MaxLogLimit = 1000
)

// LogEntry is one job log line.
type LogEntry struct {
	ID int64     `json:"id"`
	At time.Time `json:"at"`
	// Level is debug, info, warn or error.
	Level   string          `json:"level"`
	Message string          `json:"message"`
	Fields  json.RawMessage `json:"fields"`
}

// LevelName maps a slog level to the stored level text: debug, info, warn or error.
func LevelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

// AppendLog stores a job log line and returns its id. The message and the fields (a JSON object;
// nil means {}) are passed through logging.RedactSecrets first. Runners log through
// jobs.Reporter.Log, which also redacts fields by key name.
func (s *Store) AppendLog(ctx context.Context, jobID int64, level slog.Level, msg string, fields json.RawMessage) (int64, error) {
	f := "{}"
	if len(fields) > 0 {
		f = redactText(string(fields))
		if !json.Valid([]byte(f)) || !strings.HasPrefix(strings.TrimSpace(f), "{") {
			return 0, ValidationError("job log fields must be a JSON object")
		}
	}
	var id int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO job_logs (job_id, at, level, message, fields) VALUES (?, ?, ?, ?, ?) RETURNING id`,
			jobID, db.FormatTime(s.now()), LevelName(level), redactText(msg), f).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("append log to job %d: %w", jobID, err)
	}
	return id, nil
}

// ListLogs returns up to limit log lines of a job with id > afterID, oldest first (limit <= 0
// means DefaultLogLimit; capped at MaxLogLimit). A job that does not exist is ErrNotFound.
func (s *Store) ListLogs(ctx context.Context, jobID, afterID int64, limit int) ([]LogEntry, error) {
	if limit <= 0 {
		limit = DefaultLogLimit
	}
	limit = min(limit, MaxLogLimit)
	out := []LogEntry{}
	err := s.readTx(ctx, func(tx *sql.Tx) error {
		ok, err := jobExists(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("job %d: %w", jobID, ErrNotFound)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, at, level, message, fields FROM job_logs
			WHERE job_id = ? AND id > ? ORDER BY id LIMIT ?`, jobID, afterID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e LogEntry
			var at, fields string
			if err := rows.Scan(&e.ID, &at, &e.Level, &e.Message, &fields); err != nil {
				return err
			}
			if e.At, err = db.ParseTime(at); err != nil {
				return fmt.Errorf("log %d: %w", e.ID, err)
			}
			e.Fields = json.RawMessage(fields)
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list logs of job %d: %w", jobID, err)
	}
	return out, nil
}

// sensitiveKey reports whether a field name holds a secret. It mirrors internal/logging's
// (unexported) key list so job logs and the process log redact the same names.
func sensitiveKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"password", "passwd", "apikey", "api_key", "api-key", "token", "secret", "authorization", "cookie", "credential"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// redactArgs turns slog key/value args into attributes with secrets removed: attributes whose key
// looks sensitive become [REDACTED]; strings, errors, Stringers and any other value (rendered to
// JSON, where object keys are checked by name too) have registered secret values replaced.
func redactArgs(args []any) []slog.Attr {
	if len(args) == 0 {
		return nil
	}
	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "", 0)
	rec.Add(args...)
	out := make([]slog.Attr, 0, rec.NumAttrs())
	rec.Attrs(func(a slog.Attr) bool {
		if !a.Equal(slog.Attr{}) {
			out = append(out, redactAttr(a))
		}
		return true
	})
	return out
}

func redactAttr(a slog.Attr) slog.Attr {
	if sensitiveKey(a.Key) {
		return slog.String(a.Key, logging.Redacted)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		g := v.Group()
		out := make([]slog.Attr, 0, len(g))
		for _, ga := range g {
			out = append(out, redactAttr(ga))
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindString:
		return slog.String(a.Key, redactText(v.String()))
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, redactText(x.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, redactText(x.String()))
		default:
			if raw, ok := redactJSONValue(x); ok {
				return slog.Any(a.Key, raw)
			}
			return slog.String(a.Key, redactText(fmt.Sprint(x)))
		}
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}

// redactJSONValue renders v as JSON with sensitive object keys and registered secret values
// redacted. ok is false when v does not marshal.
func redactJSONValue(v any) (json.RawMessage, bool) {
	raw, err := marshalNoEscape(v)
	if err != nil {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, false
	}
	out, err := marshalNoEscape(redactTree(tree))
	if err != nil {
		return nil, false
	}
	return json.RawMessage(out), true
}

func redactTree(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if sensitiveKey(k) {
				x[k] = logging.Redacted
			} else {
				x[k] = redactTree(e)
			}
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = redactTree(e)
		}
		return x
	case string:
		return redactText(x)
	default:
		return v
	}
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// fieldsJSON renders already redacted attributes as a JSON object (groups become nested objects).
func fieldsJSON(attrs []slog.Attr) json.RawMessage {
	if len(attrs) == 0 {
		return json.RawMessage("{}")
	}
	b, err := marshalNoEscape(attrsMap(attrs))
	if err != nil {
		// Unreachable for redacted attributes (every value is JSON-safe); keep the line anyway.
		return json.RawMessage("{}")
	}
	return json.RawMessage(b)
}

func attrsMap(attrs []slog.Attr) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, a := range attrs {
		v := a.Value.Resolve()
		if v.Kind() == slog.KindGroup {
			g := attrsMap(v.Group())
			if a.Key == "" {
				for k, e := range g {
					out[k] = e
				}
			} else {
				out[a.Key] = g
			}
			continue
		}
		out[a.Key] = jsonValue(v)
	}
	return out
}

func jsonValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		f := v.Float64()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return strconv.FormatFloat(f, 'g', -1, 64)
		}
		return f
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	default:
		// After redactAttr this is a json.RawMessage or a string.
		return v.Any()
	}
}
