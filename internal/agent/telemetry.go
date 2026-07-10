package agent

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version is the firmware version reported to the portal. Override at build
// time with -ldflags "-X github.com/makerusa/ivault/internal/agent.Version=..."
var Version = "0.2.0"

type Stats struct {
	CPUPercent    float64 `json:"cpuPercent"`
	MemUsedGb     float64 `json:"memUsedGb"`
	MemTotalGb    float64 `json:"memTotalGb"`
	TempCelsius   float64 `json:"tempCelsius"`
	NvmeUsedGb    float64 `json:"nvmeUsedGb"`
	NvmeTotalGb   float64 `json:"nvmeTotalGb"`
	UptimeSeconds int     `json:"uptimeSeconds"`

	// Networking
	NetworkRxMbps    float64 `json:"networkRxMbps"`
	NetworkTxMbps    float64 `json:"networkTxMbps"`
	IPAddress        string  `json:"ipAddress"`
	MACAddress       string  `json:"macAddress"`
	NetworkInterface string  `json:"networkInterface"`
	LinkSpeed        string  `json:"linkSpeed"`

	// System Info
	FirmwareVersion string `json:"firmwareVersion"`
	ArmbianVersion  string `json:"armbianVersion"`
	KernelVersion   string `json:"kernelVersion"`

	// Virtual Drive
	VirtualDriveUsedGb  float64 `json:"virtualDriveUsedGb"`
	VirtualDriveTotalGb float64 `json:"virtualDriveTotalGb"`

	// Queue
	QueueFileCount int     `json:"queueFileCount"`
	QueueSizeGb    float64 `json:"queueSizeGb"`
}

func CollectStats(nvmePath string, imagePath string, mountPoint string, uploadQueue string) (Stats, error) {
	var s Stats

	// 1. CPU Usage
	cpu, err := getCPUUsage()
	if err == nil {
		s.CPUPercent = cpu
	}

	// 2. Memory Usage
	memUsed, memTotal, err := getMemUsage()
	if err == nil {
		s.MemUsedGb = memUsed
		s.MemTotalGb = memTotal
	}

	// 3. SoC Temperature
	temp, err := getSoCTemp()
	if err == nil {
		s.TempCelsius = temp
	}

	// 4. NVMe Usage
	nvmeUsed, nvmeTotal, err := getDiskUsage(nvmePath)
	if err == nil {
		s.NvmeUsedGb = nvmeUsed
		s.NvmeTotalGb = nvmeTotal
	}

	// 5. Uptime
	uptime, err := getUptime()
	if err == nil {
		s.UptimeSeconds = int(uptime)
	}

	// 6. System Info
	s.KernelVersion = getKernelVersion()
	s.ArmbianVersion = getArmbianVersion()
	s.FirmwareVersion = Version

	// 7. Networking
	s.NetworkInterface = getPrimaryInterface()
	if s.NetworkInterface != "" {
		s.IPAddress = getIPAddress(s.NetworkInterface)
		s.MACAddress = getMACAddress(s.NetworkInterface)
		s.LinkSpeed = getLinkSpeed(s.NetworkInterface)
		// Mbps calc would need delta over time, skipping for now
	}

	// 8. Virtual Drive (external USB-drive backing).
	// When the host owns the filesystem (not mounted internally) we can only
	// report total capacity. os.Stat().Size() works for an image file but is 0
	// for a block-device backing (whole-disk exFAT, e.g. /dev/nvme0n1) — that
	// zero was why dual-NVMe boards showed a blank/0 external size.
	if total := virtualDriveTotalBytes(imagePath); total > 0 {
		s.VirtualDriveTotalGb = float64(total) / (1024 * 1024 * 1024)
	}
	// Only when the external drive is ACTUALLY mounted internally (during a
	// maintenance cycle) do we report live used/total from it. Guard on a real
	// mount: an unmounted ingest dir just resolves to the internal ext4, so an
	// unguarded statfs(mountPoint) would clobber the external size with the
	// internal filesystem's numbers (making Virtual Drive == NVMe Storage).
	if mountPoint != "" && isMounted(mountPoint) {
		if vUsed, vTotal, err := getDiskUsage(mountPoint); err == nil {
			s.VirtualDriveUsedGb = vUsed
			s.VirtualDriveTotalGb = vTotal // live stat when mounted
		}
	}

	// 9. Queue
	if files, err := os.ReadDir(uploadQueue); err == nil {
		s.QueueFileCount = len(files)
		var totalSize int64
		for _, f := range files {
			if info, err := f.Info(); err == nil {
				totalSize += info.Size()
			}
		}
		s.QueueSizeGb = float64(totalSize) / (1024 * 1024 * 1024)
	}

	return s, nil
}

// getCPUUsage samples /proc/stat twice over a short interval and returns busy
// percentage across all cores — true utilization, unlike a load-average
// approximation (which lags reality and can read misleadingly high).
func getCPUUsage() (float64, error) {
	idle0, total0, err := readCPUSample()
	if err != nil {
		return 0, err
	}
	time.Sleep(100 * time.Millisecond)
	idle1, total1, err := readCPUSample()
	if err != nil {
		return 0, err
	}
	dTotal := float64(total1 - total0)
	if dTotal <= 0 {
		return 0, nil
	}
	usage := (1 - float64(idle1-idle0)/dTotal) * 100
	if usage < 0 {
		usage = 0
	}
	if usage > 100 {
		usage = 100
	}
	return usage, nil
}

// readCPUSample parses the aggregate "cpu" line of /proc/stat, returning idle
// (idle + iowait) and total jiffies since boot.
func readCPUSample() (idle, total uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, 0, fmt.Errorf("empty /proc/stat")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("unexpected /proc/stat cpu line")
	}
	// fields[1:] = user nice system idle iowait irq softirq steal ...
	for i, f := range fields[1:] {
		v, e := strconv.ParseUint(f, 10, 64)
		if e != nil {
			continue
		}
		total += v
		if i == 3 || i == 4 { // idle, iowait
			idle += v
		}
	}
	return idle, total, nil
}

// virtualDriveTotalBytes returns the total capacity of the external-drive
// backing: file size for an image, or block-device capacity for a whole-disk
// backing (os.Stat().Size() is 0 for a device node).
func virtualDriveTotalBytes(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if fi.Mode()&os.ModeDevice != 0 {
		return blockDeviceSizeBytes(filepath.Base(path))
	}
	return fi.Size()
}

// isMounted reports whether target is an actual mount point (an entry in
// /proc/mounts), as opposed to a plain directory on its parent filesystem.
func isMounted(target string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[1] == target {
			return true
		}
	}
	return false
}

// blockDeviceSizeBytes reads a block device's capacity from sysfs. The `size`
// attribute is always in 512-byte sectors (a Linux convention, independent of
// the device's logical block size), so bytes = sectors * 512.
func blockDeviceSizeBytes(name string) int64 {
	data, err := os.ReadFile(filepath.Join("/sys/class/block", name, "size"))
	if err != nil {
		return 0
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return sectors * 512
}

func getMemUsage() (float64, float64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	var memTotal, memAvailable int64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fmt.Sscanf(line, "MemTotal: %d kB", &memTotal)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			fmt.Sscanf(line, "MemAvailable: %d kB", &memAvailable)
		}
	}

	totalGb := float64(memTotal) / (1024 * 1024)
	usedGb := float64(memTotal-memAvailable) / (1024 * 1024)
	return usedGb, totalGb, nil
}

func getSoCTemp() (float64, error) {
	// Standard path on most Linux systems
	data, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0, err
	}
	temp, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0, err
	}
	return temp / 1000.0, nil
}

func getDiskUsage(path string) (float64, float64, error) {
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	if err != nil {
		return 0, 0, err
	}

	total := float64(stat.Blocks) * float64(stat.Bsize)
	free := float64(stat.Bfree) * float64(stat.Bsize)
	used := total - free

	return used / (1024 * 1024 * 1024), total / (1024 * 1024 * 1024), nil
}

func getUptime() (float64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	var uptime float64
	_, err = fmt.Sscanf(string(data), "%f", &uptime)
	return uptime, err
}
func getKernelVersion() string {
	data, _ := os.ReadFile("/proc/version")
	parts := strings.Split(string(data), " ")
	if len(parts) > 2 {
		return parts[2]
	}
	return ""
}

func getArmbianVersion() string {
	data, _ := os.ReadFile("/etc/armbian-release")
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "VERSION=") {
			return strings.Trim(strings.TrimPrefix(line, "VERSION="), "\"")
		}
	}
	return ""
}

func getPrimaryInterface() string {
	data, _ := os.ReadFile("/proc/net/route")
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) >= 3 && parts[2] == "00000000" { // Default gateway
			return parts[0]
		}
	}
	return "eth0"
}

func getIPAddress(iface string) string {
	// Simple way using hostname command since we don't want net package bloat
	out, _ := exec.Command("hostname", "-I").Output()
	ips := strings.Fields(string(out))
	if len(ips) > 0 {
		return ips[0]
	}
	return ""
}

func getMACAddress(iface string) string {
	data, _ := os.ReadFile("/sys/class/net/" + iface + "/address")
	return strings.TrimSpace(string(data))
}

func getLinkSpeed(iface string) string {
	data, _ := os.ReadFile("/sys/class/net/" + iface + "/speed")
	speed := strings.TrimSpace(string(data))
	if speed != "" {
		return speed + " Mbps"
	}
	return ""
}
