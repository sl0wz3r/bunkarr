package rclone

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
)

// ObscureKeyInfo is the HKDF info of the key Obscure derives its IVs with:
// Keyring.Derive(ObscureKeyInfo, 32) (§4.4).
const ObscureKeyInfo = "bunkarr rclone obscure v1"

// rcloneObscureKey is rclone's fixed obscure key (fs/config/obscure). Obscuring is not
// encryption: anyone can reveal the value. It keeps passwords out of plain sight in rclone's
// environment and config, as rclone requires for these options.
var rcloneObscureKey = []byte{
	0x9c, 0x93, 0x5b, 0x48, 0x73, 0x0a, 0x55, 0x4d,
	0x6b, 0xfd, 0x7c, 0x63, 0xc8, 0x86, 0xa9, 0x2b,
	0xd3, 0x90, 0x19, 0x8e, 0xb8, 0x12, 0x8a, 0xfb,
	0xf4, 0xde, 0x16, 0x2b, 0x8b, 0x95, 0xf6, 0x38,
}

// Obscure returns value in rclone's obscured form (AES-256-CTR under rclone's fixed key,
// base64url without padding of IV + ciphertext), with a deterministic IV:
// HMAC-SHA256(key, "<destinationID>:<field>")[:16], where key is
// Keyring.Derive(ObscureKeyInfo, 32). Each password of a destination therefore has exactly one
// obscured form: the string logging.SetSecrets registers, the environment carries and the
// recovery kit prints (§4.4, review finding SE-4). field is the credential's JSON name
// ("password", "privateKeyPassphrase", "cryptPassword", "cryptPassword2").
func Obscure(key []byte, destinationID int64, field, value string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strconv.FormatInt(destinationID, 10) + ":" + field))
	return obscureWithIV(value, mac.Sum(nil)[:aes.BlockSize])
}

// ObscureRandom obscures value with a random IV, as `rclone obscure` does: for a Test or Create
// of a destination that has no id yet, whose clear and obscured values are then passed to
// proc.Cmd.Redact for that request (§4.4).
func ObscureRandom(value string) string {
	iv := make([]byte, aes.BlockSize)
	_, _ = rand.Read(iv) // never fails (crypto/rand)
	return obscureWithIV(value, iv)
}

func obscureWithIV(value string, iv []byte) string {
	out := make([]byte, aes.BlockSize+len(value))
	copy(out, iv)
	cipher.NewCTR(obscureBlock(), iv).XORKeyStream(out[aes.BlockSize:], []byte(value))
	return base64.RawURLEncoding.EncodeToString(out)
}

func obscureBlock() cipher.Block {
	b, err := aes.NewCipher(rcloneObscureKey)
	if err != nil {
		panic(err) // a 32-byte key: unreachable
	}
	return b
}

// Reveal returns the clear value of an rclone-obscured string (any IV), as `rclone reveal` does.
// The acceptance's secrets probe passes every base64url token of a log to it (§14.6 item 4).
func Reveal(s string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("reveal: not an obscured value: %w", err)
	}
	if len(raw) < aes.BlockSize {
		return "", errors.New("reveal: not an obscured value: too short")
	}
	iv, body := raw[:aes.BlockSize], raw[aes.BlockSize:]
	out := make([]byte, len(body))
	cipher.NewCTR(obscureBlock(), iv).XORKeyStream(out, body)
	return string(out), nil
}
