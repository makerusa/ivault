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
	"strings"
	"sync"
	"time"

	"github.com/makerusa/ivault/internal/config"
	"github.com/makerusa/ivault/internal/state"
)

// Start begins the heartbeat loop in a background goroutine.
// sm is used to read the current device state for each heartbeat.
func Start(ctx context.Context, cfg *config.Config, sm *state.Machine) {
	if cfg.DeviceID == "" || cfg.DeviceAPIKey == "" || cfg.CloudEndpoint == "" {
		log.Println("agent: device not provisioned, skipping heartbeat")
		return
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
		sendHeartbeat(cfg, sm)

		for {
			select {
			case <-ctx.Done():
				log.Println("agent: stopping heartbeat loop")
				return
			case <-ticker.C:
				sendHeartbeat(cfg, sm)
			case <-trigger:
				log.Println("agent: triggering priority heartbeat due to state change")
				sendHeartbeat(cfg, sm)
			}
		}
	}()
}

func sendHeartbeat(cfg *config.Config, sm *state.Machine) {
	stats, err := CollectStats("/nvme", cfg.ImagePath, cfg.UploadQueue)
	if err != nil {
		log.Printf("agent: failed to collect stats: %v", err)
	}

	// Include the current device state and discovered local devices.
	currentStatus := sm.State().String()
	discovered := GlobalDiscovery.GetDevices()

	payload := struct {
		Stats
		Status            *string            `json:"status"`
		DiscoveredDevices []DiscoveredDevice `json:"discoveredDevices"`
	}{
		Stats:             stats,
		Status:            &currentStatus,
		DiscoveredDevices: discovered,
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
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("agent: portal returned status %d", resp.StatusCode)
		return
	}

	// Check for remote commands and configuration sync
	var response struct {
		Commands      []string           `json:"commands"`
		StorageConfig *json.RawMessage   `json:"storageConfig"`
		Destinations  []json.RawMessage  `json:"destinations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err == nil {
		for _, cmd := range response.Commands {
			if cmd == "trigger_deep_scan" {
				go GlobalDiscovery.TriggerDeepScan(context.Background())
			} else if strings.HasPrefix(cmd, "test_destination:") {
				destID := strings.TrimPrefix(cmd, "test_destination:")
				go testDestination(cfg, destID, response.Destinations)
			} else if strings.HasPrefix(cmd, "discover_shares:") {
				reqJSON := strings.TrimPrefix(cmd, "discover_shares:")
				go runShareDiscovery(cfg, reqJSON)
			}
		}

		if response.StorageConfig != nil {
			// TODO: Compare with current hardware state and trigger resize/re-label if needed
			// log.Printf("agent: received storage config sync: %s", string(*response.StorageConfig))
		}

		if len(response.Destinations) > 0 {
			UpdateActiveDestinations(response.Destinations)
			log.Printf("agent: synced %d active destinations from portal", len(response.Destinations))
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
	cmd := exec.Command("rclone", "lsd", "remote:", "--config", "/dev/null")
	cmd.Env = os.Environ()
	prefix := "RCLONE_CONFIG_REMOTE_"
	cmd.Env = append(cmd.Env, prefix+"TYPE=smb")
	cmd.Env = append(cmd.Env, prefix+"HOST="+host)
	cmd.Env = append(cmd.Env, prefix+"USER="+username)
	cmd.Env = append(cmd.Env, prefix+"PASS="+obscurePassword(password))
	if domain != "" {
		cmd.Env = append(cmd.Env, prefix+"DOMAIN="+domain)
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
			if shareName != "" && !strings.HasPrefix(shareName, "@") {
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
		tempFile.Name(), "remote:"+share+"/write_test.txt",
	)
	cmd.Env = os.Environ()
	prefix := "RCLONE_CONFIG_REMOTE_"
	cmd.Env = append(cmd.Env, prefix+"TYPE=smb")
	cmd.Env = append(cmd.Env, prefix+"HOST="+host)
	cmd.Env = append(cmd.Env, prefix+"USER="+username)
	cmd.Env = append(cmd.Env, prefix+"PASS="+obscurePassword(password))
	if domain != "" {
		cmd.Env = append(cmd.Env, prefix+"DOMAIN="+domain)
	}

	if err := cmd.Run(); err != nil {
		return false
	}

	delCmd := exec.Command("rclone", "deletefile",
		"--config", "/dev/null",
		"remote:"+share+"/write_test.txt",
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
