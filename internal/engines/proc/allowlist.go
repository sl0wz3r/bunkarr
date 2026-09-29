package proc

import (
	"bufio"
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// This file is the allow-list of safety rule S22 (docs/design/phase4.md): the only subcommands,
// flags, restic -o keys and environment names an engine command can have. It is an allow-list,
// not a deny-list: anything not listed here is refused by Validate, so -vv (which prints every
// RCLONE_CONFIG_* value in clear), --dump*, --log-file, --rc*, --no-check-certificate,
// --insecure-tls, --insecure-no-password, --password-command, -o sftp.command, -o rclone.args,
// RCLONE_LOG_*, RCLONE_RC*, RCLONE_DUMP*, RCLONE_NO_CHECK_CERTIFICATE and RESTIC_PASSWORD_COMMAND
// can never be passed. The flags are those of every command line in phase4.md §4.5, §6, §7, §8,
// §9.1 and §14.4.

// Remote names that exist only in a child's environment (§4.4).
const (
	// RemoteDest is the storage remote.
	RemoteDest = "BKDEST"
	// RemoteCrypt is the crypt remote over RemoteDest.
	RemoteCrypt = "BKCRYPT"
)

// destOptions is the closed table of BKDEST options per rclone backend type (§4.4). No other
// option is ever set, so _SSH, _SERVER_COMMAND, _MD5SUM_COMMAND, _SHA1SUM_COMMAND,
// _SHARED_CREDENTIALS_FILE, _PROFILE and the like can never appear.
var destOptions = map[string][]string{
	"s3": {"TYPE", "PROVIDER", "ENDPOINT", "REGION", "ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "ENV_AUTH",
		"NO_CHECK_BUCKET", "STORAGE_CLASS", "FORCE_PATH_STYLE"},
	"b2": {"TYPE", "ACCOUNT", "KEY", "HARD_DELETE"},
	"sftp": {"TYPE", "HOST", "PORT", "USER", "KEY_PEM", "PASS", "KEY_FILE_PASS", "KEY_USE_AGENT",
		"ASK_PASSWORD", "KNOWN_HOSTS_FILE"},
}

// cryptOptions is the closed table of BKCRYPT options.
var cryptOptions = []string{"TYPE", "REMOTE", "PASSWORD", "PASSWORD2", "FILENAME_ENCRYPTION",
	"DIRECTORY_NAME_ENCRYPTION", "STRICT_NAMES"}

// envNames are the fixed names a child's environment may hold besides the remote options (S22).
// RCLONE_CA_CERT carries a destination's own CA certificate to restic's rclone backend, which
// takes no --ca-cert flag through restic (rclone commands pass --ca-cert instead); it only adds a
// trusted certificate, never disables verification.
var envNames = []string{"PATH", "TZ", "LANG", "TMPDIR", "HOME", "RCLONE_CONFIG", "RCLONE_BWLIMIT",
	"RESTIC_REPOSITORY", "RESTIC_PASSWORD_FILE", "RESTIC_CACHE_DIR", "RESTIC_PROGRESS_FPS",
	"RCLONE_CA_CERT"}

// DestOptions returns the BKDEST options Bunkarr may set for an rclone backend type ("s3", "b2",
// "sftp"); nil for any other type.
func DestOptions(backend string) []string { return slices.Clone(destOptions[backend]) }

// CryptOptions returns the BKCRYPT options Bunkarr may set.
func CryptOptions() []string { return slices.Clone(cryptOptions) }

// EnvNames returns the fixed environment names a child may get besides the remote options.
func EnvNames() []string { return slices.Clone(envNames) }

// RemoteEnv returns the variable that sets option of remote: RemoteEnv(RemoteDest, "TYPE") is
// "RCLONE_CONFIG_BKDEST_TYPE". An option outside the closed table is a programming error and
// panics, so a builder can only produce allowed names.
func RemoteEnv(remote, option string) string {
	ok := false
	switch remote {
	case RemoteDest:
		for _, opts := range destOptions {
			ok = ok || slices.Contains(opts, option)
		}
	case RemoteCrypt:
		ok = slices.Contains(cryptOptions, option)
	}
	if !ok {
		panic(fmt.Sprintf("proc: rclone option %s of remote %s is not in the allow-list", option, remote))
	}
	return "RCLONE_CONFIG_" + remote + "_" + option
}

// valueCheck validates a flag or option value.
type valueCheck func(v string) error

// flagSpec describes one allowed flag.
type flagSpec struct {
	// value is set for a flag that takes a value ("--flag v" or "--flag=v"); nil for a switch.
	value valueCheck
	// repeat allows the flag more than once (restic --tag, -o).
	repeat bool
}

func sw() flagSpec                    { return flagSpec{} }
func val(check valueCheck) flagSpec   { return flagSpec{value: check} }
func multi(check valueCheck) flagSpec { return flagSpec{value: check, repeat: true} }

// command is one allowed subcommand.
type command struct {
	words []string
	flags map[string]flagSpec
	// args checks the positionals.
	args func(pos []string) error
}

func (c command) name() string { return strings.Join(c.words, " ") }

var (
	hexID        = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	tagRe        = regexp.MustCompile(`^[A-Za-z0-9._:-]+(,[A-Za-z0-9._:-]+)*$`)
	hostRe       = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	subsetRe     = regexp.MustCompile(`^([1-9][0-9]*/[1-9][0-9]*|[0-9]+(\.[0-9]+)?%|[0-9]+[KMGT]?)$`)
	maxUnusedRe  = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?%|[0-9]+[kKmMgGtT]?|unlimited)$`)
	fileNameRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)
	remoteNameRe = regexp.MustCompile(`^(` + RemoteDest + `|` + RemoteCrypt + `):`)
)

func checkInt(lo, hi int64) valueCheck {
	return func(v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < lo || n > hi {
			return fmt.Errorf("must be an integer %d-%d", lo, hi)
		}
		return nil
	}
}

// checkBytes accepts a byte count lo-hi written with rclone's "B" suffix (rclone reads a bare
// number in a size option as KiB).
func checkBytes(lo, hi int64) valueCheck {
	return func(v string) error {
		n, ok := strings.CutSuffix(v, "B")
		if !ok || checkInt(lo, hi)(n) != nil {
			return fmt.Errorf("must be a byte count %d-%d with the B suffix (%dB)", lo, hi, lo)
		}
		return nil
	}
}

func checkDuration(v string) error {
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return fmt.Errorf("must be a duration such as 5s or 1h30m")
	}
	return nil
}

func checkRe(re *regexp.Regexp, what string) valueCheck {
	return func(v string) error {
		if !re.MatchString(v) {
			return fmt.Errorf("must be %s", what)
		}
		return nil
	}
}

func checkEnum(values ...string) valueCheck {
	return func(v string) error {
		if !slices.Contains(values, v) {
			return fmt.Errorf("must be one of %s", strings.Join(values, ", "))
		}
		return nil
	}
}

// checkAbsPath accepts a clean absolute local path (run files, a restore target).
func checkAbsPath(v string) error {
	if !filepath.IsAbs(v) || filepath.Clean(v) != v || hasControl(v) {
		return fmt.Errorf("must be a clean absolute path")
	}
	return nil
}

// checkRemotePath accepts a path on one of the command's own remotes (BKDEST: or BKCRYPT:),
// never another remote, an on-the-fly backend (":s3,...:") or a connection string (S2).
func checkRemotePath(v string) error {
	if !remoteNameRe.MatchString(v) || hasControl(v) {
		return fmt.Errorf("must be a path on %s: or %s:", RemoteDest, RemoteCrypt)
	}
	return nil
}

// checkText accepts any value without control characters (an rclone filter pattern, a time).
func checkText(v string) error {
	if v == "" || hasControl(v) {
		return fmt.Errorf("must be a non-empty value without control characters")
	}
	return nil
}

func hasControl(v string) bool {
	return strings.ContainsFunc(v, unicode.IsControl)
}

// checkResticOption accepts restic's -o only for rclone.program=<absolute path without
// whitespace, quotes or backslashes> (restic shell-splits it) and rclone.connections=<1-128>.
func checkResticOption(v string) error {
	key, value, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("must be key=value")
	}
	switch key {
	case "rclone.program":
		if checkAbsPath(value) != nil || strings.ContainsAny(value, " \t\n\"'`\\") {
			return fmt.Errorf("rclone.program must be an absolute path without whitespace, quotes or backslashes")
		}
		return nil
	case "rclone.connections":
		return checkInt(1, 128)(value)
	default:
		return fmt.Errorf("option %q is not allowed (only rclone.program and rclone.connections)", key)
	}
}

// checkCleanupOption accepts rclone backend cleanup's -o only for max-age=<duration>.
func checkCleanupOption(v string) error {
	key, value, ok := strings.Cut(v, "=")
	if !ok || key != "max-age" {
		return fmt.Errorf("only max-age=<duration> is allowed")
	}
	return checkDuration(value)
}

// Positional rules.

func nArgs(n int, each ...valueCheck) func([]string) error {
	return func(pos []string) error {
		if len(pos) != n {
			return fmt.Errorf("takes %d positional arguments (got %d)", n, len(pos))
		}
		for i, p := range pos {
			if i < len(each) && each[i] != nil {
				if err := each[i](p); err != nil {
					return fmt.Errorf("positional argument %d %v", i+1, err)
				}
			}
		}
		return nil
	}
}

func rangeArgs(lo, hi int, check valueCheck) func([]string) error {
	return func(pos []string) error {
		if len(pos) < lo || len(pos) > hi {
			return fmt.Errorf("takes %d-%d positional arguments (got %d)", lo, hi, len(pos))
		}
		for i, p := range pos {
			if err := check(p); err != nil {
				return fmt.Errorf("positional argument %d %v", i+1, err)
			}
		}
		return nil
	}
}

// localOrRemote accepts an absolute local path or a path on the command's remotes.
func localOrRemote(v string) error {
	if checkAbsPath(v) == nil || checkRemotePath(v) == nil {
		return nil
	}
	return fmt.Errorf("must be an absolute local path or a path on %s: or %s:", RemoteDest, RemoteCrypt)
}

// copytoArgs: one side must be a remote (an upload of manifest.json, or a Fetch of a version
// file into staging); never local to local.
func copytoArgs(pos []string) error {
	if err := nArgs(2, localOrRemote, localOrRemote)(pos); err != nil {
		return err
	}
	if checkRemotePath(pos[0]) != nil && checkRemotePath(pos[1]) != nil {
		return fmt.Errorf("copies local to local")
	}
	return nil
}

// resticCatArgs: "config", "lock <id>", or "snapshot <id>" (a listing too large for one line of
// restic snapshots --json is read snapshot by snapshot).
func resticCatArgs(pos []string) error {
	switch {
	case len(pos) == 1 && pos[0] == "config":
		return nil
	case len(pos) == 2 && (pos[0] == "lock" || pos[0] == "snapshot") && hexID.MatchString(pos[1]):
		return nil
	}
	return fmt.Errorf("only cat config, cat lock <id> and cat snapshot <id> are allowed")
}

// restoreSource: "<snapshot id>[:<absolute path>]".
func restoreSource(v string) error {
	id, sub, hasSub := strings.Cut(v, ":")
	if !hexID.MatchString(id) || (hasSub && checkAbsPath(sub) != nil) {
		return fmt.Errorf("must be <snapshot id> or <snapshot id>:<absolute path>")
	}
	return nil
}

func withFlags(sets ...map[string]flagSpec) map[string]flagSpec {
	out := map[string]flagSpec{}
	for _, s := range sets {
		maps.Copy(out, s)
	}
	return out
}

// commands is the allow-list: per binary, each subcommand with its exact flags and positionals.
var commands = func() map[Binary][]command {
	// Flags of every rclone command: the JSON log at -v at most, stats, retries, a CA
	// certificate for a self-signed S3 endpoint (TLS verification is never disabled).
	rcloneLog := map[string]flagSpec{
		"--use-json-log":      sw(),
		"-v":                  sw(),
		"--stats":             val(checkDuration),
		"--stats-log-level":   val(checkEnum("NOTICE", "INFO", "ERROR")),
		"--retries":           val(checkInt(1, 10)),
		"--low-level-retries": val(checkInt(1, 20)),
		"--ca-cert":           val(checkAbsPath),
	}
	transfer := map[string]flagSpec{
		"--transfers":    val(checkInt(1, 64)),
		"--checkers":     val(checkInt(1, 128)),
		"--max-duration": val(checkDuration),
		"--cutoff-mode":  val(checkEnum("soft")),
	}
	filesFrom := map[string]flagSpec{
		"--files-from-raw": val(checkAbsPath),
		"--no-traverse":    sw(),
	}
	maxDelete := map[string]flagSpec{"--max-delete": val(checkInt(0, 1<<40))}
	remote1 := nArgs(1, checkRemotePath)

	rclone := []command{
		// §7.3, §8.3: the source is a local root (or a staged version); the target a remote.
		{[]string{"copy"}, withFlags(rcloneLog, transfer, filesFrom, maxDelete, map[string]flagSpec{
			"--backup-dir": val(checkRemotePath),
			"--exclude":    val(checkText),
		}), nArgs(2, localOrRemote, checkRemotePath)},
		// A download (Fetch and ReadFile of config versions) is capped: --max-transfer with
		// --cutoff-mode hard stops it at the cap whatever the remote serves.
		{[]string{"copyto"}, withFlags(rcloneLog, transfer, maxDelete, map[string]flagSpec{
			"--no-traverse":  sw(),
			"--max-transfer": val(checkBytes(1, 1<<50)),
			"--cutoff-mode":  val(checkEnum("soft", "hard")),
		}), copytoArgs},
		// S1: move, moveto, delete, deletefile and purge only ever name remote paths.
		{[]string{"move"}, withFlags(rcloneLog, transfer, filesFrom, maxDelete), nArgs(2, checkRemotePath, checkRemotePath)},
		{[]string{"moveto"}, withFlags(rcloneLog, transfer, maxDelete, map[string]flagSpec{"--no-traverse": sw()}), nArgs(2, checkRemotePath, checkRemotePath)},
		{[]string{"delete"}, withFlags(rcloneLog, maxDelete, map[string]flagSpec{
			"--files-from-raw": val(checkAbsPath),
			"--checkers":       val(checkInt(1, 128)),
		}), remote1},
		{[]string{"deletefile"}, withFlags(rcloneLog, maxDelete), remote1},
		{[]string{"purge"}, withFlags(rcloneLog, maxDelete), remote1},
		// statMany and the verify listing; never --stat (a missing S3 object stats as a directory).
		{[]string{"lsjson"}, withFlags(rcloneLog, map[string]flagSpec{
			"-R":               sw(),
			"--files-only":     sw(),
			"--dirs-only":      sw(),
			"--no-mimetype":    sw(),
			"--files-from-raw": val(checkAbsPath),
			"--max-depth":      val(checkInt(1, 1<<20)),
		}), remote1},
		{[]string{"lsf"}, withFlags(rcloneLog, map[string]flagSpec{
			"--max-depth":  val(checkInt(1, 1<<20)),
			"--files-only": sw(),
			"--dirs-only":  sw(),
		}), remote1},
		// §7.6: the source root against the remote, content downloaded.
		{[]string{"check"}, withFlags(rcloneLog, map[string]flagSpec{
			"--one-way":        sw(),
			"--download":       sw(),
			"--files-from-raw": val(checkAbsPath),
			"--combined":       val(checkAbsPath),
			"--checkers":       val(checkInt(1, 128)),
			"--max-duration":   val(checkDuration),
		}), nArgs(2, checkAbsPath, checkRemotePath)},
		{[]string{"cat"}, withFlags(rcloneLog, map[string]flagSpec{"--count": val(checkInt(1, 1<<40))}), remote1},
		{[]string{"rcat"}, withFlags(rcloneLog, map[string]flagSpec{"--size": val(checkInt(0, 1<<50))}), remote1},
		{[]string{"rmdirs"}, withFlags(rcloneLog, map[string]flagSpec{"--leave-root": sw()}), remote1},
		{[]string{"about"}, withFlags(rcloneLog, map[string]flagSpec{"--json": sw()}), remote1},
		// §7.5: unfinished multipart uploads under the destination's own prefix only.
		{[]string{"backend", "cleanup"}, withFlags(rcloneLog, map[string]flagSpec{"-o": val(checkCleanupOption)}),
			nArgs(1, checkRe(regexp.MustCompile(`^`+RemoteDest+`:`), "a path on "+RemoteDest+":"))},
		{[]string{"version"}, map[string]flagSpec{}, nArgs(0)},
	}

	resticOpt := map[string]flagSpec{"-o": multi(checkResticOption)}
	noLock := map[string]flagSpec{"--json": sw(), "--no-lock": sw()}
	limits := map[string]flagSpec{
		"--limit-upload":   val(checkInt(1, 1<<40)),
		"--limit-download": val(checkInt(1, 1<<40)),
	}
	retryLock := map[string]flagSpec{"--retry-lock": val(checkDuration)}
	packSize := map[string]flagSpec{"--pack-size": val(checkInt(4, 128))}
	ids := rangeArgs(0, 1<<16, checkRe(hexID, "a snapshot id"))

	restic := []command{
		{[]string{"init"}, withFlags(resticOpt, map[string]flagSpec{
			"--json":               sw(),
			"--repository-version": val(checkEnum("2")),
		}), nArgs(0)},
		{[]string{"cat"}, withFlags(resticOpt, noLock), resticCatArgs},
		{[]string{"list"}, withFlags(resticOpt, noLock), nArgs(1, checkEnum("locks", "snapshots"))},
		{[]string{"unlock"}, withFlags(resticOpt, map[string]flagSpec{"--remove-all": sw()}), nArgs(0)},
		// §6.2 step 4 and §8.2: never --dry-run (S9), never a symlink-following flag.
		{[]string{"backup"}, withFlags(resticOpt, limits, retryLock, packSize, map[string]flagSpec{
			"--json":               sw(),
			"--host":               val(checkRe(hostRe, "a host name")),
			"--tag":                multi(checkRe(tagRe, "a tag list")),
			"--files-from-raw":     val(checkAbsPath),
			"--exclude-file":       val(checkAbsPath),
			"--iexclude-file":      val(checkAbsPath),
			"--exclude-if-present": val(checkRe(fileNameRe, "a file name")),
			"--parent":             val(checkRe(hexID, "a snapshot id")),
			"--ignore-inode":       sw(),
			"--time":               val(checkText),
		}), rangeArgs(0, 1, checkAbsPath)},
		{[]string{"snapshots"}, withFlags(resticOpt, noLock, map[string]flagSpec{
			"--tag": multi(checkRe(tagRe, "a tag list")),
		}), ids},
		{[]string{"ls"}, withFlags(resticOpt, noLock), nArgs(1, checkRe(hexID, "a snapshot id"))},
		// S24: forget by id only, never with a --keep-* policy.
		{[]string{"forget"}, withFlags(resticOpt, retryLock, map[string]flagSpec{"--json": sw()}),
			rangeArgs(1, 100, checkRe(hexID, "a snapshot id"))},
		// S24: never --unsafe-* or --repack-*.
		{[]string{"prune"}, withFlags(resticOpt, limits, retryLock, packSize, map[string]flagSpec{
			"--max-unused": val(checkRe(maxUnusedRe, "a percentage, a size or unlimited")),
		}), nArgs(0)},
		{[]string{"check"}, withFlags(resticOpt, retryLock, map[string]flagSpec{
			"--json":             sw(),
			"--read-data":        sw(),
			"--read-data-subset": val(checkRe(subsetRe, "n/t, a percentage or a size")),
			"--limit-download":   val(checkInt(1, 1<<40)),
		}), nArgs(0)},
		// §6.6: restore <base>:<source root> into staging, never into a source (S1).
		{[]string{"restore"}, withFlags(resticOpt, noLock, map[string]flagSpec{
			"--target":         val(checkAbsPath),
			"--include-file":   val(checkAbsPath),
			"--limit-download": val(checkInt(1, 1<<40)),
		}), nArgs(1, restoreSource)},
		{[]string{"dump"}, withFlags(resticOpt, map[string]flagSpec{
			"--no-lock":        sw(),
			"--limit-download": val(checkInt(1, 1<<40)),
		}), nArgs(2, checkRe(hexID, "a snapshot id"), checkAbsPath)},
		{[]string{"version"}, map[string]flagSpec{"--json": sw()}, nArgs(0)},
	}
	return map[Binary][]command{Rclone: rclone, Restic: restic}
}()

// Subcommands returns the allowed subcommands of b ("backend cleanup" for a two-word one).
func Subcommands(b Binary) []string {
	var out []string
	for _, c := range commands[b] {
		out = append(out, c.name())
	}
	return out
}

// Validate checks c against the allow-list (S22): a known binary; a listed subcommand with only
// its listed flags (a value flag as "--flag v" or "--flag=v", a switch bare, each once unless
// repeatable) and valid positionals, none starting with "-"; no argument holding a value the
// runner redacts from the command's output (redactedFromOutput) or one of the secrets of its
// environment and secret files (envSecrets); no file list printed back redacted
// (fileListRedacted); environment names from the fixed list or the closed per-backend option
// tables, consistent with BKDEST's and BKCRYPT's TYPE, with RCLONE_CONFIG=/dev/null for every
// rclone command and every command that configures a remote; secret file names that are plain
// names, and a run directory when there are any. The error wraps ErrNotAllowed and never
// contains an argument that holds a secret.
func Validate(c Cmd) error {
	if c.Binary != Restic && c.Binary != Rclone {
		return errNotAllowed("unknown binary %q", c.Binary)
	}
	secrets := envSecrets(c)
	for i, a := range c.Args {
		if containsAny(a, secrets) || redactedFromOutput(a, c.Redact) {
			return errNotAllowed("%s: argument %d contains a secret or a stored credential such as an access key ID, "+
				"which engine output is redacted of (rename the bucket, prefix, folder or path that holds it)", c.Binary, i+1)
		}
		if strings.ContainsRune(a, 0) {
			return errNotAllowed("%s: argument %d contains a NUL byte", c.Binary, i+1)
		}
	}
	cmd, rest, ok := findCommand(c.Binary, c.Args)
	if !ok {
		first := ""
		if len(c.Args) > 0 {
			first = c.Args[0]
		}
		return errNotAllowed("%s: subcommand %q is not allowed", c.Binary, first)
	}
	var pos []string
	var fileList string
	seen := map[string]bool{}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if !strings.HasPrefix(a, "-") {
			if a == "" {
				return errNotAllowed("%s %s: empty argument", c.Binary, cmd.name())
			}
			pos = append(pos, a)
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		spec, ok := cmd.flags[name]
		if !ok {
			return errNotAllowed("%s %s: flag %s is not allowed", c.Binary, cmd.name(), name)
		}
		if seen[name] && !spec.repeat {
			return errNotAllowed("%s %s: flag %s given twice", c.Binary, cmd.name(), name)
		}
		seen[name] = true
		if spec.value == nil {
			if hasValue {
				return errNotAllowed("%s %s: flag %s takes no value", c.Binary, cmd.name(), name)
			}
			continue
		}
		if !hasValue {
			if i+1 >= len(rest) || strings.HasPrefix(rest[i+1], "-") {
				return errNotAllowed("%s %s: flag %s needs a value", c.Binary, cmd.name(), name)
			}
			i++
			value = rest[i]
		}
		if err := spec.value(value); err != nil {
			return errNotAllowed("%s %s: flag %s: %v", c.Binary, cmd.name(), name, err)
		}
		if name == fileListFlag {
			fileList = value
		}
	}
	if err := cmd.args(pos); err != nil {
		return errNotAllowed("%s %s: %v", c.Binary, cmd.name(), err)
	}
	if hint, ok := fileListRedacted(c, fileList, c.Binary == Rclone && cmd.name() == "lsjson"); ok {
		return errNotAllowed("%s %s: a path of the file list (%s) contains a secret or a stored credential such as an "+
			"access key ID, which engine output is redacted of (rename the folder or path that holds it)", c.Binary, cmd.name(), hint)
	}
	if err := validateEnv(c); err != nil {
		return err
	}
	if len(c.SecretFiles) > 0 && c.Dir == nil {
		return errNotAllowed("%s %s: secret files without a run directory", c.Binary, cmd.name())
	}
	for name := range c.SecretFiles {
		if !fileNameRe.MatchString(name) || name == "." || name == ".." {
			return errNotAllowed("%s %s: secret file name %q", c.Binary, cmd.name(), name)
		}
	}
	if c.Budget < 0 || c.RetryBudget < 0 {
		return errNotAllowed("%s %s: negative budget", c.Binary, cmd.name())
	}
	return nil
}

// findCommand matches the subcommand words at the start of args (the longest match).
func findCommand(b Binary, args []string) (command, []string, bool) {
	var best command
	found := false
	for _, c := range commands[b] {
		if len(args) >= len(c.words) && slices.Equal(args[:len(c.words)], c.words) && len(c.words) > len(best.words) {
			best, found = c, true
		}
	}
	if !found {
		return command{}, nil, false
	}
	return best, args[len(best.words):], true
}

// secretOptions are the options, per remote, whose values are secrets (in clear or obscured).
var secretOptions = map[string][]string{
	RemoteDest:  {"SECRET_ACCESS_KEY", "KEY", "KEY_PEM", "PASS", "KEY_FILE_PASS"},
	RemoteCrypt: {"PASSWORD", "PASSWORD2"},
}

// envSecrets returns each value of a secret option of c's environment and the content of each of
// c.SecretFiles (the restic password), with their escaped forms: no argument may hold one (S22),
// even when a caller leaves c.Redact short.
func envSecrets(c Cmd) []string {
	forms := func(v string) []string {
		if v == "" {
			return nil
		}
		return append([]string{v}, logging.EscapedForms(v)...)
	}
	var secret []string
	for _, remote := range []string{RemoteDest, RemoteCrypt} {
		for _, o := range secretOptions[remote] {
			secret = append(secret, forms(c.Env[RemoteEnv(remote, o)])...)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.SecretFiles)) {
		secret = append(secret, forms(string(c.SecretFiles[name]))...)
	}
	return secret
}

// redactedFromOutput reports whether the runner's redaction of the command's output (the
// process-wide registry and redact, RedactLine) would change a, as it is or in the escaped forms
// a JSON or Go-quoted line carries it in (logging.EscapedForms). No argument may (S22, §14.2
// "the argv holds no registered secret"): besides keeping secrets off the command line, it keeps
// the output readable. Every path a command names can come back in its output (rclone lsjson and
// its JSON log, restic ls and snapshots), and a path that came back as "[REDACTED]" would be
// misread: an object reported absent is overwritten or forgotten, a retained version is lost.
// So the forms of an S3 access key ID or B2 keyId (engines.Secrets.Values, and registered for
// every stored destination) are refused too, even though they only name an account: the command
// fails before anything changes, instead of deciding on a redacted listing.
func redactedFromOutput(a string, redact []string) bool {
	for _, f := range append([]string{a}, logging.EscapedForms(a)...) {
		if logging.RedactValues(f, redact...) != f {
			return true
		}
	}
	return false
}

// fileListFlag names the file list of rclone copy, move, delete, lsjson and check and of restic
// backup, whose entries the command's output names (rclone's JSON log and lsjson, restic ls of
// the snapshot it made).
const fileListFlag = "--files-from-raw"

// fileListRedacted reports whether the output of c would print entries of its file list (list,
// a file in c's data directory; entries end with "\n" for rclone, NUL for restic) back redacted,
// and returns a hint: the first such entry, or the prefix all entries share, with the values
// redacted. A listing (anyEntry: rclone lsjson, which statMany and verify read as the objects
// that exist) is refused when any entry would: a redacted entry reads as absent, and a job would
// drop the record of a vanished file, forget retained versions, or copy over the live one without
// retaining it. Another command is refused when every entry would, because the prefix they share
// holds the value (a restic source root /mnt/user/<value>, a destination folder <value>/): then
// no item could be read back (restic's read-back failed each as "not in the snapshot", every
// run). One such entry of a backup, copy or move fails on its own (its item is not confirmed). An
// unreadable list is not checked here (the command fails on it).
func fileListRedacted(c Cmd, list string, anyEntry bool) (string, bool) {
	if list == "" || c.Dir == nil || filepath.Dir(list) != filepath.Clean(c.Dir.DataDir()) {
		return "", false
	}
	f, err := os.Open(list)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	sep := byte('\n')
	if c.Binary == Restic {
		sep = 0
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, sep); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	hint := func(s string) string {
		return logging.RedactValues(s, append(slices.Clone(c.Redact), envSecrets(c)...)...)
	}
	var prefix []byte
	first := true
	for sc.Scan() {
		e := sc.Bytes()
		if len(e) == 0 {
			continue
		}
		if anyEntry {
			if s := string(e); redactedFromOutput(s, c.Redact) {
				return hint(s), true
			}
			continue
		}
		if first {
			prefix, first = bytes.Clone(e), false
		} else {
			n := 0
			for n < len(prefix) && n < len(e) && prefix[n] == e[n] {
				n++
			}
			prefix = prefix[:n]
		}
		if len(prefix) < minRedacted {
			return "", false
		}
	}
	if anyEntry || first || sc.Err() != nil || !redactedFromOutput(string(prefix), c.Redact) {
		return "", false
	}
	return hint(string(prefix)), true
}

// minRedacted is the length of the shortest value the redaction replaces (logging's minimum).
const minRedacted = 8

func containsAny(s string, values []string) bool {
	for _, v := range values {
		if len(v) >= 8 && strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// validateEnv checks the environment names and the few values that decide where data goes.
func validateEnv(c Cmd) error {
	var destType, cryptType string
	dest := map[string]bool{}
	crypt := map[string]bool{}
	for k, v := range c.Env {
		if strings.ContainsRune(v, 0) {
			return errNotAllowed("%s: environment %s contains a NUL byte", c.Binary, k)
		}
		switch {
		case slices.Contains(envNames, k):
		case strings.HasPrefix(k, "RCLONE_CONFIG_"+RemoteDest+"_"):
			opt := strings.TrimPrefix(k, "RCLONE_CONFIG_"+RemoteDest+"_")
			dest[opt] = true
			if opt == "TYPE" {
				destType = v
			}
		case strings.HasPrefix(k, "RCLONE_CONFIG_"+RemoteCrypt+"_"):
			opt := strings.TrimPrefix(k, "RCLONE_CONFIG_"+RemoteCrypt+"_")
			if !slices.Contains(cryptOptions, opt) {
				return errNotAllowed("%s: environment %s is not allowed", c.Binary, k)
			}
			crypt[opt] = true
			if opt == "TYPE" {
				cryptType = v
			}
		default:
			return errNotAllowed("%s: environment %s is not allowed", c.Binary, k)
		}
	}
	if len(dest) > 0 {
		allowed, ok := destOptions[destType]
		if !ok {
			return errNotAllowed("%s: RCLONE_CONFIG_%s_TYPE must be s3, b2 or sftp", c.Binary, RemoteDest)
		}
		for opt := range dest {
			if !slices.Contains(allowed, opt) {
				return errNotAllowed("%s: environment RCLONE_CONFIG_%s_%s is not allowed for type %s", c.Binary, RemoteDest, opt, destType)
			}
		}
	}
	if len(crypt) > 0 && (cryptType != "crypt" || len(dest) == 0) {
		return errNotAllowed("%s: RCLONE_CONFIG_%s needs TYPE=crypt over %s", c.Binary, RemoteCrypt, RemoteDest)
	}
	if (c.Binary == Rclone || len(dest) > 0) && c.Env["RCLONE_CONFIG"] != "/dev/null" {
		return errNotAllowed("%s: RCLONE_CONFIG must be /dev/null (no rclone.conf is read or written)", c.Binary)
	}
	for _, k := range []string{"RESTIC_PASSWORD_FILE", "RESTIC_CACHE_DIR", "RCLONE_CA_CERT"} {
		if v, ok := c.Env[k]; ok && checkAbsPath(v) != nil {
			return errNotAllowed("%s: environment %s must be an absolute path", c.Binary, k)
		}
	}
	if c.Binary == Restic && c.Env["RESTIC_REPOSITORY"] != "" {
		repo := c.Env["RESTIC_REPOSITORY"]
		if !strings.HasPrefix(repo, "rclone:"+RemoteDest+":") && checkAbsPath(repo) != nil {
			return errNotAllowed("%s: RESTIC_REPOSITORY must be rclone:%s:<path> or an absolute path", c.Binary, RemoteDest)
		}
	}
	return nil
}
