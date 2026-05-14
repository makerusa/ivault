package upload

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/umarsear/ivault/internal/db"
)

const (
	UploadQueue = "/nvme/upload_queue"
	RemoteName  = "gdrive"
	RemotePath  = "iVault"
	DestID      = 1
)

func UploadAll(ctx context.Context, database *db.DB) ([]string, error) {
	files, err := database.GetQueuedFiles()
	if err != nil {
		return nil, fmt.Errorf("get queued files: %w", err)
	}

	var uploaded []string

	for _, f := range files {
		select {
		case <-ctx.Done():
			// Return files to queued state on cancel
			database.UpdateFileState(f.ID, db.FileQueued)
			return uploaded, fmt.Errorf("upload cancelled after %d files", len(uploaded))
		default:
		}

		src := filepath.Join(UploadQueue, f.Filename)

		if _, err := os.Stat(src); os.IsNotExist(err) {
			database.UpdateFileState(f.ID, db.FileAbandoned)
			continue
		}

		dst := fmt.Sprintf("%s:%s/%s", RemoteName, RemotePath, f.Filename)

		database.UpdateFileState(f.ID, db.FileUploading)

		if err := uploadFile(ctx, src, dst); err != nil {
			if ctx.Err() != nil {
				// Cancelled — put back to queued for retry
				database.UpdateFileState(f.ID, db.FileQueued)
				return uploaded, fmt.Errorf("upload cancelled after %d files", len(uploaded))
			}
			// Genuine failure
			database.UpdateFileError(f.ID, err.Error())
			database.UpdateFileState(f.ID, db.FileFailed)
			continue
		}

		database.UpdateFileUploaded(f.ID, DestID, dst)
		os.Remove(src)
		uploaded = append(uploaded, f.Filename)
	}

	return uploaded, nil
}

func uploadFile(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "rclone", "copyto",
		"--retries", "3",
		"--low-level-retries", "10",
		"--stats", "0",
		src, dst,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled")
		}
		return fmt.Errorf("rclone: %w — %s", err, string(out))
	}
	return nil
}
