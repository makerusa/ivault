package provision

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"testing"
)

func TestDecryptWifiPassword(t *testing.T) {
	// 1. Setup plaintext and generate mock portal ciphertext
	plaintext := "MySecretWiFiPass123!"

	block, err := aes.NewCipher(ProvisioningEncryptionKey)
	if err != nil {
		t.Fatalf("failed to create cipher: %v", err)
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("failed to create gcm: %v", err)
	}

	nonce := make([]byte, aesgcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatalf("failed to read random nonce: %v", err)
	}

	ciphertext := aesgcm.Seal(nil, nonce, []byte(plaintext), nil)
	finalData := append(nonce, ciphertext...)
	encoded := base64.StdEncoding.EncodeToString(finalData)

	// 2. Test decryption
	decrypted, err := DecryptWifiPassword(encoded)
	if err != nil {
		t.Fatalf("DecryptWifiPassword failed: %v", err)
	}

	if decrypted != plaintext {
		t.Errorf("expected %q, got %q", plaintext, decrypted)
	}
}
