//go:build unix

// Package lock keeps a second Bunkarr process from using the same config directory: two servers
// on one database would both run the scheduler and write to the same destinations.
package lock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("another Bunkarr instance is already using this config directory")

// Acquire takes an exclusive, non-blocking lock on path for the life of the process. The lock is
// released by the returned function or when the process exits, whatever the reason.
func Acquire(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}
