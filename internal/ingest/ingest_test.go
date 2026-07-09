package ingest

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:])
}

func TestCopyAndVerify_Success(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.bin")
	dst := filepath.Join(dir, "dest.bin")

	content := []byte("relay test payload — video frame data")
	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatal(err)
	}

	// Exercise the REAL copyAndVerify (white-box, same package).
	if err := copyAndVerify(src, dst, sha256Hex(content)); err != nil {
		t.Fatalf("copyAndVerify failed: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal("dst file should exist after successful copy")
	}
	if string(got) != string(content) {
		t.Error("destination content does not match source")
	}
}

func TestCopyAndVerify_ChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.bin")
	dst := filepath.Join(dir, "dest.bin")

	if err := os.WriteFile(src, []byte("original content"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := copyAndVerify(src, dst, sha256Hex([]byte("different content"))); err == nil {
		t.Fatal("expected error on checksum mismatch, got nil")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Error("destination file should be deleted on checksum mismatch")
	}
}

func TestIsSystemFileOrInHiddenFolder(t *testing.T) {
	cases := map[string]bool{
		"video.mp4":                 false,
		"folder/clip.mov":           false,
		".DS_Store":                 true,
		"._resourcefork":            true,
		".hidden/file.txt":          true,
		"ivault.provision":          true,
		"sub/ivault-provision.json": true,
	}
	for path, want := range cases {
		if got := isSystemFileOrInHiddenFolder(filepath.FromSlash(path)); got != want {
			t.Errorf("isSystemFileOrInHiddenFolder(%q) = %v, want %v", path, got, want)
		}
	}
}
