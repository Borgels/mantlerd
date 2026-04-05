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
	RAMTotalMB      int
	GPUs            []GPUInfo
}

type GPUInfo struct {
	Name              string
	MemoryTotalMB     int
	Architecture      string
	ComputeCapability string
	UnifiedMemory     *bool
}

func Collect() HardwareReport {
	hostname, _ := os.Hostname()
	addresses := collectAddresses()
	cpu := runtime.NumCPU()
	ramTotalMB := readRAMMiB()
	gpuSummary, gpus := readGPUInfo()
	ramGiB := ramTotalMB / 1024

	return HardwareReport{
		Hostname:        hostname,
		Addresses:       addresses,
		HardwareSummary: fmt.Sprintf("%d vCPU / %d GB / %s", cpu, ramGiB, gpuSummary),
		RAMTotalMB:      ramTotalMB,
		GPUs:            gpus,
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

func readRAMMiB() int {
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
		return int(kib / 1024)
	}
	return 0
}

func architectureFromComputeCapability(name string, computeCapability string) string {
	cc := strings.TrimSpace(computeCapability)
	if cc != "" {
		major := cc
		if dot := strings.Index(cc, "."); dot > 0 {
			major = cc[:dot]
		}
		switch major {
		case "10":
			return "Blackwell"
		case "9":
			return "Hopper"
		case "8":
			return "Ampere/Ada"
		case "7":
			return "Turing/Volta"
		}
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.Contains(lower, "blackwell"):
		return "Blackwell"
	case strings.Contains(lower, "hopper"):
		return "Hopper"
	case strings.Contains(lower, "ampere") || strings.Contains(lower, "ada"):
		return "Ampere/Ada"
	default:
		return ""
	}
}

func readGPUInfo() (string, []GPUInfo) {
	cmd := exec.Command("nvidia-smi", "--query-gpu=name,memory.total,compute_cap", "--format=csv,noheader")
	out, err := cmd.Output()
	if err == nil {
		summary := strings.TrimSpace(string(out))
		if summary != "" {
			lines := strings.Split(summary, "\n")
			gpus := make([]GPUInfo, 0, len(lines))
			labels := make([]string, 0, len(lines))
			for _, rawLine := range lines {
				line := strings.TrimSpace(rawLine)
				if line == "" {
					continue
				}
				parts := strings.Split(line, ",")
				name := ""
				if len(parts) > 0 {
					name = strings.TrimSpace(parts[0])
				}
				memoryTotalMB := 0
				if len(parts) > 1 {
					fields := strings.Fields(strings.TrimSpace(parts[1]))
					if len(fields) > 0 {
						if value, parseErr := strconv.Atoi(fields[0]); parseErr == nil {
							memoryTotalMB = value
						}
					}
				}
				computeCapability := ""
				if len(parts) > 2 {
					computeCapability = strings.TrimSpace(parts[2])
				}
				architecture := architectureFromComputeCapability(name, computeCapability)
				gpus = append(gpus, GPUInfo{
					Name:              name,
					MemoryTotalMB:     memoryTotalMB,
					Architecture:      architecture,
					ComputeCapability: computeCapability,
				})
				labelParts := []string{name}
				if memoryTotalMB > 0 {
					labelParts = append(labelParts, fmt.Sprintf("%d MiB", memoryTotalMB))
				}
				if architecture != "" {
					labelParts = append(labelParts, architecture)
				}
				labels = append(labels, strings.Join(labelParts, ", "))
			}
			return strings.Join(labels, " | "), gpus
		}
	}

	if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
		return "NVIDIA GPU (details unavailable)", nil
	}
	return "No GPU detected", nil
}
