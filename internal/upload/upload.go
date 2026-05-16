package upload

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/makerusa/ivault/internal/db"
)

// UploadConfig holds the parameters for the upload.
type UploadConfig struct {
	UploadQueue  string // local directory of staged files, e.g. /nvme/upload_queue
	Destinations []Destination
	Workers      int    // number of concurrent uploads (default: 2)
}

type Destination struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"` // "smb", "sftp", "ftp", "google_drive"
	Host       string `json:"host"`
	Share      string `json:"share"`
	Subfolder  string `json:"subfolder"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	Domain     string `json:"domain"`
	Path       string `json:"path"`
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

			// For now, we upload to the FIRST available destination.
			// In the future, we could iterate through priorities.
			if len(cfg.Destinations) == 0 {
				return fmt.Errorf("no active destinations configured")
			}
			target := cfg.Destinations[0]

			remoteName := "ivault-dynamic"
			dst := fmt.Sprintf("%s:%s/%s", remoteName, target.Subfolder, f.Filename)
			if target.Type == "smb" {
				dst = fmt.Sprintf("%s:%s/%s", remoteName, target.Share, filepath.Join(target.Subfolder, f.Filename))
			}

			database.UpdateFileState(f.ID, db.FileUploading)

			if err := uploadFile(gctx, src, dst, target, remoteName); err != nil {
				log.Printf("agent: upload FAILED for %s to %s: %v", f.Filename, dst, err)
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

			database.UpdateFileUploaded(f.ID, 0, dst) // TODO: Handle numeric/string ID conversion
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

func uploadFile(ctx context.Context, src, dst string, target Destination, remoteName string) error {
	cmd := exec.CommandContext(ctx, "rclone", "copyto",
		"--retries", "3",
		"--low-level-retries", "10",
		"--stats", "0",
		src, dst,
	)

	// Set dynamic rclone configuration via environment variables
	cmd.Env = os.Environ()
	prefix := fmt.Sprintf("RCLONE_CONFIG_%s_", strings.ToUpper(strings.ReplaceAll(remoteName, "-", "_")))
	
	switch target.Type {
	case "smb":
		cmd.Env = append(cmd.Env, prefix+"TYPE=smb")
		cmd.Env = append(cmd.Env, prefix+"HOST="+target.Host)
		cmd.Env = append(cmd.Env, prefix+"USER="+target.Username)
		cmd.Env = append(cmd.Env, prefix+"PASS="+obscurePassword(target.Password))
		if target.Domain != "" {
			cmd.Env = append(cmd.Env, prefix+"DOMAIN="+target.Domain)
		}
	case "sftp":
		cmd.Env = append(cmd.Env, prefix+"TYPE=sftp")
		cmd.Env = append(cmd.Env, prefix+"HOST="+target.Host)
		cmd.Env = append(cmd.Env, prefix+"USER="+target.Username)
		cmd.Env = append(cmd.Env, prefix+"PASS="+obscurePassword(target.Password))
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled")
		}
		return fmt.Errorf("rclone: %w — %s", err, string(out))
	}
	return nil
}

// obscurePassword performs the rclone obscuring logic (simplified for now or uses rclone obscure)
// For actual production, we should call 'rclone obscure' or implement the simple XOR.
func obscurePassword(p string) string {
	cmd := exec.Command("rclone", "obscure", p)
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}
