package restic

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// restic's JSON output (docs/design/phase4.md §6.8, §10.2): with --json, stdout carries one JSON
// object per line by message_type (status, summary, verbose_status), stderr carries error and
// exit_error objects and plain text (retry lines, the signal line), which is skipped. Status
// lines omit their fields when they are zero. snapshots, cat config, cat lock and check print
// whole JSON documents; ls --json prints the snapshot, then one node per line.

// Status is a backup's status line (§6.8). bytes_done counts bytes read, not uploaded.
type Status struct {
	PercentDone      float64  `json:"percent_done"`
	TotalFiles       int64    `json:"total_files"`
	FilesDone        int64    `json:"files_done"`
	TotalBytes       int64    `json:"total_bytes"`
	BytesDone        int64    `json:"bytes_done"`
	CurrentFiles     []string `json:"current_files"`
	SecondsElapsed   int64    `json:"seconds_elapsed"`
	SecondsRemaining int64    `json:"seconds_remaining"`
	ErrorCount       int64    `json:"error_count"`
}

// Summary is a backup's summary line (§6.8); snapshots carry the same fields without
// snapshot_id and dry_run.
type Summary struct {
	FilesNew            int64     `json:"files_new"`
	FilesChanged        int64     `json:"files_changed"`
	FilesUnmodified     int64     `json:"files_unmodified"`
	DirsNew             int64     `json:"dirs_new"`
	DirsChanged         int64     `json:"dirs_changed"`
	DirsUnmodified      int64     `json:"dirs_unmodified"`
	DataBlobs           int64     `json:"data_blobs"`
	TreeBlobs           int64     `json:"tree_blobs"`
	DataAdded           int64     `json:"data_added"`
	DataAddedPacked     int64     `json:"data_added_packed"`
	TotalFilesProcessed int64     `json:"total_files_processed"`
	TotalBytesProcessed int64     `json:"total_bytes_processed"`
	TotalDuration       float64   `json:"total_duration"`
	BackupStart         time.Time `json:"backup_start"`
	BackupEnd           time.Time `json:"backup_end"`
	SnapshotID          string    `json:"snapshot_id,omitempty"`
	DryRun              bool      `json:"dry_run,omitempty"`
}

// ErrorLine is an error line of a backup: a file that could not be read (§6.8).
type ErrorLine struct {
	Message string `json:"message"`
	During  string `json:"during"`
	Item    string `json:"item"`
}

// ExitError is restic's exit_error line on stderr.
type ExitError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Line is one parsed output line of restic.
type Line struct {
	// Type is message_type ("status", "summary", "error", "exit_error", "verbose_status",
	// "initialized", "node", "snapshot", …); "" for a line that is not a JSON object.
	Type      string
	Status    *Status
	Summary   *Summary
	Error     *ErrorLine
	ExitError *ExitError
	// Init is the repository id of an "initialized" line.
	Init string
	Raw  string
}

// wireLine is the union of the line types ParseLine reads.
type wireLine struct {
	MessageType string          `json:"message_type"`
	Error       json.RawMessage `json:"error"`
	During      string          `json:"during"`
	Item        string          `json:"item"`
	Code        int             `json:"code"`
	Message     string          `json:"message"`
	ID          string          `json:"id"`
}

// ParseLine parses one output line; ok is false for a line that is not a JSON object with a
// message_type (plain text, which callers skip).
func ParseLine(text string) (Line, bool) {
	l := Line{Raw: text}
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "{") {
		return l, false
	}
	var w wireLine
	if err := json.Unmarshal([]byte(t), &w); err != nil || w.MessageType == "" {
		return l, false
	}
	l.Type = w.MessageType
	switch w.MessageType {
	case "status":
		var s Status
		if json.Unmarshal([]byte(t), &s) != nil {
			return l, false
		}
		l.Status = &s
	case "summary":
		var s Summary
		if json.Unmarshal([]byte(t), &s) != nil {
			return l, false
		}
		l.Summary = &s
	case "error":
		e := ErrorLine{During: w.During, Item: w.Item}
		var inner struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(w.Error, &inner) == nil {
			e.Message = inner.Message
		} else {
			e.Message = strings.Trim(string(w.Error), `"`)
		}
		l.Error = &e
	case "exit_error":
		l.ExitError = &ExitError{Code: w.Code, Message: w.Message}
	case "initialized":
		l.Init = w.ID
	}
	return l, true
}

// Snapshot is one snapshot of restic snapshots --json.
type Snapshot struct {
	ID             string    `json:"id"`
	ShortID        string    `json:"short_id"`
	Time           time.Time `json:"time"`
	Parent         string    `json:"parent,omitempty"`
	Tree           string    `json:"tree"`
	Paths          []string  `json:"paths"`
	Hostname       string    `json:"hostname"`
	Username       string    `json:"username"`
	Tags           []string  `json:"tags"`
	ProgramVersion string    `json:"program_version"`
	Summary        *Summary  `json:"summary,omitempty"`
}

// HasTags reports whether the snapshot carries every tag of want.
func (s Snapshot) HasTags(want ...string) bool {
	for _, w := range want {
		found := false
		for _, t := range s.Tags {
			if t == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ParseSnapshots parses the output of restic snapshots --json (one JSON array, possibly over
// several lines).
func ParseSnapshots(out []byte) ([]Snapshot, error) {
	t := strings.TrimSpace(string(out))
	if t == "" || t == "null" {
		return nil, nil
	}
	var s []Snapshot
	if err := json.Unmarshal([]byte(t), &s); err != nil {
		return nil, fmt.Errorf("restic snapshots: %w", err)
	}
	return s, nil
}

// Node is one entry of restic ls --json: the read-back of a snapshot's content (§6.2 step 5).
type Node struct {
	// Path is the absolute path in the snapshot.
	Path string
	// Type is file, dir, symlink, dev, chardev, fifo or socket.
	Type string
	Size int64
	// MtimeNs is the modification time in nanoseconds since the epoch; Mtime the same time
	// with restic's precision and zone.
	MtimeNs int64
	Mtime   time.Time
	Inode   uint64
}

type wireNode struct {
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	Mtime       time.Time `json:"mtime"`
	Inode       uint64    `json:"inode"`
	MessageType string    `json:"message_type"`
	StructType  string    `json:"struct_type"`
}

// ParseLsLine parses one line of restic ls --json. isNode reports a node (a file, directory,
// link …); the snapshot line and anything else give false. restic 0.17 and newer set
// message_type, older versions struct_type.
func ParseLsLine(text string) (n Node, isNode bool, err error) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "{") {
		return Node{}, false, nil
	}
	var w wireNode
	if err := json.Unmarshal([]byte(t), &w); err != nil {
		return Node{}, false, fmt.Errorf("restic ls: %w", err)
	}
	kind := w.MessageType
	if kind == "" {
		kind = w.StructType
	}
	if kind != "node" {
		return Node{}, false, nil
	}
	if w.Path == "" {
		return Node{}, false, fmt.Errorf("restic ls: a node without a path: %s", t)
	}
	return Node{Path: w.Path, Type: w.Type, Size: w.Size, Mtime: w.Mtime, MtimeNs: w.Mtime.UnixNano(), Inode: w.Inode}, true, nil
}

// Lock is a lock of restic cat lock (§6.7).
type Lock struct {
	Time      time.Time `json:"time"`
	Exclusive bool      `json:"exclusive"`
	Hostname  string    `json:"hostname"`
	Username  string    `json:"username"`
	PID       int       `json:"pid"`
	UID       int       `json:"uid"`
	GID       int       `json:"gid"`
}

// CheckResult is the summary of restic check --json (§6.6).
type CheckResult struct {
	NumErrors          int      `json:"num_errors"`
	BrokenPacks        []string `json:"broken_packs"`
	SuggestRepairIndex bool     `json:"suggest_repair_index"`
	SuggestPrune       bool     `json:"suggest_prune"`
}

// RepoConfig is restic cat config --json.
type RepoConfig struct {
	Version           int    `json:"version"`
	ID                string `json:"id"`
	ChunkerPolynomial string `json:"chunker_polynomial"`
}

// ForgetGroup is one group of restic forget --json with a --keep-* policy. Bunkarr never runs
// such a forget (S24); the type reads the spike's fixtures, which pin restic's own bucket
// semantics for KeepMedia's tests.
type ForgetGroup struct {
	Tags    []string   `json:"tags"`
	Host    string     `json:"host"`
	Paths   []string   `json:"paths"`
	Keep    []Snapshot `json:"keep"`
	Remove  []Snapshot `json:"remove"`
	Reasons []struct {
		Snapshot Snapshot `json:"snapshot"`
		Matches  []string `json:"matches"`
	} `json:"reasons"`
}
