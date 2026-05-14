package ingest

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/umarsear/ivault/internal/db"
)

const (
	ImagePath   = "/nvme/usb_disk.img"
	MountPoint  = "/nvme/ingest"
	UploadQueue = "/nvme/upload_queue"
)

func Mount() error {
	cmd := exec.Command("mount", "-o", "loop", ImagePath, MountPoint)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount failed: %w — %s", err, string(out))
	}
	return nil
}

func Unmount() error {
	cmd := exec.Command("umount", MountPoint)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Ignore "not mounted" errors
		return nil
	}
	_ = out
	return nil
}

type IngestResult struct {
	FilesFound  int
	FilesCopied int
	BytesCopied int64
	Skipped     int
}

func Run(database *db.DB, sessionID int64) (*IngestResult, error) {
	result := &IngestResult{}

	entries, err := os.ReadDir(MountPoint)
	if err != nil {
		return nil, fmt.Errorf("read mount point: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		// Skip system/metadata files
		if isSystemFile(name) {
			continue
		}

		result.FilesFound++

		src := filepath.Join(MountPoint, name)

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Checksum source
		checksum, err := checksumFile(src)
		if err != nil {
			return result, fmt.Errorf("checksum %s: %w", name, err)
		}

		// Check if already processed by checksum
		existing, err := database.GetFileByChecksum(checksum)
		if err != nil {
			return result, fmt.Errorf("db lookup %s: %w", name, err)
		}
		if existing != nil {
			result.Skipped++
			continue
		}

		// Record in DB
		fileID, err := database.InsertFile(&db.File{
			SessionID:      sessionID,
			Filename:       name,
			SizeBytes:      info.Size(),
			ChecksumSHA256: checksum,
			State:          db.FileDiscovered,
		})
		if err != nil {
			return result, fmt.Errorf("db insert %s: %w", name, err)
		}

		// Copy to upload queue
		dst := filepath.Join(UploadQueue, name)
		if err := copyFile(src, dst); err != nil {
			return result, fmt.Errorf("copy %s: %w", name, err)
		}

		// Verify copy checksum
		dstChecksum, err := checksumFile(dst)
		if err != nil || dstChecksum != checksum {
			os.Remove(dst)
			return result, fmt.Errorf("checksum mismatch for %s — copy deleted", name)
		}

		// Mark as copied + queued
		database.UpdateFileState(fileID, db.FileCopied)
		database.UpdateFileState(fileID, db.FileQueued)

		result.FilesCopied++
		result.BytesCopied += info.Size()
	}

	return result, nil
}

func isSystemFile(name string) bool {
	return strings.HasPrefix(name, "._") ||
		strings.HasPrefix(name, ".") ||
		name == ".DS_Store" ||
		name == ".Spotlight-V100" ||
		name == ".fseventsd" ||
		name == "ivault-provision.json"
}

func copyFile(src, dst string) error {
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

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	return out.Sync()
}
