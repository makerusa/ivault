package ingest

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/makerusa/ivault/internal/db"
	"github.com/makerusa/ivault/internal/provision"
)

// IngestConfig holds the filesystem paths used during ingest.
type IngestConfig struct {
	ImagePath   string // e.g. /nvme/usb_disk.img
	MountPoint  string // e.g. /nvme/ingest
	UploadQueue string // e.g. /nvme/upload_queue
	ConfigPath  string // e.g. /etc/ivault/config.json
}

func Mount(cfg IngestConfig) error {
	device := cfg.ImagePath

	if strings.HasPrefix(device, "/dev/") {
		partition := device
		if _, err := os.Stat(device + "p1"); err == nil {
			partition = device + "p1"
		} else if _, err := os.Stat(device + "1"); err == nil {
			partition = device + "1"
		}

		cmd := exec.Command("mount", partition, cfg.MountPoint)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("mount raw device failed: %w — %s", err, string(out))
		}
		return nil
	}

	// Fallback to loop image file mount
	cmd := exec.Command("mount", "-o", "loop", cfg.ImagePath, cfg.MountPoint)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount loop failed: %w — %s", err, string(out))
	}
	return nil
}

func Unmount(cfg IngestConfig) error {
	cmd := exec.Command("umount", cfg.MountPoint)
	cmd.CombinedOutput() // ignore "not mounted" errors
	return nil
}

type IngestResult struct {
	FilesFound  int
	FilesCopied int
	BytesCopied int64
	Skipped     int
}

func Run(cfg IngestConfig, database *db.DB, sessionID int64) (*IngestResult, bool, error) {
	result := &IngestResult{}

	// Run provision check
	provisioned, err := provision.Process(cfg.MountPoint, cfg.ConfigPath)
	if err != nil {
		return nil, false, fmt.Errorf("provisioning failed: %w", err)
	}

	err = filepath.WalkDir(cfg.MountPoint, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip directories themselves from being processed as files
		if d.IsDir() {
			return nil
		}

		// Compute relative path to preserve folder structure
		relPath, err := filepath.Rel(cfg.MountPoint, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", path, err)
		}

		// Skip system files, metadata, and hidden directories
		if isSystemFileOrInHiddenFolder(relPath) {
			return nil
		}

		result.FilesFound++

		info, err := d.Info()
		if err != nil {
			return nil // Skip on stat error
		}

		// Compute source checksum
		checksum, err := checksumFile(path)
		if err != nil {
			return fmt.Errorf("checksum %s: %w", relPath, err)
		}

		// Check if already processed by checksum
		existing, err := database.GetFileByChecksum(checksum)
		if err != nil {
			return fmt.Errorf("db lookup %s: %w", relPath, err)
		}
		if existing != nil {
			result.Skipped++
			return nil
		}

		// Record in DB
		fileID, err := database.InsertFile(&db.File{
			SessionID:      sessionID,
			Filename:       relPath,
			SizeBytes:      info.Size(),
			ChecksumSHA256: checksum,
			State:          db.FileDiscovered,
		})
		if err != nil {
			return fmt.Errorf("db insert %s: %w", relPath, err)
		}

		// Copy to upload queue
		dst := filepath.Join(cfg.UploadQueue, relPath)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return fmt.Errorf("failed to create upload queue directories: %w", err)
		}

		if err := copyAndVerify(path, dst, checksum); err != nil {
			return fmt.Errorf("copy %s: %w", relPath, err)
		}

		// Mark as copied + queued
		database.UpdateFileState(fileID, db.FileCopied)
		database.UpdateFileState(fileID, db.FileQueued)

		result.FilesCopied++
		result.BytesCopied += info.Size()

		return nil
	})

	if err != nil {
		return result, provisioned, err
	}

	return result, provisioned, nil
}

func isSystemFileOrInHiddenFolder(relPath string) bool {
	parts := strings.Split(relPath, string(filepath.Separator))
	for _, part := range parts {
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "._") {
			return true
		}
		if part == "ivault.provision" || part == "ivault-provision.json" {
			return true
		}
	}
	return false
}

// copyAndVerify copies src to dst while simultaneously computing the SHA-256
// of the written bytes via io.TeeReader. If the resulting hash does not match
// expectedChecksum the destination file is deleted and an error is returned.
// This reduces ingest I/O from three full file reads to two (source checksum
// for dedup + this combined copy-and-verify).
func copyAndVerify(src, dst, expectedChecksum string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	hasher := sha256.New()
	if _, err := io.Copy(out, io.TeeReader(in, hasher)); err != nil {
		os.Remove(dst)
		return err
	}

	if err := out.Sync(); err != nil {
		os.Remove(dst)
		return err
	}

	got := fmt.Sprintf("%x", hasher.Sum(nil))
	if got != expectedChecksum {
		os.Remove(dst)
		return fmt.Errorf("checksum mismatch after copy (expected %s, got %s) — file deleted", expectedChecksum, got)
	}

	return nil
}
