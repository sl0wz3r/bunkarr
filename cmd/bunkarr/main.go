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
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/api"
	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
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
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := serve(ctx, env, stdout, nil); err != nil {
			fmt.Fprintln(stderr, "bunkarr:", err)
			return 1
		}
		return 0
	}
}

// Shutdown budgets: HTTP requests in flight get httpShutdownTimeout; then the scheduler stops,
// running jobs get the job manager's grace period (20 s) to stop and be re-queued, and queued
// notifications are sent, all within servicesShutdownTimeout.
const (
	httpShutdownTimeout     = 15 * time.Second
	servicesShutdownTimeout = 30 * time.Second
)

// serve runs the server until ctx ends (SIGINT/SIGTERM) or the listener fails. Start-up: config
// dir, lock, logging, fault injection (BUNKARR_FAULTPOINT, tests only), master key, database,
// auth, the restic and rclone engines (discovered and logged), the services (api.App: stores,
// runners, scheduler, notifications; stored secrets registered for redaction; the engines' run
// directories swept), the HTTP listener, and only then the job manager (which resumes
// interrupted jobs) and the scheduler. Shutdown: stop accepting HTTP requests, stop the
// scheduler, stop the job manager (running jobs are re-queued to resume), flush notifications,
// close the database. ready, when set, receives the listening address.
func serve(ctx context.Context, env config.Env, stdout io.Writer, ready func(net.Addr)) error {
	started := time.Now()
	faultinject.InitFromEnv()
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

	settings := config.NewSettings(database, kr)
	authSvc := auth.New(database, settings, log)
	if err := authSvc.Init(ctx); err != nil {
		return err
	}
	if setup, err := authSvc.SetupRequired(ctx); err == nil && setup {
		log.Warn("No user exists yet: open the web UI to create one. Until then only the API key can use the API.")
	}

	app, err := api.NewApp(ctx, api.AppOptions{DB: database, Keyring: kr, Settings: settings, ConfigDir: env.ConfigDir, Log: log,
		Plex: api.PlexOptions(env.Docker), Engines: discoverEngines(ctx, env, log, started)})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              env.Addr(),
		Handler:           api.New(api.Options{Auth: authSvc, DB: database, Env: env, Log: log, Web: web.FS(), Started: started, App: app}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		if serr := app.Stop(context.Background()); serr != nil { // nothing started; releases the notification workers
			log.Warn("Services did not stop cleanly", "error", serr)
		}
		return fmt.Errorf("listen on %s: %w", srv.Addr, err)
	}
	log.Info("Listening", "address", ln.Addr().String())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if ready != nil {
		ready(ln.Addr())
	}

	var serveErr error
	if err := app.Start(ctx); err != nil {
		if ctx.Err() == nil { // a signal during start-up is a normal shutdown
			serveErr = err
		}
	} else {
		select {
		case serveErr = <-errc:
			errc = nil
		case <-ctx.Done():
		}
	}
	log.Info("Shutting down")
	if errc != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("Graceful HTTP shutdown timed out", "error", err)
		}
		cancel()
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) && serveErr == nil {
			serveErr = err
		}
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), servicesShutdownTimeout)
	defer cancel()
	if err := app.Stop(stopCtx); err != nil {
		log.Warn("Services did not stop cleanly", "error", err)
	}
	if serveErr != nil {
		return serveErr
	}
	log.Info("Stopped")
	return nil
}

// discoverEngines finds the restic and rclone binaries (BUNKARR_RESTIC_PATH, BUNKARR_RCLONE_PATH,
// else PATH; checked by config.ResolveEngineBinaries), builds the exec runner over the usable ones
// and runs their version commands (engines.Discover: restic 0.17+, rclone 1.66+), logging what it
// found (docs/design/phase4.md §4.4, §10.1). An unavailable engine only makes its destinations
// unusable: filecopy destinations do not need either.
func discoverEngines(ctx context.Context, env config.Env, log *slog.Logger, started time.Time) api.EngineOptions {
	resticBin, rcloneBin := config.ResolveEngineBinaries(env)
	usable := func(b config.EngineBinary) string {
		if b.Available() {
			return b.Path
		}
		return ""
	}
	runner := proc.NewExecRunner(proc.ExecOptions{ResticPath: usable(resticBin), RclonePath: usable(rcloneBin),
		Log: log.With("component", "engines")})
	avail := engines.Discover(ctx, runner, resticBin, rcloneBin)
	for _, e := range []struct {
		name string
		st   engines.BinaryStatus
	}{{"restic", avail.Restic}, {"rclone", avail.Rclone}} {
		if e.st.Available {
			log.Info("Backup engine available", "engine", e.name, "version", e.st.Version, "path", e.st.Path)
		} else {
			log.Warn("Backup engine unavailable: its destinations cannot be used", "engine", e.name, "reason", e.st.Reason, "path", e.st.Path)
		}
	}
	host, err := os.Hostname()
	if err != nil {
		log.Warn("Could not read the host name; restic lock checks cannot tell this container's locks apart", "error", err)
	} else if looksLikeContainerID(host) {
		log.Warn("The host name looks like a Docker container ID, which changes whenever the container is recreated (an update or an edit): "+
			"restic then counts the locks the previous container left as another host's and waits up to 30 minutes for them to become stale. "+
			"Give this install a stable host name of its own", "hostName", host, "fix", hostNameFix(os.Getenv("HOST_HOSTNAME")))
	}
	return api.EngineOptions{Runner: runner, Restic: resticBin, Rclone: rcloneBin, Availability: avail, HostName: host, ProcessStart: started}
}

var (
	// containerIDHost is Docker's host name for a container started without one (no --hostname,
	// no compose hostname): the first 12 hex digits of the container ID.
	containerIDHost = regexp.MustCompile(`^[0-9a-f]{12}$`)
	// hostLabel is a lower-case server name that makes bunkarr-<name> a valid host name label: at
	// most 63 characters in all, letters, digits and inner hyphens.
	hostLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,53}[a-z0-9])?$`)
)

// looksLikeContainerID reports whether host is Docker's default host name, which a recreated
// container does not keep. restic tells its own stale locks apart from another container's by the
// host name (docs/design/phase4.md §6.7), so with a new name after every update Bunkarr waits for
// the locks its previous container left instead of removing them at once.
func looksLikeContainerID(host string) bool {
	return containerIDHost.MatchString(host)
}

// hostNameFix is the advice for a host name that looksLikeContainerID. unraidServer is
// HOST_HOSTNAME, the server name Unraid's dockerMan passes to the containers it creates: when it
// makes a valid host name, the advice is the exact Extra Parameters flag the template ships
// (--hostname=bunkarr-tower on a server named Tower). Anything else (another Docker host, an
// Unraid that does not pass it, a name with other characters) gets the general advice.
func hostNameFix(unraidServer string) string {
	if name := strings.ToLower(strings.TrimSpace(unraidServer)); hostLabel.MatchString(name) {
		return "add --hostname=bunkarr-" + name + " to Extra Parameters (Advanced View) in this container's Unraid settings"
	}
	return "set --hostname=bunkarr-SERVER (docker run, or Extra Parameters on Unraid) or SERVER_NAME in deploy/.env (Docker Compose), " +
		"with this server's name for SERVER"
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
