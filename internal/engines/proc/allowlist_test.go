package proc

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// rcloneEnv is a minimal valid rclone environment (S3).
func rcloneEnv() map[string]string {
	return map[string]string{
		"RCLONE_CONFIG":                      "/dev/null",
		"RCLONE_CONFIG_BKDEST_TYPE":          "s3",
		"RCLONE_CONFIG_BKDEST_PROVIDER":      "Minio",
		"RCLONE_CONFIG_BKDEST_ACCESS_KEY_ID": "AKIAEXAMPLE",
	}
}

// TestValidateAllowedCommands: every allowed subcommand, with the command lines of phase4.md.
func TestValidateAllowedCommands(t *testing.T) {
	const run = "/config/run/12-ab-data"
	rclone := [][]string{
		{"copy", "/mnt/user/Movies", "BKCRYPT:Movies", "--files-from-raw", run + "/files", "--no-traverse",
			"--backup-dir", "BKCRYPT:.bunkarr/retention/20260927T010000Z-job12/Movies", "--max-delete", "3",
			"--use-json-log", "-v", "--stats", "5s", "--stats-log-level", "NOTICE", "--transfers", "4",
			"--checkers", "8", "--max-duration", "1h30m0s", "--cutoff-mode", "soft", "--ca-cert", "/dev/shm/bunkarr-run/12-ab/ca.pem"},
		{"copy", "/config/staging/versions/3/x", "BKCRYPT:.bunkarr/plex/Plex/20260927T010000Z", "--exclude", "/manifest.json", "--use-json-log", "-v"},
		{"copyto", "/config/staging/v/manifest.json", "BKCRYPT:.bunkarr/plex/Plex/20260927T010000Z/manifest.json"},
		{"copyto", "BKCRYPT:.bunkarr/manifests/x/manifest.json", "/config/staging/dl/manifest.json"},
		{"move", "BKCRYPT:Movies", "BKCRYPT:.bunkarr/retention/r/Movies", "--files-from-raw", run + "/files", "--no-traverse", "--max-delete", "2"},
		{"moveto", "BKCRYPT:Movies/a.mkv", "BKCRYPT:.bunkarr/retention/r/Movies/a.1.mkv", "--max-delete", "1"},
		{"delete", "BKCRYPT:.bunkarr/retention", "--files-from-raw", run + "/files", "--max-delete", "10"},
		{"deletefile", "BKCRYPT:.bunkarr/retention/r/x", "--max-delete", "1"},
		{"purge", "BKCRYPT:.bunkarr/plex/Plex/20260901T010000Z", "--max-delete", "5"},
		{"lsjson", "--files-only", "--no-mimetype", "--files-from-raw", run + "/stat", "BKCRYPT:Movies"},
		{"lsjson", "-R", "--files-only", "--no-mimetype", "BKCRYPT:Movies"},
		{"lsf", "--max-depth", "1", "BKDEST:bucket/prefix"},
		{"check", "/mnt/user/Movies", "BKCRYPT:Movies", "--one-way", "--download", "--files-from-raw", run + "/sample", "--combined", run + "/combined"},
		{"cat", "BKCRYPT:.bunkarr/destination.json"},
		{"cat", "--count", "65536", "BKCRYPT:.bunkarr/manifests/x/manifest.json"},
		{"rcat", "BKCRYPT:.bunkarr/destination.json"},
		{"rmdirs", "--leave-root", "BKCRYPT:Movies"},
		{"about", "--json", "BKDEST:upload"},
		{"backend", "cleanup", "BKDEST:bucket/prefix", "-o", "max-age=168h0m0s"},
		{"version"},
	}
	restic := [][]string{
		{"init", "--json", "--repository-version", "2", "-o", "rclone.program=/usr/bin/rclone"},
		{"cat", "config", "--json", "--no-lock", "-o", "rclone.program=/usr/bin/rclone", "-o", "rclone.connections=4"},
		{"cat", "lock", strings.Repeat("ab", 32), "--no-lock"},
		{"cat", "snapshot", strings.Repeat("cd", 32), "--no-lock"},
		{"list", "locks", "--no-lock"},
		{"list", "snapshots", "--no-lock"},
		{"unlock"},
		{"unlock", "--remove-all"},
		{"backup", "--json", "--host", "bunkarr", "--tag", "bunkarr", "--tag", "bunkarr-dest:" + strings.Repeat("0f", 16),
			"--tag", "bunkarr-kind:media", "--files-from-raw", run + "/files", "--exclude-file", run + "/excludes",
			"--iexclude-file", run + "/iexcludes", "--exclude-if-present", "bunkarr.key", "--parent", "1a2b3c4d",
			"--ignore-inode", "--limit-upload", "4096", "--limit-download", "4096", "--pack-size", "64", "--retry-lock", "30m"},
		{"backup", "--json", "--host", "bunkarr", "--time", "2026-09-27 01:00:00", "/config/staging/versions/3/.bunkarr/plex/Plex/20260927T010000Z"},
		{"snapshots", "--json", "--no-lock", "--tag", "bunkarr-dest:ab12,bunkarr-job:12"},
		{"snapshots", "--json", "--no-lock", "1a2b3c4d"},
		{"ls", "--json", "--no-lock", "1a2b3c4d"},
		{"forget", "--json", "--retry-lock", "30m", "1a2b3c4d", "5e6f7a8b"},
		{"prune", "--max-unused", "10%", "--retry-lock", "30m"},
		{"check", "--json"},
		{"check", "--json", "--read-data-subset=3/7"},
		{"check", "--json", "--read-data"},
		{"restore", "1a2b3c4d:/mnt/user/Movies", "--target", "/config/staging/verify-job12", "--include-file", run + "/sample", "--no-lock"},
		{"dump", "--no-lock", "1a2b3c4d", "/config/staging/versions/3/.bunkarr/plex/Plex/x/manifest.json"},
		{"version"},
	}
	covered := map[Binary][]string{}
	for b, cmds := range map[Binary][][]string{Rclone: rclone, Restic: restic} {
		for _, args := range cmds {
			c := Cmd{Binary: b, Args: args, Env: map[string]string{"RCLONE_CONFIG": "/dev/null"}}
			if err := Validate(c); err != nil {
				t.Errorf("Validate(%s %s) = %v", b, strings.Join(args, " "), err)
			}
			cmd, _, _ := findCommand(b, args)
			covered[b] = append(covered[b], cmd.name())
		}
	}
	for _, b := range []Binary{Rclone, Restic} {
		for _, sub := range Subcommands(b) {
			if !slices.Contains(covered[b], sub) {
				t.Errorf("allowed subcommand %s %s has no test", b, sub)
			}
		}
	}
	wantRclone := []string{"copy", "copyto", "move", "moveto", "delete", "deletefile", "purge", "lsjson", "lsf", "check", "cat", "rcat", "rmdirs", "about", "backend cleanup", "version"}
	wantRestic := []string{"init", "cat", "list", "unlock", "backup", "snapshots", "ls", "forget", "prune", "check", "restore", "dump", "version"}
	if !slices.Equal(Subcommands(Rclone), wantRclone) || !slices.Equal(Subcommands(Restic), wantRestic) {
		t.Errorf("subcommands = %v / %v", Subcommands(Rclone), Subcommands(Restic))
	}
}

// TestValidateRefused: every refused flag and -o key of S22 and §14.1, commands Bunkarr never
// runs, and malformed arguments.
func TestValidateRefused(t *testing.T) {
	remote := []string{"lsf", "BKDEST:bucket"}
	tests := []struct {
		name string
		b    Binary
		args []string
		want string
	}{
		{"-vv", Rclone, append(slices.Clone(remote), "-vv"), "flag -vv"},
		{"-v twice", Rclone, append(slices.Clone(remote), "-v", "-v"), "given twice"},
		{"--dump", Rclone, append(slices.Clone(remote), "--dump", "headers"), "flag --dump"},
		{"--dump-headers", Rclone, append(slices.Clone(remote), "--dump-headers"), "flag --dump-headers"},
		{"--dump=bodies", Rclone, append(slices.Clone(remote), "--dump=bodies"), "flag --dump"},
		{"--log-file", Rclone, append(slices.Clone(remote), "--log-file", "/tmp/x"), "flag --log-file"},
		{"--log-level DEBUG", Rclone, append(slices.Clone(remote), "--log-level", "DEBUG"), "flag --log-level"},
		{"--rc", Rclone, append(slices.Clone(remote), "--rc"), "flag --rc"},
		{"--rc-addr", Rclone, append(slices.Clone(remote), "--rc-addr", ":5572"), "flag --rc-addr"},
		{"--rc-no-auth", Rclone, append(slices.Clone(remote), "--rc-no-auth"), "flag --rc-no-auth"},
		{"--no-check-certificate", Rclone, append(slices.Clone(remote), "--no-check-certificate"), "flag --no-check-certificate"},
		{"--stats-log-level DEBUG", Rclone, append(slices.Clone(remote), "--stats-log-level", "DEBUG"), "must be one of"},
		{"--config", Rclone, append(slices.Clone(remote), "--config", "/root/.config/rclone/rclone.conf"), "flag --config"},
		{"--copy-links", Rclone, []string{"copy", "/src", "BKDEST:b", "--copy-links"}, "flag --copy-links"},
		{"-L", Rclone, []string{"copy", "/src", "BKDEST:b", "-L"}, "flag -L"},
		{"--delete-excluded", Rclone, []string{"copy", "/src", "BKDEST:b", "--delete-excluded"}, "flag --delete-excluded"},
		{"--dry-run", Rclone, []string{"copy", "/src", "BKDEST:b", "--dry-run"}, "flag --dry-run"},
		{"--track-renames", Rclone, []string{"copy", "/src", "BKDEST:b", "--track-renames"}, "flag --track-renames"},
		{"--cutoff-mode hard", Rclone, []string{"copy", "/src", "BKDEST:b", "--cutoff-mode", "hard"}, "must be one of"},
		{"lsjson --stat", Rclone, []string{"lsjson", "--stat", "BKDEST:b/x"}, "flag --stat"},
		{"-o on copy", Rclone, []string{"copy", "/src", "BKDEST:b", "-o", "max-age=1h"}, "flag -o"},
		{"cleanup -o other", Rclone, []string{"backend", "cleanup", "BKDEST:b", "-o", "chunk-size=1G"}, "max-age"},
		{"cleanup on crypt", Rclone, []string{"backend", "cleanup", "BKCRYPT:"}, "a path on BKDEST"},
		{"sync", Rclone, []string{"sync", "/src", "BKDEST:b"}, `subcommand "sync"`},
		{"bisync", Rclone, []string{"bisync", "/src", "BKDEST:b"}, `subcommand "bisync"`},
		{"top-level cleanup", Rclone, []string{"cleanup", "BKDEST:b"}, `subcommand "cleanup"`},
		{"backend other", Rclone, []string{"backend", "set", "BKDEST:b"}, `subcommand "backend"`},
		{"obscure", Rclone, []string{"obscure", "value"}, `subcommand "obscure"`},
		{"config", Rclone, []string{"config", "show"}, `subcommand "config"`},
		{"serve", Rclone, []string{"serve", "restic", "--stdio", "BKDEST:b"}, `subcommand "serve"`},
		{"mount", Rclone, []string{"mount", "BKDEST:b", "/mnt"}, `subcommand "mount"`},
		{"no subcommand", Rclone, nil, `subcommand ""`},
		{"flag before subcommand", Rclone, []string{"-v", "copy", "/src", "BKDEST:b"}, `subcommand "-v"`},
		{"move from a local source (S1)", Rclone, []string{"move", "/mnt/user/Movies", "BKDEST:b/Movies"}, "positional argument 1"},
		{"moveto from a local source (S1)", Rclone, []string{"moveto", "/mnt/user/Movies/a.mkv", "BKDEST:b/a.mkv"}, "positional argument 1"},
		{"delete a local path (S1)", Rclone, []string{"delete", "/mnt/user/Movies"}, "positional argument 1"},
		{"purge a local path (S1)", Rclone, []string{"purge", "/mnt/user"}, "positional argument 1"},
		{"copy to another remote", Rclone, []string{"copy", "/src", "s3:bucket"}, "positional argument 2"},
		{"copy to an on-the-fly backend", Rclone, []string{"copy", "/src", ":s3,provider=AWS:bucket"}, "positional argument 2"},
		{"copy to a connection string", Rclone, []string{"copy", "/src", "BKDEST,endpoint=evil:bucket"}, "positional argument 2"},
		{"copyto local to local", Rclone, []string{"copyto", "/a", "/b"}, "local to local"},
		{"relative files-from", Rclone, []string{"copy", "/src", "BKDEST:b", "--files-from-raw", "files"}, "absolute path"},
		{"backup-dir elsewhere", Rclone, []string{"copy", "/src", "BKDEST:b", "--backup-dir", "/tmp/x"}, "must be a path on"},
		{"positional starting with -", Rclone, []string{"cat", "-BKDEST:b"}, "flag -BKDEST:b"},
		{"missing value", Rclone, []string{"copy", "/src", "BKDEST:b", "--max-delete"}, "needs a value"},
		{"value looks like a flag", Rclone, []string{"copy", "/src", "BKDEST:b", "--max-delete", "-1"}, "needs a value"},
		{"switch with a value", Rclone, []string{"copy", "/src", "BKDEST:b", "--no-traverse=false"}, "takes no value"},
		{"version with args", Rclone, []string{"version", "--check"}, "flag --check"},
		{"restic --insecure-tls", Restic, []string{"snapshots", "--json", "--insecure-tls"}, "flag --insecure-tls"},
		{"restic --insecure-no-password", Restic, []string{"init", "--insecure-no-password"}, "flag --insecure-no-password"},
		{"restic --password-command", Restic, []string{"snapshots", "--password-command", "cat /x"}, "flag --password-command"},
		{"restic --password-file", Restic, []string{"snapshots", "--password-file", "/x"}, "flag --password-file"},
		{"restic -r", Restic, []string{"snapshots", "-r", "s3:evil"}, "flag -r"},
		{"restic -vv", Restic, []string{"backup", "-vv"}, "flag -vv"},
		{"restic -o sftp.command", Restic, []string{"snapshots", "-o", "sftp.command=ssh evil"}, "sftp.command"},
		{"restic -o rclone.args", Restic, []string{"snapshots", "-o", "rclone.args=serve restic --stdio --b2-hard-delete=false"}, "rclone.args"},
		{"restic -o s3.region", Restic, []string{"snapshots", "-o", "s3.region=x"}, "s3.region"},
		{"restic rclone.program relative", Restic, []string{"snapshots", "-o", "rclone.program=rclone"}, "absolute path"},
		{"restic rclone.program with a space", Restic, []string{"snapshots", "-o", "rclone.program=/usr/bin/rclone --config=/x"}, "without whitespace"},
		{"restic rclone.connections 0", Restic, []string{"snapshots", "-o", "rclone.connections=0"}, "integer"},
		{"restic backup --dry-run (S9)", Restic, []string{"backup", "--dry-run"}, "flag --dry-run"},
		{"restic forget --keep-daily (S24)", Restic, []string{"forget", "--keep-daily", "7"}, "flag --keep-daily"},
		{"restic forget --prune", Restic, []string{"forget", "--prune", "1a2b3c4d"}, "flag --prune"},
		{"restic forget without ids", Restic, []string{"forget", "--json"}, "positional arguments"},
		{"restic prune --unsafe-recover-no-free-space", Restic, []string{"prune", "--unsafe-recover-no-free-space", "x"}, "flag --unsafe-recover-no-free-space"},
		{"restic prune --repack-uncompressed", Restic, []string{"prune", "--repack-uncompressed"}, "flag --repack-uncompressed"},
		{"restic restore to a relative target", Restic, []string{"restore", "1a2b3c4d", "--target", "restore"}, "absolute path"},
		{"restic cat other", Restic, []string{"cat", "masterkey"}, "only cat config"},
		{"restic key", Restic, []string{"key", "list"}, `subcommand "key"`},
		{"restic rewrite", Restic, []string{"rewrite", "--exclude", "x"}, `subcommand "rewrite"`},
		{"restic mount", Restic, []string{"mount", "/mnt"}, `subcommand "mount"`},
		{"restic self-update", Restic, []string{"self-update"}, `subcommand "self-update"`},
		{"restic snapshot id not hex", Restic, []string{"ls", "latest"}, "snapshot id"},
		{"restic tag with a space", Restic, []string{"snapshots", "--tag", "a b"}, "tag list"},
		{"restic --no-cache", Restic, []string{"snapshots", "--no-cache"}, "flag --no-cache"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Cmd{Binary: tc.b, Args: tc.args, Env: map[string]string{"RCLONE_CONFIG": "/dev/null"}})
			if !errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want ErrNotAllowed with %q", err, tc.want)
			}
		})
	}
	if err := Validate(Cmd{Binary: "sh", Args: []string{"-c", "true"}}); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("unknown binary: %v", err)
	}
}

// TestValidateRefusesSecretsInArgv: an argument holding one of the command's own secrets (a
// Redact value, a secret option of its environment even when Redact lacks it, a secret file's
// content, or their escaped forms) is refused, and the error does not repeat it (S22).
func TestValidateRefusesSecretsInArgv(t *testing.T) {
	const local = "typed-in-a-test-form-91b2"
	const keySecret = "s3-secret-key-0f9e8d7c"
	const b2Key = "b2-application-key-5a6b"
	const obscuredPass = "obscured-crypt-pass-3c4d"
	const resticPassword = "restic-repo-password-77aa"
	const quoted = `pass"with-quote-12`
	s3 := func() map[string]string {
		env := rcloneEnv()
		env["RCLONE_CONFIG_BKDEST_SECRET_ACCESS_KEY"] = keySecret
		env["RCLONE_CONFIG_BKCRYPT_TYPE"] = "crypt"
		env["RCLONE_CONFIG_BKCRYPT_PASSWORD"] = obscuredPass
		return env
	}
	b2 := map[string]string{"RCLONE_CONFIG": "/dev/null", "RCLONE_CONFIG_BKDEST_TYPE": "b2",
		"RCLONE_CONFIG_BKDEST_ACCOUNT": "0012345abcdef0000000001", "RCLONE_CONFIG_BKDEST_KEY": b2Key}
	rd := &RunDir{}
	tests := []Cmd{
		{Binary: Restic, Args: []string{"snapshots", "--tag", "x" + local}, Redact: []string{local}},
		{Binary: Restic, Args: []string{"forget", "1a2b3c4d", "--" + local}, Redact: []string{local}},
		// The environment's secrets count without Redact.
		{Binary: Rclone, Args: []string{"lsf", "BKDEST:" + keySecret}, Env: s3()},
		{Binary: Rclone, Args: []string{"lsf", "BKDEST:b", "--stats", obscuredPass}, Env: s3()},
		{Binary: Rclone, Args: []string{"lsf", "BKDEST:bucket/" + b2Key}, Env: b2},
		// A secret file's content (the restic password), and its JSON-escaped form.
		{Binary: Restic, Args: []string{"snapshots", "--tag", resticPassword}, Dir: rd,
			SecretFiles: map[string][]byte{"password": []byte(resticPassword)}},
		{Binary: Restic, Args: []string{"snapshots", "--tag", `pass\"with-quote-12`}, Dir: rd,
			SecretFiles: map[string][]byte{"password": []byte(quoted)}},
		// An identifier that is also one of the command's secrets stays refused.
		{Binary: Rclone, Args: []string{"lsf", "BKDEST:" + keySecret}, Redact: []string{keySecret},
			Env: func() map[string]string {
				env := s3()
				env["RCLONE_CONFIG_BKDEST_ACCESS_KEY_ID"] = keySecret
				return env
			}()},
	}
	for _, c := range tests {
		if c.Env == nil {
			c.Env = map[string]string{"RCLONE_CONFIG": "/dev/null"}
		}
		err := Validate(c)
		if !errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), "contains a secret") {
			t.Errorf("Validate(%v) = %v", c.Args, err)
		}
		for _, s := range []string{local, keySecret, b2Key, obscuredPass, resticPassword, quoted} {
			if err != nil && strings.Contains(err.Error(), s) {
				t.Errorf("the error repeats the secret: %v", err)
			}
		}
	}
}

// TestValidateRefusesValuesRedactedFromOutput: an argument the runner's output redaction would
// change is refused, even when the value only names an account (the command's own S3 access key
// ID or B2 keyId, in Redact) or belongs to another stored destination (registered): the command
// names that path, its output (rclone lsjson and JSON log, restic ls) prints it back redacted,
// and a job deciding on "[REDACTED]/a.mkv" overwrites or forgets what is really there. So
// Validate allows an argument exactly when it comes through RedactLine unchanged, in clear and
// in its JSON-escaped forms.
func TestValidateRefusesValuesRedactedFromOutput(t *testing.T) {
	const keyID = "photos-backup"
	const secret = "s3-secret-key-9a8b7c6d"
	env := rcloneEnv()
	env["RCLONE_CONFIG_BKDEST_ACCESS_KEY_ID"] = keyID
	env["RCLONE_CONFIG_BKDEST_SECRET_ACCESS_KEY"] = secret
	env["RCLONE_CONFIG_BKCRYPT_TYPE"] = "crypt"
	env["RCLONE_CONFIG_BKCRYPT_REMOTE"] = "BKDEST:media/bunkarr"
	redact := []string{keyID, secret}
	const b2KeyID = "0012345abcdef0000000001"
	b2Env := map[string]string{"RCLONE_CONFIG": "/dev/null", "RCLONE_CONFIG_BKDEST_TYPE": "b2",
		"RCLONE_CONFIG_BKDEST_ACCOUNT": b2KeyID, "RCLONE_CONFIG_BKDEST_KEY": "b2-application-key-5a6b"}
	b2Redact := []string{b2KeyID, "b2-application-key-5a6b"}
	resticEnv := maps.Clone(env)
	resticEnv["RESTIC_REPOSITORY"] = "rclone:BKDEST:media/restic"
	resticEnv["RESTIC_PASSWORD_FILE"] = "/dev/shm/bunkarr-run/1-ab/password"
	rd := &RunDir{}
	restic := func(args ...string) Cmd {
		return Cmd{Binary: Restic, Args: args, Env: resticEnv, Redact: redact, Dir: rd,
			SecretFiles: map[string][]byte{"password": []byte("restic-repo-password-77aa")}}
	}

	// Another destination's values, stored and so registered process-wide.
	const otherKeyID = "media-archive-user"
	const otherPassword = "mediaserver-2024"
	logging.SetSecrets("destination:proc-test-other", otherKeyID, otherPassword)
	t.Cleanup(func() { logging.SetSecrets("destination:proc-test-other") })

	refused := []Cmd{
		// The destination folder named like the command's own access key ID: the copy that
		// would upload into it, and the moveto and copyto of a later sync.
		{Binary: Rclone, Args: []string{"copy", "/mnt/user/movies", "BKCRYPT:" + keyID}, Env: env, Redact: redact},
		{Binary: Rclone, Args: []string{"moveto", "BKCRYPT:" + keyID + "/a.mkv", "BKCRYPT:" + keyID + "/b.mkv"}, Env: env, Redact: redact},
		{Binary: Rclone, Args: []string{"copyto", "/mnt/user/movies/a.mkv", "BKCRYPT:x/" + keyID + "-a.mkv"}, Env: env, Redact: redact},
		// A bucket or prefix named like it, for S3 and for a B2 keyId.
		{Binary: Rclone, Args: []string{"lsf", "--max-depth", "1", "BKDEST:" + keyID}, Env: env, Redact: redact},
		{Binary: Rclone, Args: []string{"backend", "cleanup", "BKDEST:media/" + keyID}, Env: env, Redact: redact},
		{Binary: Rclone, Args: []string{"lsf", "BKDEST:media/" + b2KeyID}, Env: b2Env, Redact: b2Redact},
		// A source root holding it.
		{Binary: Rclone, Args: []string{"copy", "/mnt/user/" + keyID, "BKCRYPT:movies"}, Env: env, Redact: redact},
		restic("backup", "--json", "/mnt/user/"+keyID),
		restic("restore", "1a2b3c4d:/mnt/user/"+keyID, "--target", "/mnt/user/staging"),
		restic("dump", "1a2b3c4d", "/mnt/user/"+keyID+"/a.mkv"),
		// Another owner's registered value inside this command's names.
		{Binary: Rclone, Args: []string{"lsf", "BKDEST:" + otherKeyID}, Env: env, Redact: redact},
		{Binary: Rclone, Args: []string{"copy", "/mnt/user/" + otherPassword, "BKCRYPT:x"}, Env: env, Redact: redact},
		{Binary: Rclone, Args: []string{"copy", "/mnt/user/movies", "BKCRYPT:" + otherKeyID}, Env: env, Redact: redact},
		restic("restore", "1a2b3c4d:/mnt/user/"+otherPassword, "--target", "/mnt/user/staging"),
	}
	for _, c := range refused {
		err := Validate(c)
		if !errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), "contains a secret") {
			t.Errorf("Validate(%v) = %v, want a refusal", c.Args, err)
		}
		for _, v := range []string{keyID, secret, b2KeyID, otherKeyID, otherPassword} {
			if err != nil && strings.Contains(err.Error(), v) {
				t.Errorf("the error repeats %q: %v", v, err)
			}
		}
	}

	allowed := []Cmd{
		{Binary: Rclone, Args: []string{"copy", "/mnt/user/movies", "BKCRYPT:movies"}, Env: env, Redact: redact},
		{Binary: Rclone, Args: []string{"lsf", "--max-depth", "1", "BKDEST:media/bunkarr"}, Env: env, Redact: redact},
		{Binary: Rclone, Args: []string{"lsf", "BKDEST:media/bunkarr"}, Env: b2Env, Redact: b2Redact},
		restic("backup", "--json", "/mnt/user/movies"),
		// A value shorter than 8 bytes is never redacted, so it may appear.
		{Binary: Rclone, Args: []string{"lsf", "BKDEST:media/short"}, Env: env, Redact: []string{"short", secret}},
	}
	for _, c := range allowed {
		if err := Validate(c); err != nil {
			t.Errorf("Validate(%v) = %v", c.Args, err)
		}
	}

	// The invariant behind the rule: an allowed argument reads back from an output line as it
	// was; a refused one would not.
	line := func(a string) string { return `{"Path":"` + a + `/a.mkv","Size":1}` }
	for _, c := range allowed {
		for _, a := range c.Args {
			if got := RedactLine(line(a), false, c.Redact); got != line(a) {
				t.Errorf("allowed argument %q reads back as %q", a, got)
			}
		}
	}
	for _, c := range refused {
		changed := false
		for _, a := range c.Args {
			changed = changed || RedactLine(line(a), false, c.Redact) != line(a)
		}
		if !changed {
			t.Errorf("refused %v although every argument reads back unchanged", c.Args)
		}
	}
}

// TestValidateRefusesEscapedFormsRedactedFromOutput: restic and rclone print paths
// JSON-escaped (encoding/json writes "&" as \u0026), so an argument is refused when its escaped
// form would be redacted, even though the argument itself holds no registered value.
func TestValidateRefusesEscapedFormsRedactedFromOutput(t *testing.T) {
	const value = "u0026archive-01"
	logging.SetSecrets("destination:proc-test-escaped", value)
	t.Cleanup(func() { logging.SetSecrets("destination:proc-test-escaped") })
	const path = "/mnt/user/&archive-01"
	if logging.ContainsSecret(path) {
		t.Fatal("the clear path holds the value; the test needs one that does not")
	}
	raw, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	if RedactLine(string(raw), false, nil) == string(raw) {
		t.Fatalf("%s is not redacted; the test needs a path whose output is", raw)
	}
	c := Cmd{Binary: Rclone, Args: []string{"copy", path, "BKDEST:bucket/x"}, Env: rcloneEnv()}
	if err := Validate(c); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("Validate(%v) = %v", c.Args, err)
	}
	c.Args[1] = "/mnt/user/&archive-02"
	if err := Validate(c); err != nil {
		t.Errorf("Validate(%v) = %v", c.Args, err)
	}
}

// TestValidateRefusesFileListRedactedFromOutput: a file list whose entries all share a prefix
// the output redaction would change (a restic source root /mnt/user/<value>, a destination folder
// <value>/) is refused: restic ls of the snapshot and rclone's JSON log print every entry back as
// "[REDACTED]", so the restic read-back failed every item as "not in the snapshot" on every run.
// An rclone listing (statMany) is refused when any entry would come back redacted, because it
// reads as absent: a retain batch listed after the folder's name became another destination's
// access key ID dropped the record of the vanished file and left its object untracked. Other
// lists where only some entries hold the value run (those items fail on their own), and so does
// a list outside the command's data directory, which Validate does not read.
func TestValidateRefusesFileListRedactedFromOutput(t *testing.T) {
	const other = "media-archive-user"
	logging.SetSecrets("destination:proc-test-filelist", other)
	t.Cleanup(func() { logging.SetSecrets("destination:proc-test-filelist") })
	const keyID = "photos-backup"
	dirs, _, _ := testRunDirs(t, true)
	resticEnv := map[string]string{"RESTIC_REPOSITORY": "/mnt/user/restic", "RESTIC_PASSWORD_FILE": "/dev/shm/x/password"}
	backup := func(entries ...string) Cmd {
		rd, err := dirs.New(7)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rd.Remove() })
		if err := rd.WriteData("files", []byte(strings.Join(entries, "\x00")+"\x00")); err != nil {
			t.Fatal(err)
		}
		return Cmd{Binary: Restic, Args: []string{"backup", "--json", "--files-from-raw", rd.DataPath("files")},
			Env: resticEnv, Dir: rd}
	}
	lsjson := func(entries ...string) Cmd {
		rd, err := dirs.New(7)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rd.Remove() })
		if err := rd.WriteData("files", []byte(strings.Join(entries, "\n")+"\n")); err != nil {
			t.Fatal(err)
		}
		env := rcloneEnv()
		env["RCLONE_CONFIG_BKDEST_ACCESS_KEY_ID"] = keyID
		return Cmd{Binary: Rclone, Args: []string{"lsjson", "-R", "--files-only", "--files-from-raw", rd.DataPath("files"), "BKDEST:media"},
			Env: env, Dir: rd, Redact: []string{keyID}}
	}
	copyList := func(entries ...string) Cmd {
		c := lsjson(entries...)
		c.Args = []string{"copy", "/mnt/user/movies", "BKDEST:media", "--files-from-raw", c.Dir.DataPath("files")}
		return c
	}
	for _, tc := range []struct {
		name string
		c    Cmd
		ok   bool
	}{
		{"restic source root holding a registered value", backup("/mnt/user/"+other+"/a.mkv", "/mnt/user/"+other+"/d/e.srt"), false},
		{"restic single file", backup("/mnt/user/movies/" + other + ".mkv"), false},
		{"restic plain root", backup("/mnt/user/movies/a.mkv", "/mnt/user/movies/d/e.srt"), true},
		{"restic one entry holding it", backup("/mnt/user/movies/"+other+".mkv", "/mnt/user/movies/b.mkv"), true},
		{"rclone folder named like the access key", lsjson(keyID+"/a.mkv", keyID+"/d/e.srt"), false},
		{"rclone folder of another destination's key", lsjson(other+"/a.mkv", other+"/b.mkv"), false},
		{"rclone plain folder", lsjson("movies/a.mkv", "movies/d/e.srt"), true},
		// A listing is read as the objects that exist: one redacted entry would read as absent.
		{"rclone listing, one entry holding it", lsjson("movies/"+keyID+".mkv", "movies/b.mkv"), false},
		{"rclone listing of a retain batch, registered after the upload", lsjson(other+"/a.mkv", ".bunkarr/retention/20260927T231511Z-job2/"+other+"/a.mkv"), false},
		{"rclone copy, one entry holding it", copyList("movies/"+keyID+".mkv", "movies/b.mkv"), true},
		{"rclone copy, a short entry", copyList("a.mkv", keyID+"/b.mkv", keyID+"/c.mkv"), true},
		{"rclone copy, every entry holding it", copyList(keyID+"/b.mkv", keyID+"/c.mkv"), false},
	} {
		err := Validate(tc.c)
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok && (!errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), "a path of the file list")) {
			t.Errorf("%s: %v, want a refusal", tc.name, err)
		}
		if err != nil && (strings.Contains(err.Error(), other) || strings.Contains(err.Error(), keyID)) {
			t.Errorf("%s: the error repeats the value: %v", tc.name, err)
		}
	}

	// A list outside the run's data directory is not read.
	outside := filepath.Join(t.TempDir(), "files")
	if err := os.WriteFile(outside, []byte("/mnt/user/"+other+"/a.mkv\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := backup("/mnt/user/movies/a.mkv")
	c.Args[3] = outside
	if err := Validate(c); err != nil {
		t.Errorf("a list outside the data directory: %v", err)
	}
}

// TestValidateEnv: the exact names of S22 and the closed option tables of §4.4.
func TestValidateEnv(t *testing.T) {
	full := map[string]map[string]string{
		"s3": {"TYPE": "s3", "PROVIDER": "Minio", "ENDPOINT": "https://minio:9000", "REGION": "us-east-1",
			"ACCESS_KEY_ID": "k", "SECRET_ACCESS_KEY": "s", "ENV_AUTH": "false", "NO_CHECK_BUCKET": "true",
			"STORAGE_CLASS": "STANDARD", "FORCE_PATH_STYLE": "true"},
		"b2":   {"TYPE": "b2", "ACCOUNT": "a", "KEY": "k", "HARD_DELETE": "true"},
		"sftp": {"TYPE": "sftp", "HOST": "h", "PORT": "22", "USER": "u", "KEY_PEM": "p", "PASS": "x", "KEY_FILE_PASS": "y", "KEY_USE_AGENT": "false", "ASK_PASSWORD": "false", "KNOWN_HOSTS_FILE": "/dev/shm/k"},
	}
	for kind, opts := range full {
		env := map[string]string{"RCLONE_CONFIG": "/dev/null", "RCLONE_BWLIMIT": "1024k:off", "PATH": "/bin", "TZ": "UTC", "LANG": "C", "TMPDIR": "/tmp", "HOME": "/x"}
		for o, v := range opts {
			env[RemoteEnv(RemoteDest, o)] = v
		}
		if !slices.Equal(sortedKeys(opts), slices.Sorted(slices.Values(DestOptions(kind)))) {
			t.Errorf("%s: the test's option set differs from DestOptions", kind)
		}
		for _, o := range CryptOptions() {
			env[RemoteEnv(RemoteCrypt, o)] = "x"
		}
		env[RemoteEnv(RemoteCrypt, "TYPE")] = "crypt"
		if err := Validate(Cmd{Binary: Rclone, Args: []string{"version"}, Env: env}); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
		resticEnv := map[string]string{"RESTIC_REPOSITORY": "rclone:BKDEST:bucket/prefix", "RESTIC_PASSWORD_FILE": "/dev/shm/p/password",
			"RESTIC_CACHE_DIR": "/config/cache/restic/3", "RESTIC_PROGRESS_FPS": "0.5", "RCLONE_CONFIG": "/dev/null", "RCLONE_CA_CERT": "/dev/shm/p/ca.pem"}
		for o, v := range opts {
			resticEnv[RemoteEnv(RemoteDest, o)] = v
		}
		if err := Validate(Cmd{Binary: Restic, Args: []string{"version"}, Env: resticEnv}); err != nil {
			t.Errorf("restic %s: %v", kind, err)
		}
	}

	refused := []struct {
		name string
		b    Binary
		env  map[string]string
		want string
	}{
		{"BUNKARR_*", Rclone, map[string]string{"BUNKARR_CONFIG_DIR": "/config"}, "BUNKARR_CONFIG_DIR"},
		{"AWS_*", Rclone, map[string]string{"AWS_ACCESS_KEY_ID": "x"}, "AWS_ACCESS_KEY_ID"},
		{"RCLONE_LOG_FILE", Rclone, map[string]string{"RCLONE_LOG_FILE": "/tmp/l"}, "RCLONE_LOG_FILE"},
		{"RCLONE_LOG_LEVEL", Rclone, map[string]string{"RCLONE_LOG_LEVEL": "DEBUG"}, "RCLONE_LOG_LEVEL"},
		{"RCLONE_RC", Rclone, map[string]string{"RCLONE_RC": "true"}, "RCLONE_RC"},
		{"RCLONE_RC_ADDR", Rclone, map[string]string{"RCLONE_RC_ADDR": ":5572"}, "RCLONE_RC_ADDR"},
		{"RCLONE_DUMP", Rclone, map[string]string{"RCLONE_DUMP": "bodies"}, "RCLONE_DUMP"},
		{"RCLONE_NO_CHECK_CERTIFICATE", Rclone, map[string]string{"RCLONE_NO_CHECK_CERTIFICATE": "true"}, "RCLONE_NO_CHECK_CERTIFICATE"},
		{"RCLONE_VERBOSE", Rclone, map[string]string{"RCLONE_VERBOSE": "2"}, "RCLONE_VERBOSE"},
		{"RESTIC_PASSWORD_COMMAND", Restic, map[string]string{"RESTIC_PASSWORD_COMMAND": "cat /x"}, "RESTIC_PASSWORD_COMMAND"},
		{"RESTIC_PASSWORD", Restic, map[string]string{"RESTIC_PASSWORD": "x"}, "RESTIC_PASSWORD"},
		{"another remote", Rclone, map[string]string{"RCLONE_CONFIG_EVIL_TYPE": "s3"}, "RCLONE_CONFIG_EVIL_TYPE"},
		{"_SSH", Rclone, map[string]string{"RCLONE_CONFIG_BKDEST_TYPE": "sftp", "RCLONE_CONFIG_BKDEST_SSH": "sh -c evil"}, "BKDEST_SSH"},
		{"_SERVER_COMMAND", Rclone, map[string]string{"RCLONE_CONFIG_BKDEST_TYPE": "sftp", "RCLONE_CONFIG_BKDEST_SERVER_COMMAND": "x"}, "SERVER_COMMAND"},
		{"_MD5SUM_COMMAND", Rclone, map[string]string{"RCLONE_CONFIG_BKDEST_TYPE": "sftp", "RCLONE_CONFIG_BKDEST_MD5SUM_COMMAND": "x"}, "MD5SUM_COMMAND"},
		{"_SHA1SUM_COMMAND", Rclone, map[string]string{"RCLONE_CONFIG_BKDEST_TYPE": "sftp", "RCLONE_CONFIG_BKDEST_SHA1SUM_COMMAND": "x"}, "SHA1SUM_COMMAND"},
		{"_SHARED_CREDENTIALS_FILE", Rclone, map[string]string{"RCLONE_CONFIG_BKDEST_TYPE": "s3", "RCLONE_CONFIG_BKDEST_SHARED_CREDENTIALS_FILE": "x"}, "SHARED_CREDENTIALS_FILE"},
		{"_PROFILE", Rclone, map[string]string{"RCLONE_CONFIG_BKDEST_TYPE": "s3", "RCLONE_CONFIG_BKDEST_PROFILE": "x"}, "PROFILE"},
		{"an s3 option on sftp", Rclone, map[string]string{"RCLONE_CONFIG_BKDEST_TYPE": "sftp", "RCLONE_CONFIG_BKDEST_ENDPOINT": "x"}, "not allowed for type sftp"},
		{"a local backend", Rclone, map[string]string{"RCLONE_CONFIG_BKDEST_TYPE": "local"}, "TYPE must be"},
		{"options without a type", Rclone, map[string]string{"RCLONE_CONFIG_BKDEST_HOST": "h"}, "TYPE must be"},
		{"crypt without crypt type", Rclone, map[string]string{"RCLONE_CONFIG_BKDEST_TYPE": "s3", "RCLONE_CONFIG_BKCRYPT_TYPE": "alias"}, "TYPE=crypt"},
		{"crypt without a storage remote", Rclone, map[string]string{"RCLONE_CONFIG_BKCRYPT_TYPE": "crypt"}, "TYPE=crypt"},
		{"crypt option outside the table", Rclone, map[string]string{"RCLONE_CONFIG_BKCRYPT_FILENAME_ENCODING": "base32768"}, "FILENAME_ENCODING"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"RCLONE_CONFIG": "/dev/null"}
			for k, v := range tc.env {
				env[k] = v
			}
			err := Validate(Cmd{Binary: tc.b, Args: []string{"version"}, Env: env})
			if !errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
	values := []struct {
		name string
		b    Binary
		env  map[string]string
		want string
	}{
		{"rclone without RCLONE_CONFIG", Rclone, map[string]string{}, "RCLONE_CONFIG must be /dev/null"},
		{"rclone with a config file", Rclone, map[string]string{"RCLONE_CONFIG": "/root/.config/rclone/rclone.conf"}, "RCLONE_CONFIG must be /dev/null"},
		{"restic remote without RCLONE_CONFIG", Restic, rcloneEnvWithout("RCLONE_CONFIG"), "RCLONE_CONFIG must be /dev/null"},
		{"restic native s3 backend", Restic, map[string]string{"RESTIC_REPOSITORY": "s3:https://evil/bucket"}, "RESTIC_REPOSITORY"},
		{"restic sftp backend (needs ssh)", Restic, map[string]string{"RESTIC_REPOSITORY": "sftp:host:/x"}, "RESTIC_REPOSITORY"},
		{"relative password file", Restic, map[string]string{"RESTIC_PASSWORD_FILE": "password"}, "absolute path"},
		{"NUL in a value", Rclone, map[string]string{"RCLONE_CONFIG": "/dev/null", "TZ": "UTC\x00"}, "NUL"},
	}
	for _, tc := range values {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Cmd{Binary: tc.b, Args: []string{"version"}, Env: tc.env})
			if !errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
}

func rcloneEnvWithout(k string) map[string]string {
	env := rcloneEnv()
	delete(env, k)
	return env
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestValidateSecretFiles(t *testing.T) {
	dirs, _, _ := testRunDirs(t, true)
	rd, err := dirs.New(1)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Remove()
	env := map[string]string{"RCLONE_CONFIG": "/dev/null"}
	if err := Validate(Cmd{Binary: Restic, Args: []string{"version"}, SecretFiles: map[string][]byte{"password": []byte("x")}}); err == nil {
		t.Error("secret files without a run directory accepted")
	}
	for _, name := range []string{"../password", "a/b", "", ".."} {
		if err := Validate(Cmd{Binary: Rclone, Args: []string{"version"}, Env: env, Dir: rd, SecretFiles: map[string][]byte{name: nil}}); err == nil {
			t.Errorf("secret file name %q accepted", name)
		}
	}
	if err := Validate(Cmd{Binary: Rclone, Args: []string{"version"}, Env: env, Dir: rd, SecretFiles: map[string][]byte{"known_hosts": nil, "ca.pem": nil}}); err != nil {
		t.Errorf("valid secret files refused: %v", err)
	}
	if err := Validate(Cmd{Binary: Rclone, Args: []string{"version"}, Env: env, Budget: -1}); err == nil {
		t.Error("negative budget accepted")
	}
}

func TestRemoteEnvPanicsOutsideTheTable(t *testing.T) {
	if got := RemoteEnv(RemoteDest, "KEY_PEM"); got != "RCLONE_CONFIG_BKDEST_KEY_PEM" {
		t.Errorf("RemoteEnv = %q", got)
	}
	for _, c := range [][2]string{{RemoteDest, "SSH"}, {RemoteCrypt, "KEY_PEM"}, {"OTHER", "TYPE"}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("RemoteEnv(%s, %s) did not panic", c[0], c[1])
				}
			}()
			RemoteEnv(c[0], c[1])
		}()
	}
	// The exported tables are copies.
	DestOptions("s3")[0] = "SSH"
	if DestOptions("s3")[0] != "TYPE" {
		t.Error("DestOptions returned the table itself")
	}
}
