package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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

		// Send initial heartbeat immediately
		sendHeartbeat(cfg, sm)

		for {
			select {
			case <-ctx.Done():
				log.Println("agent: stopping heartbeat loop")
				return
			case <-ticker.C:
				sendHeartbeat(cfg, sm)
			}
		}
	}()
}

func sendHeartbeat(cfg *config.Config, sm *state.Machine) {
	stats, err := CollectStats("/nvme") // Assuming /nvme is the data partition
	if err != nil {
		log.Printf("agent: failed to collect stats: %v", err)
	}

	// Include the current device state so the portal dashboard stays in sync.
	currentStatus := sm.State().String()
	payload := struct {
		Stats
		Status *string `json:"status"`
	}{
		Stats:  stats,
		Status: &currentStatus,
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

	// We can decode the response to check for remote commands (e.g. reboot, factory reset)
}
