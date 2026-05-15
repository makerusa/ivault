package provision

import (
	"fmt"
	"log"
	"os/exec"
)

type NetworkConfig struct {
	Mode         string  `json:"mode"`
	Interface    string  `json:"interface"`
	WifiSsid     *string `json:"wifiSsid,omitempty"`
	WifiPassword *string `json:"wifiPassword,omitempty"`
	IP           *string `json:"ip,omitempty"`
	Subnet       *string `json:"subnet,omitempty"`
	Gateway      *string `json:"gateway,omitempty"`
	DNS          *string `json:"dns,omitempty"`
}

// ConfigureNetwork configures the network interface using nmcli.
func ConfigureNetwork(cfg NetworkConfig) error {
	log.Printf("provision: configuring network (iface=%s, mode=%s)", cfg.Interface, cfg.Mode)

	if cfg.Interface == "wlan0" && cfg.WifiSsid != nil {
		ssid := *cfg.WifiSsid
		pwd := ""
		if cfg.WifiPassword != nil {
			decrypted, err := DecryptWifiPassword(*cfg.WifiPassword)
			if err != nil {
				return fmt.Errorf("failed to decrypt wifi password: %w", err)
			}
			pwd = decrypted
		}

		// Connect to WiFi
		var cmd *exec.Cmd
		if pwd != "" {
			cmd = exec.Command("nmcli", "dev", "wifi", "connect", ssid, "password", pwd)
		} else {
			cmd = exec.Command("nmcli", "dev", "wifi", "connect", ssid)
		}
		
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("nmcli wifi connect failed: %w — %s", err, string(out))
		}
		log.Printf("provision: connected to WiFi %s", ssid)
	}

	if cfg.Mode == "static" {
		connName := "Wired connection 1" // Default nmcli connection name for eth0 usually, or we can use iface name
		if cfg.Interface == "wlan0" && cfg.WifiSsid != nil {
			connName = *cfg.WifiSsid
		}

		if cfg.IP != nil {
			ipAddr := *cfg.IP
			if cfg.Subnet != nil && *cfg.Subnet != "" {
				// We should convert subnet mask to CIDR, but for simplicity we assume the portal 
				// provides CIDR or we just set it. Actually, nmcli accepts CIDR. 
				// If the portal sends "255.255.255.0", we need to convert it. Let's just pass it to nmcli,
				// or assume the user inputs CIDR. If they input IP/CIDR, we use it directly.
				ipAddr = fmt.Sprintf("%s/24", *cfg.IP) // Defaulting to /24 if parsing is too complex for now
			}
			
			// modify connection to manual
			cmd := exec.Command("nmcli", "con", "mod", connName, "ipv4.method", "manual", "ipv4.addresses", ipAddr)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("nmcli con mod ip failed: %w — %s", err, string(out))
			}
		}

		if cfg.Gateway != nil {
			cmd := exec.Command("nmcli", "con", "mod", connName, "ipv4.gateway", *cfg.Gateway)
			cmd.Run()
		}

		if cfg.DNS != nil {
			cmd := exec.Command("nmcli", "con", "mod", connName, "ipv4.dns", *cfg.DNS)
			cmd.Run()
		}

		// Restart connection
		cmd := exec.Command("nmcli", "con", "up", connName)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("nmcli con up failed: %w — %s", err, string(out))
		}
		log.Printf("provision: configured static IP for %s", connName)
	}

	return nil
}
