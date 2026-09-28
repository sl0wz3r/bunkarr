package rclone

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// rclone's log (docs/design/phase4.md §7.7): with --use-json-log every stderr line is one JSON
// object {time, level, msg, source, object?, objectType?, size?, skipped?}, and the periodic
// stats line carries a "stats" object (with "transferring" during large transfers). Lines
// printed before the logger is set up (a flag error) and text logs are parsed as text. The
// INFO events are informational only; records always come from listings (§7.3).

// Log levels as ParseLogLine reports them (lowercase).
const (
	LevelDebug    = "debug"
	LevelInfo     = "info"
	LevelNotice   = "notice"
	LevelError    = "error"
	LevelCritical = "critical"
)

// LogLine is one parsed stderr line of rclone.
type LogLine struct {
	Time       time.Time
	Level      string
	Msg        string
	Object     string
	ObjectType string
	Size       int64
	// Skipped is what a dry run skipped ("copy", "move", "delete", "move into backup dir").
	Skipped string
	// Stats is set on a stats line.
	Stats *Stats
	// JSON reports that the line was a JSON log line (false: a text line).
	JSON bool
	// Raw is the line as printed (redacted by the runner).
	Raw string
}

// Text renders the line for an error message: "<object>: <msg>" when it names an object.
func (l LogLine) Text() string {
	if l.Object != "" && l.JSON {
		return l.Object + ": " + l.Msg
	}
	return l.Msg
}

// Stats is the "stats" object of a stats line (§7.7).
type Stats struct {
	Bytes               int64      `json:"bytes"`
	TotalBytes          int64      `json:"totalBytes"`
	Transfers           int64      `json:"transfers"`
	TotalTransfers      int64      `json:"totalTransfers"`
	Checks              int64      `json:"checks"`
	TotalChecks         int64      `json:"totalChecks"`
	Listed              int64      `json:"listed"`
	Errors              int64      `json:"errors"`
	FatalError          bool       `json:"fatalError"`
	RetryError          bool       `json:"retryError"`
	Deletes             int64      `json:"deletes"`
	DeletedDirs         int64      `json:"deletedDirs"`
	Renames             int64      `json:"renames"`
	ServerSideCopies    int64      `json:"serverSideCopies"`
	ServerSideCopyBytes int64      `json:"serverSideCopyBytes"`
	ServerSideMoves     int64      `json:"serverSideMoves"`
	ServerSideMoveBytes int64      `json:"serverSideMoveBytes"`
	Speed               float64    `json:"speed"`
	ETA                 *float64   `json:"eta"`
	ElapsedTime         float64    `json:"elapsedTime"`
	TransferTime        float64    `json:"transferTime"`
	Transferring        []Transfer `json:"transferring"`
}

// Transfer is one entry of Stats.Transferring.
type Transfer struct {
	Name       string   `json:"name"`
	Bytes      int64    `json:"bytes"`
	Size       int64    `json:"size"`
	Percentage int      `json:"percentage"`
	Speed      float64  `json:"speed"`
	SpeedAvg   float64  `json:"speedAvg"`
	ETA        *float64 `json:"eta"`
}

// CurrentFile is the first transfer in progress ("" when none): Progress.CurrentFile (§10.4).
func (s Stats) CurrentFile() string {
	if len(s.Transferring) == 0 {
		return ""
	}
	return s.Transferring[0].Name
}

// jsonLine is the wire form of a JSON log line.
type jsonLine struct {
	Time       time.Time `json:"time"`
	Level      string    `json:"level"`
	Msg        string    `json:"msg"`
	Object     string    `json:"object"`
	ObjectType string    `json:"objectType"`
	Size       *int64    `json:"size"`
	Skipped    string    `json:"skipped"`
	Stats      *Stats    `json:"stats"`
}

// textLine matches rclone's text log: "2026/09/27 13:54:22 ERROR : message".
var textLine = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) (DEBUG|INFO|NOTICE|WARNING|ERROR|CRITICAL|ALERT|EMERGENCY)\s*:\s?(.*)$`)

// ParseLogLine parses one stderr line of rclone: a JSON log line, a text log line, or any other
// text (a flag error "Error: …" is error level; anything else keeps an empty level).
func ParseLogLine(text string) LogLine {
	l := LogLine{Raw: text}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		var j jsonLine
		if err := json.Unmarshal([]byte(trimmed), &j); err == nil && j.Level != "" {
			l.JSON = true
			l.Time, l.Level, l.Msg = j.Time, strings.ToLower(j.Level), j.Msg
			l.Object, l.ObjectType, l.Skipped, l.Stats = j.Object, j.ObjectType, j.Skipped, j.Stats
			if j.Size != nil {
				l.Size = *j.Size
			}
			if l.Level == "warning" {
				l.Level = LevelNotice
			}
			return l
		}
	}
	if m := textLine.FindStringSubmatch(text); m != nil {
		if t, err := time.ParseInLocation("2006/01/02 15:04:05", m[1], time.Local); err == nil {
			l.Time = t
		}
		l.Level, l.Msg = strings.ToLower(m[2]), m[3]
		switch l.Level {
		case "warning":
			l.Level = LevelNotice
		case "alert", "emergency":
			l.Level = LevelCritical
		}
		return l
	}
	l.Msg = text
	if strings.HasPrefix(trimmed, "Error: ") || strings.HasPrefix(trimmed, "Fatal error: ") {
		l.Level = LevelError
	}
	return l
}

// ObjectEvent kinds (§7.7). They are informational: what a batch did is read from the listings.
const (
	EventCopiedNew          = "copied-new"
	EventCopiedReplaced     = "copied-replaced"
	EventServerSideCopy     = "server-side-copy"
	EventMoved              = "moved"
	EventDeleted            = "deleted"
	EventMovedIntoBackupDir = "moved-into-backup-dir"
	EventError              = "error"
)

// ObjectEvent is one per-object line of a transfer: an INFO event or an ERROR line naming an
// object. Object is relative to the command's source or destination root, as rclone prints it.
type ObjectEvent struct {
	Kind    string
	Object  string
	Size    int64
	Message string
}

// Event returns the per-object event of a JSON log line, if it is one.
func (l LogLine) Event() (ObjectEvent, bool) {
	if !l.JSON || l.Object == "" {
		return ObjectEvent{}, false
	}
	ev := ObjectEvent{Object: l.Object, Size: l.Size, Message: l.Msg}
	switch {
	case l.Level == LevelError || l.Level == LevelCritical:
		// An error about the whole remote (objectType *s3.Fs, "not deleting directories …") names
		// a filesystem, not an object.
		if strings.HasSuffix(l.ObjectType, ".Fs") {
			return ObjectEvent{}, false
		}
		ev.Kind = EventError
	case l.Level != LevelInfo:
		return ObjectEvent{}, false
	case l.Msg == "Copied (new)":
		ev.Kind = EventCopiedNew
	case l.Msg == "Copied (replaced existing)":
		ev.Kind = EventCopiedReplaced
	case l.Msg == "Copied (server-side copy)":
		ev.Kind = EventServerSideCopy
	case strings.HasPrefix(l.Msg, "Moved (server-side)"):
		ev.Kind = EventMoved
	case l.Msg == "Deleted":
		ev.Kind = EventDeleted
	case l.Msg == "Moved into backup dir":
		ev.Kind = EventMovedIntoBackupDir
	default:
		return ObjectEvent{}, false
	}
	return ev, true
}
