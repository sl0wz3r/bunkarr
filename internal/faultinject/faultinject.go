// Package faultinject names the step boundaries of Bunkarr's crash-sensitive code (copy, rename,
// DB record, plan batches) so tests can stop the program exactly there.
//
// In production nothing is registered and Point is a single atomic load. Tests use SetHook (Go
// crash-matrix tests: the hook panics with Crash and the harness recovers it) or the environment
// (end-to-end kill tests: BUNKARR_FAULTPOINT=<name> makes the process create the file named by
// BUNKARR_FAULTPOINT_FILE when it reaches that point and then block forever, so the test can kill
// it with SIGKILL at a known place).
package faultinject

import (
	"os"
	"sync/atomic"
)

// Crash is the panic value a crash-matrix hook uses; harnesses recover exactly this type.
type Crash struct{ Point string }

var hook atomic.Pointer[func(name string)]

// Point marks a step boundary. It calls the registered hook, if any.
func Point(name string) {
	if h := hook.Load(); h != nil {
		(*h)(name)
	}
}

// SetHook registers fn for every Point call (nil removes it). Tests only.
func SetHook(fn func(name string)) {
	if fn == nil {
		hook.Store(nil)
		return
	}
	hook.Store(&fn)
}

// CrashAt returns a hook that panics with Crash when point is reached for the n-th time (n >= 1).
func CrashAt(point string, n int) func(string) {
	var seen atomic.Int64
	return func(name string) {
		if name == point && seen.Add(1) == int64(n) {
			panic(Crash{Point: name})
		}
	}
}

// InitFromEnv installs the pause-and-signal hook when BUNKARR_FAULTPOINT is set. Called once by
// main; a no-op otherwise.
func InitFromEnv() {
	point := os.Getenv("BUNKARR_FAULTPOINT")
	if point == "" {
		return
	}
	signal := os.Getenv("BUNKARR_FAULTPOINT_FILE")
	SetHook(func(name string) {
		if name != point {
			return
		}
		if signal != "" {
			_ = os.WriteFile(signal, []byte(name+"\n"), 0o600)
		}
		select {} // block until killed
	})
}
