package provision

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/makerusa/ivault/internal/config"
)

type ProvisionFile struct {
	Version       int           `json:"version"`
	UserID        string        `json:"userId"`
	DeviceID      string        `json:"deviceId"`
	DeviceName    string        `json:"deviceName"`
	Token         string        `json:"token"`
	TokenExpires  string        `json:"tokenExpires"`
	CloudEndpoint string        `json:"cloudEndpoint"`
	Network       NetworkConfig `json:"network"`
}

// Process checks for ivault.provision in the mount point, and if found,
// executes the provisioning sequence.
func Process(mountPoint string, cfgPath string) error {
	provisionPath := filepath.Join(mountPoint, "ivault.provision")
	
	if _, err := os.Stat(provisionPath); os.IsNotExist(err) {
		return nil // No provision file found, nothing to do
	}

	log.Println("provision: ivault.provision detected. Starting provisioning sequence...")

	data, err := os.ReadFile(provisionPath)
	if err != nil {
		return fmt.Errorf("failed to read provision file: %w", err)
	}

	var pf ProvisionFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return fmt.Errorf("failed to parse provision file: %w", err)
	}

	// 1. Configure Network
	if err := ConfigureNetwork(pf.Network); err != nil {
		log.Printf("provision: network configuration failed: %v", err)
		// Continue anyway, maybe it's already connected via ethernet
	}

	// Wait a moment for network to settle
	time.Sleep(3 * time.Second)

	// 2. Bootstrap with Cloud API
	log.Printf("provision: bootstrapping with cloud API at %s", pf.CloudEndpoint)
	apiKey, err := bootstrapDevice(pf.CloudEndpoint, pf.DeviceID, pf.UserID, pf.Token)
	if err != nil {
		return fmt.Errorf("bootstrap failed: %w", err)
	}

	// 3. Save Configuration
	updates := config.Config{
		DeviceID:      pf.DeviceID,
		DeviceAPIKey:  apiKey,
		CloudEndpoint: pf.CloudEndpoint,
	}
	if err := config.UpdateConfig(cfgPath, updates); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	log.Println("provision: configuration saved successfully")

	// 4. Delete provision file
	if err := os.Remove(provisionPath); err != nil {
		log.Printf("provision: failed to delete provision file: %v", err)
	} else {
		log.Println("provision: ivault.provision deleted from drive")
	}

	log.Println("provision: sequence complete!")
	return nil
}

func bootstrapDevice(endpoint, deviceID, userID, token string) (string, error) {
	url := fmt.Sprintf("%s/api/devices/%s/bootstrap", endpoint, deviceID)
	
	// The portal heartbeat handler expects an empty JSON body (or ignores it)
	// and reads X-User-ID and X-Provision-Token from the headers.
	req, err := http.NewRequest("POST", url, bytes.NewBuffer([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", userID)
	req.Header.Set("X-Provision-Token", token)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("server returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		DeviceAPIKey string `json:"device_api_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.DeviceAPIKey == "" {
		return "", fmt.Errorf("received empty api key")
	}

	return result.DeviceAPIKey, nil
}
