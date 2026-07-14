package upload

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	// DeviceFolder, when set, is prepended to every remote path so each device
	// writes into its own folder at the destination (avoids two devices sharing
	// the same folder). Typically the device name, falling back to its id.
	DeviceFolder string
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

// UploadAll uploads every queued file to the highest-priority destination that
// accepts it. Destinations arrive in priority order (the portal delivers the
// primary first, then fallbacks); each file is tried against them in that order
// and the first success wins. A destination that fails is remembered for the
// rest of the cycle and skipped as a fallback, so a down/misconfigured primary
// (e.g. an offline NAS) doesn't stall every file — its files roll over to the
// fallback instead. Returns the uploaded filenames and the ids of the
// destinations that actually received at least one backup.
func UploadAll(ctx context.Context, database *db.DB, cfg UploadConfig) ([]string, []string, error) {
	files, err := database.GetQueuedFiles()
	if err != nil {
		return nil, nil, fmt.Errorf("get queued files: %w", err)
	}
	log.Printf("agent: upload engine found %d files in the database queue", len(files))

	if len(files) == 0 {
		return nil, nil, nil
	}
	if len(cfg.Destinations) == 0 {
		return nil, nil, fmt.Errorf("no active destinations configured")
	}

	remoteName := "REMOTE"

	// remoteRel prepends the per-device folder (if any) so each device's files
	// land in their own folder at the destination.
	remoteRel := func(name string) string {
		rel := filepath.ToSlash(name)
		if cfg.DeviceFolder != "" {
			return cfg.DeviceFolder + "/" + rel
		}
		return rel
	}
	// remoteDst builds the rclone destination path for a given target + file.
	remoteDst := func(target Destination, rel string) string {
		switch target.Type {
		case "google_drive":
			return fmt.Sprintf("%s:%s", remoteName, rel)
		case "smb":
			return fmt.Sprintf("%s:%s/%s", remoteName, target.Share, path.Join(target.Subfolder, rel))
		default:
			return fmt.Sprintf("%s:%s/%s", remoteName, target.Subfolder, rel)
		}
	}

	// Unique parent directories of the queued files, pre-created per destination
	// before its first upload to avoid duplicate-folder races (notably on
	// Google Drive) when workers run concurrently.
	uniqueDirs := make([]string, 0)
	seenDir := map[string]bool{}
	for _, f := range files {
		dir := filepath.ToSlash(filepath.Dir(remoteRel(f.Filename)))
		if dir != "." && dir != "/" && dir != "" && !seenDir[dir] {
			seenDir[dir] = true
			uniqueDirs = append(uniqueDirs, dir)
		}
	}
	var prepMu sync.Mutex
	prepped := map[string]bool{}
	prepDest := func(target Destination) {
		prepMu.Lock()
		defer prepMu.Unlock()
		if prepped[target.ID] {
			return
		}
		prepped[target.ID] = true
		for _, dir := range uniqueDirs {
			if err := createRemoteDir(ctx, dir, target, remoteName); err != nil {
				log.Printf("agent: warning: failed to pre-create %s:%s: %v", remoteName, dir, err)
			}
		}
	}

	// deadDest marks destinations that have already failed this cycle so a
	// broken/offline one isn't retried for every remaining file. rclone has
	// already applied its own per-file retries before returning an error, so a
	// failure here is treated as a destination-level problem for this cycle;
	// it's retried fresh next cycle.
	var deadMu sync.Mutex
	deadDest := map[string]bool{}

	workers := cfg.Workers
	if workers <= 0 {
		workers = 2
	}

	var (
		mu        sync.Mutex
		uploaded  []string
		usedDests = map[string]bool{}
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

			rel := remoteRel(f.Filename)
			database.UpdateFileState(f.ID, db.FileUploading)

			var (
				lastErr    error
				uploadedTo *Destination
				uploadMs   int64
			)
			for i := range cfg.Destinations {
				target := cfg.Destinations[i]
				deadMu.Lock()
				dead := deadDest[target.ID]
				deadMu.Unlock()
				if dead {
					continue
				}

				prepDest(target)
				dst := remoteDst(target, rel)
				role := "primary"
				if i > 0 {
					role = "fallback"
				}
				log.Printf("agent: uploading %s to %s destination '%s' (%s): %s", f.Filename, role, target.Name, target.Type, dst)

				start := time.Now()
				if err := uploadFile(gctx, src, dst, target, remoteName); err != nil {
					if gctx.Err() != nil {
						// Cancelled — return to queued for retry next cycle.
						database.UpdateFileState(f.ID, db.FileQueued)
						return fmt.Errorf("upload cancelled")
					}
					log.Printf("agent: upload FAILED for %s to '%s': %v", f.Filename, target.Name, err)
					lastErr = err
					deadMu.Lock()
					deadDest[target.ID] = true
					deadMu.Unlock()
					continue // try the next (fallback) destination
				}
				uploadMs = time.Since(start).Milliseconds()
				t := target
				uploadedTo = &t
				break
			}

			if uploadedTo == nil {
				// Every destination failed for this file this cycle.
				msg := "no reachable destination"
				if lastErr != nil {
					msg = lastErr.Error()
				}
				database.UpdateFileError(f.ID, msg)
				if f.UploadAttempts+1 >= maxUploadAttempts {
					log.Printf("agent: giving up on %s after %d attempts — marking abandoned", f.Filename, f.UploadAttempts+1)
					database.UpdateFileState(f.ID, db.FileAbandoned)
				} else {
					database.UpdateFileState(f.ID, db.FileFailed)
				}
				return nil
			}

			// Record the human-friendly destination name (falling back to type)
			// so the portal can show where each file backed up.
			destName := uploadedTo.Name
			if destName == "" {
				destName = uploadedTo.Type
			}
			database.UpdateFileUploaded(f.ID, destName, remoteDst(*uploadedTo, rel))
			_ = database.SetFileUploadMs(f.ID, uploadMs)
			os.Remove(src)

			mu.Lock()
			uploaded = append(uploaded, f.Filename)
			usedDests[uploadedTo.ID] = true
			mu.Unlock()
			return nil
		})
	}

	// Wait for all workers. errgroup cancels gctx on the first non-nil error,
	// which causes remaining rclone processes to receive SIGKILL via CommandContext.
	err = g.Wait()
	used := make([]string, 0, len(usedDests))
	for id := range usedDests {
		used = append(used, id)
	}
	return uploaded, used, err
}

// TestReachable checks whether a destination is currently reachable, in a way
// appropriate to its type: a TCP dial for host-based backends (SMB/SFTP/FTP)
// and a real rclone auth+list probe for cloud backends (Google Drive), where a
// socket dial is meaningless. Returns the probe latency (ms) and a non-nil
// error when unreachable or when the type has no meaningful check. This is the
// single source of truth for "can we reach this destination" and is used by
// both the manual Test Connection and any pre-flight check.
func TestReachable(ctx context.Context, target Destination) (int64, error) {
	start := time.Now()
	switch target.Type {
	case "google_drive":
		// Auth + reachability in one: list the app's backup folder. This
		// succeeds only if the stored refresh token still authorizes against
		// Drive — the only reachability signal that means anything for a cloud
		// destination (it has no host:port to dial).
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "rclone", "lsd", "--config", "/dev/null", "REMOTE:")
		setupRCloneEnv(cmd, target, "REMOTE", false)
		out, err := cmd.CombinedOutput()
		latency := time.Since(start).Milliseconds()
		if err != nil {
			return latency, fmt.Errorf("google drive check failed: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		return latency, nil
	case "smb", "sftp", "ftp":
		if target.Host == "" {
			return 0, fmt.Errorf("no host configured for %s destination", target.Type)
		}
		port := 445
		switch target.Type {
		case "ftp":
			port = 21
		case "sftp":
			port = 22
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", target.Host, port), 5*time.Second)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			return latency, err
		}
		conn.Close()
		return latency, nil
	default:
		return 0, fmt.Errorf("reachability test not supported for type %q", target.Type)
	}
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
		// Match the portal's drive.file scope. Uploads target an app-created
		// folder (whose ID the portal supplies as Subfolder/ROOT_FOLDER_ID), so
		// least-privilege drive.file is sufficient — no full-drive scope needed.
		addEnv("SCOPE", "drive.file")
		
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
