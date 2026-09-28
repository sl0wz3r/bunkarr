package rclone

import (
	"errors"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
)

// TestFixturesParse runs every fixture of testdata/rclone through its parser (§14.1): the JSON
// logs line by line (stats with transferring included), the text logs, the combined check
// reports and the lsjson --stat output. A fixture this test does not know fails it, so a new
// spike file cannot go unparsed.
func TestFixturesParse(t *testing.T) {
	combined := map[string]map[string]rune{
		"check-combined.txt": {"changing.txt": MarkMatch, "TV/Show/Season 01/S01E01.srt": MarkMatch,
			"Movies/Nosferatu (1922)/Nosferatu.mkv": MarkMatch, "TV/Show/Season 01/hardlink-of-nosferatu.mkv": MarkMatch},
		"check-differences-combined.txt": {"changing.txt": MarkDiffer, "Movies/Nosferatu (1922)/Nosferatu.mkv": MarkMatch,
			"TV/Show/Season 01/hardlink-of-nosferatu.mkv": MarkMatch},
	}
	// Per JSON log: the events it holds, by kind.
	events := map[string]map[string]int{
		"check.jsonl":                   {},
		"cryptcheck.jsonl":              {},
		"copy.jsonl":                    {EventCopiedNew: 4},
		"crypt-copy.jsonl":              {EventCopiedNew: 3},
		"sftp-copy.jsonl":               {EventCopiedNew: 3},
		"copy-after-kill.jsonl":         {EventCopiedNew: 2},
		"copy-bwlimit-progress.jsonl":   {EventCopiedNew: 1},
		"copy-dry-run.jsonl":            {},
		"sync-backup-dir-dry-run.jsonl": {},
		"sync-backup-dir.jsonl":         {EventServerSideCopy: 2, EventDeleted: 2, EventCopiedNew: 1, EventMovedIntoBackupDir: 1},
		"sync-max-delete.jsonl":         {EventError: 1, EventDeleted: 1},
	}
	for _, name := range enginetest.FixtureNames(t, "rclone") {
		t.Run(name, func(t *testing.T) {
			f := enginetest.Fixture(t, "rclone", name)
			switch {
			case combined[name] != nil:
				got, err := ParseCombined(f.Raw)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != len(combined[name]) {
					t.Fatalf("marks %v, want %v", got, combined[name])
				}
				for p, m := range combined[name] {
					if got[p] != m {
						t.Errorf("%s: mark %q, want %q", p, got[p], m)
					}
				}
			case name == "lsjson-stat-missing-object-s3.json":
				o, ok, err := ParseLsJSONLine(strings.Join(strings.Fields(string(f.Raw)), " "))
				if err != nil || !ok || !o.IsDir || o.Size != -1 {
					t.Fatalf("stat of a missing S3 object = %+v %v %v, want a directory of size -1", o, ok, err)
				}
			case strings.HasSuffix(name, ".jsonl"):
				want, known := events[name]
				if !known {
					t.Fatalf("no expectation for JSON log %s", name)
				}
				got := map[string]int{}
				stats := 0
				for _, line := range f.Lines {
					l := ParseLogLine(line)
					if !l.JSON || l.Level == "" || l.Time.IsZero() {
						t.Fatalf("not a JSON log line: %s", line)
					}
					if l.Stats != nil {
						stats++
					}
					if ev, ok := l.Event(); ok {
						got[ev.Kind]++
					}
				}
				if stats == 0 {
					t.Error("no stats line")
				}
				for k, n := range want {
					if got[k] != n {
						t.Errorf("%d %s events, want %d (all: %v)", got[k], k, n, got)
					}
				}
				for k := range got {
					if _, ok := want[k]; !ok {
						t.Errorf("unexpected %s events: %d", k, got[k])
					}
				}
			case strings.HasSuffix(name, ".txt"):
				for _, line := range f.Lines {
					l := ParseLogLine(line)
					if l.JSON || l.Level == "" {
						t.Errorf("text log line without a level: %q", line)
					}
				}
			default:
				t.Fatalf("no parser for fixture %s", name)
			}
		})
	}
}

func TestStatsTransferring(t *testing.T) {
	f := enginetest.Fixture(t, "rclone", "copy-bwlimit-progress.jsonl")
	var last *Stats
	for _, line := range f.Lines {
		if l := ParseLogLine(line); l.Stats != nil && len(l.Stats.Transferring) > 0 {
			last = l.Stats
			break
		}
	}
	if last == nil {
		t.Fatal("no stats line with transferring")
	}
	if last.CurrentFile() != "big.mkv" || last.TotalBytes != 41943040 || last.Bytes == 0 || last.Transferring[0].Size != 41943040 {
		t.Fatalf("stats = %+v", last)
	}
	final := ParseLogLine(enginetest.Fixture(t, "rclone", "copy.jsonl").Lines[4])
	if final.Stats == nil || final.Stats.ETA != nil || final.Stats.Transfers != 4 || final.Stats.CurrentFile() != "" {
		t.Fatalf("final stats = %+v", final.Stats)
	}
}

func TestParseLogLineText(t *testing.T) {
	for _, tc := range []struct {
		line, level, msg string
	}{
		{"2026/09/27 13:54:22 ERROR : error listing: directory not found", LevelError, "error listing: directory not found"},
		{"2026/09/27 13:53:47 NOTICE: Failed to copy: x", LevelNotice, "Failed to copy: x"},
		{"2026/09/27 13:53:42 INFO  : Starting bandwidth limiter", LevelInfo, "Starting bandwidth limiter"},
		{"2026/09/27 13:54:22 CRITICAL: Failed to create file system", LevelCritical, "Failed to create file system"},
		{`Error: invalid argument "25:00,1M" for "--bwlimit" flag`, LevelError, `Error: invalid argument "25:00,1M" for "--bwlimit" flag`},
		{"Usage:", "", "Usage:"},
		{`{"level":"warning","msg":"w","time":"2026-09-27T13:53:10Z"}`, LevelNotice, "w"},
		{`{"not":"a log line"}`, "", `{"not":"a log line"}`},
	} {
		l := ParseLogLine(tc.line)
		if l.Level != tc.level || l.Msg != tc.msg {
			t.Errorf("ParseLogLine(%q) = %q %q, want %q %q", tc.line, l.Level, l.Msg, tc.level, tc.msg)
		}
	}
}

// TestExitClasses pins §10.3's table and the error of each fixture's exit.
func TestExitClasses(t *testing.T) {
	for _, tc := range []struct {
		code    int
		objects bool
		want    ExitClass
	}{
		{0, false, ExitOK}, {1, true, ExitObjects}, {1, false, ExitFatal}, {2, false, ExitFatal}, {3, false, ExitFatal},
		{4, false, ExitItemNotFound}, {5, false, ExitTemporary}, {6, false, ExitObjects}, {7, true, ExitFatal},
		{9, false, ExitFatal}, {10, false, ExitCutoff}, {130, false, ExitSignal}, {143, false, ExitSignal}, {137, false, ExitFatal},
	} {
		if got := ClassifyExit(tc.code, tc.objects); got != tc.want {
			t.Errorf("ClassifyExit(%d, %v) = %s, want %s", tc.code, tc.objects, got, tc.want)
		}
	}
	for _, tc := range []struct {
		fixture string
		want    error
	}{
		{"exit3-bucket-not-found.txt", ErrPathNotFound},
		{"exit3-source-not-found.txt", ErrPathNotFound},
		{"exit1-bad-credentials.txt", ErrFailed},
		{"exit1-unknown-remote.txt", ErrFailed},
		{"exit10-max-duration-soft.txt", ErrCutoff},
		{"sync-max-delete.jsonl", ErrMaxDelete},
		{"sync-backup-dir-overlap.txt", ErrFatal},
		{"bwlimit-invalid.txt", ErrUsage},
		{"crypt-wrong-password-strict-names.txt", ErrUndecryptable},
		{"check-differences.txt", ErrFailed},
	} {
		f := enginetest.Fixture(t, "rclone", tc.fixture)
		var out outcome
		for _, line := range f.Lines {
			out.note(ParseLogLine(line))
		}
		out.status.Code = f.Exit
		e := fail(command{words: []string{"copy"}}, out)
		if !errors.Is(e, tc.want) {
			t.Errorf("%s (exit %d): %v, want %v", tc.fixture, f.Exit, e, tc.want)
		}
		if e.Message == "" {
			t.Errorf("%s: no message", tc.fixture)
		}
	}
}
