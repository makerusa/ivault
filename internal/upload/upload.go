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

// maxUploadAttempts bounds how many times a file is retried across maintenance
// cycles before it is marked abandoned. Without this a permanently failing
// upload (e.g. a destination permission error) re-sends the whole file every
// cycle forever.
const maxUploadAttempts = 3

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

// MaskSecret safely masks a secret key/token/password for logging purposes.
// It reveals only presence and length — never any characters of the secret —
// because these logs are shipped to the portal (e.g. a refresh token's first/
// last chars must not leak into the log pipeline).
func MaskSecret(s string) string {
	if s == "" {
		return "<empty>"
	}
	return fmt.Sprintf("<present, len=%d>", len(s))
}

// LogDetails logs metadata about the destination while keeping credentials masked.
func (d Destination) LogDetails(prefix string) {
	log.Printf("%s - Destination ID=%q, Name=%q, Type=%q, Host=%q, Subfolder=%q, Username=%q",
		prefix, d.ID, d.Name, d.Type, d.Host, d.Subfolder, d.Username)
	log.Printf("%s - Credentials details: HasPassword=%t (%s), HasClientID=%t (%s), HasClientSecret=%t (%s)",
		prefix, d.Password != "", MaskSecret(d.Password), d.ClientID != "", MaskSecret(d.ClientID), d.ClientSecret != "", MaskSecret(d.ClientSecret))
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

	if len(files) == 0 {
		return nil, nil
	}

	// Validate destinations up front before pre-creating directories.
	if len(cfg.Destinations) == 0 {
		return nil, fmt.Errorf("no active destinations configured")
	}
	target := cfg.Destinations[0]

	// Extract unique parent directories of queued files to pre-create sequentially.
	// This avoids duplicate folder creation race conditions on target systems (like Google Drive).
	uniqueDirs := make(map[string]bool)
	for _, f := range files {
		dir := filepath.ToSlash(filepath.Dir(f.Filename))
		if dir != "." && dir != "/" && dir != "" {
			uniqueDirs[dir] = true
		}
	}

	remoteName := "REMOTE"
	for dir := range uniqueDirs {
		log.Printf("agent: pre-creating remote directory: %s:%s", remoteName, dir)
		if err := createRemoteDir(ctx, dir, target, remoteName); err != nil {
			log.Printf("agent: warning: failed to pre-create remote directory %s:%s: %v", remoteName, dir, err)
		}
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

			log.Printf("agent: targeting destination '%s' (%s) type=%s", target.Name, target.Host, target.Type)

			var dst string
			if target.Type == "google_drive" {
				dst = fmt.Sprintf("%s:%s", remoteName, f.Filename)
			} else if target.Type == "smb" {
				dst = fmt.Sprintf("%s:%s/%s", remoteName, target.Share, filepath.Join(target.Subfolder, f.Filename))
			} else {
				dst = fmt.Sprintf("%s:%s/%s", remoteName, target.Subfolder, f.Filename)
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
				// Genuine failure — record the error (increments upload_attempts)
				// and continue with the remaining files.
				database.UpdateFileError(f.ID, err.Error())
				// Abandon after too many attempts so a permanently failing file
				// doesn't re-upload every maintenance cycle forever.
				if f.UploadAttempts+1 >= maxUploadAttempts {
					log.Printf("agent: giving up on %s after %d attempts — marking abandoned", f.Filename, f.UploadAttempts+1)
					database.UpdateFileState(f.ID, db.FileAbandoned)
				} else {
					database.UpdateFileState(f.ID, db.FileFailed)
				}
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
		"--retries", "3",
		"--low-level-retries", "10",
		"--stats", "1s",
		"-vv",
		src, dst,
	)

	setupRCloneEnv(cmd, target, remoteName, true)

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

func createRemoteDir(ctx context.Context, dir string, target Destination, remoteName string) error {
	cmd := exec.CommandContext(ctx, "rclone", "mkdir",
		"--config", "/dev/null",
		fmt.Sprintf("%s:%s", remoteName, dir),
	)
	setupRCloneEnv(cmd, target, remoteName, false)
	return cmd.Run()
}

func setupRCloneEnv(cmd *exec.Cmd, target Destination, remoteName string, enableLogging bool) {
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
		// Must match the scope the portal's refresh token was granted. Full
		// drive (not drive.file) is required to upload into a user-owned folder
		// the app did not itself create.
		addEnv("SCOPE", "drive")
		
		clientID := target.ClientID
		if enableLogging {
			log.Printf("agent: [upload-debug] evaluating google_drive ClientID:")
			log.Printf("  - Destination ClientID from Portal: %s", MaskSecret(target.ClientID))
			log.Printf("  - System GOOGLE_CLIENT_ID from Env: %s", MaskSecret(os.Getenv("GOOGLE_CLIENT_ID")))
		}
		
		if clientID == "" {
			clientID = os.Getenv("GOOGLE_CLIENT_ID")
			if enableLogging {
				log.Printf("  - Result: Using GOOGLE_CLIENT_ID from environment")
			}
		} else {
			if enableLogging {
				log.Printf("  - Result: Using ClientID provided dynamically by Portal")
			}
		}
		if clientID != "" {
			addEnv("CLIENT_ID", clientID)
			if enableLogging {
				log.Printf("  - Set CLIENT_ID: %s", MaskSecret(clientID))
			}
		} else {
			if enableLogging {
				log.Printf("  - WARNING: No CLIENT_ID is set!")
			}
		}

		clientSecret := target.ClientSecret
		if enableLogging {
			log.Printf("agent: [upload-debug] evaluating google_drive ClientSecret:")
			log.Printf("  - Destination ClientSecret from Portal: %s", MaskSecret(target.ClientSecret))
			log.Printf("  - System GOOGLE_CLIENT_SECRET from Env: %s", MaskSecret(os.Getenv("GOOGLE_CLIENT_SECRET")))
		}
		
		if clientSecret == "" {
			clientSecret = os.Getenv("GOOGLE_CLIENT_SECRET")
			if enableLogging {
				log.Printf("  - Result: Using GOOGLE_CLIENT_SECRET from environment")
			}
		} else {
			if enableLogging {
				log.Printf("  - Result: Using ClientSecret provided dynamically by Portal")
			}
		}
		if clientSecret != "" {
			addEnv("CLIENT_SECRET", clientSecret)
			if enableLogging {
				log.Printf("  - Set CLIENT_SECRET: %s", MaskSecret(clientSecret))
			}
		} else {
			if enableLogging {
				log.Printf("  - WARNING: No CLIENT_SECRET is set!")
			}
		}

		tokenJSON := fmt.Sprintf(`{"access_token":"","token_type":"Bearer","refresh_token":"%s","expiry":"0001-01-01T00:00:00Z"}`, target.Password)
		addEnv("TOKEN", tokenJSON)
		addEnv("ROOT_FOLDER_ID", target.Subfolder)
		if enableLogging {
			log.Printf("agent: [upload-debug] evaluating google_drive token:")
			log.Printf("  - Set TOKEN using refresh_token: %s", MaskSecret(target.Password))
			log.Printf("  - Set ROOT_FOLDER_ID: %s", target.Subfolder)
		}
	}

	if enableLogging {
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
	}
}

// obscurePassword performs the rclone obscuring logic (simplified for now or uses rclone obscure)
// For actual production, we should call 'rclone obscure' or implement the simple XOR.
func obscurePassword(p string) string {
	cmd := exec.Command("rclone", "obscure", p)
	out, err := cmd.Output()
	if err != nil {
		// Returning "" would silently produce an empty password and a
		// confusing auth failure downstream — surface it instead.
		log.Printf("agent: warning: 'rclone obscure' failed (%v); SMB/SFTP password will be empty", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}
