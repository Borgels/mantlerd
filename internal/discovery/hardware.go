package discovery

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/Borgels/mantlerd/internal/types"
)

type HardwareReport struct {
	Hostname         string
	Addresses        []string
	OS               string
	CPUArch          string
	GPUVendor        string
	HardwareSummary  string
	RAMTotalMB       int
	GPUs             []GPUInfo
	Interconnect     *types.InterconnectReport
	GPUInterconnect  *types.GPUInterconnectReport
	AcceleratorStack *types.AcceleratorStackReport
}

type GPUInfo struct {
	Index               int
	UUID                string
	PCIBusID            string
	Name                string
	MemoryTotalMB       int
	MemoryUsedMB        int
	MemoryFreeMB        int
	Architecture        string
	ComputeCapability   string
	UnifiedMemory       *bool
	MemoryBandwidthGBps float64
	BandwidthSource     string
}

// DetectUnifiedMemory checks for unified CPU/GPU memory architecture (e.g., DGX Spark / Grace-Blackwell).
func DetectUnifiedMemory() *bool {
	if runtime.GOOS == "darwin" {
		unified := runtime.GOARCH == "arm64"
		return &unified
	}

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

// IsDGXSpark returns true when the host matches DGX Spark characteristics.
func IsDGXSpark() bool {
	if normalizeArch(runtime.GOARCH) != "arm64" {
		return false
	}
	unified := DetectUnifiedMemory()
	return unified != nil && *unified
}

func Collect() HardwareReport {
	hostname, _ := os.Hostname()
	addresses := collectAddresses()
	cpu := runtime.NumCPU()
	ramTotalMB := readRAMMiB()
	gpuSummary, gpus := readGPUInfo()
	gpuVendor := inferGPUVendor(gpus)
	ramGiB := ramTotalMB / 1024

	return HardwareReport{
		Hostname:         hostname,
		Addresses:        addresses,
		OS:               runtime.GOOS,
		CPUArch:          normalizeArch(runtime.GOARCH),
		GPUVendor:        gpuVendor,
		HardwareSummary:  fmt.Sprintf("%d vCPU / %d GB / %s", cpu, ramGiB, gpuSummary),
		RAMTotalMB:       ramTotalMB,
		GPUs:             gpus,
		Interconnect:     CollectInterconnect(),
		GPUInterconnect:  CollectGPUInterconnect(gpuVendor, gpus),
		AcceleratorStack: CollectAcceleratorStack(gpuVendor),
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
	if len(gpus) == 0 {
		if runtime.GOOS == "darwin" {
			return "apple"
		}
		return "none"
	}
	hasNvidia := false
	hasAMD := false
	hasApple := false
	for _, gpu := range gpus {
		lower := strings.ToLower(strings.TrimSpace(gpu.Name))
		switch {
		case strings.Contains(lower, "nvidia"):
			hasNvidia = true
		case strings.Contains(lower, "amd"), strings.Contains(lower, "radeon"), strings.Contains(lower, "ati"):
			hasAMD = true
		case strings.Contains(lower, "apple"), strings.Contains(lower, "metal"):
			hasApple = true
		}
	}
	switch {
	case hasNvidia && hasAMD:
		return "mixed"
	case hasNvidia:
		return "nvidia"
	case hasAMD:
		return "amd"
	case hasApple:
		return "apple"
	default:
		return "unknown"
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
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		raw := strings.TrimSpace(string(out))
		if raw == "" {
			return 0
		}
		bytes, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || bytes <= 0 {
			return 0
		}
		return int(bytes / (1024 * 1024))
	}

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
	if runtime.GOOS == "darwin" {
		return readAppleGPUInfo()
	}

	cmd := exec.Command(
		"nvidia-smi",
		"--query-gpu=index,uuid,pci.bus_id,name,memory.total,memory.used,compute_cap",
		"--format=csv,noheader",
	)
	out, err := cmd.Output()
	if err == nil {
		summary := strings.TrimSpace(string(out))
		if summary != "" {
			lines := strings.Split(summary, "\n")
			gpus := make([]GPUInfo, 0, len(lines))
			labels := make([]string, 0, len(lines))
			knownNames := make(map[string]struct{})
			for _, rawLine := range lines {
				line := strings.TrimSpace(rawLine)
				if line == "" {
					continue
				}
				parts := strings.Split(line, ",")
				index := 0
				if len(parts) > 0 {
					index, _ = strconv.Atoi(strings.TrimSpace(parts[0]))
				}
				uuid := ""
				if len(parts) > 1 {
					uuid = strings.TrimSpace(parts[1])
				}
				pciBusID := ""
				if len(parts) > 2 {
					pciBusID = strings.TrimSpace(parts[2])
				}
				name := ""
				if len(parts) > 3 {
					name = strings.TrimSpace(parts[3])
				}
				memoryTotalMB := 0
				if len(parts) > 4 {
					fields := strings.Fields(strings.TrimSpace(parts[4]))
					if len(fields) > 0 {
						if value, parseErr := strconv.Atoi(fields[0]); parseErr == nil {
							memoryTotalMB = value
						}
					}
				}
				memoryUsedMB := 0
				if len(parts) > 5 {
					fields := strings.Fields(strings.TrimSpace(parts[5]))
					if len(fields) > 0 {
						if value, parseErr := strconv.Atoi(fields[0]); parseErr == nil {
							memoryUsedMB = value
						}
					}
				}
				computeCapability := ""
				if len(parts) > 6 {
					computeCapability = strings.TrimSpace(parts[6])
				}
				memoryFreeMB := 0
				if memoryTotalMB > 0 && memoryUsedMB >= 0 && memoryTotalMB >= memoryUsedMB {
					memoryFreeMB = memoryTotalMB - memoryUsedMB
				}
				architecture := architectureFromComputeCapability(name, computeCapability)
				unifiedMemory := DetectUnifiedMemory()
				gpus = append(gpus, GPUInfo{
					Index:             index,
					UUID:              uuid,
					PCIBusID:          pciBusID,
					Name:              name,
					MemoryTotalMB:     memoryTotalMB,
					MemoryUsedMB:      memoryUsedMB,
					MemoryFreeMB:      memoryFreeMB,
					Architecture:      architecture,
					ComputeCapability: computeCapability,
					UnifiedMemory:     unifiedMemory,
				})
				knownNames[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
				labelParts := []string{name}
				if memoryTotalMB > 0 {
					labelParts = append(labelParts, fmt.Sprintf("%d MiB", memoryTotalMB))
				}
				if architecture != "" {
					labelParts = append(labelParts, architecture)
				}
				labels = append(labels, strings.Join(labelParts, ", "))
			}
			for _, extra := range readDisplayGPUInfoFromLSPCI() {
				key := strings.ToLower(strings.TrimSpace(extra.Name))
				if key == "" {
					continue
				}
				if _, exists := knownNames[key]; exists {
					continue
				}
				knownNames[key] = struct{}{}
				gpus = append(gpus, extra)
				labels = append(labels, extra.Name)
			}
			return strings.Join(labels, " | "), gpus
		}
	}

	gpus := readDisplayGPUInfoFromLSPCI()
	if len(gpus) > 0 {
		labels := make([]string, 0, len(gpus))
		for _, gpu := range gpus {
			labels = append(labels, gpu.Name)
		}
		return strings.Join(labels, " | "), gpus
	}
	if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
		return "NVIDIA GPU (details unavailable)", nil
	}
	return "No GPU detected", nil
}

func readDisplayGPUInfoFromLSPCI() []GPUInfo {
	cmd := exec.Command("lspci")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	result := make([]GPUInfo, 0, len(lines))
	seen := make(map[string]struct{})
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "vga compatible controller") &&
			!strings.Contains(lower, "3d controller") &&
			!strings.Contains(lower, "display controller") {
			continue
		}
		colonIndex := strings.Index(line, ":")
		if colonIndex < 0 || colonIndex+1 >= len(line) {
			continue
		}
		name := strings.TrimSpace(line[colonIndex+1:])
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, GPUInfo{Name: name})
	}
	return result
}

func readAppleGPUInfo() (string, []GPUInfo) {
	cmd := exec.Command("system_profiler", "SPDisplaysDataType", "-json")
	out, err := cmd.Output()
	if err != nil {
		return "Apple GPU (details unavailable)", nil
	}

	var payload map[string][]map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "Apple GPU (details unavailable)", nil
	}

	devices := payload["SPDisplaysDataType"]
	if len(devices) == 0 {
		return "Apple GPU (details unavailable)", nil
	}

	gpus := make([]GPUInfo, 0, len(devices))
	labels := make([]string, 0, len(devices))
	for _, device := range devices {
		name := firstNonEmptyString(device["sppci_model"], device["_name"], device["spdisplays_device-id"])
		if strings.TrimSpace(name) == "" {
			continue
		}
		totalMB := parseAppleVRAMMB(device)
		unifiedMemory := DetectUnifiedMemory()
		gpus = append(gpus, GPUInfo{
			Name:              strings.TrimSpace(name),
			MemoryTotalMB:     totalMB,
			MemoryUsedMB:      0,
			MemoryFreeMB:      0,
			Architecture:      "Apple Silicon",
			ComputeCapability: "",
			UnifiedMemory:     unifiedMemory,
		})

		label := strings.TrimSpace(name)
		if totalMB > 0 {
			label = fmt.Sprintf("%s, %d MiB", label, totalMB)
		}
		labels = append(labels, label)
	}

	if len(gpus) == 0 {
		return "Apple GPU (details unavailable)", nil
	}
	return strings.Join(labels, " | "), gpus
}

func firstNonEmptyString(values ...interface{}) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func parseAppleVRAMMB(device map[string]interface{}) int {
	raw := firstNonEmptyString(device["spdisplays_vram_shared"], device["spdisplays_vram"])
	if raw == "" {
		return 0
	}
	cleaned := strings.ToLower(strings.TrimSpace(raw))
	cleaned = strings.ReplaceAll(cleaned, "unified", "")
	cleaned = strings.ReplaceAll(cleaned, ",", "")
	fields := strings.Fields(cleaned)
	if len(fields) == 0 {
		return 0
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || value <= 0 {
		return 0
	}
	unit := "mb"
	if len(fields) > 1 {
		unit = fields[1]
	}
	switch unit {
	case "gb", "gib":
		return int(value * 1024)
	case "mb", "mib":
		return int(value)
	default:
		return int(value)
	}
}
