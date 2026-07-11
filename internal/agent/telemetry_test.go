package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVirtualDriveTotalBytes_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usb_disk.img")
	const size = 4096
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := virtualDriveTotalBytes(path); got != size {
		t.Errorf("virtualDriveTotalBytes(file) = %d, want %d", got, size)
	}
}

func TestVirtualDriveTotalBytes_Missing(t *testing.T) {
	if got := virtualDriveTotalBytes("/nonexistent/path/xyz"); got != 0 {
		t.Errorf("missing path should return 0, got %d", got)
	}
}

func TestReadCPUSample(t *testing.T) {
	idle, total, err := readCPUSample()
	if err != nil {
		t.Skipf("/proc/stat not readable in this environment: %v", err)
	}
	if total == 0 {
		t.Error("total jiffies should be > 0")
	}
	if idle > total {
		t.Errorf("idle (%d) should not exceed total (%d)", idle, total)
	}
}

func TestGetCPUUsage_Range(t *testing.T) {
	usage, err := getCPUUsage()
	if err != nil {
		t.Skipf("cannot sample CPU in this environment: %v", err)
	}
	if usage < 0 || usage > 100 {
		t.Errorf("cpu usage out of range: %f", usage)
	}
}

func TestIsMounted(t *testing.T) {
	if _, err := os.ReadFile("/proc/mounts"); err != nil {
		t.Skip("/proc/mounts not readable")
	}
	if !isMounted("/") {
		t.Error("root / should be reported as mounted")
	}
	if isMounted("/definitely/not/a/mount/point/xyz") {
		t.Error("bogus path should not be reported as mounted")
	}
}

func TestRateMbps(t *testing.T) {
	// 1,000,000 bytes/s = 8 Mbps.
	if got := rateMbps(1_000_000, 1); got != 8.0 {
		t.Errorf("rateMbps(1MB, 1s) = %f, want 8.0", got)
	}
	// 12,500,000 bytes over 1s = 100 Mbps.
	if got := rateMbps(12_500_000, 1); got != 100.0 {
		t.Errorf("rateMbps(12.5MB, 1s) = %f, want 100.0", got)
	}
	if got := rateMbps(1_000_000, 0); got != 0 {
		t.Errorf("zero/negative elapsed must return 0, got %f", got)
	}
}

func TestParseNvmeSmart_Kelvin(t *testing.T) {
	// nvme-cli style with temperature in Kelvin and percent_used.
	j := []byte(`{"temperature":313,"percent_used":4,"data_units_written":1953125}`)
	temp, wear, writtenTB, ok := parseNvmeSmart(j)
	if !ok {
		t.Fatal("expected ok")
	}
	if temp < 39.8 || temp > 40.2 {
		t.Errorf("temp = %f, want ~40 (313K)", temp)
	}
	if wear != 4 {
		t.Errorf("wear = %f, want 4", wear)
	}
	// 1,953,125 units * 512,000 bytes = 1e12 bytes = 1.0 TB.
	if writtenTB < 0.99 || writtenTB > 1.01 {
		t.Errorf("writtenTB = %f, want ~1.0", writtenTB)
	}
}

func TestParseNvmeSmart_CelsiusAndAltKey(t *testing.T) {
	// temperature already in Celsius, alternate "percentage_used" key.
	j := []byte(`{"temperature":45,"percentage_used":12,"data_units_written":0}`)
	temp, wear, _, ok := parseNvmeSmart(j)
	if !ok {
		t.Fatal("expected ok")
	}
	if temp != 45 {
		t.Errorf("temp = %f, want 45 (already Celsius)", temp)
	}
	if wear != 12 {
		t.Errorf("wear = %f, want 12", wear)
	}
}

func TestParseNvmeSmart_Garbage(t *testing.T) {
	if _, _, _, ok := parseNvmeSmart([]byte("not json")); ok {
		t.Error("garbage should not parse ok")
	}
	if _, _, _, ok := parseNvmeSmart([]byte(`{"unrelated":1}`)); ok {
		t.Error("json without smart fields should return ok=false")
	}
}
