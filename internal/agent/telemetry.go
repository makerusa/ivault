package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

	// NVMe health (SMART) — omitted when unavailable so we don't overwrite good data with zeros.
	NvmeModel          string   `json:"nvmeModel,omitempty"`
	NvmeTempCelsius    *float64 `json:"nvmeTempCelsius,omitempty"`
	NvmeLifePercent    *float64 `json:"nvmeLifePercent,omitempty"`
	NvmeTotalWrittenTb *float64 `json:"nvmeTotalWrittenTb,omitempty"`

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
	VirtualDriveUsedGb     float64 `json:"virtualDriveUsedGb"`
	VirtualDriveTotalGb    float64 `json:"virtualDriveTotalGb"`
	VirtualDriveMeasuredAt string  `json:"virtualDriveMeasuredAt,omitempty"` // when external usage was last read (during a maintenance mount)

	// Queue
	QueueFileCount int     `json:"queueFileCount"`
	QueueSizeGb    float64 `json:"queueSizeGb"`

	// Last successful sync (data reached the cloud), reported for the portal.
	LastSyncAt          string `json:"lastSyncAt,omitempty"`
	LastSyncDestination string `json:"lastSyncDestination,omitempty"`
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

	// 4b. NVMe SMART health for the drive backing the internal storage. Best
	// effort: fields stay nil (and the portal keeps prior values) if nvme-cli
	// or SMART isn't available.
	if dev := nvmeDeviceForPath(nvmePath); dev != "" {
		if m := nvmeModel(dev); m != "" {
			s.NvmeModel = m
		}
		if t, wear, written, ok := nvmeSmart(dev); ok {
			s.NvmeTempCelsius = &t
			// SMART reports percentage *used* (wear); the portal shows "Life
			// Remaining", so report the complement. 3% used -> 97% remaining.
			remaining := 100 - wear
			if remaining < 0 {
				remaining = 0
			}
			s.NvmeLifePercent = &remaining
			s.NvmeTotalWrittenTb = &written
		}
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
		s.NetworkRxMbps, s.NetworkTxMbps = networkRateMbps(s.NetworkInterface)
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

// Network throughput sampler. We remember the previous rx/tx byte counters and
// timestamp between heartbeats and derive Mbps from the delta — an average over
// the (real) heartbeat interval, which is stabler and more meaningful than a
// sub-second sample.
var (
	netMu        sync.Mutex
	netLastRx    uint64
	netLastTx    uint64
	netLastTime  time.Time
	netLastIface string
)

// networkRateMbps returns receive/transmit throughput in Mbps since the last
// call for this interface. The first call (or after an interface change)
// establishes a baseline and returns 0.
func networkRateMbps(iface string) (rx, tx float64) {
	rxBytes, txBytes, err := readNetBytes(iface)
	if err != nil {
		return 0, 0
	}
	now := time.Now()

	netMu.Lock()
	defer netMu.Unlock()

	if netLastTime.IsZero() || iface != netLastIface {
		netLastRx, netLastTx, netLastTime, netLastIface = rxBytes, txBytes, now, iface
		return 0, 0
	}
	elapsed := now.Sub(netLastTime).Seconds()
	prevRx, prevTx := netLastRx, netLastTx
	netLastRx, netLastTx, netLastTime = rxBytes, txBytes, now
	if elapsed <= 0 {
		return 0, 0
	}
	// Guard against counter resets (interface down/up) producing a negative delta.
	var dRx, dTx uint64
	if rxBytes >= prevRx {
		dRx = rxBytes - prevRx
	}
	if txBytes >= prevTx {
		dTx = txBytes - prevTx
	}
	return rateMbps(dRx, elapsed), rateMbps(dTx, elapsed)
}

// rateMbps converts a byte delta over an elapsed interval to megabits/second.
func rateMbps(deltaBytes uint64, elapsedSec float64) float64 {
	if elapsedSec <= 0 {
		return 0
	}
	return float64(deltaBytes) * 8 / elapsedSec / 1e6
}

func readNetBytes(iface string) (rx, tx uint64, err error) {
	base := filepath.Join("/sys/class/net", iface, "statistics")
	rb, err := os.ReadFile(filepath.Join(base, "rx_bytes"))
	if err != nil {
		return 0, 0, err
	}
	tb, err := os.ReadFile(filepath.Join(base, "tx_bytes"))
	if err != nil {
		return 0, 0, err
	}
	rx, err = strconv.ParseUint(strings.TrimSpace(string(rb)), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	tx, err = strconv.ParseUint(strings.TrimSpace(string(tb)), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return rx, tx, nil
}

// ── NVMe SMART health ────────────────────────────────────────────────────────

var nvmePartRe = regexp.MustCompile(`^(/dev/nvme\d+n\d+)p\d+$`)

// nvmeDeviceForPath resolves the NVMe namespace device backing a path, e.g.
// "/nvme" -> "/dev/nvme1n1p1" -> "/dev/nvme1n1". Returns "" if the path isn't
// on an NVMe device.
func nvmeDeviceForPath(path string) string {
	out, err := exec.Command("findmnt", "-nro", "SOURCE", "--target", path).Output()
	if err != nil {
		return ""
	}
	src := strings.TrimSpace(string(out))
	if !strings.HasPrefix(src, "/dev/nvme") {
		return ""
	}
	if m := nvmePartRe.FindStringSubmatch(src); m != nil {
		return m[1]
	}
	return src
}

// nvmeModel reads the drive's model string from sysfs.
func nvmeModel(dev string) string {
	data, err := os.ReadFile(filepath.Join("/sys/class/block", filepath.Base(dev), "device", "model"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// nvmeSmart shells out to nvme-cli for SMART data and returns temperature (°C),
// wear (percentage of rated life used), and total data written (TB). Field-name
// and temperature-unit variations across nvme-cli versions are handled.
func nvmeSmart(dev string) (tempC, wearPct, writtenTB float64, ok bool) {
	out, err := exec.Command("nvme", "smart-log", dev, "-o", "json").Output()
	if err != nil {
		return 0, 0, 0, false
	}
	return parseNvmeSmart(out)
}

// parseNvmeSmart extracts temperature/wear/data-written from nvme-cli JSON,
// tolerating field-name and temperature-unit variation across versions.
func parseNvmeSmart(out []byte) (tempC, wearPct, writtenTB float64, ok bool) {
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		return 0, 0, 0, false
	}
	num := func(keys ...string) (float64, bool) {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if f, ok := v.(float64); ok {
					return f, true
				}
			}
		}
		return 0, false
	}

	temp, hasTemp := num("temperature")
	if temp > 200 { // reported in Kelvin
		temp -= 273.15
	}
	wear, _ := num("percent_used", "percentage_used")
	// Each NVMe "data unit" is 1000 * 512 = 512,000 bytes.
	units, _ := num("data_units_written")

	if !hasTemp && wear == 0 && units == 0 {
		return 0, 0, 0, false // nothing usable parsed
	}
	return temp, wear, units * 512000 / 1e12, true
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
