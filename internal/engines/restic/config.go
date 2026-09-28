// Package restic is the restic engine (docs/design/phase4.md §6): a driver for the restic binary
// behind engines.Engine. config.go builds what every restic command is configured with (safety
// rule S22): the repository string (restic reaches every remote through its rclone: backend,
// D21) and the child's environment, whose storage options are those of rclone.Env for the
// BKDEST remote, never a crypt remote (restic encrypts the repository itself).
package restic

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
)

// ProgressFPS is restic's status-line rate (RESTIC_PROGRESS_FPS): one line every 2 s.
const ProgressFPS = "0.5"

// Repository returns RESTIC_REPOSITORY for d (§4.4): "rclone:BKDEST:<bucket>/<prefix>" for s3 and
// b2, "rclone:BKDEST:<path>" for sftp, the resolved absolute path for local.
func Repository(d engines.Destination) (string, error) {
	switch d.Kind {
	case engines.Local:
		if !filepath.IsAbs(d.Target) || filepath.Clean(d.Target) != d.Target {
			return "", fmt.Errorf("restic repository: %q is not a clean absolute path", d.Target)
		}
		return d.Target, nil
	case engines.S3, engines.B2, engines.SFTP:
		root := rclone.StorageRoot(d)
		if root == "" {
			return "", fmt.Errorf("restic repository: the %s remote is missing", d.Kind)
		}
		return "rclone:" + root, nil
	}
	return "", fmt.Errorf("restic repository: unknown destination kind %q", d.Kind)
}

// CacheDir is a restic destination's cache directory, <config>/cache/restic/<destinationId>
// (RESTIC_CACHE_DIR; it holds encrypted data only).
func CacheDir(configDir string, destinationID int64) string {
	return filepath.Join(configDir, "cache", "restic", strconv.FormatInt(destinationID, 10))
}

// Env returns the environment of a restic command for d: RESTIC_REPOSITORY, RESTIC_PASSWORD_FILE
// (the run directory's password secret file), RESTIC_CACHE_DIR (CacheDir), RESTIC_PROGRESS_FPS,
// and for remote kinds the variables of rclone.Env for BKDEST (RCLONE_CONFIG=/dev/null, the
// storage options, RCLONE_BWLIMIT when bwlimit is set), which the `rclone serve restic --stdio`
// child restic starts inherits. A CA certificate (caCertPath) goes to that child as
// RCLONE_CA_CERT, since restic passes no --ca-cert to it. Never a BKCRYPT variable. For a local
// repository bwlimit, knownHostsPath and caCertPath are ignored (restic takes --limit-upload and
// --limit-download per batch there, §9.1).
func Env(d engines.Destination, s engines.Secrets, passwordFile, cacheDir, bwlimit, knownHostsPath, caCertPath string) (map[string]string, error) {
	repo, err := Repository(d)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(passwordFile) || !filepath.IsAbs(cacheDir) {
		return nil, errors.New("restic environment: the password file and the cache directory must be absolute paths")
	}
	if s.Encryption.ResticPassword == "" {
		return nil, errors.New("restic environment: no repository password")
	}
	env := map[string]string{
		"RESTIC_REPOSITORY":    repo,
		"RESTIC_PASSWORD_FILE": passwordFile,
		"RESTIC_CACHE_DIR":     cacheDir,
		"RESTIC_PROGRESS_FPS":  ProgressFPS,
	}
	if !d.Kind.Remote() {
		return env, nil
	}
	storage := d
	storage.Encryption = engines.EncryptionNone // restic's own encryption; never a crypt remote
	renv, err := rclone.Env(rclone.EnvInput{Dest: storage, Secrets: s, KnownHostsPath: knownHostsPath, CACertPath: caCertPath, BWLimit: bwlimit})
	if err != nil {
		return nil, err
	}
	for k, v := range renv {
		if !strings.HasPrefix(k, "RCLONE_CONFIG_"+proc.RemoteCrypt+"_") {
			env[k] = v
		}
	}
	if caCertPath != "" {
		env["RCLONE_CA_CERT"] = caCertPath
	}
	return env, nil
}
