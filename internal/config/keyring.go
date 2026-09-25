package config

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// masterKeyLen is the size of the random master key in bytes.
const masterKeyLen = 32

// sealedPrefix marks the format of a sealed value: v1 = AES-256-GCM, 12-byte nonce, base64url.
const sealedPrefix = "v1:"

// ErrKeyMismatch means a sealed value could not be opened: the master key is not the one that
// sealed it (bunkarr.key was replaced or lost), or the value was altered.
var ErrKeyMismatch = errors.New("cannot decrypt stored secret: bunkarr.key does not match the database (restore the original key file)")

// LoadOrCreateMasterKey reads the master key at path, creating it (0600, 32 random bytes as hex)
// when it does not exist. Creation is atomic: a crash never leaves a truncated key behind, and
// two processes racing to create it end up with the same key. created reports a new key.
func LoadOrCreateMasterKey(path string) (key []byte, created bool, err error) {
	key, err = readMasterKey(path)
	if err == nil {
		return key, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}

	key = make([]byte, masterKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, false, fmt.Errorf("generate master key: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".bunkarr.key.*.tmp")
	if err != nil {
		return nil, false, fmt.Errorf("create master key: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, false, fmt.Errorf("create master key: %w", err)
	}
	if _, err := tmp.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		_ = tmp.Close()
		return nil, false, fmt.Errorf("write master key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, false, fmt.Errorf("write master key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, false, fmt.Errorf("write master key: %w", err)
	}
	// Link fails if path appeared in the meantime; then the other writer's key wins.
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			key, err = readMasterKey(path)
			return key, false, err
		}
		return nil, false, fmt.Errorf("install master key: %w", err)
	}
	return key, true, nil
}

func readMasterKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(string(bytes.TrimSpace(raw)))
	if err != nil || len(key) != masterKeyLen {
		return nil, fmt.Errorf("%s is not a valid Bunkarr master key (expected %d hex characters)", path, masterKeyLen*2)
	}
	return key, nil
}

// Keyring seals and opens secrets with a key derived from the master key.
type Keyring struct {
	aead cipher.AEAD
}

// NewKeyring derives the settings encryption key from master (HKDF-SHA256).
func NewKeyring(master []byte) (*Keyring, error) {
	if len(master) != masterKeyLen {
		return nil, fmt.Errorf("master key must be %d bytes", masterKeyLen)
	}
	k, err := hkdf.Key(sha256.New, master, nil, "bunkarr settings v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Keyring{aead: aead}, nil
}

// Seal encrypts plaintext. aad binds the ciphertext to its context (e.g. the setting's key), so a
// sealed value copied to another setting does not open.
func (k *Keyring) Seal(plaintext, aad string) (string, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := k.aead.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return sealedPrefix + base64.RawURLEncoding.EncodeToString(out), nil
}

// Open decrypts a value produced by Seal with the same aad.
func (k *Keyring) Open(sealed, aad string) (string, error) {
	body, ok := strings.CutPrefix(sealed, sealedPrefix)
	if !ok {
		return "", fmt.Errorf("sealed value has an unknown format")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) < k.aead.NonceSize()+k.aead.Overhead() {
		return "", ErrKeyMismatch
	}
	n := k.aead.NonceSize()
	plain, err := k.aead.Open(nil, raw[:n], raw[n:], []byte(aad))
	if err != nil {
		return "", ErrKeyMismatch
	}
	return string(plain), nil
}
