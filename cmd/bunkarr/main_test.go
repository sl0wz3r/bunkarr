package main

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
)

func TestVersionAndUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "Bunkarr ") {
		t.Fatalf("version: %d %q", code, out.String())
	}
	if code := run([]string{"frobnicate"}, &out, &errb); code != 2 {
		t.Fatalf("unknown command: exit %d, want 2", code)
	}
}

func TestHealthcheckFailsWithoutServer(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	var out, errb bytes.Buffer
	t.Setenv("BUNKARR_CONFIG_DIR", t.TempDir())
	if code := run([]string{"healthcheck", "-port", strconv.Itoa(port)}, &out, &errb); code != 1 {
		t.Fatalf("healthcheck with nothing listening: exit %d, want 1", code)
	}
}

func TestResetAuth(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(dir, "bunkarr.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	kr, _ := config.NewKeyring(make([]byte, 32))
	a := auth.New(d, config.NewSettings(d, kr), nil)
	a.SetBcryptCost(4)
	_ = a.Init(ctx)
	if _, err := a.Setup(ctx, "admin", "correct horse"); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"reset-auth", "-config", dir}, &out, &errb); code != 0 {
		t.Fatalf("reset-auth: exit %d: %s", code, errb.String())
	}
	d, _ = db.Open(ctx, filepath.Join(dir, "bunkarr.db"), nil)
	defer d.Close()
	a = auth.New(d, config.NewSettings(d, kr), nil)
	if req, _ := a.SetupRequired(ctx); !req {
		t.Fatal("setup not required after reset-auth")
	}
}
