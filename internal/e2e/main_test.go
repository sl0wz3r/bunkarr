//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// binary is the bunkarr binary under test, built once on first use (Docker-only runs never build
// it). It is the package's only mutable global: written once under once, removed by TestMain.
var binary struct {
	once sync.Once
	dir  string
	path string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binary.dir != "" {
		_ = os.RemoveAll(binary.dir)
	}
	os.Exit(code)
}

// bunkarrBinary returns the path of the binary under test: $BUNKARR_E2E_BINARY when set, else
// `go build ./cmd/bunkarr` of this module into a temporary directory.
func bunkarrBinary(t *testing.T) string {
	t.Helper()
	binary.once.Do(func() {
		if p := os.Getenv("BUNKARR_E2E_BINARY"); p != "" {
			binary.path, binary.err = filepath.Abs(p)
			return
		}
		root, err := moduleRoot()
		if err != nil {
			binary.err = err
			return
		}
		dir, err := os.MkdirTemp("", "bunkarr-e2e-bin-")
		if err != nil {
			binary.err = fmt.Errorf("temp dir for the binary: %w", err)
			return
		}
		binary.dir = dir
		out := filepath.Join(dir, "bunkarr")
		cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/bunkarr")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			binary.err = fmt.Errorf("go build ./cmd/bunkarr: %w\n%s", err, b)
			return
		}
		binary.path = out
	})
	if binary.err != nil {
		t.Fatal(binary.err)
	}
	return binary.path
}

// moduleRoot returns the directory of this module's go.mod.
func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		return "", fmt.Errorf("not inside a Go module (go env GOMOD = %q)", mod)
	}
	return filepath.Dir(mod), nil
}
