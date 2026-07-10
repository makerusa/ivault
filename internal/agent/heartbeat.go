package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/makerusa/ivault/internal/config"
	"github.com/makerusa/ivault/internal/db"
	"github.com/makerusa/ivault/internal/led"
	"github.com/makerusa/ivault/internal/secret"
	"github.com/makerusa/ivault/internal/state"
	"github.com/makerusa/ivault/internal/upload"
)

// statusLED reflects portal-connection status: solid when a heartbeat
// succeeds, slow pulse when it fails or the device isn't provisioned. Set by
// main via SetStatusLED; nil-safe.
var statusLED *led.Indicator

// SetStatusLED wires the shared status LED into the agent.
func SetStatusLED(i *led.Indicator) { statusLED = i }

// secretMgr encrypts/decrypts the cached destinations blob (cloud refresh
// tokens, passwords) at rest in the SQLite config table. Initialised in Start.
var secretMgr *secret.Manager

// encryptBlob encrypts s for storage; returns plaintext unchanged if the
// secret manager is unavailable (best-effort, never blocks persistence).
func encryptBlob(s string) string {
	if secretMgr == nil {
		return s
	}
	if enc, err := secretMgr.Encrypt(s); err == nil {
		return enc
	}
	return s
}

// decryptBlob reverses encryptBlob. Legacy plaintext (no version marker) is
// returned as-is so it migrates to ciphertext on the next write.
func decryptBlob(s string) string {
	if secretMgr == nil {
		return s
	}
	if dec, err := secretMgr.Decrypt(s); err == nil {
		return dec
	}
	return s
}

// agentStarted ensures the heartbeat loop is launched at most once. main calls
// Start at boot and again from the runtime provision-file handler, so without
// this guard a device provisioned at runtime would run two heartbeat loops and
// leak a duplicate state-change handler.
var agentStarted atomic.Bool

// Start begins the heartbeat loop in a background goroutine.
// sm is used to read the current device state for each heartbeat.
func Start(ctx context.Context, cfg *config.Config, sm *state.Machine, database *db.DB) {
	if cfg.DeviceID == "" || cfg.DeviceAPIKey == "" || cfg.CloudEndpoint == "" {
		log.Println("agent: device not provisioned, skipping heartbeat")
		if statusLED != nil {
			statusLED.SlowPulse() // not provisioned → not connected
		}
		return
	}

	// Guard after the provisioned check so a boot-time skip (not yet
	// provisioned) doesn't prevent a later real start once provisioned.
	if !agentStarted.CompareAndSwap(false, true) {
		log.Println("agent: heartbeat already running — skipping duplicate Start")
		return
	}

	// Initialise at-rest encryption for cached credentials. Key lives beside
	// the DB, root-only. On failure we log and continue with plaintext rather
	// than block the appliance.
	if mgr, err := secret.NewManager(filepath.Join(filepath.Dir(cfg.DBPath), "secret.key")); err != nil {
		log.Printf("agent: could not init credential encryption (%v); destinations stored unencrypted", err)
	} else {
		secretMgr = mgr
	}

	// Load persisted destinations on startup for offline resilience
	if val, err := database.GetConfig("active_destinations"); err == nil && val != "" {
		val = decryptBlob(val)
		var rawDests []json.RawMessage
		if err := json.Unmarshal([]byte(val), &rawDests); err == nil {
			UpdateActiveDestinations(rawDests)
			log.Printf("agent: loaded %d persisted active destinations from local database config table", len(rawDests))
			for i, raw := range rawDests {
				var d upload.Destination
				if err := json.Unmarshal(raw, &d); err == nil {
					d.LogDetails(fmt.Sprintf("agent: [startup-load] Destination #%d", i+1))
				} else {
					log.Printf("agent: [startup-load] failed to parse destination #%d: %v", i+1, err)
				}
			}
		} else {
			log.Printf("agent: failed to unmarshal persisted active destinations: %v", err)
		}
	}

	log.Printf("agent: starting heartbeat loop for device %s", cfg.DeviceID)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		trigger := make(chan struct{}, 1)
		
		// Send heartbeat immediately on state transition
		sm.OnChange(func(old, new state.State) {
			select {
			case trigger <- struct{}{}:
			default:
				// Already a trigger pending
			}
		})

		// Send initial heartbeat
		sendHeartbeat(cfg, sm, database)

		for {
			select {
			case <-ctx.Done():
				log.Println("agent: stopping heartbeat loop")
				return
			case <-ticker.C:
				sendHeartbeat(cfg, sm, database)
			case <-trigger:
				log.Println("agent: triggering priority heartbeat due to state change")
				sendHeartbeat(cfg, sm, database)
			}
		}
	}()
}

func sendHeartbeat(cfg *config.Config, sm *state.Machine, database *db.DB) {
	// Measure internal storage on the filesystem that actually holds it (the
	// upload queue lives there), not a hardcoded "/nvme" — the no-NVMe fallback
	// layout mounts internal storage under /var/lib/ivault-storage instead.
	internalPath := cfg.UploadQueue
	if internalPath == "" {
		internalPath = "/nvme"
	}
	stats, err := CollectStats(internalPath, cfg.ImagePath, cfg.MountPoint, cfg.UploadQueue)
	if err != nil {
		log.Printf("agent: failed to collect stats: %v", err)
	}

	// External-drive usage: CollectStats can only read it live during a
	// maintenance mount. While the USB host owns the drive it's unmounted, so
	// backfill the last usage that ingest measured and persisted, giving the
	// portal a real "Virtual Drive" figure instead of 0.
	if !isMounted(cfg.MountPoint) {
		if v, e := database.GetConfig("ext_drive_used_bytes"); e == nil && v != "" {
			if used, pe := strconv.ParseUint(v, 10, 64); pe == nil {
				stats.VirtualDriveUsedGb = float64(used) / (1024 * 1024 * 1024)
			}
		}
		if v, e := database.GetConfig("ext_drive_total_bytes"); e == nil && v != "" {
			if total, pe := strconv.ParseUint(v, 10, 64); pe == nil && total > 0 {
				stats.VirtualDriveTotalGb = float64(total) / (1024 * 1024 * 1024)
			}
		}
	}

	// Include the current device state and discovered local devices.
	currentStatus := sm.State().String()
	discovered := GlobalDiscovery.GetDevices()

	// File delta: send only files changed since the watermark the portal last
	// acknowledged. New files carry full detail; subsequent heartbeats carry
	// just the ones whose state moved. Capped so a big backlog trickles up.
	watermark, _ := database.GetConfig("file_sync_watermark")
	changed, err := database.GetFilesChangedSince(watermark, 200)
	if err != nil {
		log.Printf("agent: failed to read file changes: %v", err)
	}
	newWatermark := watermark
	for _, c := range changed {
		if c.UpdatedAt > newWatermark {
			newWatermark = c.UpdatedAt
		}
	}

	payload := struct {
		Stats
		Status            *string            `json:"status"`
		DiscoveredDevices []DiscoveredDevice `json:"discoveredDevices"`
		Files             []db.FileChange    `json:"files"`
	}{
		Stats:             stats,
		Status:            &currentStatus,
		DiscoveredDevices: discovered,
		Files:             changed,
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/devices/%s/heartbeat", cfg.CloudEndpoint, cfg.DeviceID)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		log.Printf("agent: failed to create request: %v", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", cfg.UserID)
	req.Header.Set("X-Device-Key", cfg.DeviceAPIKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("agent: heartbeat failed: %v", err)
		if statusLED != nil {
			statusLED.SlowPulse() // can't reach portal → not connected
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("agent: portal returned status %d", resp.StatusCode)
		if statusLED != nil {
			statusLED.SlowPulse()
		}
		return
	}

	// Connected: provisioned and portal reachable.
	if statusLED != nil {
		statusLED.Solid()
	}

	// Portal acknowledged delivery — advance the file-sync watermark so acked
	// changes are not resent. On a failed heartbeat we skip this, so those
	// changes stay pending and are retried next time (at-least-once).
	if len(changed) > 0 && newWatermark != watermark {
		if err := database.SetConfig("file_sync_watermark", newWatermark); err != nil {
			log.Printf("agent: failed to persist file sync watermark: %v", err)
		}
	}

	// Check for remote commands and configuration sync
	var response struct {
		Commands      []string           `json:"commands"`
		StorageConfig *json.RawMessage   `json:"storageConfig"`
		Destinations  []json.RawMessage  `json:"destinations"`
		LogLevel      string             `json:"logLevel"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err == nil {
		// Apply the portal's tier-effective log ship level (layer 1).
		SetLogShipLevel(response.LogLevel)
		for _, cmd := range response.Commands {
			if cmd == "trigger_deep_scan" {
				go GlobalDiscovery.TriggerDeepScan(context.Background())
			} else if strings.HasPrefix(cmd, "test_destination:") {
				destID := strings.TrimPrefix(cmd, "test_destination:")
				go testDestination(cfg, destID, response.Destinations)
			} else if strings.HasPrefix(cmd, "discover_shares:") {
				reqJSON := strings.TrimPrefix(cmd, "discover_shares:")
				go runShareDiscovery(cfg, reqJSON)
			} else if cmd == "trigger_sync" {
				log.Println("agent: received trigger_sync command from portal — triggering maintenance cycle via SIGUSR1")
				proc, err := os.FindProcess(os.Getpid())
				if err == nil {
					_ = proc.Signal(syscall.SIGUSR1)
				}
			} else if cmd == "restart_service" {
				log.Println("agent: received restart_service command from portal")
				go func() {
					time.Sleep(1 * time.Second)
					c := exec.Command("sudo", "systemctl", "restart", "ivault.service")
					if out, err := c.CombinedOutput(); err != nil {
						log.Printf("agent: restart_service failed: %v (output: %s)", err, string(out))
					} else {
						log.Println("agent: restart_service successfully initiated")
					}
				}()
			} else if cmd == "reboot" {
				log.Println("agent: received reboot command from portal")
				go func() {
					time.Sleep(1 * time.Second)
					c := exec.Command("sudo", "reboot")
					if out, err := c.CombinedOutput(); err != nil {
						log.Printf("agent: reboot failed: %v (output: %s)", err, string(out))
					} else {
						log.Println("agent: reboot successfully initiated")
					}
				}()
			} else if cmd == "factory_reset" {
				log.Println("agent: received factory_reset command from portal — resetting device!")
				go runFactoryReset(cfg)
			}
		}

		if response.StorageConfig != nil {
			// TODO: Compare with current hardware state and trigger resize/re-label if needed
			// log.Printf("agent: received storage config sync: %s", string(*response.StorageConfig))
		}

		if len(response.Destinations) > 0 {
			UpdateActiveDestinations(response.Destinations)
			log.Printf("agent: synced %d active destinations from portal", len(response.Destinations))
			for i, raw := range response.Destinations {
				var d upload.Destination
				if err := json.Unmarshal(raw, &d); err == nil {
					d.LogDetails(fmt.Sprintf("agent: [portal-sync] Destination #%d", i+1))
				} else {
					log.Printf("agent: [portal-sync] failed to parse destination #%d: %v", i+1, err)
				}
			}
			
			// Persist dynamic destinations to local database config table for offline startup resilience
			log.Printf("agent: persisting %d active destinations to local SQLite database config table...", len(response.Destinations))
			bytes, err := json.Marshal(response.Destinations)
			if err == nil {
				if err := database.SetConfig("active_destinations", encryptBlob(string(bytes))); err != nil {
					log.Printf("agent: failed to persist active destinations to local database: %v", err)
				} else {
					log.Println("agent: successfully persisted active destinations to local SQLite database config table")
				}
			}
		}
	}
}

func testDestination(cfg *config.Config, destID string, rawDests []json.RawMessage) {
	var targetHost string
	var targetPort int = 445 // Default SMB

	// Find the destination in the list we just received
	for _, raw := range rawDests {
		var d struct {
			ID   string `json:"id"`
			Host string `json:"host"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &d); err == nil && d.ID == destID {
			targetHost = d.Host
			if d.Type == "ftp" {
				targetPort = 21
			}
			break
		}
	}

	if targetHost == "" {
		log.Printf("agent: test failed, destination %s not found in response", destID)
		return
	}

	start := time.Now()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", targetHost, targetPort), 5*time.Second)
	latency := time.Since(start).Milliseconds()

	success := err == nil
	if success {
		conn.Close()
		log.Printf("agent: test destination %s (%s) SUCCESS in %dms", destID, targetHost, latency)
	} else {
		log.Printf("agent: test destination %s (%s) FAILED: %v", destID, targetHost, err)
	}

	// Report result back to portal via a separate POST
	reportTestResult(cfg, destID, success, latency, err)
}

func reportTestResult(cfg *config.Config, destID string, success bool, latency int64, dialErr error) {
	msg := "Successfully reached the destination."
	if !success {
		msg = fmt.Sprintf("Failed to connect: %v", dialErr)
	}

	payload := struct {
		Success   bool   `json:"success"`
		Message   string `json:"message"`
		LatencyMs int64  `json:"latencyMs"`
	}{
		Success:   success,
		Message:   msg,
		LatencyMs: latency,
	}

	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/devices/%s/destinations/%s/test-result", cfg.CloudEndpoint, cfg.DeviceID, destID)

	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Key", cfg.DeviceAPIKey)

	client := &http.Client{Timeout: 5 * time.Second}
	_, _ = client.Do(req)
}

func runShareDiscovery(cfg *config.Config, reqJSON string) {
	var req struct {
		Host     string `json:"host"`
		Username string `json:"username"`
		Password string `json:"password"`
		Domain   string `json:"domain"`
	}
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		log.Printf("agent: failed to unmarshal discover_shares req: %v", err)
		return
	}

	log.Printf("agent: running share discovery for %s", req.Host)
	shares, err := listSharesNatively(req.Host, req.Username, req.Password, req.Domain)
	var errStr string
	if err != nil {
		errStr = err.Error()
		log.Printf("agent: share discovery failed: %v", err)
	} else {
		log.Printf("agent: discovered %d writeable shares", len(shares))
	}

	reportShareScanResult(cfg, shares, errStr)
}

func listSharesNatively(host, username, password, domain string) ([]string, error) {
	cmd := exec.Command("rclone", "lsd", "REMOTE:", "--config", "/dev/null")
	cmd.Env = os.Environ()
	
	addEnv := func(key, value string) {
		cmd.Env = append(cmd.Env, "RCLONE_CONFIG_REMOTE_"+key+"="+value)
		cmd.Env = append(cmd.Env, "RCLONE_CONFIG_remote_"+key+"="+value)
	}

	addEnv("TYPE", "smb")
	addEnv("HOST", host)
	addEnv("USER", username)
	addEnv("PASS", obscurePassword(password))
	if domain != "" {
		addEnv("DOMAIN", domain)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, string(out))
	}

	allShares := []string{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, " -1 ")
		if len(parts) >= 2 {
			shareName := strings.TrimSpace(parts[len(parts)-1])
			if shareName != "" && !strings.HasPrefix(shareName, "@") && shareName != "relay-storage" {
				allShares = append(allShares, shareName)
			}
		}
	}

	writeable := []string{}
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, s := range allShares {
		wg.Add(1)
		go func(share string) {
			defer wg.Done()
			if testWriteAccessNatively(host, username, password, domain, share) {
				mu.Lock()
				writeable = append(writeable, share)
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()

	return writeable, nil
}

func testWriteAccessNatively(host, username, password, domain, share string) bool {
	tempFile, err := os.CreateTemp("", "ivault_write_test_*.txt")
	if err != nil {
		return false
	}
	defer os.Remove(tempFile.Name())
	_, _ = tempFile.WriteString("test")
	_ = tempFile.Close()

	cmd := exec.Command("rclone", "copyto",
		"--config", "/dev/null",
		"--retries", "1",
		"--low-level-retries", "1",
		tempFile.Name(), "REMOTE:"+share+"/write_test.txt",
	)
	cmd.Env = os.Environ()
	
	addEnv := func(key, value string) {
		cmd.Env = append(cmd.Env, "RCLONE_CONFIG_REMOTE_"+key+"="+value)
		cmd.Env = append(cmd.Env, "RCLONE_CONFIG_remote_"+key+"="+value)
	}

	addEnv("TYPE", "smb")
	addEnv("HOST", host)
	addEnv("USER", username)
	addEnv("PASS", obscurePassword(password))
	if domain != "" {
		addEnv("DOMAIN", domain)
	}

	if err := cmd.Run(); err != nil {
		return false
	}

	delCmd := exec.Command("rclone", "deletefile",
		"--config", "/dev/null",
		"REMOTE:"+share+"/write_test.txt",
	)
	delCmd.Env = cmd.Env
	_ = delCmd.Run()

	return true
}

func reportShareScanResult(cfg *config.Config, shares []string, errStr string) {
	payload := struct {
		Shares []string `json:"shares"`
		Error  string   `json:"error"`
	}{
		Shares: shares,
		Error:  errStr,
	}

	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/devices/%s/share-scan-result", cfg.CloudEndpoint, cfg.DeviceID)

	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Key", cfg.DeviceAPIKey)

	client := &http.Client{Timeout: 5 * time.Second}
	_, _ = client.Do(req)
}

func obscurePassword(p string) string {
	cmd := exec.Command("rclone", "obscure", p)
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func runFactoryReset(cfg *config.Config) {
	time.Sleep(1 * time.Second)

	// 1. Wipe config file
	log.Println("agent: wiping configuration file...")
	_ = os.Remove("/etc/ivault/config.json")

	// 2. Wipe SQLite database file
	if cfg.DBPath != "" {
		log.Printf("agent: wiping database file %s...", cfg.DBPath)
		_ = os.Remove(cfg.DBPath)
		_ = os.Remove(cfg.DBPath + "-wal")
		_ = os.Remove(cfg.DBPath + "-shm")
	}

	// 3. Clear upload queue files (but keep the directory)
	if cfg.UploadQueue != "" {
		log.Printf("agent: clearing upload queue %s...", cfg.UploadQueue)
		if entries, err := os.ReadDir(cfg.UploadQueue); err == nil {
			for _, entry := range entries {
				_ = os.RemoveAll(cfg.UploadQueue + "/" + entry.Name())
			}
		}
	}

	// 4. Reboot
	log.Println("agent: factory reset complete. Rebooting hardware...")
	c := exec.Command("sudo", "reboot")
	if out, err := c.CombinedOutput(); err != nil {
		log.Printf("agent: reboot failed: %v (output: %s)", err, string(out))
	}
}
