package proc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// TestExecEnvironment: the child gets exactly PATH, TZ, LANG, TMPDIR, HOME (its run directory)
// and the command's own variables; nothing else of Bunkarr's environment, so no BUNKARR_*,
// AWS_*, RCLONE_* or RESTIC_* variable reaches it (S22).
func TestExecEnvironment(t *testing.T) {
	t.Setenv("BUNKARR_TEST_INHERITED", "bunkarr-secret-in-parent-env")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret-in-parent-env")
	t.Setenv("RESTIC_PASSWORD_COMMAND", "cat /etc/shadow")
	t.Setenv("RCLONE_LOG_FILE", "/tmp/rclone.log")
	t.Setenv("TZ", "Europe/Berlin")
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("TMPDIR", os.TempDir())
	dirs, _, _ := testRunDirs(t, true)
	rd := helperDir(t, dirs, helperConfig{Mode: "env"})
	env := rcloneEnv()
	p, err := testRunner(t, ExecOptions{}).Start(context.Background(), Cmd{Binary: Rclone, Args: []string{"version"}, Env: env, Dir: rd})
	if err != nil {
		t.Fatal(err)
	}
	lines, st, err := collect(t, p)
	if err != nil || st.Code != 0 {
		t.Fatalf("exit %+v, %v", st, err)
	}
	got := map[string]string{}
	for _, l := range lines {
		k, v, _ := strings.Cut(l.Text, "=")
		got[k] = v
	}
	want := []string{"HOME", "LANG", "PATH", "TMPDIR", "TZ"}
	for k := range env {
		want = append(want, k)
	}
	slices.Sort(want)
	if keys := sortedKeys(got); !slices.Equal(keys, want) {
		t.Fatalf("child environment %v, want exactly %v", keys, want)
	}
	if got["HOME"] != rd.DataDir() || got["TZ"] != "Europe/Berlin" || got["RCLONE_CONFIG"] != "/dev/null" {
		t.Errorf("values: HOME %q TZ %q RCLONE_CONFIG %q", got["HOME"], got["TZ"], got["RCLONE_CONFIG"])
	}
}

// TestExecSecretFiles: secret files exist with mode 0600 in a 0700 directory while the command
// runs (on tmpfs, or in the fallback) and are gone with the run directory after Wait.
func TestExecSecretFiles(t *testing.T) {
	for _, tmpfs := range []bool{true, false} {
		t.Run(map[bool]string{true: "tmpfs", false: "fallback"}[tmpfs], func(t *testing.T) {
			dirs, _, shm := testRunDirs(t, tmpfs)
			rd, err := dirs.New(9)
			if err != nil {
				t.Fatal(err)
			}
			cfg := helperConfig{Mode: "stat", Paths: []string{rd.SecretDir(), rd.SecretPath("password"), rd.SecretPath("known_hosts")}}
			raw, _ := jsonMarshal(cfg)
			if err := rd.WriteData(helperFile, raw); err != nil {
				t.Fatal(err)
			}
			c := Cmd{Binary: Restic, Args: []string{"version"}, Dir: rd, SecretFiles: map[string][]byte{
				"password":    []byte("restic-password-value"),
				"known_hosts": []byte("[h]:2222 ssh-ed25519 AAAA"),
			}}
			p, err := testRunner(t, ExecOptions{}).Start(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			lines, st, err := collect(t, p)
			if err != nil || st.Code != 0 {
				t.Fatalf("exit %+v, %v", st, err)
			}
			text := joinLines(lines)
			for _, want := range []string{rd.SecretDir() + " 0700", rd.SecretPath("password") + " 0600 restic-password-value", rd.SecretPath("known_hosts") + " 0600 [h]:2222"} {
				if !strings.Contains(text, want) {
					t.Errorf("while running: no %q in\n%s", want, text)
				}
			}
			if tmpfs != strings.HasPrefix(rd.SecretDir(), shm) {
				t.Errorf("secret dir %s (tmpfs %v)", rd.SecretDir(), tmpfs)
			}
			if !gone(rd.SecretDir()) || !gone(rd.DataDir()) {
				t.Error("run directory left after Wait")
			}
		})
	}
}

// TestExecRunDirRemovedOnFailure: a command that is refused, cannot start, or panics before it
// starts leaves no run directory, and a refused one logs its argv but never its environment.
func TestExecRunDirRemovedOnFailure(t *testing.T) {
	dirs, _, _ := testRunDirs(t, true)
	var logBuf bytes.Buffer
	log, closer, err := logging.New(logging.Options{Level: "debug", StdoutFormat: "json", Stdout: &logBuf})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	secretFile := []byte("secret-file-content-4711")

	// Refused by the allow-list.
	rd, _ := dirs.New(1)
	env := rcloneEnv()
	env["RCLONE_CONFIG_BKDEST_SECRET_ACCESS_KEY"] = "env-secret-value-0815"
	_, err = testRunner(t, ExecOptions{Log: log}).Start(context.Background(), Cmd{Binary: Rclone, Args: []string{"lsf", "BKDEST:bucket", "-vv"}, Env: env, Dir: rd, SecretFiles: map[string][]byte{"ca.pem": secretFile}})
	if !errors.Is(err, ErrNotAllowed) || !gone(rd.SecretDir()) || !gone(rd.DataDir()) {
		t.Errorf("refused: err %v, dirs left %v", err, !gone(rd.SecretDir()))
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "lsf BKDEST:bucket -vv") || strings.Contains(logged, "env-secret-value-0815") || strings.Contains(logged, "secret-file-content") {
		t.Errorf("refusal log: %s", logged)
	}

	// The program is missing.
	rd, _ = dirs.New(2)
	missing := NewExecRunner(ExecOptions{ResticPath: filepath.Join(t.TempDir(), "restic")})
	if _, err := missing.Start(context.Background(), Cmd{Binary: Restic, Args: []string{"version"}, Dir: rd, SecretFiles: map[string][]byte{"password": secretFile}}); err == nil || !gone(rd.SecretDir()) {
		t.Errorf("missing program: err %v, dir left %v", err, !gone(rd.SecretDir()))
	}
	// No path for the engine.
	rd, _ = dirs.New(3)
	if _, err := NewExecRunner(ExecOptions{}).Start(context.Background(), Cmd{Binary: Rclone, Args: []string{"version"}, Env: rcloneEnv(), Dir: rd}); !errors.Is(err, ErrNoBinary) || !gone(rd.SecretDir()) {
		t.Errorf("no binary: err %v", err)
	}

	// A panic after the secret files were written.
	rd, _ = dirs.New(4)
	testHookBeforeStart = func(Cmd) { panic("boom") }
	defer func() { testHookBeforeStart = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("no panic")
			}
		}()
		_, _ = testRunner(t, ExecOptions{}).Start(context.Background(), Cmd{Binary: Restic, Args: []string{"version"}, Dir: rd, SecretFiles: map[string][]byte{"password": secretFile}})
	}()
	if !gone(rd.SecretDir()) || !gone(rd.DataDir()) {
		t.Error("run directory left after a panic in Start")
	}
}

// TestExecCallerPanicThenCancel: a caller that panics after Start and never reads the output
// still gets its run directory removed once its context is cancelled; Wait then reports the
// cancellation.
func TestExecCallerPanicThenCancel(t *testing.T) {
	dirs, _, _ := testRunDirs(t, true)
	rd := helperDir(t, dirs, helperConfig{Mode: "sleep"})
	ctx, cancel := context.WithCancel(context.Background())
	var p Process
	func() {
		defer func() { _ = recover() }()
		var err error
		p, err = testRunner(t, ExecOptions{}).Start(ctx, Cmd{Binary: Restic, Args: []string{"version"}, Dir: rd, SecretFiles: map[string][]byte{"password": []byte("x")}})
		if err != nil {
			t.Error(err)
		}
		panic("caller bug")
	}()
	cancel()
	deadline := time.Now().Add(10 * time.Second)
	for !gone(rd.SecretDir()) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !gone(rd.SecretDir()) || !gone(rd.DataDir()) {
		t.Fatal("run directory left after the caller panicked and cancelled")
	}
	st, err := p.Wait()
	if !st.Cancelled || !errors.Is(err, context.Canceled) || st.Code != 130 {
		t.Fatalf("Wait = %+v, %v", st, err)
	}
}

// TestExecRedaction: every line is redacted before anyone sees it: a registered secret, its
// JSON-escaped form, the command's own Redact values, and a secret across the 1 MiB cut of a
// longer line; the stderr tail keeps 20 lines of at most 512 bytes (S22, §10.2).
func TestExecRedaction(t *testing.T) {
	const registered = `exec-registered"secret-5a1f`
	logging.RegisterSecret(registered)
	escaped := logging.EscapedForms(registered)[0]
	const local = "exec-local-typed-secret-77e0"
	long := strings.Repeat("a", MaxLineBytes-10) + registered + strings.Repeat("b", MaxLineBytes/2)
	errLines := []string{"plain error " + local, `{"message_type":"error","error":{"message":"` + escaped + `"}}`}
	for i := range 25 {
		errLines = append(errLines, strings.Repeat("e", 600)+string(rune('A'+i)))
	}
	dirs, _, _ := testRunDirs(t, true)
	rd := helperDir(t, dirs, helperConfig{Mode: "cat", Out: []string{"token " + registered, long, "ok"}, Err: errLines, Exit: 3})
	p, err := testRunner(t, ExecOptions{}).Start(context.Background(), Cmd{Binary: Restic, Args: []string{"version"}, Dir: rd, Redact: []string{local}})
	if err != nil {
		t.Fatal(err)
	}
	lines, st, err := collect(t, p)
	if err != nil || st.Code != 3 {
		t.Fatalf("exit %+v, %v", st, err)
	}
	if len(lines) != 3+len(errLines) {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		for _, s := range []string{registered, escaped, local, "exec-registered", "exec-local"} {
			if strings.Contains(l.Text, s) {
				t.Errorf("line leaked %q: %.80q", s, l.Text)
			}
		}
		if len(l.Text) > MaxLineBytes {
			t.Errorf("line of %d bytes", len(l.Text))
		}
	}
	if len(st.StderrTail) != StderrTailLines || !strings.HasSuffix(st.StderrTail[StderrTailLines-1], "e") || len(st.StderrTail[0]) > StderrTailBytes {
		t.Errorf("stderr tail: %d lines, last %.20q", len(st.StderrTail), st.StderrTail[len(st.StderrTail)-1])
	}
}

// TestExecStopEscalation: Interrupt and cancellation send SIGINT to the whole process group,
// then SIGTERM, then SIGKILL (S26).
func TestExecStopEscalation(t *testing.T) {
	for _, how := range []string{"interrupt", "cancel", "budget"} {
		t.Run(how, func(t *testing.T) {
			dirs, _, _ := testRunDirs(t, true)
			sigLog := filepath.Join(t.TempDir(), "signals.log")
			rd := helperDir(t, dirs, helperConfig{Mode: "signals", Log: sigLog})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := Cmd{Binary: Restic, Args: []string{"version"}, Dir: rd}
			if how == "budget" {
				c.Budget = 2 * time.Second
			}
			p, err := testRunner(t, ExecOptions{}).Start(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			drain := linesUntil(t, p, "ready")
			start := time.Now()
			switch how {
			case "interrupt":
				p.Interrupt()
			case "cancel":
				cancel()
			}
			st, err := p.Wait()
			drain.Wait()
			if st.Signal != "SIGKILL" || st.Code != 137 {
				t.Errorf("status %+v", st)
			}
			switch how {
			case "interrupt":
				if !st.Interrupted || err != nil {
					t.Errorf("interrupt: %+v, %v", st, err)
				}
			case "cancel":
				if !st.Cancelled || !errors.Is(err, context.Canceled) {
					t.Errorf("cancel: %+v, %v", st, err)
				}
			case "budget":
				if !st.BudgetExceeded || err != nil {
					t.Errorf("budget: %+v, %v", st, err)
				}
			}
			if how != "budget" && time.Since(start) > 5*time.Second {
				t.Errorf("stop took %v", time.Since(start))
			}
			got := readLines(t, sigLog)
			order := func(s string) int { return slices.Index(got, s) }
			for _, role := range []string{"parent", "child"} {
				i, term := order(role+" INT"), order(role+" TERM")
				if i < 0 || term < 0 || term < i {
					t.Errorf("%s signals out of order: %v", role, got)
				}
			}
			if !gone(rd.SecretDir()) {
				t.Error("run directory left")
			}
		})
	}
}

// TestExecRetryWatch: a command whose "returned error, retrying" lines last longer than its
// retry budget is stopped, with the last retry line (S26).
func TestExecRetryWatch(t *testing.T) {
	dirs, _, _ := testRunDirs(t, true)
	rd := helperDir(t, dirs, helperConfig{Mode: "retry"})
	start := time.Now()
	p, err := testRunner(t, ExecOptions{}).Start(context.Background(), Cmd{Binary: Restic, Args: []string{"version"}, Dir: rd, RetryBudget: 400 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, st, err := collect(t, p)
	if err != nil || !st.RetryBudgetExceeded || !strings.Contains(st.LastRetryLine, RetryMarker) || st.Code != 1 {
		t.Fatalf("status %+v, %v", st, err)
	}
	if d := time.Since(start); d < 400*time.Millisecond || d > 10*time.Second {
		t.Errorf("stopped after %v", d)
	}
}

// TestExecIdleWatchdog: OnIdle is called after IdleTimeout without output and again every
// IdleRepeat; the command is not stopped.
func TestExecIdleWatchdog(t *testing.T) {
	dirs, _, _ := testRunDirs(t, true)
	rd := helperDir(t, dirs, helperConfig{Mode: "sleep", Sleep: "900ms"})
	var mu sync.Mutex
	var silents []time.Duration
	c := Cmd{Binary: Restic, Args: []string{"version"}, Dir: rd, IdleTimeout: 200 * time.Millisecond, OnIdle: func(silent time.Duration) {
		mu.Lock()
		silents = append(silents, silent)
		mu.Unlock()
	}}
	p, err := testRunner(t, ExecOptions{IdleRepeat: 250 * time.Millisecond}).Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	_, st, err := collect(t, p)
	if err != nil || st.Code != 0 || st.Stopped() {
		t.Fatalf("status %+v, %v", st, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(silents) < 2 || silents[0] < 200*time.Millisecond {
		t.Fatalf("OnIdle calls %v", silents)
	}
	for i := 1; i < len(silents); i++ {
		if gap := silents[i] - silents[i-1]; gap < 240*time.Millisecond {
			t.Errorf("OnIdle repeated after %v, want every 250ms: %v", gap, silents)
		}
	}
}

func TestExecStdin(t *testing.T) {
	dirs, _, _ := testRunDirs(t, true)
	rd := helperDir(t, dirs, helperConfig{Mode: "stdin"})
	p, err := testRunner(t, ExecOptions{}).Start(context.Background(), Cmd{Binary: Rclone, Args: []string{"rcat", "BKCRYPT:.bunkarr/destination.json"}, Env: rcloneEnv(), Dir: rd, Stdin: strings.NewReader("{\"id\":\"x\"}\nsecond")})
	if err != nil {
		t.Fatal(err)
	}
	lines, st, err := collect(t, p)
	if err != nil || st.Code != 0 || joinLines(lines) != "{\"id\":\"x\"}\nsecond" {
		t.Fatalf("lines %q, %+v, %v", joinLines(lines), st, err)
	}
}

// TestCmdLogsNoEnvironment: a Cmd logged at debug through logging.New, and printed with every
// verb, shows its binary and arguments but no environment value and no secret file (S22).
func TestCmdLogsNoEnvironment(t *testing.T) {
	const envSecret, fileSecret, local = "cmd-env-secret-value-1", "cmd-file-secret-value-2", "cmd-local-arg-value-3"
	c := Cmd{Binary: Rclone, Args: []string{"lsf", "BKDEST:bucket/" + local}, Redact: []string{local},
		Env:         map[string]string{"RCLONE_CONFIG_BKDEST_SECRET_ACCESS_KEY": envSecret},
		SecretFiles: map[string][]byte{"password": []byte(fileSecret)}}
	for _, format := range []string{"json", "text"} {
		var buf bytes.Buffer
		log, closer, err := logging.New(logging.Options{Level: "debug", StdoutFormat: format, Stdout: &buf})
		if err != nil {
			t.Fatal(err)
		}
		log.Debug("running", "cmd", c, "ptr", &c, "any", slog.AnyValue(c))
		_ = closer.Close()
		out := buf.String()
		if !strings.Contains(out, "lsf") || strings.Contains(out, envSecret) || strings.Contains(out, fileSecret) || strings.Contains(out, local) {
			t.Errorf("%s log: %s", format, out)
		}
	}
	for _, s := range []string{fmtAll(c), fmtAll(&c)} {
		if strings.Contains(s, envSecret) || strings.Contains(s, fileSecret) || strings.Contains(s, local) || !strings.Contains(s, "lsf") {
			t.Errorf("formatted: %s", s)
		}
	}
	b, _ := jsonMarshal(c)
	if string(b) != `{"binary":"rclone","args":["lsf","BKDEST:bucket/[REDACTED]"]}` {
		t.Errorf("JSON %s", b)
	}
}
