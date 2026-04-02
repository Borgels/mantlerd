package discovery

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

type HardwareReport struct {
	Hostname        string
	Addresses       []string
	HardwareSummary string
}

func Collect() HardwareReport {
	hostname, _ := os.Hostname()
	addresses := collectAddresses()
	cpu := runtime.NumCPU()
	ram := readRAMGiB()
	gpu := readGPUInfo()

	return HardwareReport{
		Hostname:        hostname,
		Addresses:       addresses,
		HardwareSummary: fmt.Sprintf("%d vCPU / %d GB / %s", cpu, ram, gpu),
	}
}

func collectAddresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	unique := make(map[string]struct{})
	for _, ifc := range interfaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil || ipNet.IP.IsLoopback() {
				continue
			}
			if ip := ipNet.IP.To4(); ip != nil {
				unique[ip.String()] = struct{}{}
			}
		}
	}

	result := make([]string, 0, len(unique))
	for addr := range unique {
		result = append(result, addr)
	}
	return result
}

func readRAMGiB() int {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return int(kib / 1024 / 1024)
	}
	return 0
}

func readGPUInfo() string {
	cmd := exec.Command("nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader")
	out, err := cmd.Output()
	if err == nil {
		summary := strings.TrimSpace(string(out))
		if summary != "" {
			lines := strings.Split(summary, "\n")
			return strings.Join(lines, " | ")
		}
	}

	if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
		return "NVIDIA GPU (details unavailable)"
	}
	return "No GPU detected"
}
