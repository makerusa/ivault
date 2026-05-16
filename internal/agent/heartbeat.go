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

	// Check for remote commands
	var response struct {
		Commands []string `json:"commands"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err == nil {
		for _, cmd := range response.Commands {
			if cmd == "trigger_deep_scan" {
				// Use background context as the scan might outlive the heartbeat cycle
				go GlobalDiscovery.TriggerDeepScan(context.Background())
			}
		}
	}
}
