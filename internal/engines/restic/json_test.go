package restic

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// TestFixturesParse runs every fixture of testdata/restic through its parser (§14.1 restic):
// status lines with omitted fields, summaries, error and exit_error lines, the non-JSON lines
// restic mixes into stderr, snapshots, locks, check and init output, and the forget groups of the
// spike. A fixture this test does not know fails it.
func TestFixturesParse(t *testing.T) {
	for _, name := range enginetest.FixtureNames(t, "restic") {
		t.Run(name, func(t *testing.T) {
			f := enginetest.Fixture(t, "restic", name)
			counts := map[string]int{}
			var exit *ExitError
			var summary *Summary
			for _, line := range f.Lines {
				l, ok := ParseLine(line)
				if !ok {
					counts["text"]++
					continue
				}
				counts[l.Type]++
				switch {
				case l.ExitError != nil:
					exit = l.ExitError
				case l.Summary != nil:
					summary = l.Summary
				case l.Status != nil && l.Status.TotalFiles == 0 && l.Status.PercentDone == 0:
					t.Errorf("status line without its fields: %s", line)
				case l.Error != nil && (l.Error.Message == "" || l.Error.Item == ""):
					t.Errorf("error line %+v", l.Error)
				}
			}
			switch {
			case strings.HasPrefix(name, "backup-") && strings.HasSuffix(name, ".jsonl") && !strings.Contains(name, ".stderr."):
				if name == "backup-killed.jsonl" {
					if summary != nil || counts["status"] == 0 {
						t.Fatalf("a killed backup: %v, summary %v", counts, summary)
					}
					return
				}
				if summary == nil || summary.SnapshotID == "" || summary.TotalFilesProcessed == 0 {
					t.Fatalf("no summary: %v", counts)
				}
				if name == "backup-dry-run.jsonl" && !summary.DryRun {
					t.Fatal("dry run summary without dry_run")
				}
			case strings.Contains(name, ".stderr."):
				if exit == nil || exit.Code != f.Exit || exit.Message == "" {
					t.Fatalf("exit_error %+v, want code %d", exit, f.Exit)
				}
				if name == "backup-partial.stderr.jsonl" && counts["error"] != 1 {
					t.Fatalf("error lines %v", counts)
				}
				if name == "s3-bad-credentials-retrying.stderr.txt" {
					retries := 0
					for _, l := range f.Lines {
						if strings.Contains(l, proc.RetryMarker) {
							retries++
						}
					}
					if retries != 6 || counts["text"] != 7 {
						t.Fatalf("retry lines %d, text %v", retries, counts)
					}
				}
			case name == "snapshots.json" || name == "snapshots-latest.json":
				snaps, err := ParseSnapshots(f.Raw)
				if err != nil || len(snaps) == 0 {
					t.Fatalf("%v %v", snaps, err)
				}
				for _, s := range snaps {
					if len(s.ID) != 64 || s.Time.IsZero() || len(s.Paths) == 0 || s.Hostname != Host || s.Summary == nil {
						t.Errorf("snapshot %+v", s)
					}
				}
			case strings.HasPrefix(name, "forget-"):
				var groups []ForgetGroup
				if err := json.Unmarshal(f.Raw, &groups); err != nil || len(groups) != 1 || len(groups[0].Keep) == 0 {
					t.Fatalf("%v %v", groups, err)
				}
			case name == "lock.json":
				var l Lock
				if err := json.Unmarshal(f.Raw, &l); err != nil || l.PID != 782 || l.Hostname != "457fa3ca0fed" || l.Time.IsZero() || l.Exclusive {
					t.Fatalf("%+v %v", l, err)
				}
			case name == "check.json":
				var c CheckResult
				if counts["summary"] != 1 || json.Unmarshal(f.Raw, &c) != nil || c.NumErrors != 0 {
					t.Fatalf("%v %+v", counts, c)
				}
			case name == "init.json" || name == "init-sftp.json":
				l, _ := ParseLine(f.Lines[0])
				if len(l.Init) != 64 {
					t.Fatalf("init id %q", l.Init)
				}
			case strings.HasSuffix(name, ".txt"):
				if counts["text"] != len(f.Lines) {
					t.Fatalf("a text output with JSON lines: %v", counts)
				}
			default:
				t.Fatalf("no parser for fixture %s", name)
			}
		})
	}
}

func TestStatusOmittedFields(t *testing.T) {
	l, ok := ParseLine(`{"message_type":"status","percent_done":1,"total_files":1,"files_done":1,"total_bytes":41943040,"bytes_done":41943040}`)
	if !ok || l.Status == nil || l.Status.CurrentFiles != nil || l.Status.SecondsRemaining != 0 || l.Status.FilesDone != 1 {
		t.Fatalf("%+v", l.Status)
	}
	for _, line := range []string{"\x1b[2Ksignal interrupt received, cleaning up", "", "{broken", `{"no":"type"}`, "[1,2]"} {
		if _, ok := ParseLine(line); ok {
			t.Errorf("ParseLine(%q) parsed", line)
		}
	}
}

// TestLsNodes parses restic ls --json output: the snapshot line first, then one node per line
// (message_type in 0.17+, struct_type before).
func TestLsNodes(t *testing.T) {
	lines := []string{
		`{"time":"2026-09-27T13:47:41.074096584Z","tree":"7227","paths":["/src"],"hostname":"bunkarr","username":"root","id":"f71a","short_id":"f71a","message_type":"snapshot","struct_type":"snapshot"}`,
		`{"name":"src","type":"dir","path":"/src","uid":0,"gid":0,"mode":2147484141,"permissions":"drwxr-xr-x","mtime":"2026-09-27T13:47:40.1Z","atime":"2026-09-27T13:47:40.1Z","ctime":"2026-09-27T13:47:40.1Z","inode":5,"message_type":"node","struct_type":"node"}`,
		`{"name":"Nosferatu.mkv","type":"file","path":"/src/Movies/Nosferatu (1922)/Nosferatu.mkv","uid":0,"gid":0,"size":3000000,"mode":420,"permissions":"-rw-r--r--","mtime":"2026-09-27T13:47:39.123456789Z","atime":"2026-09-27T13:47:39Z","ctime":"2026-09-27T13:47:39Z","inode":17,"message_type":"node","struct_type":"node"}`,
		`{"name":"a\nb","type":"file","path":"/src/a\nb","size":0,"mtime":"2026-09-27T13:47:39+02:00","struct_type":"node"}`,
		`{"name":"link","type":"symlink","path":"/src/link","linktarget":"Movies","mtime":"2026-09-27T13:47:39Z","message_type":"node"}`,
	}
	var nodes []Node
	for _, l := range lines {
		n, ok, err := ParseLsLine(l)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) != 4 {
		t.Fatalf("nodes %+v", nodes)
	}
	f := nodes[1]
	if f.Path != "/src/Movies/Nosferatu (1922)/Nosferatu.mkv" || f.Type != "file" || f.Size != 3000000 || f.MtimeNs%1_000_000_000 != 123456789 || f.Inode != 17 {
		t.Fatalf("file node %+v", f)
	}
	if nodes[2].Path != "/src/a\nb" || nodes[2].MtimeNs != nodes[2].Mtime.UnixNano() || nodes[3].Type != "symlink" {
		t.Fatalf("nodes %+v", nodes[2:])
	}
	if _, _, err := ParseLsLine(`{"message_type":"node","type":"file"}`); err == nil {
		t.Fatal("a node without a path parsed")
	}
}

// TestExitClasses: the exit codes of §10.3 and the error each fixture's exit gives.
func TestExitClasses(t *testing.T) {
	for code, want := range map[int]string{0: "ok", 3: "partial", 10: "repository-missing", 11: "locked", 12: "wrong-password",
		1: "fatal", 130: "signal", 143: "signal", 137: "fatal"} {
		if got := Class(code); got != want {
			t.Errorf("Class(%d) = %s, want %s", code, got, want)
		}
	}
	for _, tc := range []struct {
		fixture string
		want    error
	}{
		{"repo-missing.stderr.jsonl", engines.ErrRepositoryMissing},
		{"wrong-password.stderr.jsonl", engines.ErrWrongPassword},
		{"locked.stderr.jsonl", engines.ErrLocked},
		{"locked-stale-retry-lock.stderr.jsonl", engines.ErrLocked},
		{"init-already-initialized.stderr.jsonl", ErrFailed},
		{"empty-password.stderr.jsonl", ErrFailed},
		{"forget-no-policy.stderr.jsonl", ErrFailed},
		{"sftp-no-ssh-binary.stderr.jsonl", ErrFailed},
		{"backup-sigint.stderr.txt", ErrFailed},
		{"rclone-backend-hostkey-mismatch.stderr.txt", engines.ErrHostKeyChanged},
	} {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		f.Expect(proc.Restic, enginetest.Prefix("cat", "config"), enginetest.FixtureScript(t, "restic", tc.fixture))
		_, err := connect(t, d, dest, sec).CatConfig(t.Context())
		var e *Error
		if !errors.Is(err, tc.want) || !errors.As(err, &e) || e.Message == "" {
			t.Errorf("%s: %v, want %v with a message", tc.fixture, err, tc.want)
		}
	}
}

func TestTags(t *testing.T) {
	media, err := Tags(TagInput{EngineTag: testEngineTag, Kind: engines.VersionMedia, JobID: 42, SourceID: 3, Batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bunkarr", "bunkarr-dest:" + testEngineTag, "bunkarr-kind:media", "bunkarr-job:42", "bunkarr-source:3", "bunkarr-batch:2"}
	if !slices.Equal(media, want) {
		t.Fatalf("media tags %q", media)
	}
	plex, _ := Tags(TagInput{EngineTag: testEngineTag, Kind: engines.VersionPlexDB, JobID: 7, IntegrationID: 2, Version: "20260924T120000Z-job7"})
	manifest, _ := Tags(TagInput{EngineTag: testEngineTag, Kind: engines.VersionManifest, JobID: 8, Version: "20260924T120000Z"})
	for _, bad := range []TagInput{
		{EngineTag: "short", Kind: engines.VersionMedia, JobID: 1, SourceID: 1, Batch: 1},
		{EngineTag: testEngineTag, Kind: "retention", JobID: 1},
		{EngineTag: testEngineTag, Kind: engines.VersionMedia, JobID: 1, SourceID: 1},
		{EngineTag: testEngineTag, Kind: engines.VersionMedia, SourceID: 1, Batch: 1},
		{EngineTag: testEngineTag, Kind: engines.VersionArr, JobID: 1, Version: "v"},
		{EngineTag: testEngineTag, Kind: engines.VersionManifest, JobID: 1, Version: "a,b"},
	} {
		if _, err := Tags(bad); err == nil {
			t.Errorf("Tags(%+v) accepted", bad)
		}
	}
	for _, tc := range []struct {
		name string
		tags []string
		want TagInfo
		ok   bool
	}{
		{"media", media, TagInfo{Group: Group{Kind: "media", SourceID: 3}, JobID: 42, Batch: 2}, true},
		{"plex", plex, TagInfo{Group: Group{Kind: "plexdb", IntegrationID: 2}, JobID: 7, Version: "20260924T120000Z-job7"}, true},
		{"manifest", manifest, TagInfo{Group: Group{Kind: "manifest"}, JobID: 8, Version: "20260924T120000Z"}, true},
		{"with a user's own tag", append(slices.Clone(media), "keep-forever"), TagInfo{Group: Group{Kind: "media", SourceID: 3}, JobID: 42, Batch: 2}, true},
		{"another engine tag", mediaTags(t, otherTag, 3, 42, 2), TagInfo{}, false},
		{"duplicated source", append(slices.Clone(media), "bunkarr-source:4"), TagInfo{}, false},
		{"duplicated exact tag", append(slices.Clone(media), "bunkarr-batch:2"), TagInfo{}, false},
		{"two engine tags", append(slices.Clone(media), "bunkarr-dest:"+otherTag), TagInfo{}, false},
		{"no bunkarr tag", media[1:], TagInfo{}, false},
		{"no job", slices.Delete(slices.Clone(media), 3, 4), TagInfo{}, false},
		{"no batch", media[:5], TagInfo{}, false},
		{"unknown bunkarr tag", append(slices.Clone(media), "bunkarr-extra:1"), TagInfo{}, false},
		{"media with an integration", append(slices.Clone(media), "bunkarr-integration:1"), TagInfo{}, false},
		{"plex without integration", slices.DeleteFunc(slices.Clone(plex), func(s string) bool { return strings.HasPrefix(s, TagIntegrationPfx) }), TagInfo{}, false},
		{"leading zero", []string{"bunkarr", "bunkarr-dest:" + testEngineTag, "bunkarr-kind:media", "bunkarr-job:042", "bunkarr-source:3", "bunkarr-batch:2"}, TagInfo{}, false},
		{"bad kind", []string{"bunkarr", "bunkarr-dest:" + testEngineTag, "bunkarr-kind:other", "bunkarr-job:1"}, TagInfo{}, false},
		{"no tags", nil, TagInfo{}, false},
	} {
		got, ok := ParseTags(tc.tags, testEngineTag)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: ParseTags = %+v %v, want %+v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := ParseGroup(media, "not-a-tag"); ok {
		t.Error("a group parsed for an invalid engine tag")
	}
}

func TestCompressIncludes(t *testing.T) {
	live := []string{
		"Movies/A (2001)/A.mkv", "Movies/A (2001)/A.srt", "Movies/B/B.mkv", "Movies/B/Extras/x.mkv", "Movies/B/Extras/y.mkv",
		"TV/Show/S01/e1.mkv", "TV/Show/S01/e2.mkv", "TV/Show/S02/e1.mkv", "top.nfo",
		"odd/new\nline.mkv", "odd/star*.mkv", "odd/[bracket].mkv", `odd/back\slash.mkv`,
	}
	all := func(string) bool { return true }
	for _, tc := range []struct {
		name    string
		root    string
		exclude []string
		want    []string
	}{
		{"no rules: the root", "/mnt/media", nil, []string{"/mnt/media"}},
		{"root /", "/", nil, []string{"/"}},
		{"a held file splits its directories", "/mnt/media", []string{"Movies/B/Extras/x.mkv"}, []string{
			"/mnt/media/top.nfo", "/mnt/media/Movies/A (2001)", "/mnt/media/Movies/B/B.mkv", "/mnt/media/Movies/B/Extras/y.mkv",
			"/mnt/media/TV", "/mnt/media/odd"}},
		{"nested whole directories", "/mnt/media", []string{"top.nfo", "Movies/A (2001)/A.srt"}, []string{
			"/mnt/media/Movies/A (2001)/A.mkv", "/mnt/media/Movies/B", "/mnt/media/TV", "/mnt/media/odd"}},
		{"odd names kept byte for byte", "/mnt/media", []string{"odd/star*.mkv", "Movies/A (2001)/A.mkv"}, []string{
			"/mnt/media/top.nfo", "/mnt/media/Movies/A (2001)/A.srt", "/mnt/media/Movies/B", "/mnt/media/TV",
			"/mnt/media/odd/[bracket].mkv", `/mnt/media/odd/back\slash.mkv`, "/mnt/media/odd/new\nline.mkv"}},
		{"a whole season", "/src/", []string{"TV/Show/S02/e1.mkv"}, []string{"/src/top.nfo", "/src/Movies", "/src/TV/Show/S01", "/src/odd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CompressIncludes(tc.root, live, func(rel string) bool { return !slices.Contains(tc.exclude, rel) })
			if !slices.Equal(got, tc.want) {
				t.Fatalf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
	if got := CompressIncludes("/src", live, func(string) bool { return false }); got != nil {
		t.Fatalf("nothing included: %q", got)
	}
	if got := CompressIncludes("/src", nil, all); got != nil {
		t.Fatalf("no live files: %q", got)
	}
	// Only some files of a directory that is otherwise excluded by the plan.
	got := CompressIncludes("/src", []string{"a/1", "a/2", "b/1"}, func(rel string) bool { return rel == "a/1" })
	if !slices.Equal(got, []string{"/src/a/1"}) {
		t.Fatalf("got %q", got)
	}
}
