package agent

import (
	"bufio"
	"fmt"
	"os"
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
}

func CollectStats(nvmePath string) (Stats, error) {
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

	return s, nil
}

func getCPUUsage() (float64, error) {
	f, err := os.Open("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var load1, load5, load15 float64
	_, err = fmt.Fscanf(f, "%f %f %f", &load1, &load5, &load15)
	if err != nil {
		return 0, err
	}
	// For simplicity, we'll return the 1-minute load average
	return load1, nil
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
