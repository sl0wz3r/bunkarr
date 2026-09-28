package rclone

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

const testPEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\nQyNTUxOQAAACD0bUE4j8wN1nq2mJ1z0fBmXkq3n4bT1c5ywh1sRkQ6kgAAAJDl7hNs5e4T\n-----END OPENSSH PRIVATE KEY-----\n"

// hostKey is a real ed25519 host key (base64 SSH wire format).
var hostKey = engines.HostKey{Type: "ssh-ed25519", Key: "AAAAC3NzaC1lZDI1NTE5AAAAIPRtQTiPzA3WeraYnXPR8GZeSrefhtPVznLCHWxGRDqS"}

func s3Dest(crypt bool) (engines.Destination, engines.Secrets) {
	d := engines.Destination{ID: 3, Kind: engines.S3, Engine: engines.Rclone, Encryption: engines.EncryptionNone,
		Remote: engines.Remote{S3: &engines.S3Remote{Provider: "Minio", Endpoint: "https://minio.lan:9000", Region: "us-east-1",
			Bucket: "media", Prefix: "bunkarr/offsite", StorageClass: "STANDARD", ForcePathStyle: true}}}
	s := engines.Secrets{Credentials: engines.Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "s3-secret-access-key"}, Obscured: map[string]string{}}
	if crypt {
		d.Encryption = engines.EncryptionCrypt
		s.Encryption = engines.EncryptionSecret{CryptPassword: "crypt-password-1", CryptPassword2: "crypt-password-2"}
		s.Obscured[engines.FieldCryptPassword] = Obscure(testKey, d.ID, engines.FieldCryptPassword, s.Encryption.CryptPassword)
		s.Obscured[engines.FieldCryptPassword2] = Obscure(testKey, d.ID, engines.FieldCryptPassword2, s.Encryption.CryptPassword2)
	}
	return d, s
}

func b2Dest() (engines.Destination, engines.Secrets) {
	return engines.Destination{ID: 4, Kind: engines.B2, Engine: engines.Rclone, Encryption: engines.EncryptionNone,
			Remote: engines.Remote{B2: &engines.B2Remote{Bucket: "media-b2"}}},
		engines.Secrets{Credentials: engines.Credentials{KeyID: "b2-key-id", ApplicationKey: "b2-application-key"}}
}

func sftpDest(password bool) (engines.Destination, engines.Secrets) {
	d := engines.Destination{ID: 5, Kind: engines.SFTP, Engine: engines.Rclone, Encryption: engines.EncryptionNone,
		Remote: engines.Remote{SFTP: &engines.SFTPRemote{Host: "nas.example.org", Port: 2222, User: "backup", Path: "/srv/backup",
			HostKeys: []engines.HostKey{hostKey}}}}
	s := engines.Secrets{Obscured: map[string]string{}}
	if password {
		s.Credentials.Password = "sftp-password-1"
		s.Obscured[engines.FieldPassword] = Obscure(testKey, d.ID, engines.FieldPassword, s.Credentials.Password)
	} else {
		s.Credentials.PrivateKey = testPEM
		s.Credentials.PrivateKeyPassphrase = "key-passphrase-1"
		s.Obscured[engines.FieldPrivateKeyPassphrase] = Obscure(testKey, d.ID, engines.FieldPrivateKeyPassphrase, s.Credentials.PrivateKeyPassphrase)
	}
	return d, s
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func withPrefix(remote string, opts ...string) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = "RCLONE_CONFIG_" + remote + "_" + o
	}
	return out
}

// TestEnvKeySets: the environment of each kind, with and without crypt, is pinned to its exact
// key set, which never holds _SSH, _SERVER_COMMAND, _MD5SUM_COMMAND, _SHA1SUM_COMMAND,
// _SHARED_CREDENTIALS_FILE or _PROFILE (§4.4, §14.1 rclone), and proc.Validate accepts it.
func TestEnvKeySets(t *testing.T) {
	cryptKeys := withPrefix("BKCRYPT", "TYPE", "REMOTE", "PASSWORD", "PASSWORD2", "FILENAME_ENCRYPTION", "DIRECTORY_NAME_ENCRYPTION", "STRICT_NAMES")
	s3Keys := withPrefix("BKDEST", "TYPE", "PROVIDER", "ENDPOINT", "REGION", "ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "ENV_AUTH", "NO_CHECK_BUCKET", "STORAGE_CLASS", "FORCE_PATH_STYLE")
	type tc struct {
		name string
		in   EnvInput
		want []string
	}
	var tests []tc
	for _, crypt := range []bool{false, true} {
		extra := []string{"RCLONE_CONFIG", "RCLONE_BWLIMIT"}
		if crypt {
			extra = append(extra, cryptKeys...)
		}
		d, s := s3Dest(crypt)
		tests = append(tests, tc{"s3 crypt=" + strconv.FormatBool(crypt), EnvInput{Dest: d, Secrets: s, BWLimit: "1024k:off"}, append(slices.Clone(s3Keys), extra...)})
		d, s = b2Dest()
		if crypt {
			d.Encryption = engines.EncryptionCrypt
			s.Obscured = map[string]string{engines.FieldCryptPassword: "x-obscured-1", engines.FieldCryptPassword2: "x-obscured-2"}
		}
		tests = append(tests, tc{"b2 crypt=" + strconv.FormatBool(crypt), EnvInput{Dest: d, Secrets: s, BWLimit: "Mon-08:00,512k:off Mon-23:00,off:off"},
			append(withPrefix("BKDEST", "TYPE", "ACCOUNT", "KEY", "HARD_DELETE"), extra...)})
		d, s = sftpDest(false)
		if crypt {
			d.Encryption = engines.EncryptionCrypt
			s.Obscured[engines.FieldCryptPassword] = "x-obscured-1"
			s.Obscured[engines.FieldCryptPassword2] = "x-obscured-2"
		}
		tests = append(tests, tc{"sftp key crypt=" + strconv.FormatBool(crypt), EnvInput{Dest: d, Secrets: s, KnownHostsPath: "/dev/shm/bunkarr-run/1-a/known_hosts", BWLimit: "1k:1k"},
			append(withPrefix("BKDEST", "TYPE", "HOST", "PORT", "USER", "KEY_PEM", "KEY_FILE_PASS", "KEY_USE_AGENT", "ASK_PASSWORD", "KNOWN_HOSTS_FILE"), extra...)})
	}
	d, s := sftpDest(true)
	tests = append(tests, tc{"sftp password", EnvInput{Dest: d, Secrets: s, KnownHostsPath: "/k"},
		append(withPrefix("BKDEST", "TYPE", "HOST", "PORT", "USER", "PASS", "KEY_USE_AGENT", "ASK_PASSWORD", "KNOWN_HOSTS_FILE"), "RCLONE_CONFIG")})
	d, s = s3Dest(false)
	d.Remote.S3 = &engines.S3Remote{Provider: "AWS", Region: "eu-west-1", Bucket: "b"}
	tests = append(tests, tc{"aws without endpoint or class", EnvInput{Dest: d, Secrets: s},
		append(withPrefix("BKDEST", "TYPE", "PROVIDER", "REGION", "ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "ENV_AUTH", "NO_CHECK_BUCKET", "FORCE_PATH_STYLE"), "RCLONE_CONFIG")})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, err := Env(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(tc.want)
			if got := keys(env); !slices.Equal(got, tc.want) {
				t.Fatalf("keys %v\nwant %v", got, tc.want)
			}
			for k := range env {
				for _, bad := range []string{"_SSH", "_SERVER_COMMAND", "_MD5SUM_COMMAND", "_SHA1SUM_COMMAND", "_SHARED_CREDENTIALS_FILE", "_PROFILE"} {
					if strings.HasSuffix(k, bad) {
						t.Errorf("key %s", k)
					}
				}
			}
			if env["RCLONE_CONFIG"] != "/dev/null" {
				t.Errorf("RCLONE_CONFIG = %q", env["RCLONE_CONFIG"])
			}
			if err := proc.Validate(proc.Cmd{Binary: proc.Rclone, Args: []string{"lsf", Root(tc.in.Dest)}, Env: env}); err != nil {
				t.Errorf("proc.Validate refuses the environment: %v", err)
			}
		})
	}
}

// TestEnvValues: the values that decide where data goes and how it is authenticated.
func TestEnvValues(t *testing.T) {
	d, s := s3Dest(true)
	env, err := Env(EnvInput{Dest: d, Secrets: s})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"RCLONE_CONFIG_BKDEST_TYPE": "s3", "RCLONE_CONFIG_BKDEST_PROVIDER": "Minio", "RCLONE_CONFIG_BKDEST_ENDPOINT": "https://minio.lan:9000",
		"RCLONE_CONFIG_BKDEST_ENV_AUTH": "false", "RCLONE_CONFIG_BKDEST_NO_CHECK_BUCKET": "true", "RCLONE_CONFIG_BKDEST_FORCE_PATH_STYLE": "true",
		"RCLONE_CONFIG_BKDEST_SECRET_ACCESS_KEY": "s3-secret-access-key",
		"RCLONE_CONFIG_BKCRYPT_TYPE":             "crypt", "RCLONE_CONFIG_BKCRYPT_REMOTE": "BKDEST:media/bunkarr/offsite",
		"RCLONE_CONFIG_BKCRYPT_PASSWORD":            s.Obscured[engines.FieldCryptPassword],
		"RCLONE_CONFIG_BKCRYPT_PASSWORD2":           s.Obscured[engines.FieldCryptPassword2],
		"RCLONE_CONFIG_BKCRYPT_FILENAME_ENCRYPTION": "standard", "RCLONE_CONFIG_BKCRYPT_DIRECTORY_NAME_ENCRYPTION": "true",
		"RCLONE_CONFIG_BKCRYPT_STRICT_NAMES": "true",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if clear, _ := Reveal(env["RCLONE_CONFIG_BKCRYPT_PASSWORD"]); clear != "crypt-password-1" {
		t.Errorf("crypt password reveals to %q", clear)
	}
	d, s = sftpDest(false)
	env, err = Env(EnvInput{Dest: d, Secrets: s, KnownHostsPath: "/k"})
	if err != nil {
		t.Fatal(err)
	}
	pem, err := strconv.Unquote(`"` + env["RCLONE_CONFIG_BKDEST_KEY_PEM"] + `"`)
	if err != nil || pem != testPEM || strings.ContainsAny(env["RCLONE_CONFIG_BKDEST_KEY_PEM"], "\n\r") {
		t.Errorf("KEY_PEM does not unquote to the key (rclone's form): %v", err)
	}
	if env["RCLONE_CONFIG_BKDEST_PORT"] != "2222" || env["RCLONE_CONFIG_BKDEST_KEY_USE_AGENT"] != "false" || env["RCLONE_CONFIG_BKDEST_ASK_PASSWORD"] != "false" ||
		env["RCLONE_CONFIG_BKDEST_KNOWN_HOSTS_FILE"] != "/k" {
		t.Errorf("sftp env %v", env)
	}
	if pass, _ := Reveal(env["RCLONE_CONFIG_BKDEST_KEY_FILE_PASS"]); pass != "key-passphrase-1" {
		t.Errorf("KEY_FILE_PASS reveals to %q", pass)
	}
}

func TestEnvRefuses(t *testing.T) {
	sftpNoKeys, s := sftpDest(true)
	sftpNoKeys.Remote.SFTP.HostKeys = nil
	s3, s3s := s3Dest(false)
	s3ca := s3
	caRemote := *s3.Remote.S3
	caRemote.CACert = "-----BEGIN CERTIFICATE-----"
	s3ca.Remote.S3 = &caRemote
	crypt, cs := s3Dest(true)
	delete(cs.Obscured, engines.FieldCryptPassword)
	lf, lfs := s3Dest(false)
	lfRemote := *lf.Remote.S3
	lfRemote.Prefix = "a\nb"
	lf.Remote.S3 = &lfRemote
	pw, pws := sftpDest(true)
	delete(pws.Obscured, engines.FieldPassword)
	tests := []struct {
		name string
		in   EnvInput
		want string
	}{
		{"sftp without host keys", EnvInput{Dest: sftpNoKeys, Secrets: s, KnownHostsPath: "/k"}, "no pinned host keys"},
		{"sftp without a known_hosts file", EnvInput{Dest: func() engines.Destination { d, _ := sftpDest(true); return d }(), Secrets: s}, "no known_hosts file"},
		{"sftp without credentials", EnvInput{Dest: pw, Secrets: engines.Secrets{}, KnownHostsPath: "/k"}, "lack privateKey or password"},
		{"sftp password not obscured", EnvInput{Dest: pw, Secrets: pws, KnownHostsPath: "/k"}, "no obscured password"},
		{"s3 without the secret", EnvInput{Dest: s3, Secrets: engines.Secrets{Credentials: engines.Credentials{AccessKeyID: "a"}}}, "lack secretAccessKey"},
		{"s3 CA without a path", EnvInput{Dest: s3ca, Secrets: s3s}, "ca.pem"},
		{"crypt without its obscured password", EnvInput{Dest: crypt, Secrets: cs}, "no obscured cryptPassword"},
		{"a line break in a value", EnvInput{Dest: lf, Secrets: lfs}, "line break"},
		{"a local destination", EnvInput{Dest: engines.Destination{Kind: engines.Local, Target: "/mnt"}}, "kind"},
		{"a line break in the limit", EnvInput{Dest: s3, Secrets: s3s, BWLimit: "1k\n"}, "line break"},
	}
	for _, tc := range tests {
		if _, err := Env(tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Env = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestRoots(t *testing.T) {
	s3, _ := s3Dest(false)
	s3c, _ := s3Dest(true)
	b2, _ := b2Dest()
	sftp, _ := sftpDest(true)
	tests := []struct {
		d             engines.Destination
		root, storage string
	}{
		{s3, "BKDEST:media/bunkarr/offsite", "BKDEST:media/bunkarr/offsite"},
		{s3c, "BKCRYPT:", "BKDEST:media/bunkarr/offsite"},
		{b2, "BKDEST:media-b2", "BKDEST:media-b2"},
		{sftp, "BKDEST:/srv/backup", "BKDEST:/srv/backup"},
		{engines.Destination{Kind: engines.Local, Target: "/mnt/x"}, "", ""},
	}
	for _, tc := range tests {
		if Root(tc.d) != tc.root || StorageRoot(tc.d) != tc.storage {
			t.Errorf("%s: Root %q StorageRoot %q", tc.d.Kind, Root(tc.d), StorageRoot(tc.d))
		}
	}
}

// TestConfFile: the kit's rclone.conf has exactly two sections with Env's values, leaves out
// storage credentials it was not given, and refuses CR or LF in any value (§5.2).
func TestConfFile(t *testing.T) {
	d, s := sftpDest(false)
	d.Encryption = engines.EncryptionCrypt
	s.Obscured[engines.FieldCryptPassword] = "obscured-crypt-1"
	s.Obscured[engines.FieldCryptPassword2] = "obscured-crypt-2"
	conf, err := ConfFile(d, s, "./bunkarr_known_hosts")
	if err != nil {
		t.Fatal(err)
	}
	want := "[bunkarr-dest]\ntype = sftp\nhost = nas.example.org\nport = 2222\nuser = backup\nkey_pem = " + pemEscape(testPEM) +
		"\nkey_file_pass = " + s.Obscured[engines.FieldPrivateKeyPassphrase] +
		"\nkey_use_agent = false\nask_password = false\nknown_hosts_file = ./bunkarr_known_hosts\n\n" +
		"[bunkarr-crypt]\ntype = crypt\nremote = bunkarr-dest:/srv/backup\npassword = obscured-crypt-1\npassword2 = obscured-crypt-2\n" +
		"filename_encryption = standard\ndirectory_name_encryption = true\nstrict_names = true\n"
	if conf != want {
		t.Errorf("conf:\n%s\nwant:\n%s", conf, want)
	}
	if sections := strings.Count(conf, "\n["); sections != 1 || !strings.HasPrefix(conf, "[") {
		t.Errorf("%d sections", sections+1)
	}

	// Without storage credentials (the kit's default).
	s3, s3s := s3Dest(true)
	s3s.Credentials = engines.Credentials{}
	conf, err = ConfFile(s3, s3s, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(conf, "access_key") || !strings.Contains(conf, "remote = bunkarr-dest:media/bunkarr/offsite") {
		t.Errorf("conf without credentials:\n%s", conf)
	}

	for _, bad := range []func(d *engines.Destination){
		func(d *engines.Destination) { r := *d.Remote.S3; r.Prefix = "x\r\n[evil]"; d.Remote.S3 = &r },
		func(d *engines.Destination) { r := *d.Remote.S3; r.Endpoint = "https://e\n"; d.Remote.S3 = &r },
	} {
		d, s := s3Dest(false)
		bad(&d)
		if _, err := ConfFile(d, s, ""); err == nil {
			t.Error("ConfFile accepted a line break")
		}
	}
}

// TestConfFileRefusesRereadValues: a value rclone's config file parser would read back as
// another value (trimmed white space, a leading ` or """ quote, a %(name)s reference) is
// refused, so a kit restore never lists another prefix through crypt than the jobs wrote under.
// Values that round-trip (TestRealConfFileRoundTrip) are kept as they are.
func TestConfFileRefusesRereadValues(t *testing.T) {
	s3Prefix := func(p string) func(d *engines.Destination) {
		return func(d *engines.Destination) { r := *d.Remote.S3; r.Prefix = p; d.Remote.S3 = &r }
	}
	for name, bad := range map[string]func(d *engines.Destination){
		"prefix trailing space": s3Prefix("backups "),
		"prefix trailing tab":   s3Prefix("backups\t"),
		"prefix trailing NBSP":  s3Prefix("backups\u00a0"),
		"prefix trailing NEL":   s3Prefix("backups\u0085"),
		"prefix variable":       s3Prefix("backups/%(type)s"),
		"b2 prefix": func(d *engines.Destination) {
			d.Kind = engines.B2
			d.Remote.B2 = &engines.B2Remote{Bucket: "b", Prefix: "p "}
		},
		"endpoint leading":    func(d *engines.Destination) { r := *d.Remote.S3; r.Endpoint = " https://minio.lan"; d.Remote.S3 = &r },
		"region backtick":     func(d *engines.Destination) { r := *d.Remote.S3; r.Region = "`us`"; d.Remote.S3 = &r },
		"region triple quote": func(d *engines.Destination) { r := *d.Remote.S3; r.Region = `"""us"""`; d.Remote.S3 = &r },
	} {
		d, s := s3Dest(true)
		bad(&d)
		if d.Kind == engines.B2 {
			s.Credentials = engines.Credentials{}
		}
		if conf, err := ConfFile(d, s, ""); err == nil {
			t.Errorf("%s: ConfFile accepted a value rclone rereads:\n%s", name, conf)
		}
	}
	d, s := sftpDest(true)
	d.Encryption = engines.EncryptionCrypt
	s.Obscured[engines.FieldCryptPassword] = "obscured-crypt-1"
	r := *d.Remote.SFTP
	r.Path = "/srv/backup "
	d.Remote.SFTP = &r
	if _, err := ConfFile(d, s, "./bunkarr_known_hosts"); err == nil {
		t.Error("ConfFile accepted an sftp path with trailing white space")
	}

	// White space, quotes and % inside a value round-trip, so they are kept.
	d, s = s3Dest(true)
	s3 := *d.Remote.S3
	s3.Prefix = "my backups/a`b \"c\" %d ; # = : x"
	d.Remote.S3 = &s3
	conf, err := ConfFile(d, s, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, "\nremote = bunkarr-dest:media/"+s3.Prefix+"\n") {
		t.Errorf("conf:\n%s", conf)
	}
}

// TestEnvCryptRefusesRereadValues: custody can be confirmed without the kit being rendered (a
// typed crypt secret, an attach), so Env itself refuses, for crypt, every value the kit's
// rclone.conf would read back as another: the create's marker write and every job then fail
// instead of uploading under a location ("backups ") no kit can name. Without crypt the values go
// through the environment only (restic's kit exports them too), so they are kept; so is an sftp
// known_hosts path, which the kit replaces with its own.
func TestEnvCryptRefusesRereadValues(t *testing.T) {
	s3Prefix := func(p string) func(d *engines.Destination, s *engines.Secrets) {
		return func(d *engines.Destination, _ *engines.Secrets) { r := *d.Remote.S3; r.Prefix = p; d.Remote.S3 = &r }
	}
	for name, bad := range map[string]func(d *engines.Destination, s *engines.Secrets){
		"prefix trailing space": s3Prefix("backups "),
		"prefix trailing NBSP":  s3Prefix("backups\u00a0"),
		"prefix variable":       s3Prefix("b/%(type)s"),
		"region backtick": func(d *engines.Destination, _ *engines.Secrets) {
			r := *d.Remote.S3
			r.Region = "`us`"
			d.Remote.S3 = &r
		},
		"access key": func(_ *engines.Destination, s *engines.Secrets) { s.Credentials.AccessKeyID = " AKIAEXAMPLE" },
	} {
		d, s := s3Dest(true)
		bad(&d, &s)
		if _, err := Env(EnvInput{Dest: d, Secrets: s}); err == nil {
			t.Errorf("%s: Env accepted a crypt value the kit cannot carry", name)
		}
		d.Encryption = engines.EncryptionNone
		if _, err := Env(EnvInput{Dest: d, Secrets: s}); err != nil {
			t.Errorf("%s without crypt: %v", name, err)
		}
	}

	d, s := sftpDest(true)
	d.Encryption = engines.EncryptionCrypt
	s.Obscured[engines.FieldCryptPassword] = "obscured-crypt-1"
	if _, err := Env(EnvInput{Dest: d, Secrets: s, KnownHostsPath: "/run dir/%(type)s/known_hosts "}); err != nil {
		t.Errorf("Env refused a run directory known_hosts path the kit does not carry: %v", err)
	}
	r := *d.Remote.SFTP
	r.Path = "/srv/backup "
	d.Remote.SFTP = &r
	if _, err := Env(EnvInput{Dest: d, Secrets: s, KnownHostsPath: "/k"}); err == nil {
		t.Error("Env accepted a crypt sftp path with trailing white space")
	}
}
