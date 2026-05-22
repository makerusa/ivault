package upload

import (
	"bufio"
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
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"` // "smb", "sftp", "ftp", "google_drive"
	Host         string `json:"host"`
	Share        string `json:"share"`
	Subfolder    string `json:"subfolder"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	Domain       string `json:"domain"`
	Path         string `json:"path"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
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
	log.Printf("agent: upload engine found %d files in the database queue", len(files))

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

			log.Printf("agent: targeting destination '%s' (%s) type=%s", target.Name, target.Host, target.Type)

			remoteName := "REMOTE"
			dst := fmt.Sprintf("%s:%s/%s", remoteName, target.Subfolder, f.Filename)
			if target.Type == "smb" {
				dst = fmt.Sprintf("%s:%s/%s", remoteName, target.Share, filepath.Join(target.Subfolder, f.Filename))
			}
			log.Printf("agent: rclone destination path: %s", dst)

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
		"--config", "/dev/null",
		"--retries", "1", // Reduced for faster debugging
		"--low-level-retries", "1",
		"--stats", "1s",
		"-vv",
		src, dst,
	)

	// Set dynamic rclone configuration via environment variables
	cmd.Env = os.Environ()
	
	upperRemote := strings.ToUpper(remoteName)
	lowerRemote := strings.ToLower(remoteName)
	
	addEnv := func(key, value string) {
		cmd.Env = append(cmd.Env, fmt.Sprintf("RCLONE_CONFIG_%s_%s=%s", upperRemote, key, value))
		cmd.Env = append(cmd.Env, fmt.Sprintf("RCLONE_CONFIG_%s_%s=%s", lowerRemote, key, value))
	}

	switch target.Type {
	case "smb":
		addEnv("TYPE", "smb")
		addEnv("HOST", target.Host)
		addEnv("USER", target.Username)
		addEnv("PASS", obscurePassword(target.Password))
		if target.Domain != "" {
			addEnv("DOMAIN", target.Domain)
		}
	case "sftp":
		addEnv("TYPE", "sftp")
		addEnv("HOST", target.Host)
		addEnv("USER", target.Username)
		addEnv("PASS", obscurePassword(target.Password))
	case "google_drive":
		addEnv("TYPE", "drive")
		addEnv("SCOPE", "drive.file")
		
		clientID := target.ClientID
		if clientID == "" {
			clientID = os.Getenv("GOOGLE_CLIENT_ID")
		}
		if clientID != "" {
			addEnv("CLIENT_ID", clientID)
		}

		clientSecret := target.ClientSecret
		if clientSecret == "" {
			clientSecret = os.Getenv("GOOGLE_CLIENT_SECRET")
		}
		if clientSecret != "" {
			addEnv("CLIENT_SECRET", clientSecret)
		}

		tokenJSON := fmt.Sprintf(`{"access_token":"","token_type":"Bearer","refresh_token":"%s","expiry":"0001-01-01T00:00:00Z"}`, target.Password)
		addEnv("TOKEN", tokenJSON)
		addEnv("ROOT_FOLDER_ID", target.Subfolder)
	}

	// Log the environment variables set for debugging, obscuring sensitive data.
	log.Printf("agent: running rclone command: %s", cmd.String())
	log.Println("agent: rclone configuration environment variables:")
	for _, env := range cmd.Env {
		if strings.HasPrefix(env, "RCLONE_CONFIG_") {
			parts := strings.SplitN(env, "=", 2)
			if len(parts) == 2 {
				key, val := parts[0], parts[1]
				// Obscure password, token, or client secret for security
				if strings.Contains(key, "_PASS") || strings.Contains(key, "_TOKEN") || strings.Contains(key, "_CLIENT_SECRET") {
					val = "[REDACTED]"
				}
				log.Printf("  %s=%s", key, val)
			}
		}
	}

	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create pipe: %w", err)
	}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		w.Close()
		r.Close()
		return fmt.Errorf("rclone start: %w", err)
	}

	w.Close()

	scanner := bufio.NewScanner(r)
	go func() {
		for scanner.Scan() {
			log.Printf("agent: rclone | %s", scanner.Text())
		}
		r.Close()
	}()

	err = cmd.Wait()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled")
		}
		return fmt.Errorf("rclone: %w", err)
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
