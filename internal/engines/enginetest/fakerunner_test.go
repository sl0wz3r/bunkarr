package enginetest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// recorder captures a FakeRunner's test failures instead of failing the test.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) Helper() {}

func (r *recorder) failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.errs)
}

func (r *recorder) has(sub string) bool {
	return slices.ContainsFunc(r.failures(), func(s string) bool { return strings.Contains(s, sub) })
}

func collect(t *testing.T, p proc.Process) ([]proc.Line, proc.ExitStatus, error) {
	t.Helper()
	var lines []proc.Line
	for l := range p.Lines() {
		lines = append(lines, l)
	}
	st, err := p.Wait()
	return lines, st, err
}

func resticEnv() map[string]string {
	return map[string]string{"RESTIC_REPOSITORY": "/mnt/repo", "RESTIC_PASSWORD_FILE": "/dev/shm/x/password", "RESTIC_CACHE_DIR": "/config/cache/restic/1", "RESTIC_PROGRESS_FPS": "0.5"}
}

// TestFakeRunnerScriptsAndFiles: an expected command is answered with its fixture; its secret
// files exist (0600) while the script runs and are gone after Wait; stdin and flags are recorded.
func TestFakeRunnerScriptsAndFiles(t *testing.T) {
	r := NewFakeRunner(t)
	dirs := RunDirs(t)
	rd, err := dirs.New(12)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.WriteData("files", []byte("/src/a\x00/src/b\x00")); err != nil {
		t.Fatal(err)
	}
	var sawSecret, sawFiles bool
	s := FixtureScript(t, "restic", "backup-partial.stderr.jsonl")
	s.Hook = func(c *Call) {
		fi, err := os.Stat(c.Dir.SecretPath("password"))
		sawSecret = err == nil && fi.Mode().Perm() == 0o600
		sawFiles = string(c.DataFile("files")) == "/src/a\x00/src/b\x00"
	}
	r.Expect(proc.Restic, Prefix("backup"), s).Env("RESTIC_CACHE_DIR", "RESTIC_PASSWORD_FILE", "RESTIC_PROGRESS_FPS", "RESTIC_REPOSITORY")
	p, err := r.Start(context.Background(), proc.Cmd{Binary: proc.Restic, Dir: rd, Env: resticEnv(),
		Args:        []string{"backup", "--json", "--host", "bunkarr", "--tag", "bunkarr", "--tag", "bunkarr-job:12", "--files-from-raw", rd.DataPath("files")},
		SecretFiles: map[string][]byte{"password": []byte("restic-password-1")}})
	if err != nil {
		t.Fatal(err)
	}
	lines, st, err := collect(t, p)
	if err != nil || st.Code != 3 {
		t.Fatalf("status %+v, %v", st, err)
	}
	fx := Fixture(t, "restic", "backup-partial.stderr.jsonl")
	if len(lines) != len(fx.Lines) || !lines[0].Stderr || lines[0].Text != fx.Lines[0] {
		t.Errorf("lines %v", lines)
	}
	if !sawSecret || !sawFiles {
		t.Errorf("while running: secret file %v, data file %v", sawSecret, sawFiles)
	}
	if _, err := os.Stat(rd.SecretDir()); !errors.Is(err, os.ErrNotExist) {
		t.Error("run directory left")
	}
	calls := r.Calls()
	if len(calls) != 1 || calls[0].Subcommand() != "backup" || !slices.Equal(calls[0].FlagValues("--tag"), []string{"bunkarr", "bunkarr-job:12"}) ||
		string(calls[0].SecretFiles["password"]) != "restic-password-1" || calls[0].Status.Code != 3 || !calls[0].Has("--json") {
		t.Errorf("call %+v", calls[0])
	}
	if v, ok := calls[0].Flag("--host"); !ok || v != "bunkarr" {
		t.Errorf("--host = %q", v)
	}
}

// TestFakeRunnerHandler: a handler answers every command of its subcommand with state.
func TestFakeRunnerHandler(t *testing.T) {
	r := NewFakeRunner(t)
	var n int
	r.Handle(proc.Rclone, "rcat", func(c *Call) Script {
		n++
		return Script{Stdout: []string{fmt.Sprintf("stored %d: %s", n, c.Stdin)}}
	})
	r.Handle(proc.Rclone, "backend cleanup", func(c *Call) Script { return Script{Exit: 0} })
	env := map[string]string{"RCLONE_CONFIG": "/dev/null"}
	for i := range 2 {
		p, err := r.Start(context.Background(), proc.Cmd{Binary: proc.Rclone, Args: []string{"rcat", "BKCRYPT:.bunkarr/destination.json"}, Env: env, Stdin: strings.NewReader("marker")})
		if err != nil {
			t.Fatal(err)
		}
		lines, _, _ := collect(t, p)
		if len(lines) != 1 || lines[0].Text != fmt.Sprintf("stored %d: marker", i+1) {
			t.Errorf("lines %v", lines)
		}
	}
	p, _ := r.Start(context.Background(), proc.Cmd{Binary: proc.Rclone, Args: []string{"backend", "cleanup", "BKDEST:b/p", "-o", "max-age=168h"}, Env: env})
	if _, st, _ := collect(t, p); st.Code != 0 || len(r.CallsOf(proc.Rclone, "backend cleanup")) != 1 {
		t.Errorf("backend cleanup: %+v", st)
	}
}

// TestFakeRunnerFailures: an unexpected command fails the test and exits 2; a command outside the
// allow-list is refused; a pinned environment that differs, an sftp command without a real
// known_hosts file, and an unmet expectation fail the test.
func TestFakeRunnerFailures(t *testing.T) {
	rec := &recorder{}
	t.Run("run", func(t *testing.T) {
		rec.TB = t
		r := NewFakeRunner(rec)
		r.Expect(proc.Restic, Args("version"), Script{}).Env("RESTIC_REPOSITORY")
		r.Expect(proc.Restic, Args("never", "runs"), Script{})
		env := map[string]string{"RCLONE_CONFIG": "/dev/null"}

		p, err := r.Start(context.Background(), proc.Cmd{Binary: proc.Rclone, Args: []string{"lsf", "BKDEST:b"}, Env: env})
		if err != nil {
			t.Fatal(err)
		}
		if _, st, _ := collect(t, p); st.Code != 2 || !rec.has("unexpected command") {
			t.Errorf("unexpected command: %+v %v", st, rec.failures())
		}
		if _, err := r.Start(context.Background(), proc.Cmd{Binary: proc.Rclone, Args: []string{"lsf", "BKDEST:b", "-vv"}, Env: env}); !errors.Is(err, proc.ErrNotAllowed) || !rec.has("flag -vv") {
			t.Errorf("-vv: %v", err)
		}
		p, _ = r.Start(context.Background(), proc.Cmd{Binary: proc.Restic, Args: []string{"version"}})
		collect(t, p)
		if !rec.has("environment [] , want exactly [RESTIC_REPOSITORY]") && !rec.has("want exactly [RESTIC_REPOSITORY]") {
			t.Errorf("pinned environment: %v", rec.failures())
		}

		sftp := map[string]string{"RCLONE_CONFIG": "/dev/null", "RCLONE_CONFIG_BKDEST_TYPE": "sftp",
			"RCLONE_CONFIG_BKDEST_KNOWN_HOSTS_FILE": "/nonexistent/known_hosts"}
		r.Handle(proc.Rclone, "lsjson", func(*Call) Script { return Script{} })
		p, _ = r.Start(context.Background(), proc.Cmd{Binary: proc.Rclone, Args: []string{"lsjson", "BKDEST:x"}, Env: sftp})
		collect(t, p)
		if !rec.has("known_hosts /nonexistent/known_hosts is not a non-empty 0600 file") {
			t.Errorf("known_hosts: %v", rec.failures())
		}
		// With the pinned keys written as a secret file, the check passes.
		before := len(rec.failures())
		rd, _ := RunDirs(t).New(1)
		sftp["RCLONE_CONFIG_BKDEST_KNOWN_HOSTS_FILE"] = rd.SecretPath("known_hosts")
		p, _ = r.Start(context.Background(), proc.Cmd{Binary: proc.Rclone, Args: []string{"lsjson", "BKDEST:x"}, Env: sftp, Dir: rd,
			SecretFiles: map[string][]byte{"known_hosts": []byte("[h]:2222 ssh-ed25519 AAAA\n")}})
		collect(t, p)
		if len(rec.failures()) != before {
			t.Errorf("a valid sftp command failed: %v", rec.failures()[before:])
		}
	})
	if !rec.has("ran 0 of 1 times") {
		t.Errorf("an unmet expectation did not fail: %v", rec.failures())
	}
}

// TestFakeRunnerInterruptAndCancel: a script that runs until interrupted ends with the interrupt
// exit code; a cancelled one reports the cancellation; output is redacted with Cmd.Redact.
func TestFakeRunnerInterruptAndCancel(t *testing.T) {
	r := NewFakeRunner(t)
	const local = "typed-secret-value-5"
	r.Expect(proc.Restic, Prefix("backup"), Script{Stdout: []string{`{"message_type":"status","current_files":["` + local + `"]}`}, UntilInterrupted: true}).Times(2)
	env := resticEnv()
	p, _ := r.Start(context.Background(), proc.Cmd{Binary: proc.Restic, Args: []string{"backup", "--json"}, Env: env, Redact: []string{local}})
	first := <-p.Lines()
	if strings.Contains(first.Text, local) {
		t.Errorf("line not redacted: %s", first.Text)
	}
	p.Interrupt()
	if _, st, err := collect(t, p); !st.Interrupted || st.Code != 130 || err != nil {
		t.Errorf("interrupt: %+v %v", st, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p, _ = r.Start(ctx, proc.Cmd{Binary: proc.Restic, Args: []string{"backup", "--json"}, Env: env})
	<-p.Lines()
	time.AfterFunc(10*time.Millisecond, cancel)
	if _, st, err := collect(t, p); !st.Cancelled || !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %+v %v", st, err)
	}
}
