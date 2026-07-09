package provision

import (
	"strings"
	"testing"
)

func ptr(s string) *string { return &s }

func TestBuildWpaSupplicantConf_WPA(t *testing.T) {
	got := buildWpaSupplicantConf("MyNet", "s3cr3t")
	for _, want := range []string{
		"ctrl_interface=/run/wpa_supplicant",
		"update_config=1",
		`ssid="MyNet"`,
		`psk="s3cr3t"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("conf missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "key_mgmt=NONE") {
		t.Errorf("secured network should not be key_mgmt=NONE:\n%s", got)
	}
}

func TestBuildWpaSupplicantConf_Open(t *testing.T) {
	got := buildWpaSupplicantConf("OpenNet", "")
	if !strings.Contains(got, "key_mgmt=NONE") {
		t.Errorf("open network should be key_mgmt=NONE:\n%s", got)
	}
	if strings.Contains(got, "psk=") {
		t.Errorf("open network should have no psk:\n%s", got)
	}
}

func TestWpaQuoteEscaping(t *testing.T) {
	// A password containing a quote and a backslash must be escaped.
	got := wpaQuote(`a"b\c`)
	want := `"a\"b\\c"`
	if got != want {
		t.Errorf("wpaQuote = %q, want %q", got, want)
	}
}

func TestBuildNetworkdConf_DHCP(t *testing.T) {
	got := buildNetworkdConf("wlP2p33s0", NetworkConfig{Mode: "dhcp"})
	if !strings.Contains(got, "Name=wlP2p33s0") {
		t.Errorf("missing Match Name:\n%s", got)
	}
	if !strings.Contains(got, "DHCP=yes") {
		t.Errorf("dhcp mode should emit DHCP=yes:\n%s", got)
	}
	if strings.Contains(got, "Address=") {
		t.Errorf("dhcp mode should not emit Address:\n%s", got)
	}
}

func TestBuildNetworkdConf_Static(t *testing.T) {
	cfg := NetworkConfig{
		Mode:    "static",
		IP:      ptr("192.168.1.50"),
		Subnet:  ptr("255.255.255.0"),
		Gateway: ptr("192.168.1.1"),
		DNS:     ptr("1.1.1.1, 8.8.8.8"),
	}
	got := buildNetworkdConf("eth0", cfg)
	for _, want := range []string{
		"Address=192.168.1.50/24",
		"Gateway=192.168.1.1",
		"DNS=1.1.1.1",
		"DNS=8.8.8.8",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("static conf missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "DHCP=yes") {
		t.Errorf("static mode should not emit DHCP=yes:\n%s", got)
	}
}

func TestStaticCIDR(t *testing.T) {
	cases := []struct {
		ip     string
		subnet *string
		want   string
	}{
		{"10.0.0.5/16", nil, "10.0.0.5/16"},              // already CIDR → unchanged
		{"192.168.1.5", ptr("255.255.255.0"), "192.168.1.5/24"},
		{"192.168.1.5", ptr("255.255.0.0"), "192.168.1.5/16"},
		{"192.168.1.5", ptr("/25"), "192.168.1.5/25"},    // plain prefix
		{"192.168.1.5", nil, "192.168.1.5/24"},           // default
		{"192.168.1.5", ptr("garbage"), "192.168.1.5/24"}, // unparseable → default
	}
	for _, c := range cases {
		if got := staticCIDR(c.ip, c.subnet); got != c.want {
			t.Errorf("staticCIDR(%q, %v) = %q, want %q", c.ip, c.subnet, got, c.want)
		}
	}
}
