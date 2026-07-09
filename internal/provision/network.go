package provision

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// ConfigureNetwork applies the provisioned network settings.
//
// It does NOT assume a fixed interface name: Wi-Fi is configured whenever an
// SSID is present, and the wireless interface is auto-detected (interface names
// are not portable — e.g. the RK3588 Wi-Fi adapter enumerates as "wlP2p33s0",
// not "wlan0"). It also does NOT assume NetworkManager: it uses nmcli only when
// NetworkManager is actually running, and otherwise falls back to
// systemd-networkd + wpa_supplicant, which is the default stack on Armbian
// minimal images.
func ConfigureNetwork(cfg NetworkConfig) error {
	log.Printf("provision: configuring network (iface=%q, mode=%q, wifi=%v)",
		cfg.Interface, cfg.Mode, cfg.WifiSsid != nil && *cfg.WifiSsid != "")

	// Wi-Fi requested whenever an SSID is present, regardless of the interface
	// string the portal happened to send.
	if cfg.WifiSsid != nil && *cfg.WifiSsid != "" {
		return configureWifi(cfg)
	}

	// Wired: only a static config needs action; DHCP is the image default and
	// is already up (we reached the portal to get provisioned).
	if cfg.Mode == "static" {
		iface := cfg.Interface
		if iface == "" {
			iface = "eth0"
		}
		return configureStaticWired(iface, cfg)
	}

	log.Printf("provision: wired DHCP — leaving existing network configuration in place")
	return nil
}

func configureWifi(cfg NetworkConfig) error {
	ssid := *cfg.WifiSsid
	pwd := ""
	if cfg.WifiPassword != nil && *cfg.WifiPassword != "" {
		decrypted, err := DecryptWifiPassword(*cfg.WifiPassword)
		if err != nil {
			return fmt.Errorf("failed to decrypt wifi password: %w", err)
		}
		pwd = decrypted
	}

	iface, err := resolveWirelessInterface(cfg.Interface)
	if err != nil {
		return fmt.Errorf("cannot configure Wi-Fi: %w", err)
	}
	log.Printf("provision: configuring Wi-Fi on interface %q (ssid=%q)", iface, ssid)

	if networkManagerAvailable() {
		return wifiViaNetworkManager(iface, ssid, pwd, cfg)
	}
	return wifiViaNetworkd(iface, ssid, pwd, cfg)
}

// resolveWirelessInterface returns the name of a wireless interface, preferring
// the one the portal named if it is actually wireless, otherwise the first
// wireless interface found under /sys/class/net.
func resolveWirelessInterface(preferred string) (string, error) {
	if preferred != "" && isWireless(preferred) {
		return preferred, nil
	}
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return "", fmt.Errorf("read /sys/class/net: %w", err)
	}
	for _, e := range entries {
		if isWireless(e.Name()) {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("no wireless interface found under /sys/class/net (checked for a wireless/phy80211 device)")
}

// isWireless reports whether iface is a wireless device. A wireless interface
// has a "wireless" directory or a "phy80211" symlink under its sysfs node.
func isWireless(iface string) bool {
	if iface == "" {
		return false
	}
	base := filepath.Join("/sys/class/net", iface)
	if _, err := os.Stat(filepath.Join(base, "wireless")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(base, "phy80211")); err == nil {
		return true
	}
	return false
}

// networkManagerAvailable reports whether nmcli exists AND NetworkManager is the
// active network manager. Both must hold; on Armbian minimal nmcli is usually
// absent and systemd-networkd is in charge.
func networkManagerAvailable() bool {
	if _, err := exec.LookPath("nmcli"); err != nil {
		return false
	}
	out, _ := exec.Command("systemctl", "is-active", "NetworkManager").CombinedOutput()
	return strings.TrimSpace(string(out)) == "active"
}

// ── NetworkManager backend ───────────────────────────────────────────────────

func wifiViaNetworkManager(iface, ssid, pwd string, cfg NetworkConfig) error {
	_ = exec.Command("nmcli", "radio", "wifi", "on").Run()

	args := []string{"dev", "wifi", "connect", ssid, "ifname", iface}
	if pwd != "" {
		args = append(args, "password", pwd)
	}
	if out, err := exec.Command("nmcli", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli wifi connect failed: %w — %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("provision: connected to Wi-Fi %q via NetworkManager", ssid)

	// nmcli names the new connection after the SSID.
	if cfg.Mode == "static" {
		return nmApplyStatic(ssid, cfg)
	}
	return nil
}

func nmActiveConnection(iface string) string {
	out, err := exec.Command("nmcli", "-g", "GENERAL.CONNECTION", "dev", "show", iface).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func nmApplyStatic(conn string, cfg NetworkConfig) error {
	if conn == "" {
		return fmt.Errorf("no NetworkManager connection found to apply a static IP to")
	}
	addr := ""
	if cfg.IP != nil {
		addr = staticCIDR(*cfg.IP, cfg.Subnet)
	}
	args := []string{"con", "mod", conn, "ipv4.method", "manual", "ipv4.addresses", addr}
	if cfg.Gateway != nil && *cfg.Gateway != "" {
		args = append(args, "ipv4.gateway", *cfg.Gateway)
	}
	if cfg.DNS != nil && *cfg.DNS != "" {
		args = append(args, "ipv4.dns", *cfg.DNS)
	}
	if out, err := exec.Command("nmcli", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli con mod failed: %w — %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("nmcli", "con", "up", conn).CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli con up failed: %w — %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("provision: applied static IP to connection %q", conn)
	return nil
}

// ── systemd-networkd + wpa_supplicant backend (Armbian minimal default) ──────

func wifiViaNetworkd(iface, ssid, pwd string, cfg NetworkConfig) error {
	if _, err := exec.LookPath("wpa_supplicant"); err != nil {
		return fmt.Errorf("wpa_supplicant is not installed and NetworkManager is unavailable — cannot configure Wi-Fi (install wpasupplicant)")
	}

	// 1. Per-interface wpa_supplicant config (matches the wpa_supplicant@.service
	//    template: /etc/wpa_supplicant/wpa_supplicant-<iface>.conf).
	if err := os.MkdirAll("/etc/wpa_supplicant", 0o755); err != nil {
		return fmt.Errorf("create /etc/wpa_supplicant: %w", err)
	}
	wpaPath := fmt.Sprintf("/etc/wpa_supplicant/wpa_supplicant-%s.conf", iface)
	if err := os.WriteFile(wpaPath, []byte(buildWpaSupplicantConf(ssid, pwd)), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", wpaPath, err)
	}

	// 2. systemd-networkd config bringing the interface up (DHCP or static).
	if err := os.MkdirAll("/etc/systemd/network", 0o755); err != nil {
		return fmt.Errorf("create /etc/systemd/network: %w", err)
	}
	netPath := fmt.Sprintf("/etc/systemd/network/25-ivault-%s.network", iface)
	if err := os.WriteFile(netPath, []byte(buildNetworkdConf(iface, cfg)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", netPath, err)
	}

	// 3. Bring it all up.
	_ = exec.Command("ip", "link", "set", iface, "up").Run()
	svc := fmt.Sprintf("wpa_supplicant@%s.service", iface)
	if out, err := exec.Command("systemctl", "enable", "--now", svc).CombinedOutput(); err != nil {
		return fmt.Errorf("enable %s failed: %w — %s", svc, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "enable", "--now", "systemd-networkd").CombinedOutput(); err != nil {
		return fmt.Errorf("enable systemd-networkd failed: %w — %s", err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("systemctl", "restart", "systemd-networkd").Run()

	log.Printf("provision: configured Wi-Fi on %s via systemd-networkd + wpa_supplicant (ssid=%q)", iface, ssid)
	return nil
}

func configureStaticWired(iface string, cfg NetworkConfig) error {
	if networkManagerAvailable() {
		return nmApplyStatic(nmActiveConnection(iface), cfg)
	}
	if err := os.MkdirAll("/etc/systemd/network", 0o755); err != nil {
		return fmt.Errorf("create /etc/systemd/network: %w", err)
	}
	netPath := fmt.Sprintf("/etc/systemd/network/25-ivault-%s.network", iface)
	if err := os.WriteFile(netPath, []byte(buildNetworkdConf(iface, cfg)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", netPath, err)
	}
	if out, err := exec.Command("systemctl", "restart", "systemd-networkd").CombinedOutput(); err != nil {
		return fmt.Errorf("restart systemd-networkd failed: %w — %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("provision: configured static wired IP on %s via systemd-networkd", iface)
	return nil
}

// ── Config file builders (pure — unit tested) ────────────────────────────────

// buildWpaSupplicantConf renders a wpa_supplicant config for a single network.
// An empty password produces an open (key_mgmt=NONE) network.
func buildWpaSupplicantConf(ssid, pwd string) string {
	var b strings.Builder
	b.WriteString("ctrl_interface=/run/wpa_supplicant\n")
	b.WriteString("update_config=1\n\n")
	b.WriteString("network={\n")
	fmt.Fprintf(&b, "\tssid=%s\n", wpaQuote(ssid))
	if pwd == "" {
		b.WriteString("\tkey_mgmt=NONE\n")
	} else {
		fmt.Fprintf(&b, "\tpsk=%s\n", wpaQuote(pwd))
	}
	b.WriteString("}\n")
	return b.String()
}

// wpaQuote quotes a string for a wpa_supplicant quoted field, escaping the
// backslash and double-quote sequences its parser understands.
func wpaQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// buildNetworkdConf renders a systemd-networkd .network file for iface: a static
// address when cfg.Mode == "static" and an IP is supplied, otherwise DHCP.
func buildNetworkdConf(iface string, cfg NetworkConfig) string {
	var b strings.Builder
	b.WriteString("[Match]\n")
	fmt.Fprintf(&b, "Name=%s\n\n", iface)
	b.WriteString("[Network]\n")
	if cfg.Mode == "static" && cfg.IP != nil && *cfg.IP != "" {
		fmt.Fprintf(&b, "Address=%s\n", staticCIDR(*cfg.IP, cfg.Subnet))
		if cfg.Gateway != nil && *cfg.Gateway != "" {
			fmt.Fprintf(&b, "Gateway=%s\n", *cfg.Gateway)
		}
		if cfg.DNS != nil && *cfg.DNS != "" {
			for _, d := range strings.FieldsFunc(*cfg.DNS, func(r rune) bool { return r == ',' || r == ' ' }) {
				if d != "" {
					fmt.Fprintf(&b, "DNS=%s\n", d)
				}
			}
		}
	} else {
		b.WriteString("DHCP=yes\n")
	}
	return b.String()
}

// staticCIDR combines an IP and optional subnet into CIDR notation. If the IP
// already carries a prefix it is returned as-is; a dotted-quad mask is converted
// to a prefix length; anything else defaults to /24.
func staticCIDR(ip string, subnet *string) string {
	if strings.Contains(ip, "/") {
		return ip
	}
	prefix := 24
	if subnet != nil && *subnet != "" {
		if p, ok := maskToPrefix(*subnet); ok {
			prefix = p
		} else if p, err := parsePlainPrefix(*subnet); err == nil {
			prefix = p
		}
	}
	return fmt.Sprintf("%s/%d", ip, prefix)
}

// maskToPrefix converts a dotted-quad netmask (e.g. 255.255.255.0) to a prefix
// length (e.g. 24). Returns false if the string is not a dotted-quad mask.
func maskToPrefix(mask string) (int, bool) {
	parts := strings.Split(mask, ".")
	if len(parts) != 4 {
		return 0, false
	}
	prefix := 0
	for _, p := range parts {
		var octet int
		if _, err := fmt.Sscanf(p, "%d", &octet); err != nil || octet < 0 || octet > 255 {
			return 0, false
		}
		for octet > 0 {
			prefix += octet & 1
			octet >>= 1
		}
	}
	return prefix, true
}

func parsePlainPrefix(s string) (int, error) {
	var p int
	if _, err := fmt.Sscanf(strings.TrimPrefix(s, "/"), "%d", &p); err != nil {
		return 0, err
	}
	if p < 0 || p > 32 {
		return 0, fmt.Errorf("prefix out of range: %d", p)
	}
	return p, nil
}
