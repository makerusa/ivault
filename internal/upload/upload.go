package upload

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/makerusa/ivault/internal/db"
)

// UploadConfig holds the parameters for the upload destination.
type UploadConfig struct {
	UploadQueue  string // local directory of staged files, e.g. /nvme/upload_queue
	RcloneRemote string // rclone remote name, e.g. gdrive
	RclonePath   string // remote sub-path, e.g. iVault
	DestID       int64  // destination ID recorded in the DB
	Workers      int    // number of concurrent uploads (default: 2)
}

// UploadAll uploads all queued files using a bounded worker pool and returns
// the names of successfully uploaded files.
// If the context is cancelled, in-flight uploads are interrupted and their
// files are returned to the "queued" state for retry on the next cycle.
func UploadAll(ctx context.Context, database *db.DB, cfg UploadConfig) ([]string, error) {
	files, err := database.GetQueuedFiles()
	if err != nil {
		return nil, fmt.Errorf("get queued files: %w", err)
	}

	workers := cfg.Workers
	if workers <= 0 {
		workers = 2
	}

	var (
		mu       sync.Mutex
		uploaded []string
	)

	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, workers)

	for _, f := range files {
		f := f // capture loop variable

		sem <- struct{}{} // acquire slot (blocks when all workers are busy)
		g.Go(func() error {
			defer func() { <-sem }() // release slot

			src := filepath.Join(cfg.UploadQueue, f.Filename)

			if _, err := os.Stat(src); os.IsNotExist(err) {
				database.UpdateFileState(f.ID, db.FileAbandoned)
				return nil
			}

			dst := fmt.Sprintf("%s:%s/%s", cfg.RcloneRemote, cfg.RclonePath, f.Filename)

			database.UpdateFileState(f.ID, db.FileUploading)

			if err := uploadFile(gctx, src, dst); err != nil {
				if gctx.Err() != nil {
					// Cancelled — return to queued for retry next cycle
					database.UpdateFileState(f.ID, db.FileQueued)
					return fmt.Errorf("upload cancelled")
				}
				// Genuine failure — record and continue with remaining files
				database.UpdateFileError(f.ID, err.Error())
				database.UpdateFileState(f.ID, db.FileFailed)
				return nil
			}

			database.UpdateFileUploaded(f.ID, cfg.DestID, dst)
			os.Remove(src)

			mu.Lock()
			uploaded = append(uploaded, f.Filename)
			mu.Unlock()

			return nil
		})
	}

	// Wait for all workers. errgroup cancels gctx on the first non-nil error,
	// which causes remaining rclone processes to receive SIGKILL via CommandContext.
	if err := g.Wait(); err != nil {
		return uploaded, err
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
