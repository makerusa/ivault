// Package secret provides AES-256-GCM encryption for small secrets stored at
// rest (e.g. the cached destinations blob, which holds cloud refresh tokens and
// passwords), using a per-device key kept in a root-only file.
//
// Threat model: this protects the SQLite DB if it is copied, backed up, or
// shipped in logs WITHOUT the key file. It is NOT a boundary against an
// attacker who already has root on the device — they can read the key too.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// magic prefixes ciphertext so we can distinguish it from legacy plaintext that
// was stored before encryption was introduced (enables transparent migration).
const magic = "v1:"

// ErrNotEncrypted is returned by Decrypt when the input lacks the version
// marker, i.e. it is legacy plaintext.
var ErrNotEncrypted = errors.New("not encrypted")

type Manager struct {
	gcm cipher.AEAD
}

// NewManager loads the key at keyPath, generating a fresh random 32-byte key
// (written 0600) if the file does not exist.
func NewManager(keyPath string) (*Manager, error) {
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Manager{gcm: gcm}, nil
}

// Encrypt returns a version-marked, base64-encoded ciphertext.
func (m *Manager) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, m.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := m.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return magic + base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt reverses Encrypt. It returns ErrNotEncrypted for legacy plaintext so
// callers can fall back and migrate on the next write.
func (m *Manager) Decrypt(s string) (string, error) {
	if len(s) < len(magic) || s[:len(magic)] != magic {
		return "", ErrNotEncrypted
	}
	data, err := base64.StdEncoding.DecodeString(s[len(magic):])
	if err != nil {
		return "", err
	}
	ns := m.gcm.NonceSize()
	if len(data) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := data[:ns], data[ns:]
	pt, err := m.gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func loadOrCreateKey(keyPath string) ([]byte, error) {
	if b, err := os.ReadFile(keyPath); err == nil {
		if len(b) == 32 {
			return b, nil
		}
		return nil, fmt.Errorf("key file %s has wrong size %d (want 32)", keyPath, len(b))
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
