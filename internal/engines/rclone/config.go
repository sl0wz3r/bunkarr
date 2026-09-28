// Package rclone is the rclone engine (docs/design/phase4.md §7): a driver for the rclone binary
// behind engines.Engine. This file and its siblings config.go, obscure.go, knownhosts.go and
// timetable.go build what every rclone command (and restic's rclone backend) is configured with,
// from the closed option table of §4.4 only (safety rule S22): the child's environment
// (RCLONE_CONFIG=/dev/null plus RCLONE_CONFIG_BKDEST_* and RCLONE_CONFIG_BKCRYPT_*), the
// destination root, the recovery kit's rclone.conf, rclone's password obscuring with a
// deterministic IV, the pinned SFTP known_hosts file and the bandwidth timetable string.
package rclone

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// Remote names in the recovery kit's rclone.conf (§5.2).
const (
	ConfDest  = "bunkarr-dest"
	ConfCrypt = "bunkarr-crypt"
)

// EnvInput is what Env builds a child's environment from.
type EnvInput struct {
	Dest    engines.Destination
	Secrets engines.Secrets
	// KnownHostsPath is the run directory's known_hosts secret file (KnownHosts rendered);
	// required for sftp.
	KnownHostsPath string
	// CACertPath is the run directory's ca.pem when the S3 remote has a caCert; required then.
	// Env does not put it in the environment: rclone commands pass --ca-cert CACertPath.
	CACertPath string
	// BWLimit is BWLimit(bandwidth) ("" = no limit).
	BWLimit string
}

// option is one rclone option and its value.
type option struct{ name, value string }

// Env returns the environment of an rclone command for in.Dest, exactly §4.4:
// RCLONE_CONFIG=/dev/null, RCLONE_BWLIMIT when set, BKDEST (the storage remote) and, when the
// destination is encrypted with crypt, BKCRYPT over it. Only options of proc's closed table are
// ever set, so no user-supplied name becomes an option. Passwords come from Secrets.Obscured.
// It fails for a local destination, missing credentials, an sftp remote without pinned host keys
// or known_hosts file (rclone would check no host key), a value with CR, LF or NUL and, for
// crypt, a value the recovery kit's rclone.conf cannot carry verbatim (checkConfValue).
func Env(in EnvInput) (map[string]string, error) {
	dest, err := destOptions(in.Dest, in.Secrets, in.KnownHostsPath, true)
	if err != nil {
		return nil, err
	}
	if s3 := in.Dest.Remote.S3; in.Dest.Kind == engines.S3 && s3 != nil && s3.CACert != "" && in.CACertPath == "" {
		return nil, errors.New("rclone environment: the S3 remote has a CA certificate but no ca.pem path was given")
	}
	env := map[string]string{"RCLONE_CONFIG": "/dev/null"}
	if in.BWLimit != "" {
		if err := checkValue("bandwidth limit", in.BWLimit); err != nil {
			return nil, err
		}
		env["RCLONE_BWLIMIT"] = in.BWLimit
	}
	for _, o := range dest {
		env[proc.RemoteEnv(proc.RemoteDest, o.name)] = o.value
	}
	if in.Dest.Encryption == engines.EncryptionCrypt {
		crypt, err := cryptOptions(in.Dest, in.Secrets, StorageRoot(in.Dest))
		if err != nil {
			return nil, err
		}
		// Only the recovery kit's rclone.conf can decrypt a crypt destination, so every command
		// (the create's marker write and attach's probe included) refuses a value that file
		// cannot carry as the jobs use it; else jobs would upload where no kit can name.
		if err := checkConfOptions(dest, "KNOWN_HOSTS_FILE"); err != nil {
			return nil, fmt.Errorf("rclone environment: %w", err)
		}
		if err := checkConfOptions(crypt, ""); err != nil {
			return nil, fmt.Errorf("rclone environment: %w", err)
		}
		for _, o := range crypt {
			env[proc.RemoteEnv(proc.RemoteCrypt, o.name)] = o.value
		}
	}
	return env, nil
}

// checkConfOptions runs checkConfValue on each option but skip (one the kit writes its own value
// for, like its known_hosts_file path).
func checkConfOptions(opts []option, skip string) error {
	for _, o := range opts {
		if o.name == skip {
			continue
		}
		if err := checkConfValue(strings.ToLower(o.name), o.value); err != nil {
			return err
		}
	}
	return nil
}

// Root returns the destination root every rclone command names: "BKCRYPT:" when the destination
// is encrypted with crypt, else StorageRoot (§4.4).
func Root(d engines.Destination) string {
	if d.Encryption == engines.EncryptionCrypt {
		return proc.RemoteCrypt + ":"
	}
	return StorageRoot(d)
}

// StorageRoot returns the storage location on BKDEST: "BKDEST:<bucket>/<prefix>" (s3, b2; no
// slash for an empty prefix) or "BKDEST:<path>" (sftp); "" for a local destination or a missing
// remote.
func StorageRoot(d engines.Destination) string {
	return remoteRoot(proc.RemoteDest, d)
}

func remoteRoot(remote string, d engines.Destination) string {
	join := func(bucket, prefix string) string {
		if prefix == "" {
			return remote + ":" + bucket
		}
		return remote + ":" + bucket + "/" + prefix
	}
	switch {
	case d.Kind == engines.S3 && d.Remote.S3 != nil:
		return join(d.Remote.S3.Bucket, d.Remote.S3.Prefix)
	case d.Kind == engines.B2 && d.Remote.B2 != nil:
		return join(d.Remote.B2.Bucket, d.Remote.B2.Prefix)
	case d.Kind == engines.SFTP && d.Remote.SFTP != nil:
		return remote + ":" + d.Remote.SFTP.Path
	}
	return ""
}

// ConfFile renders the recovery kit's rclone.conf (§5.2): [bunkarr-dest] with the storage
// options and, for crypt, [bunkarr-crypt] over it, with the same values as Env (obscured
// passwords). Storage credentials are written only when s has them (the kit asks for them with
// includeStorageCredentials). knownHostsPath is the sftp known_hosts_file line
// ("./bunkarr_known_hosts"). A value with CR or LF is refused, so no value can add a line or a
// section, and so is a value rclone would read back from the file as another value
// (checkConfValue): the environment passes values verbatim, so the kit would name another
// location or credential than the jobs use.
func ConfFile(d engines.Destination, s engines.Secrets, knownHostsPath string) (string, error) {
	dest, err := destOptions(d, s, knownHostsPath, false)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	section := func(name string, opts []option) error {
		if err := checkConfOptions(opts, ""); err != nil {
			return err
		}
		fmt.Fprintf(&b, "[%s]\n", name)
		for _, o := range opts {
			fmt.Fprintf(&b, "%s = %s\n", strings.ToLower(o.name), o.value)
		}
		return nil
	}
	if err := section(ConfDest, dest); err != nil {
		return "", err
	}
	if d.Encryption == engines.EncryptionCrypt {
		crypt, err := cryptOptions(d, s, remoteRoot(ConfDest, d))
		if err != nil {
			return "", err
		}
		b.WriteString("\n")
		if err := section(ConfCrypt, crypt); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

// confVariable is a variable reference of rclone's config file parser (github.com/unknwon/goconfig).
var confVariable = regexp.MustCompile(`%\([^)]+\)s`)

// checkConfValue refuses a value that rclone's config file parser would read back as another
// value (measured with rclone 1.74.1, TestRealConfFileRoundTrip): it trims leading and trailing
// white space (unicode.IsSpace, so NBSP too), takes a value that starts with ` or """ as quoted
// and strips the quotes, and replaces %(name)s with the value of key name (or nothing). rclone
// takes the environment's values verbatim, so the kit's rclone.conf would name another location
// (a prefix "backups " is read as "backups") or credential than the jobs used.
func checkConfValue(name, v string) error {
	switch {
	case strings.TrimSpace(v) != v:
		return fmt.Errorf("rclone.conf: %s starts or ends with white space, which rclone's config file drops", name)
	case strings.HasPrefix(v, "`"), strings.HasPrefix(v, `"""`):
		return fmt.Errorf("rclone.conf: %s starts with a quote rclone's config file strips", name)
	case confVariable.MatchString(v):
		return fmt.Errorf("rclone.conf: %s contains a %%(name)s reference rclone's config file expands", name)
	}
	return nil
}

// destOptions returns BKDEST's options in table order. requireCreds is false for the kit, which
// may leave the storage credentials out.
func destOptions(d engines.Destination, s engines.Secrets, knownHostsPath string, requireCreds bool) ([]option, error) {
	c := s.Credentials
	var opts []option
	add := func(name, value string) { opts = append(opts, option{name, value}) }
	addSet := func(name, value string) {
		if value != "" {
			add(name, value)
		}
	}
	needs := func(fields ...string) error {
		if !requireCreds {
			return nil
		}
		have := c.Fields()
		for _, f := range fields {
			if !have[f] {
				return fmt.Errorf("rclone environment: %s credentials lack %s", d.Kind, f)
			}
		}
		return nil
	}
	switch {
	case d.Kind == engines.S3 && d.Remote.S3 != nil:
		r := d.Remote.S3
		if r.Provider == "" || r.Bucket == "" {
			return nil, errors.New("rclone environment: the S3 remote lacks its provider or bucket")
		}
		if err := needs(engines.FieldAccessKeyID, engines.FieldSecretAccessKey); err != nil {
			return nil, err
		}
		add("TYPE", "s3")
		add("PROVIDER", r.Provider)
		addSet("ENDPOINT", r.Endpoint)
		addSet("REGION", r.Region)
		addSet("ACCESS_KEY_ID", c.AccessKeyID)
		addSet("SECRET_ACCESS_KEY", c.SecretAccessKey)
		add("ENV_AUTH", "false")
		add("NO_CHECK_BUCKET", "true")
		addSet("STORAGE_CLASS", r.StorageClass)
		add("FORCE_PATH_STYLE", strconv.FormatBool(r.ForcePathStyle))
	case d.Kind == engines.B2 && d.Remote.B2 != nil:
		if d.Remote.B2.Bucket == "" {
			return nil, errors.New("rclone environment: the B2 remote lacks its bucket")
		}
		if err := needs(engines.FieldKeyID, engines.FieldApplicationKey); err != nil {
			return nil, err
		}
		add("TYPE", "b2")
		addSet("ACCOUNT", c.KeyID)
		addSet("KEY", c.ApplicationKey)
		add("HARD_DELETE", "true")
	case d.Kind == engines.SFTP && d.Remote.SFTP != nil:
		r := d.Remote.SFTP
		if r.Host == "" || r.User == "" {
			return nil, errors.New("rclone environment: the SFTP remote lacks its host or user")
		}
		// Fail closed: rclone checks no host key when known_hosts_file is empty (§4.4).
		if len(r.HostKeys) == 0 {
			return nil, errors.New("rclone environment: the SFTP remote has no pinned host keys")
		}
		if knownHostsPath == "" {
			return nil, errors.New("rclone environment: no known_hosts file for the SFTP remote")
		}
		if requireCreds && c.PrivateKey == "" && c.Password == "" {
			return nil, errors.New("rclone environment: sftp credentials lack privateKey or password")
		}
		port := r.Port
		if port == 0 {
			port = 22
		}
		add("TYPE", "sftp")
		add("HOST", r.Host)
		add("PORT", strconv.Itoa(port))
		add("USER", r.User)
		if c.PrivateKey != "" {
			add("KEY_PEM", pemEscape(c.PrivateKey))
		}
		if c.Password != "" {
			obscured, err := obscured(s, engines.FieldPassword)
			if err != nil {
				return nil, err
			}
			add("PASS", obscured)
		}
		if c.PrivateKeyPassphrase != "" {
			obscured, err := obscured(s, engines.FieldPrivateKeyPassphrase)
			if err != nil {
				return nil, err
			}
			add("KEY_FILE_PASS", obscured)
		}
		add("KEY_USE_AGENT", "false")
		add("ASK_PASSWORD", "false")
		add("KNOWN_HOSTS_FILE", knownHostsPath)
	default:
		return nil, fmt.Errorf("rclone environment: no %s remote for a destination of kind %q", d.Kind, d.Kind)
	}
	for _, o := range opts {
		if err := checkValue(strings.ToLower(o.name), o.value); err != nil {
			return nil, err
		}
	}
	// The bucket, prefix or path reach the crypt remote and every command's arguments.
	if err := checkValue("location", StorageRoot(d)); err != nil {
		return nil, err
	}
	return opts, nil
}

// cryptOptions returns BKCRYPT's options over storage (the BKDEST or bunkarr-dest root).
func cryptOptions(d engines.Destination, s engines.Secrets, storage string) ([]option, error) {
	if storage == "" {
		return nil, fmt.Errorf("rclone environment: crypt over a destination of kind %q", d.Kind)
	}
	password, err := obscured(s, engines.FieldCryptPassword)
	if err != nil {
		return nil, err
	}
	opts := []option{{"TYPE", "crypt"}, {"REMOTE", storage}, {"PASSWORD", password}}
	if s.Obscured[engines.FieldCryptPassword2] != "" {
		opts = append(opts, option{"PASSWORD2", s.Obscured[engines.FieldCryptPassword2]})
	}
	opts = append(opts, option{"FILENAME_ENCRYPTION", "standard"}, option{"DIRECTORY_NAME_ENCRYPTION", "true"},
		option{"STRICT_NAMES", "true"})
	for _, o := range opts {
		if err := checkValue(strings.ToLower(o.name), o.value); err != nil {
			return nil, err
		}
	}
	return opts, nil
}

// obscured returns the obscured form of a set field.
func obscured(s engines.Secrets, field string) (string, error) {
	v := s.Obscured[field]
	if v == "" {
		return "", fmt.Errorf("rclone environment: no obscured %s", field)
	}
	return v, nil
}

// pemEscape writes a PEM key on one line with its line breaks as `\n`: rclone's key_pem form,
// which rclone unquotes (strconv.Unquote) before it parses the key.
func pemEscape(pem string) string {
	q := strconv.Quote(strings.TrimSpace(pem) + "\n")
	return q[1 : len(q)-1]
}

// checkValue refuses a value that could break a config line or the environment.
func checkValue(name, v string) error {
	if strings.ContainsAny(v, "\r\n\x00") {
		return fmt.Errorf("rclone environment: %s contains a line break or NUL", name)
	}
	return nil
}
