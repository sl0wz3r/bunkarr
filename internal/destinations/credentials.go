package destinations

import (
	"bytes"
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// Storage credentials (phase4.md §4.3, S22). They are sealed in destinations.credentials (AAD
// "destination:<id>:credentials"), never returned (the API shows hasCredentials), merged field by
// field on update inside the write transaction, and registered with the log redaction.

// minPasswordLen is the shortest SFTP password or key passphrase: the redaction registry ignores
// shorter values, so a shorter secret could reach a log (S22, open question 10).
const minPasswordLen = 8

// maxCredentialLen bounds one credential value (a PEM private key is a few KiB).
const maxCredentialLen = 64 << 10

// CredentialsInput is the write-only credentials of Input and TestInput. It decodes strictly:
// an unknown field, a field sent as null or "", and {} are refused (ValidationError from Fields,
// or a decode error), because a field that is sent replaces the stored one (§4.3). Like
// engines.Credentials, whose methods it has, it never shows a value.
type CredentialsInput struct {
	engines.Credentials
	// invalid is why the JSON was refused ("" when it was not).
	invalid string
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *CredentialsInput) UnmarshalJSON(b []byte) error {
	*c = CredentialsInput{}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return errors.New("credentials: an object is required")
	}
	if raw == nil {
		c.invalid = "credentials: an object with at least one field is required"
		return nil
	}
	fields := map[string]*string{
		engines.FieldPrivateKey: &c.PrivateKey, engines.FieldPrivateKeyPassphrase: &c.PrivateKeyPassphrase,
		engines.FieldPassword: &c.Password, engines.FieldAccessKeyID: &c.AccessKeyID,
		engines.FieldSecretAccessKey: &c.SecretAccessKey, engines.FieldKeyID: &c.KeyID, engines.FieldApplicationKey: &c.ApplicationKey,
	}
	names := make([]string, 0, len(raw))
	for k := range raw {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		dst, ok := fields[k]
		if !ok {
			return fmt.Errorf("credentials: unknown field %q", k)
		}
		v := bytes.TrimSpace(raw[k])
		if string(v) == "null" {
			c.invalid = fmt.Sprintf("credentials.%s: a field that is sent replaces the stored one; null is not a value (leave it out to keep it)", k)
			continue
		}
		if err := json.Unmarshal(v, dst); err != nil {
			return fmt.Errorf("credentials.%s: a string is required", k)
		}
		if *dst == "" {
			c.invalid = fmt.Sprintf("credentials.%s: an empty value is not allowed (leave the field out to keep the stored one)", k)
		}
	}
	if len(raw) == 0 {
		c.invalid = "credentials: {} changes nothing; send the fields to replace, or leave credentials out"
	}
	return nil
}

// check refuses what UnmarshalJSON found invalid, and an empty set.
func (c *CredentialsInput) check() error {
	if c == nil {
		return nil
	}
	if c.invalid != "" {
		return ValidationError(c.invalid)
	}
	if c.Empty() {
		return ValidationError("credentials: {} changes nothing; send the fields to replace, or leave credentials out")
	}
	return nil
}

// kindFields are the credential fields of each remote kind.
var kindFields = map[engines.DestKind][]string{
	engines.SFTP: {engines.FieldPrivateKey, engines.FieldPrivateKeyPassphrase, engines.FieldPassword},
	engines.S3:   {engines.FieldAccessKeyID, engines.FieldSecretAccessKey},
	engines.B2:   {engines.FieldKeyID, engines.FieldApplicationKey},
}

// checkCredentialFields refuses fields of another kind and malformed values in a (possibly
// partial) credentials input of kind.
func checkCredentialFields(kind engines.DestKind, c engines.Credentials) error {
	allowed := kindFields[kind]
	for f, set := range c.Fields() {
		if set && !slices.Contains(allowed, f) {
			if len(allowed) == 0 {
				return ValidationError(fmt.Sprintf("credentials.%s: a %s destination has no storage credentials", f, kind))
			}
			return ValidationError(fmt.Sprintf("credentials.%s is not a credential of a %s destination (use %s)", f, kind, strings.Join(allowed, ", ")))
		}
	}
	for _, f := range [][2]string{{engines.FieldPrivateKey, c.PrivateKey}, {engines.FieldPrivateKeyPassphrase, c.PrivateKeyPassphrase},
		{engines.FieldPassword, c.Password}, {engines.FieldAccessKeyID, c.AccessKeyID}, {engines.FieldSecretAccessKey, c.SecretAccessKey},
		{engines.FieldKeyID, c.KeyID}, {engines.FieldApplicationKey, c.ApplicationKey}} {
		if f[1] == "" {
			continue
		}
		if len(f[1]) > maxCredentialLen {
			return ValidationError(fmt.Sprintf("credentials.%s is too long", f[0]))
		}
		if f[0] != engines.FieldPrivateKey && strings.ContainsFunc(f[1], unicode.IsControl) {
			return ValidationError(fmt.Sprintf("credentials.%s contains control characters", f[0]))
		}
	}
	for _, f := range [][2]string{{engines.FieldPassword, c.Password}, {engines.FieldPrivateKeyPassphrase, c.PrivateKeyPassphrase}} {
		if f[1] != "" && len(f[1]) < minPasswordLen {
			return ValidationError(fmt.Sprintf("credentials.%s: at least %d characters (a shorter secret could not be kept out of the logs)", f[0], minPasswordLen))
		}
	}
	if c.PrivateKey != "" {
		if strings.ContainsRune(c.PrivateKey, 0) || !strings.Contains(c.PrivateKey, "-----BEGIN ") {
			return ValidationError("credentials.privateKey: a PEM private key (OpenSSH or PKCS#8) is required")
		}
	}
	return nil
}

// checkCompleteCredentials checks a complete credential set of kind (a create, or the merge of an
// update): the fields the engine needs are there, and an SFTP key parses (with its passphrase).
func checkCompleteCredentials(kind engines.DestKind, c engines.Credentials) error {
	if err := checkCredentialFields(kind, c); err != nil {
		return err
	}
	switch kind {
	case engines.S3:
		if c.AccessKeyID == "" || c.SecretAccessKey == "" {
			return ValidationError("credentials: an S3 destination needs accessKeyId and secretAccessKey")
		}
	case engines.B2:
		if c.KeyID == "" || c.ApplicationKey == "" {
			return ValidationError("credentials: a B2 destination needs keyId and applicationKey")
		}
	case engines.SFTP:
		switch {
		case c.PrivateKey == "" && c.Password == "":
			return ValidationError("credentials: an SFTP destination needs privateKey or password")
		case c.PrivateKey == "" && c.PrivateKeyPassphrase != "":
			return ValidationError("credentials.privateKeyPassphrase needs privateKey")
		}
		if c.PrivateKey != "" {
			if err := parsePrivateKey(c.PrivateKey, c.PrivateKeyPassphrase); err != nil {
				return err
			}
		}
	case engines.Local:
		if !c.Empty() {
			return ValidationError("credentials: a local destination has no storage credentials")
		}
	}
	return nil
}

// parsePrivateKey checks that an SFTP private key parses (OpenSSH or PKCS#8 PEM), with its
// passphrase when it is encrypted. Nothing of the key reaches an error text.
func parsePrivateKey(key, passphrase string) error {
	if block, _ := pem.Decode([]byte(key)); block == nil {
		return ValidationError("credentials.privateKey: not a PEM block")
	} else if block.Type == "PRIVATE KEY" {
		if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
			return ValidationError("credentials.privateKey: the PKCS#8 key does not parse")
		}
		return nil
	}
	var err error
	if passphrase != "" {
		_, err = ssh.ParseRawPrivateKeyWithPassphrase([]byte(key), []byte(passphrase))
	} else {
		_, err = ssh.ParseRawPrivateKey([]byte(key))
	}
	var missing *ssh.PassphraseMissingError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &missing):
		return ValidationError("credentials.privateKeyPassphrase: the private key is encrypted; its passphrase is required")
	case errors.Is(err, x509.IncorrectPasswordError):
		return ValidationError("credentials.privateKeyPassphrase does not decrypt the private key")
	}
	return ValidationError("credentials.privateKey does not parse (OpenSSH or PKCS#8 PEM is required)")
}

// mergeCredentials returns stored with every field that update sets replaced (§4.3). An SFTP
// login is one set, not three fields: an update that sends privateKey or password replaces the
// whole set (privateKey, privateKeyPassphrase, password), so a key rotated to an unencrypted one
// drops the old passphrase, and a switch between key and password drops the other method
// instead of keeping it sealed, sending it first and printing it in a kit with the storage
// credentials. A null or "" field is refused (CredentialsInput), so this is the one way to remove
// a stored SFTP field. An update that sends only privateKeyPassphrase keeps the stored key, and
// one that sends a new encrypted privateKey without a passphrase keeps the stored passphrase when
// it decrypts the new key (the form leaves a stored field empty to keep it).
func mergeCredentials(kind engines.DestKind, stored, update engines.Credentials) engines.Credentials {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	if kind == engines.SFTP && (update.PrivateKey != "" || update.Password != "") {
		var keep string
		if update.PrivateKey != "" && update.PrivateKeyPassphrase == "" && opensEncryptedKey(update.PrivateKey, stored.PrivateKeyPassphrase) {
			keep = stored.PrivateKeyPassphrase
		}
		stored.PrivateKey, stored.PrivateKeyPassphrase, stored.Password = "", keep, ""
	}
	set(&stored.PrivateKey, update.PrivateKey)
	set(&stored.PrivateKeyPassphrase, update.PrivateKeyPassphrase)
	set(&stored.Password, update.Password)
	set(&stored.AccessKeyID, update.AccessKeyID)
	set(&stored.SecretAccessKey, update.SecretAccessKey)
	set(&stored.KeyID, update.KeyID)
	set(&stored.ApplicationKey, update.ApplicationKey)
	return stored
}

// opensEncryptedKey reports whether key is an encrypted SFTP private key that passphrase decrypts.
func opensEncryptedKey(key, passphrase string) bool {
	if passphrase == "" {
		return false
	}
	var missing *ssh.PassphraseMissingError
	if _, err := ssh.ParseRawPrivateKey([]byte(key)); !errors.As(err, &missing) {
		return false
	}
	_, err := ssh.ParseRawPrivateKeyWithPassphrase([]byte(key), []byte(passphrase))
	return err == nil
}

// credentialsAAD is the AAD of destination id's sealed credentials column (ADR 0003).
func credentialsAAD(id int64) string {
	return "destination:" + strconv.FormatInt(id, 10) + ":credentials"
}

// secretAAD is the AAD of destination id's sealed encryption_secret column.
func secretAAD(id int64) string {
	return "destination:" + strconv.FormatInt(id, 10) + ":encryptionSecret"
}

// secretOwner is the logging.SetSecrets owner of a destination's secrets.
func secretOwner(id int64) string { return "destination:" + strconv.FormatInt(id, 10) }

// errNoKeyring means a store without a keyring was asked to seal or open a secret.
var errNoKeyring = errors.New("destinations: no keyring configured (engine destinations need bunkarr.key)")

// sealCredentials seals c for destination id ("" when c is empty).
func (s *Store) sealCredentials(id int64, c engines.Credentials) (string, error) {
	if c.Empty() {
		return "", nil
	}
	if s.opts.Keyring == nil {
		return "", errNoKeyring
	}
	plain, err := c.PlainJSON()
	if err != nil {
		return "", fmt.Errorf("encode credentials: %w", err)
	}
	return s.opts.Keyring.Seal(string(plain), credentialsAAD(id))
}

// openCredentials unseals destination id's credentials column ("" is none).
func (s *Store) openCredentials(id int64, sealed string) (engines.Credentials, error) {
	var c engines.Credentials
	if sealed == "" {
		return c, nil
	}
	if s.opts.Keyring == nil {
		return c, errNoKeyring
	}
	plain, err := s.opts.Keyring.Open(sealed, credentialsAAD(id))
	if err != nil {
		return c, fmt.Errorf("destination %d: credentials: %w", id, err)
	}
	type plainCreds engines.Credentials
	var p plainCreds
	if err := json.Unmarshal([]byte(plain), &p); err != nil {
		return c, fmt.Errorf("destination %d: credentials: %w", id, err)
	}
	return engines.Credentials(p), nil
}

// sealSecret seals an encryption secret for destination id (NULL when it is empty).
func (s *Store) sealSecret(id int64, e engines.EncryptionSecret) (sql.NullString, error) {
	if len(e.Fields()) == 0 {
		return sql.NullString{}, nil
	}
	if s.opts.Keyring == nil {
		return sql.NullString{}, errNoKeyring
	}
	plain, err := e.PlainJSON()
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode the encryption secret: %w", err)
	}
	sealed, err := s.opts.Keyring.Seal(string(plain), secretAAD(id))
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: sealed, Valid: true}, nil
}

// openSecret unseals destination id's encryption_secret column (NULL is none).
func (s *Store) openSecret(id int64, sealed sql.NullString) (engines.EncryptionSecret, error) {
	var e engines.EncryptionSecret
	if !sealed.Valid || sealed.String == "" {
		return e, nil
	}
	if s.opts.Keyring == nil {
		return e, errNoKeyring
	}
	plain, err := s.opts.Keyring.Open(sealed.String, secretAAD(id))
	if err != nil {
		return e, fmt.Errorf("destination %d: encryption secret: %w", id, err)
	}
	type plainSecret engines.EncryptionSecret
	var p plainSecret
	if err := json.Unmarshal([]byte(plain), &p); err != nil {
		return e, fmt.Errorf("destination %d: encryption secret: %w", id, err)
	}
	return engines.EncryptionSecret(p), nil
}

// obscureKey is Options.ObscureKey, or the key derived from the keyring (rclone.ObscureKeyInfo).
func (s *Store) obscureKey() []byte {
	if len(s.opts.ObscureKey) > 0 {
		return s.opts.ObscureKey
	}
	if s.opts.Keyring != nil {
		return s.opts.Keyring.Derive(rclone.ObscureKeyInfo, 32)
	}
	return nil
}

// secretsOf builds the Secrets of destination id: the credentials, the encryption secret, and the
// deterministic obscured form of every password (rclone.Obscure, §4.4).
func (s *Store) secretsOf(id int64, c engines.Credentials, e engines.EncryptionSecret) engines.Secrets {
	sec := engines.Secrets{Credentials: c, Encryption: e, Obscured: map[string]string{}}
	key := s.obscureKey()
	for field, v := range obscurable(c, e) {
		if key != nil {
			sec.Obscured[field] = rclone.Obscure(key, id, field, v)
		} else {
			sec.Obscured[field] = rclone.ObscureRandom(v)
		}
	}
	return sec
}

// requestSecrets builds the Secrets of a Test (no destination id yet): each password is obscured
// once with a random IV, and the redaction of that request takes both forms (Values).
func requestSecrets(c engines.Credentials, e engines.EncryptionSecret) engines.Secrets {
	sec := engines.Secrets{Credentials: c, Encryption: e, Obscured: map[string]string{}}
	for field, v := range obscurable(c, e) {
		sec.Obscured[field] = rclone.ObscureRandom(v)
	}
	return sec
}

// obscurable returns the set values of engines.ObscuredFields by field.
func obscurable(c engines.Credentials, e engines.EncryptionSecret) map[string]string {
	out := map[string]string{}
	for field, v := range map[string]string{
		engines.FieldPassword: c.Password, engines.FieldPrivateKeyPassphrase: c.PrivateKeyPassphrase,
		engines.FieldCryptPassword: e.CryptPassword, engines.FieldCryptPassword2: e.CryptPassword2,
	} {
		if v != "" && slices.Contains(engines.ObscuredFields, field) {
			out[field] = v
		}
	}
	return out
}

// registerSecrets makes the redaction hold destination id's secrets in every form (clear,
// obscured, JSON-escaped, PEM lines): logging.SetSecrets("destination:<id>", …).
func registerSecrets(id int64, sec engines.Secrets) {
	logging.SetSecrets(secretOwner(id), sec.Values()...)
}

// RegisterSecrets registers the secrets of every engine destination with the log redaction
// (start-up; each create and update registers its own). A destination whose secrets cannot be
// opened (another bunkarr.key) is reported in the joined error; the others are registered.
func (s *Store) RegisterSecrets(ctx context.Context) error {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT id FROM destinations WHERE engine <> 'filecopy' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("register destination secrets: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("register destination secrets: %w", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("register destination secrets: %w", err)
	}
	var errs []error
	for _, id := range ids {
		_, sec, err := s.SecretsFor(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		registerSecrets(id, sec)
	}
	return errors.Join(errs...)
}

// SecretsFor returns an engine destination's location and its secrets, read from ONE row in one
// query (S8 for engines, §4.3): a job builds a child's environment only from this pair, so a
// stored secret goes only where it was saved. There is no call that returns credentials without
// their location. A filecopy destination returns ErrNotEngine.
func (s *Store) SecretsFor(ctx context.Context, id int64) (engines.Destination, engines.Secrets, error) {
	var (
		row                 destRow
		settings, retention string
		encryptionSecret    sql.NullString
	)
	err := s.db.Reader().QueryRowContext(ctx, `SELECT `+selectCols+`, encryption_secret FROM destinations WHERE id = ?`, id).
		Scan(append(row.targets(&settings, &retention), &encryptionSecret)...)
	if errors.Is(err, sql.ErrNoRows) {
		return engines.Destination{}, engines.Secrets{}, ErrNotFound
	}
	if err != nil {
		return engines.Destination{}, engines.Secrets{}, fmt.Errorf("read destination %d: %w", id, err)
	}
	d, err := row.destination(settings, retention)
	if err != nil {
		return engines.Destination{}, engines.Secrets{}, err
	}
	if !d.IsEngine() {
		return engines.Destination{}, engines.Secrets{}, ErrNotEngine
	}
	c, err := s.openCredentials(id, row.credentials)
	if err != nil {
		return engines.Destination{}, engines.Secrets{}, err
	}
	e, err := s.openSecret(id, encryptionSecret)
	if err != nil {
		return engines.Destination{}, engines.Secrets{}, err
	}
	return d.EngineDestination(), s.secretsOf(id, c, e), nil
}

// redactErr returns err with values (and every registered secret) redacted from its text, keeping
// errors.Is for the sentinel it wraps.
func redactErr(err error, values []string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	red := logging.RedactValues(msg, values...)
	if red == msg {
		return err
	}
	return &redactedError{msg: red, err: err}
}

// redactedError is an error whose text had a secret redacted; it unwraps to the original (whose
// text must then not be printed).
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }

// Is reports whether the original error matches target, without exposing it through Unwrap.
func (e *redactedError) Is(target error) bool { return errors.Is(e.err, target) }
