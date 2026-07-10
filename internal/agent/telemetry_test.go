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
