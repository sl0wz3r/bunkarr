// Package proc is the exec layer of the restic and rclone engines (docs/design/phase4.md §10.1,
// §10.2, safety rules S22 and S26). Every engine command goes through a Runner:
//
//   - a Cmd is checked against the allow-list of allowlist.go before anything runs (Validate):
//     only the listed subcommands, each with its exact flags, restic -o only for rclone.program
//     and rclone.connections, no positional that looks like a flag, only the listed environment
//     names, and no argument that the output redaction would change (a registered value or one
//     of its Redact values, an S3 access key ID or B2 keyId included, so a path the command names
//     never comes back in its output as "[REDACTED]") or that holds a secret of its environment
//     or secret files, and no --files-from-raw list whose entries would come back redacted (any
//     entry of an rclone lsjson listing, the prefix shared by every entry of another list);
//   - secrets reach the child only through its environment and through secret files, written
//     with O_CREATE|O_EXCL|O_NOFOLLOW and mode 0600 into a per-command directory (0700) on tmpfs
//     when statfs says so, else in <config>/run with a warning (RunDirs); the directory is removed
//     when the command ends, also on error, panic and cancellation;
//   - the child gets no inherited environment beyond PATH, TZ, LANG and TMPDIR (never BUNKARR_*
//     or AWS_*), runs without a shell in its own process group, and is stopped with SIGINT, then
//     SIGTERM after 30 s, then SIGKILL after 10 s more, on cancellation, at its budget, when its
//     "returned error, retrying" lines outlast the retry budget, and on Process.Interrupt;
//   - every line it prints, stdout and stderr, is redacted (logging.RedactTruncated: registered
//     secrets and Cmd.Redact) before anything else sees it, and a line longer than MaxLineBytes is
//     redacted before it is cut;
//   - an idle watchdog reports a command that printed nothing for 15 minutes, then hourly.
//
// NewExecRunner is the production Runner (os/exec); enginetest.FakeRunner is the scripted one
// the unit tests use, with the same checks.
package proc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// Binary names an engine program.
type Binary string

// Engine programs.
const (
	Restic Binary = "restic"
	Rclone Binary = "rclone"
)

// Limits and defaults of the exec layer (§10.2, S26).
const (
	// MaxLineBytes is the longest line kept from a child; a longer one is redacted, then cut.
	MaxLineBytes = 1 << 20
	// StderrTailLines is how many of the last stderr lines ExitStatus keeps.
	StderrTailLines = 20
	// StderrTailBytes is the length each of those lines is cut to.
	StderrTailBytes = 512
	// DefaultIdleTimeout is how long a command may print nothing before OnIdle is called.
	DefaultIdleTimeout = 15 * time.Minute
	// IdleRepeat is how often OnIdle is called again while the command stays silent.
	IdleRepeat = time.Hour
	// DefaultTermAfter is how long a command has after SIGINT before SIGTERM.
	DefaultTermAfter = 30 * time.Second
	// DefaultKillAfter is how long a command has after SIGTERM before SIGKILL.
	DefaultKillAfter = 10 * time.Second
	// RetryMarker is the text of restic's (and its rclone backend's) retry lines.
	RetryMarker = "returned error, retrying"
)

// Errors of the exec layer.
var (
	// ErrNotAllowed means a Cmd failed Validate: a programming error, or an argument holding a
	// value its output would be redacted of (a bucket, folder or path named like a stored
	// credential). Nothing ran.
	ErrNotAllowed = errors.New("engine command not allowed")
	// ErrNoBinary means the runner has no path for the command's binary (the engine is
	// unavailable).
	ErrNoBinary = errors.New("engine binary not available")
)

// Cmd is one engine command. Args holds the subcommand words, then flags and positionals, never a
// secret; Env holds every variable the child gets beyond PATH, TZ, LANG, TMPDIR and HOME;
// SecretFiles are written into Dir right before the command starts. Cmd's String, GoString,
// LogValue and MarshalJSON show the binary and the arguments only, never Env, SecretFiles or
// Redact.
type Cmd struct {
	Binary Binary
	// Args are the subcommand words, flags and positionals: {"copy", "/src", "BKCRYPT:Movies",
	// "--files-from-raw", "/config/run/12-ab-data/files", ...}.
	Args []string
	// Env are the child's variables (names from the allow-list only).
	Env map[string]string
	// SecretFiles maps a file name to its content, written to Dir.SecretPath(name) (0600) right
	// before the command starts and removed with Dir when it ends. Requires Dir.
	SecretFiles map[string][]byte
	// Dir is the command's run directory, which the runner removes when the command ends. HOME is
	// its data directory. Nil only for commands without files (version).
	Dir *RunDir
	// Stdin is the child's standard input (nil: none).
	Stdin io.Reader
	// Budget bounds the command's wall-clock time (0: none; S26: 60 s for Test and Create, 10 min
	// for listings, marker reads and unlock). ExitStatus.BudgetExceeded reports a stop.
	Budget time.Duration
	// IdleTimeout is how long the command may print nothing before OnIdle is called (0:
	// DefaultIdleTimeout; negative: never).
	IdleTimeout time.Duration
	// RetryBudget bounds a streak of RetryMarker lines on stderr (0: no retry watch; S26: 10 min,
	// 20 s for Test and Create). ExitStatus.RetryBudgetExceeded reports a stop.
	RetryBudget time.Duration
	// Redact are values to redact from every line besides the registered secrets: this
	// command's secrets in clear, obscured and escaped form (engines.Secrets.Values, its S3
	// access key ID or B2 keyId included), for a Test or Create whose values are not registered.
	// No argument may contain one or a registered value (Validate), since the output that names
	// the argument would be read back redacted.
	Redact []string
	// OnIdle is called (from the runner's goroutine) when the command printed nothing for
	// IdleTimeout, and again every IdleRepeat while it stays silent.
	OnIdle func(silent time.Duration)
}

// Argv is the binary followed by the arguments.
func (c Cmd) Argv() []string {
	return append([]string{string(c.Binary)}, c.Args...)
}

// String renders the binary and arguments, with Redact values replaced (a Validate failure
// logs this).
func (c Cmd) String() string {
	return logging.RedactValues(strings.Join(c.Argv(), " "), c.Redact...)
}

// GoString implements fmt.GoStringer like String, so %#v shows no environment or secret file.
func (c Cmd) GoString() string { return "proc.Cmd{" + c.String() + "}" }

// LogValue implements slog.LogValuer: the binary and the arguments.
func (c Cmd) LogValue() slog.Value {
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = logging.RedactValues(a, c.Redact...)
	}
	return slog.GroupValue(slog.String("binary", string(c.Binary)), slog.Any("args", args))
}

// MarshalJSON implements json.Marshaler: {"binary": ..., "args": [...]}.
func (c Cmd) MarshalJSON() ([]byte, error) {
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = logging.RedactValues(a, c.Redact...)
	}
	return json.Marshal(struct {
		Binary Binary   `json:"binary"`
		Args   []string `json:"args"`
	}{c.Binary, args})
}

// Line is one line a child printed, already redacted.
type Line struct {
	Stderr bool
	Text   string
}

// ExitStatus is how a command ended.
type ExitStatus struct {
	// Code is the exit code; 128+n when the process died of signal n (Signal names it), so a
	// SIGINT is 130 and a SIGTERM 143 as in §10.3.
	Code   int
	Signal string
	// Interrupted: Bunkarr stopped it with Process.Interrupt (a window's end).
	Interrupted bool
	// Cancelled: the context was cancelled.
	Cancelled bool
	// BudgetExceeded: the command ran longer than Cmd.Budget and was stopped.
	BudgetExceeded bool
	// RetryBudgetExceeded: its retry lines lasted longer than Cmd.RetryBudget and it was
	// stopped; LastRetryLine is the last one (redacted).
	RetryBudgetExceeded bool
	LastRetryLine       string
	// StderrTail is the last StderrTailLines lines of stderr, each cut to StderrTailBytes.
	StderrTail []string
}

// Stopped reports whether Bunkarr stopped the command (any reason) rather than it ending by
// itself.
func (s ExitStatus) Stopped() bool {
	return s.Interrupted || s.Cancelled || s.BudgetExceeded || s.RetryBudgetExceeded
}

// Runner starts engine commands.
type Runner interface {
	// Start validates c and starts it. A Validate failure (ErrNotAllowed) is a programming error:
	// it is returned, the argv is logged, and nothing runs. On any error Start removes c.Dir.
	Start(ctx context.Context, c Cmd) (Process, error)
}

// Process is a started command.
type Process interface {
	// Lines yields the command's output lines, stdout and stderr interleaved as they arrive,
	// redacted; it is closed after the command ended and both streams are drained. Read it until
	// it is closed, then call Wait (or call Wait alone, which discards unread lines).
	Lines() <-chan Line
	// Interrupt stops the command on Bunkarr's behalf (a transfer window's end): SIGINT to its
	// process group, then SIGTERM and SIGKILL if it does not exit. ExitStatus.Interrupted is set.
	Interrupt()
	// Wait waits for the command to end and its run directory to be removed. The error is
	// non-nil only when the command did not run to its own end for a reason other than
	// Interrupt, budget or retry budget (then ExitStatus says which): the context's error after
	// a cancellation, or a failure of the runner itself.
	Wait() (ExitStatus, error)
}

// RedactLine redacts one output line of c (registered secrets and c.Redact) and cuts it to
// MaxLineBytes; partial says the line was already cut while it was read. The runners call it for
// every line, before anything else sees it.
func RedactLine(text string, partial bool, redact []string) string {
	return logging.RedactTruncated(text, MaxLineBytes, partial, redact...)
}

// tailLine is how a stderr line is kept in ExitStatus.StderrTail.
func tailLine(text string) string {
	return logging.RedactTruncated(text, StderrTailBytes, false)
}

// errNotAllowed wraps ErrNotAllowed with detail.
func errNotAllowed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotAllowed, fmt.Sprintf(format, args...))
}
