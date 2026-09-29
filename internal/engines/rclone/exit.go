package rclone

import (
	"errors"
	"fmt"

	"github.com/sl0wz3r/bunkarr/internal/engines"
)

// ExitClass is what an rclone exit code means for Bunkarr (docs/design/phase4.md §10.3).
type ExitClass int

// Exit classes.
const (
	// ExitOK: exit 0.
	ExitOK ExitClass = iota
	// ExitObjects: exit 1 with ERROR lines naming objects, or exit 6: those objects' items
	// fail, the rest of the batch is decided by the listing.
	ExitObjects
	// ExitItemNotFound: exit 4, a file not found; the item fails.
	ExitItemNotFound
	// ExitTemporary: exit 5; run retries the command once after RetryWait, then it is fatal.
	ExitTemporary
	// ExitCutoff: exit 10, --max-duration reached: a window deferral (§9.2), nothing failed.
	ExitCutoff
	// ExitSignal: exit 130 or 143 (SIGINT, SIGTERM): a cancellation or a deferral.
	ExitSignal
	// ExitFatal: exit 1 without object errors, 2 (usage), 3 (bucket or path not found), 7
	// (--max-delete reached, overlap) and anything else: the job fails.
	ExitFatal
)

// String names the class.
func (c ExitClass) String() string {
	switch c {
	case ExitOK:
		return "ok"
	case ExitObjects:
		return "per-object"
	case ExitItemNotFound:
		return "item-not-found"
	case ExitTemporary:
		return "temporary"
	case ExitCutoff:
		return "cutoff"
	case ExitSignal:
		return "signal"
	}
	return "fatal"
}

// ClassifyExit maps an rclone exit code to its class; objectErrors reports ERROR lines that
// named objects (§10.3).
func ClassifyExit(code int, objectErrors bool) ExitClass {
	switch code {
	case 0:
		return ExitOK
	case 1:
		if objectErrors {
			return ExitObjects
		}
		return ExitFatal
	case 4:
		return ExitItemNotFound
	case 5:
		return ExitTemporary
	case 6:
		return ExitObjects
	case 10:
		return ExitCutoff
	case 130, 143:
		return ExitSignal
	}
	return ExitFatal
}

// Errors of the rclone driver. *Error wraps one of them (or an engines sentinel); errors.Is
// identifies them.
var (
	// ErrPathNotFound: exit 3, the bucket or a directory does not exist.
	ErrPathNotFound = errors.New("bucket or path not found")
	// ErrObjectNotFound: exit 4.
	ErrObjectNotFound = errors.New("object not found")
	// ErrTemporary: exit 5 twice (the command was retried once).
	ErrTemporary = errors.New("temporary error (retried once)")
	// ErrUsage: exit 2, rclone refused its arguments (a Bunkarr bug; the argv is logged).
	ErrUsage = errors.New("rclone refused its arguments")
	// ErrMaxDelete: exit 7 because --max-delete was reached (S23).
	ErrMaxDelete = errors.New("--max-delete threshold reached")
	// ErrFatal: exit 7 for another reason (an overlap), or any other fatal exit.
	ErrFatal = errors.New("rclone stopped with a fatal error")
	// ErrFailed: exit 1 without an error naming an object (credentials, connection).
	ErrFailed = errors.New("rclone failed")
	// ErrMaxTransfer: exit 8, a download reached its --max-transfer cap (Download): the object is
	// larger than it was listed.
	ErrMaxTransfer = errors.New("--max-transfer reached (the object is larger than it was listed)")
	// ErrCutoff: exit 10 where a cutoff is not expected.
	ErrCutoff = errors.New("--max-duration reached")
	// ErrInterrupted: Bunkarr interrupted the command (a transfer window's end) where the caller
	// did not expect it.
	ErrInterrupted = errors.New("interrupted")
	// ErrBudget: the command ran longer than its wall-clock budget (S26).
	ErrBudget = errors.New("ran longer than its time budget")
	// ErrRetryBudget: the command kept retrying longer than the retry budget (S26).
	ErrRetryBudget = errors.New("kept retrying longer than the retry budget")
	// ErrUndecryptable: a crypt listing met names it cannot decrypt: a wrong crypt password, or
	// data of another crypt remote (strict_names, S25).
	ErrUndecryptable = errors.New("names cannot be decrypted (wrong crypt password, or data of another crypt remote)")
)

// Error is a failed rclone command.
type Error struct {
	// Command is "rclone <subcommand>".
	Command string
	Code    int
	Class   ExitClass
	// Message is the most telling line rclone printed (redacted).
	Message string
	// Err is the sentinel (ErrPathNotFound, engines.ErrHostKeyChanged, …).
	Err error
}

// Error implements error.
func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %v", e.Command, e.Err)
	if e.Code != 0 {
		s += fmt.Sprintf(" (exit %d)", e.Code)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Unwrap returns the sentinel.
func (e *Error) Unwrap() error { return e.Err }

// fail turns an outcome that is not a success into an *Error. Callers decide first which
// classes are results for them (per-object errors, a cutoff, an interrupt).
func fail(cmd command, out outcome) *Error {
	st := out.status
	e := &Error{Command: cmd.name(), Code: st.Code, Class: ClassifyExit(st.Code, len(out.objectErrors) > 0), Message: out.message()}
	switch {
	case st.BudgetExceeded:
		e.Err = ErrBudget
	case st.RetryBudgetExceeded:
		e.Err, e.Message = ErrRetryBudget, st.LastRetryLine
	case st.Interrupted:
		e.Err = ErrInterrupted
	case out.hostKey:
		e.Err = engines.ErrHostKeyChanged
	case out.undecryptable:
		e.Err = ErrUndecryptable
	case st.Code == 2:
		e.Err = ErrUsage
	case st.Code == 3:
		e.Err = ErrPathNotFound
	case st.Code == 4:
		e.Err = ErrObjectNotFound
	case st.Code == 5:
		e.Err = ErrTemporary
	case st.Code == 7 && out.maxDelete:
		e.Err = ErrMaxDelete
	case st.Code == 8:
		e.Err = ErrMaxTransfer
	case st.Code == 10:
		e.Err = ErrCutoff
	case st.Code == 1:
		e.Err = ErrFailed
	default:
		e.Err = ErrFatal
	}
	return e
}

// check returns nil for exit 0 and an *Error for anything else.
func check(cmd command, out outcome) error {
	if out.status.Code == 0 && !out.status.Stopped() {
		return nil
	}
	return fail(cmd, out)
}
