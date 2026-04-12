package tools

import (
	"fmt"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

type Manager struct {
	specs                map[types.ToolType]ToolSpec
	drivers              map[types.ToolType]Driver
	lastDiagnosticErrors map[types.ToolType]string
}

func NewManager() *Manager {
	m := &Manager{
		specs:                make(map[types.ToolType]ToolSpec, len(toolCatalog)),
		drivers:              make(map[types.ToolType]Driver, len(toolCatalog)),
		lastDiagnosticErrors: make(map[types.ToolType]string, len(toolCatalog)),
	}
	for _, spec := range toolCatalog {
		m.specs[spec.Name] = spec
		m.drivers[spec.Name] = spec.CreateDriver()
	}
	return m
}

func (m *Manager) Install(toolType types.ToolType) error {
	driver, ok := m.drivers[toolType]
	if !ok {
		return fmt.Errorf("unknown tool type: %s", toolType)
	}
	return driver.Install()
}

func (m *Manager) Uninstall(toolType types.ToolType) error {
	driver, ok := m.drivers[toolType]
	if !ok {
		return fmt.Errorf("unknown tool type: %s", toolType)
	}
	return driver.Uninstall()
}

func (m *Manager) RunDiagnostic(toolType types.ToolType, level string) (DiagnosticResult, error) {
	driver, ok := m.drivers[toolType]
	if !ok {
		return DiagnosticResult{}, fmt.Errorf("unknown tool type: %s", toolType)
	}
	return driver.RunDiagnostic(level)
}

func (m *Manager) InstalledTools(gpuVendor string) []types.InstalledTool {
	result := make([]types.InstalledTool, 0, len(toolCatalog))
	for _, spec := range toolCatalog {
		driver := m.drivers[spec.Name]
		if driver == nil {
			continue
		}
		if !supportsVendor(spec.SupportedVendors, gpuVendor) {
			result = append(result, types.InstalledTool{
				ID:     string(spec.Name),
				Type:   spec.Name,
				Name:   spec.DisplayName,
				Status: types.ToolUnsupported,
				Detail: "unsupported for detected GPU vendor",
			})
			continue
		}
		status := types.ToolNotInstalled
		if driver.IsInstalled() {
			if driver.IsReady() {
				status = types.ToolReady
			} else {
				status = types.ToolFailed
			}
		}
		result = append(result, types.InstalledTool{
			ID:              string(spec.Name),
			Type:            spec.Name,
			Name:            spec.DisplayName,
			Status:          status,
			Version:         driver.Version(),
			DiagnosticError: strings.TrimSpace(m.lastDiagnosticErrors[spec.Name]),
		})
	}
	return result
}

func (m *Manager) MeasureGPUBandwidth(gpuVendor string, gpuName string) (float64, string) {
	normalizedVendor := strings.ToLower(strings.TrimSpace(gpuVendor))
	normalizedName := strings.ToLower(strings.TrimSpace(gpuName))

	if normalizedVendor == "apple" {
		switch {
		case strings.Contains(normalizedName, "m4 max"):
			return 546, "known_spec"
		case strings.Contains(normalizedName, "m4 pro"):
			return 273, "known_spec"
		case strings.Contains(normalizedName, "m3 max"):
			return 400, "known_spec"
		case strings.Contains(normalizedName, "m3 pro"):
			return 150, "known_spec"
		case strings.Contains(normalizedName, "m2 ultra"):
			return 800, "known_spec"
		case strings.Contains(normalizedName, "m2 max"):
			return 400, "known_spec"
		case strings.Contains(normalizedName, "m2 pro"):
			return 200, "known_spec"
		case strings.Contains(normalizedName, "m1 ultra"):
			return 800, "known_spec"
		case strings.Contains(normalizedName, "m1 max"):
			return 400, "known_spec"
		case strings.Contains(normalizedName, "m1 pro"):
			return 200, "known_spec"
		}
	}

	if normalizedVendor == "nvidia" {
		if driver, ok := m.drivers[types.ToolDCGM]; ok && driver.IsInstalled() {
			if result, err := driver.RunDiagnostic("quick"); err == nil && result.MemoryBandwidthGBps > 0 {
				delete(m.lastDiagnosticErrors, types.ToolDCGM)
				return result.MemoryBandwidthGBps, "measured_dcgm"
			} else if err != nil {
				m.lastDiagnosticErrors[types.ToolDCGM] = strings.TrimSpace(err.Error())
			}
		}
		if driver, ok := m.drivers[types.ToolNvBandwidth]; ok && driver.IsInstalled() {
			if result, err := driver.RunDiagnostic("quick"); err == nil && result.MemoryBandwidthGBps > 0 {
				delete(m.lastDiagnosticErrors, types.ToolNvBandwidth)
				return result.MemoryBandwidthGBps, "measured_nvbandwidth"
			} else if err != nil {
				m.lastDiagnosticErrors[types.ToolNvBandwidth] = strings.TrimSpace(err.Error())
			}
		}
		if value := measureWithEmbeddedCudaBenchmark(); value > 0 {
			return value, "measured_cuda_embed"
		}
		if value := measureWithCudaBandwidthTest(); value > 0 {
			return value, "measured_cuda_bandwidth_test"
		}
		if value := knownNvidiaBandwidthGbps(normalizedName); value > 0 {
			return value, "known_spec"
		}
	}

	if normalizedVendor == "amd" {
		if driver, ok := m.drivers[types.ToolRocmBandwidthTest]; ok && driver.IsInstalled() {
			if result, err := driver.RunDiagnostic("quick"); err == nil && result.MemoryBandwidthGBps > 0 {
				delete(m.lastDiagnosticErrors, types.ToolRocmBandwidthTest)
				return result.MemoryBandwidthGBps, "measured_rocm"
			} else if err != nil {
				m.lastDiagnosticErrors[types.ToolRocmBandwidthTest] = strings.TrimSpace(err.Error())
			}
		}
	}

	return 0, "unknown"
}

func supportsVendor(vendors []string, gpuVendor string) bool {
	if len(vendors) == 0 {
		return true
	}
	normalized := strings.ToLower(strings.TrimSpace(gpuVendor))
	for _, vendor := range vendors {
		if vendor == "all" || strings.EqualFold(vendor, normalized) {
			return true
		}
	}
	return false
}

func measureWithCudaBandwidthTest() float64 {
	candidates := []string{
		"bandwidthTest",
		"/usr/local/cuda/extras/demo_suite/bandwidthTest",
	}
	for _, binary := range candidates {
		if binary != "bandwidthTest" && !commandExists(binary) {
			continue
		}
		if binary == "bandwidthTest" && !commandExists(binary) {
			continue
		}
		output, err := commandOutput(90*time.Second, binary, "--dtod", "--mode=quick")
		if err != nil {
			continue
		}
		if value := parseBandwidthTestOutputGBps(output); value > 0 {
			return value
		}
	}
	return 0
}

func knownNvidiaBandwidthGbps(gpuName string) float64 {
	switch {
	case strings.Contains(gpuName, "gb10"), strings.Contains(gpuName, "dgx spark"), strings.Contains(gpuName, "digits"):
		return 273
	case strings.Contains(gpuName, "h100"):
		return 3000
	case strings.Contains(gpuName, "a100"):
		return 1935
	case strings.Contains(gpuName, "rtx 4090"):
		return 1008
	case strings.Contains(gpuName, "rtx 3090"):
		return 936
	default:
		return 0
	}
}
