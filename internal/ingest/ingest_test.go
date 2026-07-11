package ingest

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/makerusa/ivault/internal/db"
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
	// Provision sentinels are handled separately by isProvisionFile now, so the
	// system-file check only covers dotfiles / hidden dirs / OS metadata.
	cases := map[string]bool{
		"video.mp4":        false,
		"folder/clip.mov":  false,
		"ivault.provision": false,
		".DS_Store":        true,
		"._resourcefork":   true,
		".hidden/file.txt": true,
	}
	for path, want := range cases {
		if got := isSystemFileOrInHiddenFolder(filepath.FromSlash(path)); got != want {
			t.Errorf("isSystemFileOrInHiddenFolder(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIsProvisionFile(t *testing.T) {
	cases := map[string]bool{
		"ivault.provision":          true,
		"sub/ivault-provision.json": true,
		"video.mp4":                 false,
		".DS_Store":                 false,
	}
	for path, want := range cases {
		if got := isProvisionFile(filepath.FromSlash(path)); got != want {
			t.Errorf("isProvisionFile(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestLoadFilters(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Defaults when nothing has been synced: skip-system on, no size/ext limits.
	f := loadFilters(database)
	if !f.skipSystemFiles || f.minSizeBytes != 0 || len(f.allowedExts) != 0 {
		t.Fatalf("unexpected defaults: %+v", f)
	}

	// Values persisted by the heartbeat are parsed, normalized, and enforced.
	_ = database.SetConfig("filter_skip_system_files", "false")
	_ = database.SetConfig("filter_skip_files_under_mb", "2")
	_ = database.SetConfig("filter_allowed_extensions", "MP4, .mov ,wav")
	f = loadFilters(database)
	if f.skipSystemFiles {
		t.Error("skipSystemFiles should be false")
	}
	if f.minSizeBytes != 2*1024*1024 {
		t.Errorf("minSizeBytes = %d, want %d", f.minSizeBytes, 2*1024*1024)
	}
	for _, ext := range []string{"mp4", "mov", "wav"} {
		if !f.allowedExts[ext] {
			t.Errorf("extension %q should be allowed; got %+v", ext, f.allowedExts)
		}
	}
	if f.allowedExts["avi"] {
		t.Error("extension avi should not be allowed")
	}
}
