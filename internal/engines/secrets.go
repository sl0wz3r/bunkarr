package engines

import (
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// Credential and secret field names: the camelCase JSON names of the API, of the sealed columns,
// of hasCredentials and of Secrets.Obscured (§4.3).
const (
	FieldPrivateKey           = "privateKey"
	FieldPrivateKeyPassphrase = "privateKeyPassphrase"
	FieldPassword             = "password"
	FieldAccessKeyID          = "accessKeyId"
	FieldSecretAccessKey      = "secretAccessKey"
	FieldKeyID                = "keyId"
	FieldApplicationKey       = "applicationKey"
	FieldResticPassword       = "resticPassword"
	FieldCryptPassword        = "cryptPassword"
	FieldCryptPassword2       = "cryptPassword2"
)

// ObscuredFields are the fields whose rclone-obscured form Secrets.Obscured holds: the values
// rclone takes obscured (§4.4).
var ObscuredFields = []string{FieldPassword, FieldPrivateKeyPassphrase, FieldCryptPassword, FieldCryptPassword2}

// Credentials are a destination's storage credentials (destinations.credentials, sealed; §4.3):
// sftp privateKey (PEM) with an optional privateKeyPassphrase, or password; s3 accessKeyId and
// secretAccessKey; b2 keyId and applicationKey. The JSON tags decode API input; encoding shows
// only which fields are set (MarshalJSON), and PlainJSON is the one form with values, for
// sealing. String, GoString and LogValue never show a value (S22).
type Credentials struct {
	PrivateKey           string `json:"privateKey,omitempty"`
	PrivateKeyPassphrase string `json:"privateKeyPassphrase,omitempty"`
	Password             string `json:"password,omitempty"`
	AccessKeyID          string `json:"accessKeyId,omitempty"`
	SecretAccessKey      string `json:"secretAccessKey,omitempty"`
	KeyID                string `json:"keyId,omitempty"`
	ApplicationKey       string `json:"applicationKey,omitempty"`
}

// named returns the fields in a fixed order with their names.
func (c Credentials) named() [][2]string {
	return [][2]string{
		{FieldPrivateKey, c.PrivateKey},
		{FieldPrivateKeyPassphrase, c.PrivateKeyPassphrase},
		{FieldPassword, c.Password},
		{FieldAccessKeyID, c.AccessKeyID},
		{FieldSecretAccessKey, c.SecretAccessKey},
		{FieldKeyID, c.KeyID},
		{FieldApplicationKey, c.ApplicationKey},
	}
}

// Fields reports which fields are set: the API's hasCredentials.
func (c Credentials) Fields() map[string]bool { return setFields(c.named()) }

// Empty reports whether no field is set.
func (c Credentials) Empty() bool { return len(c.Fields()) == 0 }

// String implements fmt.Stringer without any value.
func (c Credentials) String() string { return "engines.Credentials{" + setList(c.named()) + "}" }

// GoString implements fmt.GoStringer without any value.
func (c Credentials) GoString() string { return c.String() }

// LogValue implements slog.LogValuer: the names of the set fields.
func (c Credentials) LogValue() slog.Value {
	return slog.GroupValue(slog.String("set", setList(c.named())))
}

// MarshalJSON implements json.Marshaler as {<field>: true} for the set fields.
func (c Credentials) MarshalJSON() ([]byte, error) { return json.Marshal(c.Fields()) }

// PlainJSON is the JSON with the values, for sealing into destinations.credentials only.
func (c Credentials) PlainJSON() ([]byte, error) {
	type plain Credentials
	return json.Marshal(plain(c))
}

// EncryptionSecret is a destination's encryption secret (destinations.encryption_secret, sealed,
// written once; S21): the restic repository password, or the crypt password and password2. Like
// Credentials, it never shows a value except through PlainJSON.
type EncryptionSecret struct {
	ResticPassword string `json:"resticPassword,omitempty"`
	CryptPassword  string `json:"cryptPassword,omitempty"`
	CryptPassword2 string `json:"cryptPassword2,omitempty"`
}

func (e EncryptionSecret) named() [][2]string {
	return [][2]string{
		{FieldResticPassword, e.ResticPassword},
		{FieldCryptPassword, e.CryptPassword},
		{FieldCryptPassword2, e.CryptPassword2},
	}
}

// Fields reports which fields are set.
func (e EncryptionSecret) Fields() map[string]bool { return setFields(e.named()) }

// String implements fmt.Stringer without any value.
func (e EncryptionSecret) String() string {
	return "engines.EncryptionSecret{" + setList(e.named()) + "}"
}

// GoString implements fmt.GoStringer without any value.
func (e EncryptionSecret) GoString() string { return e.String() }

// LogValue implements slog.LogValuer: the names of the set fields.
func (e EncryptionSecret) LogValue() slog.Value {
	return slog.GroupValue(slog.String("set", setList(e.named())))
}

// MarshalJSON implements json.Marshaler as {<field>: true} for the set fields.
func (e EncryptionSecret) MarshalJSON() ([]byte, error) { return json.Marshal(e.Fields()) }

// PlainJSON is the JSON with the values, for sealing into destinations.encryption_secret only.
func (e EncryptionSecret) PlainJSON() ([]byte, error) {
	type plain EncryptionSecret
	return json.Marshal(plain(e))
}

// Secrets are a destination's unsealed secrets for one job or one Test/Create, only in memory:
// the storage credentials, the encryption secret, and the rclone-obscured form of each value in
// ObscuredFields that is set (field -> obscured; rclone.Obscure with the destination's id, or
// rclone.ObscureRandom for a Test or Create). String, GoString, LogValue and MarshalJSON never
// show a value (S22).
type Secrets struct {
	Credentials Credentials
	Encryption  EncryptionSecret
	Obscured    map[string]string
}

// String implements fmt.Stringer without any value.
func (s Secrets) String() string {
	return "engines.Secrets{credentials: " + setList(s.Credentials.named()) +
		"; encryption: " + setList(s.Encryption.named()) +
		"; obscured: " + strings.Join(slices.Sorted(maps.Keys(s.Obscured)), ",") + "}"
}

// GoString implements fmt.GoStringer without any value.
func (s Secrets) GoString() string { return s.String() }

// LogValue implements slog.LogValuer: the names of the set fields.
func (s Secrets) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("credentials", setList(s.Credentials.named())),
		slog.String("encryption", setList(s.Encryption.named())),
		slog.String("obscured", strings.Join(slices.Sorted(maps.Keys(s.Obscured)), ",")),
	)
}

// MarshalJSON implements json.Marshaler with the set field names only.
func (s Secrets) MarshalJSON() ([]byte, error) {
	obscured := slices.Sorted(maps.Keys(s.Obscured))
	if obscured == nil {
		obscured = []string{}
	}
	return json.Marshal(struct {
		Credentials map[string]bool `json:"credentials"`
		Encryption  map[string]bool `json:"encryption"`
		Obscured    []string        `json:"obscured"`
	}{s.Credentials.Fields(), s.Encryption.Fields(), obscured})
}

// Values returns every form in which one of these secrets can appear in a command's output or
// arguments: each clear value, each obscured value, their JSON-escaped and Go-quoted forms
// (logging.EscapedForms; the Go-quoted private key is what the KEY_PEM variable carries), and each
// line of a PEM private key. proc.Cmd.Redact and logging.SetSecrets take this list (§4.3, S22).
// Values shorter than 8 bytes are kept; the redaction ignores them.
func (s Secrets) Values() []string {
	var out []string
	add := func(v string) {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	var values []string
	for _, f := range s.Credentials.named() {
		values = append(values, f[1])
	}
	for _, f := range s.Encryption.named() {
		values = append(values, f[1])
	}
	for _, k := range slices.Sorted(maps.Keys(s.Obscured)) {
		values = append(values, s.Obscured[k])
	}
	for _, v := range values {
		if v == "" {
			continue
		}
		add(v)
		for _, e := range logging.EscapedForms(v) {
			add(e)
		}
	}
	for _, line := range strings.Split(s.Credentials.PrivateKey, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") && !strings.Contains(line, ":") {
			add(line)
		}
	}
	return out
}

func setFields(named [][2]string) map[string]bool {
	out := map[string]bool{}
	for _, f := range named {
		if f[1] != "" {
			out[f[0]] = true
		}
	}
	return out
}

func setList(named [][2]string) string {
	var names []string
	for _, f := range named {
		if f[1] != "" {
			names = append(names, f[0])
		}
	}
	return strings.Join(names, ",")
}
