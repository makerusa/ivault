package secret

import (
	"path/filepath"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "secret.key")
	m, err := NewManager(keyPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	plaintext := `[{"type":"google_drive","password":"1//0refreshtoken"}]`
	enc, err := m.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if enc == plaintext {
		t.Fatal("ciphertext equals plaintext")
	}

	got, err := m.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("round trip mismatch: got %q want %q", got, plaintext)
	}
}

func TestDecryptLegacyPlaintextReportsNotEncrypted(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := m.Decrypt(`[{"type":"smb"}]`); err != ErrNotEncrypted {
		t.Fatalf("expected ErrNotEncrypted for legacy plaintext, got %v", err)
	}
}

func TestKeyPersistsAcrossManagers(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "secret.key")
	m1, err := NewManager(keyPath)
	if err != nil {
		t.Fatalf("NewManager m1: %v", err)
	}
	enc, err := m1.Encrypt("hello")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// A second manager loading the same key file must decrypt m1's output.
	m2, err := NewManager(keyPath)
	if err != nil {
		t.Fatalf("NewManager m2: %v", err)
	}
	got, err := m2.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt with reloaded key: %v", err)
	}
	if got != "hello" {
		t.Fatalf("got %q want %q", got, "hello")
	}
}
