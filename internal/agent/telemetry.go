package agent

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

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

func CollectStats(nvmePath string, imagePath string, uploadQueue string) (Stats, error) {
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
	s.FirmwareVersion = "iVault v0.1.0" // Hardcoded for now

	// 7. Networking
	s.NetworkInterface = getPrimaryInterface()
	if s.NetworkInterface != "" {
		s.IPAddress = getIPAddress(s.NetworkInterface)
		s.MACAddress = getMACAddress(s.NetworkInterface)
		s.LinkSpeed = getLinkSpeed(s.NetworkInterface)
		// Mbps calc would need delta over time, skipping for now
	}

	// 8. Virtual Drive (the image file itself)
	if _, err := os.Stat(imagePath); err == nil {
		if info, err := os.Stat(imagePath); err == nil {
			s.VirtualDriveTotalGb = float64(info.Size()) / (1024 * 1024 * 1024)
		}
	}
	// If it's mounted, we can get internal usage
	if vUsed, vTotal, err := getDiskUsage("/mnt/ivault"); err == nil {
		s.VirtualDriveUsedGb = vUsed
		s.VirtualDriveTotalGb = vTotal // Use the live stat if available
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

func getCPUUsage() (float64, error) {
	// Simple way: read /proc/loadavg and multiply by 100/cores
	// A better way is to parse /proc/stat twice, but this is a quick fix.
	f, err := os.Open("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var load1 float64
	_, err = fmt.Fscanf(f, "%f", &load1)
	if err != nil {
		return 0, err
	}

	// Rock 5T has 8 cores
	usage := (load1 / 8.0) * 100.0
	if usage > 100 {
		usage = 100
	}
	return usage, nil
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
