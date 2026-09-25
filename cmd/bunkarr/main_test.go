package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// TestServeWiresTheServicesAndStops runs the real server in-process: it answers, the Phase 1
// routes need the API key and work with it, the scheduler is running (the seeded retention
// schedule has a next run), and cancelling the context shuts everything down cleanly.
func TestServeWiresTheServicesAndStops(t *testing.T) {
	dir := t.TempDir()
	env := config.Env{ConfigDir: dir, Bind: "127.0.0.1", Port: 0, LogLevel: "warn", LogFormat: "text"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan net.Addr, 1)
	errc := make(chan error, 1)
	go func() { errc <- serve(ctx, env, io.Discard, func(a net.Addr) { addrc <- a }) }()
	var base string
	select {
	case a := <-addrc:
		base = "http://" + a.String() + "/api/v1"
	case err := <-errc:
		t.Fatalf("serve: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not start")
	}

	get := func(path, key string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", base+path, nil)
		if key != "" {
			req.Header.Set("X-Api-Key", key)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, _ := get("/health", ""); code != 200 {
		t.Fatalf("health: %d", code)
	}
	if code, _ := get("/sources", ""); code != 401 {
		t.Fatalf("sources without a key: %d", code)
	}

	// The API key, read the way the server stores it.
	master, _, err := config.LoadOrCreateMasterKey(env.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	kr, _ := config.NewKeyring(master)
	d, err := db.Open(context.Background(), env.DBPath(), nil)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := config.NewSettings(d, kr).GetSecret(context.Background(), config.KeyAPIKey)
	_ = d.Close()
	if err != nil || key == "" {
		t.Fatalf("API key: %v", err)
	}
	if code, body := get("/sources", key); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Fatalf("sources: %d %s", code, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body := get("/schedules", key)
		if code == 200 && strings.Contains(body, `"jobType":"retention"`) && !strings.Contains(body, `"nextRunAt":null`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("schedules (scheduler not running?): %d %s", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not stop")
	}
}
