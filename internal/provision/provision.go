package provision

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

// Detect reports whether a provision file is present at mountPoint, without
// processing it. Used to light the "provisioning in progress" indicator as
// early as possible.
func Detect(mountPoint string) bool {
	_, err := os.Stat(filepath.Join(mountPoint, "ivault.provision"))
	return err == nil
}

// Process checks for ivault.provision in the mount point, and if found,
// executes the provisioning sequence. Returns (true, nil) if provisioning
// was successful, (false, nil) if no file was found, or (false, error) on failure.
func Process(mountPoint string, cfgPath string) (bool, error) {
	provisionPath := filepath.Join(mountPoint, "ivault.provision")
	
	if _, err := os.Stat(provisionPath); os.IsNotExist(err) {
		return false, nil // No provision file found, nothing to do
	} else if err != nil {
		log.Printf("provision: error checking for file: %v", err)
		return false, nil
	}

	log.Println("provision: ivault.provision detected. Starting provisioning sequence...")

	data, err := os.ReadFile(provisionPath)
	if err != nil {
		return false, fmt.Errorf("failed to read provision file: %w", err)
	}

	var pf ProvisionFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return false, fmt.Errorf("failed to parse provision file: %w", err)
	}

	// Always delete the provision file once we've started processing it to avoid 
	// infinite retry loops if the handshake fails.
	defer os.Remove(provisionPath)

	// 1. Configure Network
	if err := ConfigureNetwork(pf.Network); err != nil {
		log.Printf("provision: network configuration failed: %v", err)
		// Continue anyway, maybe it's already connected via ethernet
	}

	// Wait for the network to actually be usable before contacting the portal.
	// After ConfigureNetwork returns, Wi-Fi association + DHCP + DNS can take
	// many seconds; the old fixed 3s sleep raced ahead and the bootstrap DNS
	// lookup failed. Poll until the portal endpoint resolves and is reachable.
	if err := waitForNetwork(pf.CloudEndpoint, 90*time.Second); err != nil {
		log.Printf("provision: %v — attempting bootstrap anyway", err)
	}

	// 2. Bootstrap with Cloud API
	log.Printf("provision: bootstrapping with cloud API at %s", pf.CloudEndpoint)
	apiKey, err := bootstrapDevice(pf.CloudEndpoint, pf.DeviceID, pf.UserID, pf.Token)
	if err != nil {
		// If the server says it's already provisioned, we can treat this as a success 
		// (though we won't have the API key if this is our first time seeing the file 
		// and it failed previously). 
		if strings.Contains(err.Error(), "already provisioned") {
			log.Println("provision: device is already provisioned on server")
			return true, nil
		}
		return false, fmt.Errorf("bootstrap failed: %w", err)
	}

	// 3. Save Configuration
	updates := config.Config{
		UserID:        pf.UserID,
		DeviceID:      pf.DeviceID,
		DeviceName:    pf.DeviceName,
		DeviceAPIKey:  apiKey,
		CloudEndpoint: pf.CloudEndpoint,
	}
	if err := config.UpdateConfig(cfgPath, updates); err != nil {
		return false, fmt.Errorf("failed to save config: %w", err)
	}

	log.Println("provision: configuration saved successfully")

	log.Println("provision: sequence complete!")
	return true, nil
}

// waitForNetwork blocks until the portal endpoint's host resolves via DNS and a
// TCP connection to it succeeds, or until timeout. This replaces a fixed sleep
// so provisioning over freshly-configured Wi-Fi waits for association + DHCP +
// DNS to complete instead of racing ahead and failing the bootstrap lookup.
func waitForNetwork(endpoint string, timeout time.Duration) error {
	host, port := endpointHostPort(endpoint)
	if host == "" {
		return fmt.Errorf("could not parse host from endpoint %q", endpoint)
	}
	addr := net.JoinHostPort(host, port)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		if _, err := net.LookupHost(host); err != nil {
			lastErr = err
			log.Printf("provision: waiting for network — DNS for %s not resolving yet (attempt %d)", host, attempt)
			time.Sleep(2 * time.Second)
			continue
		}
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			lastErr = err
			log.Printf("provision: waiting for network — %s not reachable yet (attempt %d)", addr, attempt)
			time.Sleep(2 * time.Second)
			continue
		}
		_ = conn.Close()
		log.Printf("provision: network is up — %s reachable after %d attempt(s)", addr, attempt)
		return nil
	}
	return fmt.Errorf("network did not come up within %s (last error: %v)", timeout, lastErr)
}

// endpointHostPort extracts the host and port from a cloud endpoint URL,
// defaulting the port from the scheme (443 for https, 80 for http).
func endpointHostPort(endpoint string) (host, port string) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return "", ""
	}
	host = u.Hostname()
	port = u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	return host, port
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
		DeviceAPIKey string `json:"deviceApiKey"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.DeviceAPIKey == "" {
		return "", fmt.Errorf("received empty api key")
	}

	return result.DeviceAPIKey, nil
}
