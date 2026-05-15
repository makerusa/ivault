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
)

// Start begins the heartbeat loop in a background goroutine.
func Start(ctx context.Context, cfg *config.Config) {
	if cfg.DeviceID == "" || cfg.DeviceAPIKey == "" || cfg.CloudEndpoint == "" {
		log.Println("agent: device not provisioned, skipping heartbeat")
		return
	}

	log.Printf("agent: starting heartbeat loop for device %s", cfg.DeviceID)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		// Send initial heartbeat immediately
		sendHeartbeat(cfg)

		for {
			select {
			case <-ctx.Done():
				log.Println("agent: stopping heartbeat loop")
				return
			case <-ticker.C:
				sendHeartbeat(cfg)
			}
		}
	}()
}

func sendHeartbeat(cfg *config.Config) {
	stats, err := CollectStats("/nvme") // Assuming /nvme is the data partition
	if err != nil {
		log.Printf("agent: failed to collect stats: %v", err)
	}

	// Add additional metadata (status can be pulled from state machine if we passed it, 
	// but for now we'll just send 'online')
	payload := struct {
		Stats
		Status string `json:"status"`
	}{
		Stats:  stats,
		Status: "online",
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

	// Wait, the portal expects X-User-ID! I need to ensure the config stores it.
	// In the provision step, I didn't save UserID to the config. I should fix that.

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
