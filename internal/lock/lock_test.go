//go:build unix

package lock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSecondAcquireFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bunkarr.lock")
	release, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire: err = %v, want ErrLocked", err)
	}
	release()
	release2, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	release2()
}
