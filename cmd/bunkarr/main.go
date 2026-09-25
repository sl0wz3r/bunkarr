// Command bunkarr is the Bunkarr server: backups for Plex libraries and the *arr stack.
//
//	bunkarr [serve] [flags]     run the server (default)
//	bunkarr version             print the version
//	bunkarr healthcheck         exit 0 when the local server answers /api/v1/health (Docker HEALTHCHECK)
//	bunkarr reset-auth          remove the UI user so the first-run setup appears again
//
// Configuration comes from BUNKARR_* environment variables; flags override them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/api"
	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/lock"
	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/version"
	"github.com/sl0wz3r/bunkarr/web"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	env, err := config.EnvFromOS()
	if err != nil {
		fmt.Fprintln(stderr, "bunkarr:", err)
		return 2
	}
	fs := flag.NewFlagSet("bunkarr "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&env.ConfigDir, "config", env.ConfigDir, "config directory (database, key, logs) [BUNKARR_CONFIG_DIR]")
	fs.StringVar(&env.Bind, "bind", env.Bind, "listen address, empty for all interfaces [BUNKARR_BIND]")
	fs.IntVar(&env.Port, "port", env.Port, "listen port [BUNKARR_PORT]")
	fs.StringVar(&env.LogLevel, "log-level", env.LogLevel, "debug, info, warn or error [BUNKARR_LOG_LEVEL]")

	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "Bunkarr %s (commit %s, built %s)\n", version.Version, version.Commit, orUnknown(version.BuildDate))
		return 0
	case "serve", "healthcheck", "reset-auth":
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, "usage: bunkarr [serve|version|healthcheck|reset-auth] [flags]")
		fs.SetOutput(stdout)
		fs.PrintDefaults()
		return 0
	default:
		fmt.Fprintf(stderr, "bunkarr: unknown command %q (serve, version, healthcheck, reset-auth)\n", cmd)
		return 2
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := env.Validate(); err != nil {
		fmt.Fprintln(stderr, "bunkarr:", err)
		return 2
	}

	switch cmd {
	case "healthcheck":
		return healthcheck(env, stderr)
	case "reset-auth":
		return resetAuth(env, stdout, stderr)
	default:
		if err := serve(env, stdout); err != nil {
			fmt.Fprintln(stderr, "bunkarr:", err)
			return 1
		}
		return 0
	}
}

func serve(env config.Env, stdout io.Writer) error {
	started := time.Now()
	if err := os.MkdirAll(env.ConfigDir, 0o750); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	release, err := lock.Acquire(filepath.Join(env.ConfigDir, "bunkarr.lock"))
	if err != nil {
		return err
	}
	defer release()

	log, logCloser, err := logging.New(logging.Options{Dir: env.LogDir(), Level: env.LogLevel, StdoutFormat: env.LogFormat, Stdout: stdout})
	if err != nil {
		return err
	}
	defer logCloser.Close()
	slog.SetDefault(log)
	log.Info("Starting Bunkarr", "version", version.Version, "commit", version.Commit, "configDir", env.ConfigDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	master, created, err := config.LoadOrCreateMasterKey(env.KeyPath())
	if err != nil {
		return err
	}
	if created {
		log.Info("Created a new master key; back up bunkarr.key together with bunkarr.db", "path", env.KeyPath())
	} else if fi, err := os.Stat(env.KeyPath()); err == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Warn("bunkarr.key is readable by other users; chmod 600 it", "path", env.KeyPath(), "mode", fi.Mode().Perm().String())
	}
	kr, err := config.NewKeyring(master)
	if err != nil {
		return err
	}

	database, err := db.Open(ctx, env.DBPath(), log)
	if err != nil {
		return err
	}
	defer database.Close()

	authSvc := auth.New(database, config.NewSettings(database, kr), log)
	if err := authSvc.Init(ctx); err != nil {
		return err
	}
	if setup, err := authSvc.SetupRequired(ctx); err == nil && setup {
		log.Warn("No user exists yet: open the web UI to create one. Until then only the API key can use the API.")
	}

	srv := &http.Server{
		Addr:              env.Addr(),
		Handler:           api.New(api.Options{Auth: authSvc, DB: database, Env: env, Log: log, Web: web.FS(), Started: started}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", srv.Addr, err)
	}
	log.Info("Listening", "address", ln.Addr().String())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("Shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("Graceful shutdown timed out", "error", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("Stopped")
	return nil
}

func healthcheck(env config.Env, stderr io.Writer) int {
	host := env.Bind
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, strconv.Itoa(env.Port)) + "/api/v1/health"
	c := &http.Client{Timeout: 5 * time.Second}
	res, err := c.Get(url)
	if err != nil {
		fmt.Fprintln(stderr, "unhealthy:", err)
		return 1
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fmt.Fprintln(stderr, "unhealthy: HTTP", res.StatusCode)
		return 1
	}
	return 0
}

func resetAuth(env config.Env, stdout, stderr io.Writer) int {
	ctx := context.Background()
	database, err := db.Open(ctx, env.DBPath(), nil)
	if err != nil {
		fmt.Fprintln(stderr, "bunkarr:", err)
		return 1
	}
	defer database.Close()
	if err := auth.ResetAuth(ctx, database); err != nil {
		fmt.Fprintln(stderr, "bunkarr:", err)
		return 1
	}
	fmt.Fprintln(stdout, "The Bunkarr user was removed. Open the web UI to create a new one (the API key is unchanged).")
	return 0
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
