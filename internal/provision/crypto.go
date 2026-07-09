package provision

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"sync"
)

// ProvisioningEncryptionKey is the historical hardcoded key. It is insecure —
// it ships in public source, so anyone can decrypt provision-file secrets — and
// is retained only as a fallback (and for the round-trip test) when
// IVAULT_PROVISION_KEY is not set. Real deployments MUST set that env var to a
// per-deployment 32-byte key, matched on the portal that produces the file.
var ProvisioningEncryptionKey = []byte("iVault-Appliance-Secret-Key-2026") // 32 bytes

var (
	keyOnce sync.Once
	provKey []byte
)

// provisioningKey returns the AES-256 key for provision-file secrets. It
// prefers IVAULT_PROVISION_KEY (32 raw bytes, or base64/hex of 32 bytes) and
// falls back to the built-in key with a one-time warning.
func provisioningKey() []byte {
	keyOnce.Do(func() {
		if v := os.Getenv("IVAULT_PROVISION_KEY"); v != "" {
			if k := parseKey(v); k != nil {
				provKey = k
				return
			}
			log.Println("provision: IVAULT_PROVISION_KEY is set but not a valid 32-byte key; ignoring it")
		}
		log.Println("provision: WARNING — using the built-in provisioning key. Set IVAULT_PROVISION_KEY (and match it on the portal) for a secure deployment.")
		provKey = ProvisioningEncryptionKey
	})
	return provKey
}

// parseKey accepts a 32-byte raw string, or a base64/hex encoding of 32 bytes.
func parseKey(v string) []byte {
	if len(v) == 32 {
		return []byte(v)
	}
	if b, err := base64.StdEncoding.DecodeString(v); err == nil && len(b) == 32 {
		return b
	}
	if b, err := hex.DecodeString(v); err == nil && len(b) == 32 {
		return b
	}
	return nil
}

// DecryptWifiPassword reverses the AES-256-GCM encryption performed by the portal.
// base64Ciphertext contains both the nonce (first 12 bytes) and the ciphertext.
func DecryptWifiPassword(base64Ciphertext string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(base64Ciphertext)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(provisioningKey())
	if err != nil {
		return "", err
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := aesgcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}
