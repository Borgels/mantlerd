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
	OS              string
	CPUArch         string
	GPUVendor       string
	HardwareSummary string
	RAMTotalMB      int
	GPUs            []GPUInfo
}

type GPUInfo struct {
	Name              string
	MemoryTotalMB     int
	MemoryUsedMB      int
	MemoryFreeMB      int
	Architecture      string
	ComputeCapability string
	UnifiedMemory     *bool
}

// DetectUnifiedMemory checks for unified CPU/GPU memory architecture (e.g., DGX Spark / Grace-Blackwell).
func DetectUnifiedMemory() *bool {
	// Method 1: nvidia-smi memory query returns "[N/A]" on unified memory systems
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader").Output()
	if err == nil && strings.Contains(string(out), "[N/A]") {
		unified := true
		return &unified
	}

	// Method 2: Check for Grace CPU (ARM + NVIDIA GPU = likely unified)
	cpuInfo, _ := os.ReadFile("/proc/cpuinfo")
	cpuStr := strings.ToLower(string(cpuInfo))
	isARM := strings.Contains(cpuStr, "aarch64") || strings.Contains(cpuStr, "arm")

	// Method 3: Check GPU name for known unified memory chips
	out, err = exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Output()
	if err == nil {
		name := strings.ToLower(string(out))
		// GB10 is DGX Spark, Grace-Blackwell has GB200
		if strings.Contains(name, "gb10") || strings.Contains(name, "grace") ||
			(isARM && strings.Contains(name, "blackwell")) {
			unified := true
			return &unified
		}
	}

	// Method 4: Check for /sys/devices/system/cpu/cpu0/cacheinfo presence of grace indicators
	if isARM {
		// Grace-Hopper and Grace-Blackwell are ARM-based with unified memory
		if _, err := exec.Command("nvidia-smi").Output(); err == nil {
			// ARM CPU with NVIDIA GPU present - likely unified memory system
			unified := true
			return &unified
		}
	}

	unified := false
	return &unified
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
		OS:              runtime.GOOS,
		CPUArch:         normalizeArch(runtime.GOARCH),
		GPUVendor:       inferGPUVendor(gpus),
		HardwareSummary: fmt.Sprintf("%d vCPU / %d GB / %s", cpu, ramGiB, gpuSummary),
		RAMTotalMB:      ramTotalMB,
		GPUs:            gpus,
	}
}

func normalizeArch(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "amd64", "x64", "x86_64":
		return "x86_64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func inferGPUVendor(gpus []GPUInfo) string {
	if len(gpus) > 0 {
		return "nvidia"
	}
	if runtime.GOOS == "darwin" {
		return "apple"
	}
	return "none"
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
	cmd := exec.Command(
		"nvidia-smi",
		"--query-gpu=name,memory.total,memory.used,compute_cap",
		"--format=csv,noheader",
	)
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
				memoryUsedMB := 0
				if len(parts) > 2 {
					fields := strings.Fields(strings.TrimSpace(parts[2]))
					if len(fields) > 0 {
						if value, parseErr := strconv.Atoi(fields[0]); parseErr == nil {
							memoryUsedMB = value
						}
					}
				}
				computeCapability := ""
				if len(parts) > 3 {
					computeCapability = strings.TrimSpace(parts[3])
				}
				memoryFreeMB := 0
				if memoryTotalMB > 0 && memoryUsedMB >= 0 && memoryTotalMB >= memoryUsedMB {
					memoryFreeMB = memoryTotalMB - memoryUsedMB
				}
				architecture := architectureFromComputeCapability(name, computeCapability)
				unifiedMemory := DetectUnifiedMemory()
				gpus = append(gpus, GPUInfo{
					Name:              name,
					MemoryTotalMB:     memoryTotalMB,
					MemoryUsedMB:      memoryUsedMB,
					MemoryFreeMB:      memoryFreeMB,
					Architecture:      architecture,
					ComputeCapability: computeCapability,
					UnifiedMemory:     unifiedMemory,
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
