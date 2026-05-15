package provision

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
)

var ProvisioningEncryptionKey = []byte("iVault-Appliance-Secret-Key-2026") // 32 bytes

// DecryptWifiPassword reverses the AES-256-GCM encryption performed by the portal.
// base64Ciphertext contains both the nonce (first 12 bytes) and the ciphertext.
func DecryptWifiPassword(base64Ciphertext string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(base64Ciphertext)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(ProvisioningEncryptionKey)
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
