package webhooks

import (
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
)

// Limits of a webhook body (S13).
const (
	// MaxBody is the largest body the intake reads (413 beyond).
	MaxBody = 16 << 20
	// MaxPayload is the largest payload stored as it came (compacted); a larger one is stored as
	// {"truncated":true,"eventType":…,"ids":[…]}.
	MaxPayload = 64 << 10
	// maxEventType is the longest eventType accepted.
	maxEventType = 64
	// maxKept bounds the raw bytes held while a body is decoded, to build the stored payload: a
	// body larger than that is stored truncated whatever its compact size.
	maxKept = 1 << 20
	// maxSummaryFiles and maxSummaryText bound the event list's summary of one event.
	maxSummaryFiles = 50
	maxSummaryText  = 1024
)

// ErrTooLarge means the body is larger than MaxBody (HTTP 413).
var ErrTooLarge = errors.New("the webhook body is larger than 16 MiB")

// InvalidError is a body that is not a single JSON object with an eventType string of 1-64
// characters (HTTP 400).
type InvalidError struct{ msg string }

// Error implements error.
func (e *InvalidError) Error() string { return "invalid webhook body: " + e.msg }

func invalid(format string, args ...any) error {
	return &InvalidError{msg: fmt.Sprintf(format, args...)}
}

// Event is what Parse keeps of a webhook body.
type Event struct {
	// EventType is the body's eventType as sent (Test, Download, MovieFileDelete, ...).
	EventType string
	// Class is how the processor schedules the event.
	Class Class
	// Targets are the *arr item ids the event names (movie.id, series.id or artist.id): empty for
	// Test and ignored events (S12: Test is never looked up; Sonarr's names series 1).
	Targets []int64
	// Payload is what is stored: the compact body when it is at most MaxPayload, else a
	// {"truncated":true,"eventType":…,"ids":[…]} object (Truncated set); {} for ignored events.
	Payload   []byte
	Truncated bool
	// Summary is what the event list shows.
	Summary Summary
}

// Summary is the event list's summary of an event (design §13 WebhookEvent.summary). The file
// paths are as the *arr reports them: shown, never opened (S12).
type Summary struct {
	Title   string   `json:"title,omitempty"`
	ItemIDs []int64  `json:"itemIds"`
	Files   []string `json:"files"`
}

// fields are the parts of a body Parse decodes.
type fields struct {
	eventType    string
	hasEventType bool
	itemID       int64
	title        string
	deleteReason string
	deletedFiles bool
	files        []string
}

// Parse decodes a webhook body of app (sonarr, radarr or lidarr) as a stream: only the event
// type, the item id, the fields that decide the class (isUpgrade needs none: the Download class
// covers it; deleteReason; deletedFiles) and the summary fields are kept, everything else is
// skipped without being held in memory. It returns ErrTooLarge for a body larger than MaxBody
// and an *InvalidError for anything that is not a single JSON object with an eventType string of
// 1-64 characters.
func Parse(app integrations.Type, body io.Reader) (Event, error) {
	lr := &countReader{r: io.LimitReader(body, MaxBody+1)}
	keep := &keepBuffer{max: maxKept}
	dec := jsontext.NewDecoder(io.TeeReader(lr, keep), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	f, err := decodeFields(app, dec)
	if lr.n > MaxBody {
		return Event{}, ErrTooLarge
	}
	if err != nil {
		return Event{}, err
	}
	ev := Event{EventType: f.eventType, Class: classify(app, f)}
	if ev.Class.schedules() && f.itemID > 0 {
		ev.Targets = []int64{f.itemID}
	}
	ev.Summary = Summary{Title: clip(f.title), ItemIDs: nonNil(ev.Targets), Files: f.files}
	if ev.Summary.Files == nil {
		ev.Summary.Files = []string{}
	}
	switch {
	case ev.Class == ClassIgnored:
		ev.Payload = []byte("{}")
	case !keep.over:
		v := jsontext.Value(keep.buf)
		if err := v.Compact(jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err == nil && len(v) <= MaxPayload {
			ev.Payload = []byte(v)
		}
	}
	if ev.Payload == nil {
		ev.Payload, ev.Truncated = truncatedPayload(ev.EventType, ev.Targets), true
	}
	return ev, nil
}

// truncatedPayload is the stored form of a body too large to keep.
func truncatedPayload(eventType string, ids []int64) []byte {
	b, _ := json.Marshal(struct {
		Truncated bool    `json:"truncated"`
		EventType string  `json:"eventType"`
		IDs       []int64 `json:"ids"`
	}{true, eventType, nonNil(ids)}) // a struct of a bool, a string and ints always marshals
	return b
}

// Summarize returns the summary of a stored event of app: decoded again from its payload (a
// truncated payload gives only its ids, an ignored event's {} nothing).
func Summarize(app integrations.Type, payload []byte, truncated bool) Summary {
	empty := Summary{ItemIDs: []int64{}, Files: []string{}}
	if truncated {
		var t struct {
			IDs []int64 `json:"ids"`
		}
		if json.Unmarshal(payload, &t) != nil {
			return empty
		}
		empty.ItemIDs = nonNil(t.IDs)
		return empty
	}
	ev, err := Parse(app, strings.NewReader(string(payload)))
	if err != nil {
		return empty
	}
	return ev.Summary
}

func nonNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// clip shortens s to maxSummaryText bytes on a rune boundary.
func clip(s string) string {
	if len(s) <= maxSummaryText {
		return s
	}
	cut := maxSummaryText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// itemObject is the object of the payload that names the app's item.
func itemObject(app integrations.Type) string {
	switch app {
	case integrations.TypeRadarr:
		return "movie"
	case integrations.TypeSonarr:
		return "series"
	case integrations.TypeLidarr:
		return "artist"
	}
	return ""
}

// fileKeys are the payload members that hold one file object; fileListKeys hold arrays of them.
var (
	fileKeys     = map[string]bool{"movieFile": true, "episodeFile": true, "trackFile": true}
	fileListKeys = map[string]bool{"episodeFiles": true, "trackFiles": true, "renamedMovieFiles": true,
		"renamedEpisodeFiles": true, "renamedTrackFiles": true}
)

// decodeFields walks the top-level object.
func decodeFields(app integrations.Type, dec *jsontext.Decoder) (fields, error) {
	var f fields
	tok, err := dec.ReadToken()
	if err != nil {
		return f, readErr(err)
	}
	if tok.Kind() != jsontext.KindBeginObject {
		return f, invalid("the body is not a JSON object")
	}
	for {
		switch dec.PeekKind() {
		case jsontext.KindEndObject:
			if _, err := dec.ReadToken(); err != nil {
				return f, readErr(err)
			}
			if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
				if err != nil {
					return f, readErr(err)
				}
				return f, invalid("more than one JSON value")
			}
			if !f.hasEventType {
				return f, invalid("no eventType")
			}
			return f, nil
		case jsontext.KindInvalid:
			_, err := dec.ReadToken()
			return f, readErr(err)
		}
		name, err := dec.ReadToken()
		if err != nil {
			return f, readErr(err)
		}
		if err := decodeMember(app, dec, name.String(), &f); err != nil {
			return f, err
		}
	}
}

// decodeMember decodes (or skips) the value of the top-level member name.
func decodeMember(app integrations.Type, dec *jsontext.Decoder, name string, f *fields) error {
	switch {
	case name == "eventType":
		tok, err := dec.ReadToken()
		if err != nil {
			return readErr(err)
		}
		if tok.Kind() != jsontext.KindString {
			return invalid("eventType is not a string")
		}
		s := tok.String()
		if s == "" || utf8.RuneCountInString(s) > maxEventType {
			return invalid("eventType must be 1-%d characters", maxEventType)
		}
		f.eventType, f.hasEventType = s, true
		return nil
	case name == itemObject(app):
		return decodeObject(dec, func(key string) (bool, error) {
			switch key {
			case "id":
				if dec.PeekKind() != jsontext.KindNumber {
					return false, nil
				}
				tok, err := dec.ReadToken()
				if err != nil {
					return true, readErr(err)
				}
				if n, err := tok.Int(); err == nil {
					f.itemID = n
				}
				return true, nil
			case "title", "name":
				if dec.PeekKind() != jsontext.KindString {
					return false, nil
				}
				tok, err := dec.ReadToken()
				if err != nil {
					return true, readErr(err)
				}
				if f.title == "" {
					f.title = tok.String()
				}
				return true, nil
			}
			return false, nil
		})
	case name == "deleteReason":
		if dec.PeekKind() != jsontext.KindString {
			return skip(dec)
		}
		tok, err := dec.ReadToken()
		if err != nil {
			return readErr(err)
		}
		f.deleteReason = clip(tok.String())
		return nil
	case name == "deletedFiles":
		switch dec.PeekKind() {
		case jsontext.KindTrue, jsontext.KindFalse:
			tok, err := dec.ReadToken()
			if err != nil {
				return readErr(err)
			}
			f.deletedFiles = tok.Bool()
			return nil
		case jsontext.KindBeginArray:
			return decodeFileList(dec, f)
		}
		return skip(dec)
	case fileKeys[name]:
		return decodeFile(dec, f)
	case fileListKeys[name]:
		return decodeFileList(dec, f)
	}
	return skip(dec)
}

// decodeObject walks an object, calling member for each key: member reads the value itself
// (true) or leaves it to be skipped (false). A value that is not an object is skipped.
func decodeObject(dec *jsontext.Decoder, member func(key string) (bool, error)) error {
	if dec.PeekKind() != jsontext.KindBeginObject {
		return skip(dec)
	}
	if _, err := dec.ReadToken(); err != nil {
		return readErr(err)
	}
	for {
		switch dec.PeekKind() {
		case jsontext.KindEndObject:
			_, err := dec.ReadToken()
			return readErr(err)
		case jsontext.KindInvalid:
			_, err := dec.ReadToken()
			return readErr(err)
		}
		key, err := dec.ReadToken()
		if err != nil {
			return readErr(err)
		}
		read, err := member(key.String())
		if err != nil {
			return err
		}
		if !read {
			if err := skip(dec); err != nil {
				return err
			}
		}
	}
}

// decodeFile reads the path of a file object.
func decodeFile(dec *jsontext.Decoder, f *fields) error {
	return decodeObject(dec, func(key string) (bool, error) {
		if key != "path" || dec.PeekKind() != jsontext.KindString {
			return false, nil
		}
		tok, err := dec.ReadToken()
		if err != nil {
			return true, readErr(err)
		}
		if len(f.files) < maxSummaryFiles {
			f.files = append(f.files, clip(tok.String()))
		}
		return true, nil
	})
}

// decodeFileList reads the paths of an array of file objects.
func decodeFileList(dec *jsontext.Decoder, f *fields) error {
	if dec.PeekKind() != jsontext.KindBeginArray {
		return skip(dec)
	}
	if _, err := dec.ReadToken(); err != nil {
		return readErr(err)
	}
	for {
		switch dec.PeekKind() {
		case jsontext.KindEndArray:
			_, err := dec.ReadToken()
			return readErr(err)
		case jsontext.KindInvalid:
			_, err := dec.ReadToken()
			return readErr(err)
		}
		if err := decodeFile(dec, f); err != nil {
			return err
		}
	}
}

func skip(dec *jsontext.Decoder) error { return readErr(dec.SkipValue()) }

// readErr turns a decoder error into an *InvalidError (nil stays nil). A read error of the body
// itself (too large, a broken connection) is kept for the caller.
func readErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return invalid("the body ends before the JSON object does")
	}
	var se *jsontext.SyntacticError
	if errors.As(err, &se) {
		return invalid("%v", se.Err)
	}
	return invalid("%v", err)
}

// classify is the class of an event (design §7.2).
func classify(app integrations.Type, f fields) Class {
	switch f.eventType {
	case "Test":
		return ClassTest
	case "Download":
		return ClassDownload
	}
	upgrade := strings.EqualFold(f.deleteReason, "upgrade")
	switch app {
	case integrations.TypeRadarr:
		switch f.eventType {
		case "MovieFileDelete":
			if upgrade {
				return ClassUpgradeDelete
			}
			return ClassChange
		case "MovieDelete":
			if f.deletedFiles {
				return ClassDelete
			}
			return ClassChange
		case "Rename", "MovieAdded":
			return ClassChange
		}
	case integrations.TypeSonarr:
		switch f.eventType {
		case "EpisodeFileDelete":
			if upgrade {
				return ClassUpgradeDelete
			}
			return ClassChange
		case "SeriesDelete":
			if f.deletedFiles {
				return ClassDelete
			}
			return ClassChange
		case "Rename", "SeriesAdd":
			return ClassChange
		}
	case integrations.TypeLidarr:
		switch f.eventType {
		case "AlbumDelete", "ArtistDelete":
			if f.deletedFiles {
				return ClassDelete
			}
			return ClassChange
		case "Rename", "Retag", "ArtistAdd":
			return ClassChange
		}
	}
	return ClassIgnored
}

// countReader counts the bytes read through it.
type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// keepBuffer keeps the first max bytes written to it and notes whether more came.
type keepBuffer struct {
	max  int
	buf  []byte
	over bool
}

func (k *keepBuffer) Write(p []byte) (int, error) {
	if !k.over {
		if len(k.buf)+len(p) > k.max {
			k.over, k.buf = true, nil
		} else {
			k.buf = append(k.buf, p...)
		}
	}
	return len(p), nil
}
