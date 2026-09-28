package destinations

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"github.com/sl0wz3r/bunkarr/internal/engines"
)

// Encryption at create (phase4.md S21, §5, §12).

// minUserSecretLen is the shortest encryption secret a user may type (S21).
const minUserSecretLen = 16

// generatedSecretBytes is how many random bytes a generated secret has (base64url, S21).
const generatedSecretBytes = 32

// EncryptionInput is Input.encryption: mode restic (restic), crypt (rclone's default) or none
// (rclone, only with AcceptUnencrypted); Generate (default true) or the user's Secret, which an
// attach needs. Secret2 is rclone crypt's password2 (the salt) with a user Secret: a crypt remote
// Bunkarr created has a generated password2, which its recovery kit prints, and re-attaching it
// (§5.3) needs both; without Secret2 password2 stays rclone's default. Like the credential inputs
// it never shows a secret (String, GoString, LogValue, MarshalJSON).
type EncryptionInput struct {
	Mode              engines.EncryptionMode `json:"mode"`
	Generate          *bool                  `json:"generate"`
	AcceptUnencrypted bool                   `json:"acceptUnencrypted"`
	Secret            string                 `json:"secret"`
	Secret2           string                 `json:"secret2"`
}

// String implements fmt.Stringer without the secret.
func (e EncryptionInput) String() string {
	return fmt.Sprintf("destinations.EncryptionInput{mode: %s, secret set: %t, secret2 set: %t}", e.Mode, e.Secret != "", e.Secret2 != "")
}

// GoString implements fmt.GoStringer without the secret.
func (e EncryptionInput) GoString() string { return e.String() }

// LogValue implements slog.LogValuer without the secret.
func (e EncryptionInput) LogValue() slog.Value {
	return slog.GroupValue(slog.String("mode", string(e.Mode)), slog.Bool("secretSet", e.Secret != ""),
		slog.Bool("secret2Set", e.Secret2 != ""), slog.Bool("acceptUnencrypted", e.AcceptUnencrypted))
}

// MarshalJSON implements json.Marshaler without the secret.
func (e EncryptionInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Mode              engines.EncryptionMode `json:"mode"`
		Generate          *bool                  `json:"generate,omitempty"`
		AcceptUnencrypted bool                   `json:"acceptUnencrypted"`
		SecretSet         bool                   `json:"secretSet"`
		Secret2Set        bool                   `json:"secret2Set"`
	}{e.Mode, e.Generate, e.AcceptUnencrypted, e.Secret != "", e.Secret2 != ""})
}

// keeps reports whether an update's encryption input leaves mode as it is (an update may resend
// the mode, and nothing else).
func (e *EncryptionInput) keeps(mode engines.EncryptionMode) bool {
	return e == nil || ((e.Mode == "" || e.Mode == mode) && e.Generate == nil && !e.AcceptUnencrypted && e.Secret == "" && e.Secret2 == "")
}

// encryptionPlan is what a create stores: the mode and, when there is one, the secret and who
// chose it.
type encryptionPlan struct {
	mode   engines.EncryptionMode
	secret engines.EncryptionSecret
	origin string // OriginGenerated, OriginUser or ""
}

// planEncryption decides the encryption of a new destination of engine and kind (S21): filecopy
// has none; restic always encrypts; rclone uses crypt unless mode none and AcceptUnencrypted. A
// secret is generated unless the user gives one (attach needs the existing one).
func planEncryption(engine string, kind engines.DestKind, in *EncryptionInput, attach bool) (encryptionPlan, error) {
	var e EncryptionInput
	if in != nil {
		e = *in
	}
	switch engine {
	case EngineFilecopy:
		if e.Mode != "" && e.Mode != engines.EncryptionNone || e.Secret != "" || e.Secret2 != "" || e.Generate != nil {
			return encryptionPlan{}, ValidationError("encryption: a filecopy destination stores plain files (use restic or rclone crypt to encrypt)")
		}
		return encryptionPlan{mode: engines.EncryptionNone}, nil
	case EngineRestic:
		if e.Mode == "" {
			e.Mode = engines.EncryptionRestic
		}
		if e.Mode != engines.EncryptionRestic {
			return encryptionPlan{}, ValidationError(fmt.Sprintf("encryption.mode %q: a restic repository is always encrypted (mode restic)", e.Mode))
		}
		if e.Secret2 != "" {
			return encryptionPlan{}, ValidationError("encryption.secret2 is rclone crypt's password2: a restic repository has one password (encryption.secret)")
		}
	case EngineRclone:
		if e.Mode == "" {
			e.Mode = engines.EncryptionCrypt
		}
		switch e.Mode {
		case engines.EncryptionCrypt:
		case engines.EncryptionNone:
			if !e.AcceptUnencrypted {
				return encryptionPlan{}, ValidationError("encryption.mode none stores plain files off-site: set encryption.acceptUnencrypted to confirm")
			}
			if e.Secret != "" || e.Secret2 != "" || e.Generate != nil && *e.Generate {
				return encryptionPlan{}, ValidationError("encryption: mode none has no secret")
			}
			return encryptionPlan{mode: engines.EncryptionNone}, nil
		default:
			return encryptionPlan{}, ValidationError(fmt.Sprintf("encryption.mode %q: use crypt (the default) or none", e.Mode))
		}
	default:
		return encryptionPlan{}, ValidationError(fmt.Sprintf("engine %q: use filecopy, restic or rclone", engine))
	}
	if e.AcceptUnencrypted {
		return encryptionPlan{}, ValidationError("encryption.acceptUnencrypted applies only to mode none")
	}
	generate := e.Secret == ""
	if e.Generate != nil {
		if *e.Generate && e.Secret != "" {
			return encryptionPlan{}, ValidationError("encryption: give a secret or generate one, not both")
		}
		if !*e.Generate && e.Secret == "" {
			return encryptionPlan{}, ValidationError("encryption.secret is required when generate is false")
		}
	}
	if generate && e.Secret2 != "" {
		return encryptionPlan{}, ValidationError("encryption.secret2 (crypt's password2) needs encryption.secret (its password)")
	}
	if generate {
		if attach {
			return encryptionPlan{}, ValidationError("encryption.secret: attach needs the existing repository's or crypt remote's password")
		}
		p := encryptionPlan{mode: e.Mode, origin: OriginGenerated}
		if e.Mode == engines.EncryptionRestic {
			p.secret.ResticPassword = generateSecret()
		} else {
			p.secret.CryptPassword, p.secret.CryptPassword2 = generateSecret(), generateSecret()
		}
		return p, nil
	}
	if err := checkUserSecret("encryption.secret", e.Secret); err != nil {
		return encryptionPlan{}, err
	}
	p := encryptionPlan{mode: e.Mode, origin: OriginUser}
	if e.Mode == engines.EncryptionRestic {
		p.secret.ResticPassword = e.Secret
		return p, nil
	}
	// crypt's password2 (the salt) is Secret2 when it is given (a crypt remote Bunkarr created
	// with a generated password2, attached again from its kit); otherwise it stays rclone's
	// default, so an existing crypt remote made with a password only is attached with that one.
	if e.Secret2 != "" {
		if err := checkUserSecret("encryption.secret2", e.Secret2); err != nil {
			return encryptionPlan{}, err
		}
	}
	p.secret.CryptPassword, p.secret.CryptPassword2 = e.Secret, e.Secret2
	return p, nil
}

// checkUserSecret checks a secret the user typed in field (S21): at least 16 characters, no
// leading or trailing whitespace (restic trims its password file, so the kit would print another
// secret) and no control characters.
func checkUserSecret(field, s string) error {
	switch {
	case len([]rune(s)) < minUserSecretLen:
		return ValidationError(fmt.Sprintf("%s: at least %d characters", field, minUserSecretLen))
	case strings.TrimSpace(s) != s:
		return ValidationError(field + ": no leading or trailing whitespace (restic trims it)")
	case strings.ContainsFunc(s, unicode.IsControl):
		return ValidationError(field + " contains control characters")
	}
	return nil
}

// generateSecret returns 32 random bytes as base64url (S21).
func generateSecret() string {
	b := make([]byte, generatedSecretBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return base64.RawURLEncoding.EncodeToString(b)
}

// newEngineTag returns a restic destination's random tag: 16 bytes in hex (D32).
func newEngineTag() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// secretText is the encryption secret as the check code hashes it (§5.2): the restic password,
// or the crypt password and password2 joined by NUL.
func secretText(e engines.EncryptionSecret) string {
	if e.ResticPassword != "" {
		return e.ResticPassword
	}
	return e.CryptPassword + "\x00" + e.CryptPassword2
}
