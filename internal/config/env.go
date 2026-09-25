// Package config holds Bunkarr's bootstrap configuration (environment variables and flags), the
// master key that seals secrets at rest, and the settings store in SQLite.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultPort is the web UI and API port.
const DefaultPort = 8787

// Env is the bootstrap configuration: only what is needed before the database is open. Everything
// else lives in the settings table.
type Env struct {
	// ConfigDir holds the database, the master key and the logs (/config in the container).
	ConfigDir string
	// Bind is the listen address ("" = all interfaces).
	Bind string
	// Port is the listen port.
	Port int
	// LogLevel is debug, info, warn or error.
	LogLevel string
	// LogFormat is the stdout format: text or json. The log file is always JSON.
	LogFormat string
	// Docker is true inside the official image (BUNKARR_DOCKER=1).
	Docker bool
}

// EnvFromOS reads BUNKARR_* variables, applying defaults.
func EnvFromOS() (Env, error) {
	e := Env{
		ConfigDir: os.Getenv("BUNKARR_CONFIG_DIR"),
		Bind:      os.Getenv("BUNKARR_BIND"),
		Port:      DefaultPort,
		LogLevel:  strings.ToLower(strings.TrimSpace(os.Getenv("BUNKARR_LOG_LEVEL"))),
		LogFormat: strings.ToLower(strings.TrimSpace(os.Getenv("BUNKARR_LOG_FORMAT"))),
		Docker:    os.Getenv("BUNKARR_DOCKER") == "1",
	}
	if e.ConfigDir == "" {
		if e.Docker {
			e.ConfigDir = "/config"
		} else {
			e.ConfigDir = "config"
		}
	}
	if p := strings.TrimSpace(os.Getenv("BUNKARR_PORT")); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return e, fmt.Errorf("BUNKARR_PORT=%q is not a number", p)
		}
		e.Port = n
	}
	if e.LogLevel == "" {
		e.LogLevel = "info"
	}
	if e.LogFormat == "" {
		e.LogFormat = "text"
	}
	return e, nil
}

// Validate checks the values and makes ConfigDir absolute.
func (e *Env) Validate() error {
	if e.Port < 1 || e.Port > 65535 {
		return fmt.Errorf("port %d is out of range (1-65535)", e.Port)
	}
	switch e.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log level %q: use debug, info, warn or error", e.LogLevel)
	}
	switch e.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("log format %q: use text or json", e.LogFormat)
	}
	abs, err := filepath.Abs(e.ConfigDir)
	if err != nil {
		return fmt.Errorf("config dir %q: %w", e.ConfigDir, err)
	}
	e.ConfigDir = abs
	return nil
}

// DBPath is the SQLite database file.
func (e Env) DBPath() string { return filepath.Join(e.ConfigDir, "bunkarr.db") }

// KeyPath is the master key file.
func (e Env) KeyPath() string { return filepath.Join(e.ConfigDir, "bunkarr.key") }

// LogDir is the directory of the rotated JSON logs.
func (e Env) LogDir() string { return filepath.Join(e.ConfigDir, "logs") }

// Addr is the listen address for net/http.
func (e Env) Addr() string { return e.Bind + ":" + strconv.Itoa(e.Port) }
