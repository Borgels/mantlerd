package commands

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Borgels/mantlerd/internal/discovery"
)

const (
	healthCheckPass = "pass"
	healthCheckWarn = "warn"
	healthCheckFail = "fail"
)

type healthCheckDiskTarget struct {
	path      string
	minFreeMB uint64
}

var healthCheckDiskTargets = []healthCheckDiskTarget{
	{path: "/var/cache/huggingface", minFreeMB: 10240},
	{path: "/var/lib/mantler", minFreeMB: 5120},
}

func (e *Executor) runHealthCheck() (map[string]any, error) {
	checkedAt := time.Now().UTC()
	agent, agentStatus := e.healthCheckAgent(checkedAt)
	server, serverStatus := e.healthCheckServer(checkedAt)
	runtimes, runtimeStatus := e.healthCheckRuntimes()
	gpu, gpuStatus := e.healthCheckGPU()
	disk, diskStatus := e.healthCheckDisk()
	load, loadStatus := e.healthCheckLoad()
	memory, memoryStatus := e.healthCheckMemory()

	overallStatus := aggregateHealthStatuses(
		agentStatus,
		serverStatus,
		runtimeStatus,
		gpuStatus,
		diskStatus,
		loadStatus,
		memoryStatus,
	)

	return map[string]any{
		"version":   1,
		"status":    overallStatus,
		"checkedAt": checkedAt.Format(time.RFC3339),
		"checks": map[string]any{
			"agent":              agent,
			"serverConnectivity": server,
			"runtimes":           runtimes,
			"gpu":                gpu,
			"disk":               disk,
			"load":               load,
			"memory":             memory,
		},
	}, nil
}

func (e *Executor) healthCheckAgent(now time.Time) (map[string]any, string) {
	uptime := time.Duration(0)
	if !e.processStarted.IsZero() && now.After(e.processStarted) {
		uptime = now.Sub(e.processStarted)
	}
	version := resolveBuildVersion()
	status := healthCheckPass
	if version == "unknown" {
		status = healthCheckWarn
	}
	return map[string]any{
		"status":           status,
		"pid":              os.Getpid(),
		"version":          version,
		"uptimeSeconds":    int64(uptime.Seconds()),
		"processStartedAt": e.processStarted.UTC().Format(time.RFC3339),
	}, status
}

func (e *Executor) healthCheckServer(now time.Time) (map[string]any, string) {
	serverURL := strings.TrimRight(strings.TrimSpace(e.cfg.ServerURL), "/")
	if serverURL == "" {
		return map[string]any{
			"status":  healthCheckFail,
			"message": "Server URL is not configured",
		}, healthCheckFail
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if e.cfg.Insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/health", nil)
	if err != nil {
		return map[string]any{
			"status":  healthCheckFail,
			"message": fmt.Sprintf("Failed to create request: %v", err),
		}, healthCheckFail
	}
	resp, err := client.Do(req)
	if err != nil {
		return map[string]any{
			"status":  healthCheckFail,
			"message": fmt.Sprintf("Server health endpoint unreachable: %v", err),
		}, healthCheckFail
	}
	_ = resp.Body.Close()
	latencyMs := time.Since(start).Milliseconds()
	status := healthCheckPass
	message := "Server connectivity healthy"
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status = healthCheckFail
		message = fmt.Sprintf("Unexpected server status: %d", resp.StatusCode)
	} else if latencyMs > 1500 {
		status = healthCheckWarn
		message = "Server reachable but latency is high"
	}
	return map[string]any{
		"status":      status,
		"message":     message,
		"checkedAt":   now.Format(time.RFC3339),
		"latencyMs":   latencyMs,
		"httpStatus":  resp.StatusCode,
		"serverUrl":   serverURL,
		"insecureTLS": e.cfg.Insecure,
	}, status
}

func (e *Executor) healthCheckRuntimes() (map[string]any, string) {
	installed := e.runtimeManager.InstalledRuntimes()
	ready := e.runtimeManager.ReadyRuntimes()
	readySet := make(map[string]struct{}, len(ready))
	for _, runtimeName := range ready {
		readySet[runtimeName] = struct{}{}
	}

	items := make([]map[string]any, 0, len(installed))
	status := healthCheckPass
	if len(installed) == 0 {
		status = healthCheckWarn
	}
	for _, runtimeName := range installed {
		runtimeStatus := healthCheckWarn
		if _, ok := readySet[runtimeName]; ok {
			runtimeStatus = healthCheckPass
		} else if status == healthCheckPass {
			status = healthCheckWarn
		}
		items = append(items, map[string]any{
			"name":    runtimeName,
			"status":  runtimeStatus,
			"ready":   runtimeStatus == healthCheckPass,
			"version": strings.TrimSpace(e.runtimeManager.RuntimeVersion(runtimeName)),
		})
	}

	return map[string]any{
		"status":          status,
		"installedCount":  len(installed),
		"readyCount":      len(ready),
		"installed":       installed,
		"ready":           ready,
		"runtimeStatuses": items,
	}, status
}

func (e *Executor) healthCheckGPU() (map[string]any, string) {
	report := discovery.Collect()
	gpuCount := len(report.GPUs)
	status := healthCheckPass
	if gpuCount == 0 {
		status = healthCheckWarn
	}
	items := make([]map[string]any, 0, gpuCount)
	for _, gpu := range report.GPUs {
		items = append(items, map[string]any{
			"name":              gpu.Name,
			"memoryTotalMb":     gpu.MemoryTotalMB,
			"memoryUsedMb":      gpu.MemoryUsedMB,
			"memoryFreeMb":      gpu.MemoryFreeMB,
			"architecture":      gpu.Architecture,
			"computeCapability": gpu.ComputeCapability,
		})
	}
	return map[string]any{
		"status":          status,
		"gpuVendor":       report.GPUVendor,
		"hardwareSummary": report.HardwareSummary,
		"gpuCount":        gpuCount,
		"gpus":            items,
	}, status
}

func (e *Executor) healthCheckDisk() (map[string]any, string) {
	results := make([]map[string]any, 0, len(healthCheckDiskTargets))
	status := healthCheckPass
	for _, target := range healthCheckDiskTargets {
		targetStatus := healthCheckPass
		entry := map[string]any{
			"path": target.path,
		}
		var stat syscall.Statfs_t
		if err := syscall.Statfs(target.path, &stat); err != nil {
			targetStatus = healthCheckWarn
			entry["status"] = targetStatus
			entry["message"] = fmt.Sprintf("Unable to read filesystem stats: %v", err)
			if status == healthCheckPass {
				status = healthCheckWarn
			}
			results = append(results, entry)
			continue
		}
		freeBytes := stat.Bavail * uint64(stat.Bsize)
		freeMB := freeBytes / (1024 * 1024)
		totalBytes := stat.Blocks * uint64(stat.Bsize)
		entry["freeBytes"] = freeBytes
		entry["freeMb"] = freeMB
		entry["totalBytes"] = totalBytes
		entry["minRecommendedFreeMb"] = target.minFreeMB
		if freeMB < target.minFreeMB {
			targetStatus = healthCheckWarn
			if status == healthCheckPass {
				status = healthCheckWarn
			}
		}
		entry["status"] = targetStatus
		results = append(results, entry)
	}
	return map[string]any{
		"status":  status,
		"targets": results,
	}, status
}

func (e *Executor) healthCheckLoad() (map[string]any, string) {
	values := readLoadAverages()
	status := healthCheckPass
	message := "System load is healthy"
	if len(values) == 0 {
		status = healthCheckWarn
		message = "Load average is unavailable on this system"
		return map[string]any{
			"status":  status,
			"message": message,
		}, status
	}
	if values[0] >= 8 {
		status = healthCheckWarn
		message = "High 1-minute load average"
	}
	return map[string]any{
		"status":       status,
		"message":      message,
		"loadAverages": values,
	}, status
}

func (e *Executor) healthCheckMemory() (map[string]any, string) {
	totalKB, availableKB, err := readMemoryKB()
	if err != nil {
		return map[string]any{
			"status":  healthCheckWarn,
			"message": fmt.Sprintf("Unable to read memory info: %v", err),
		}, healthCheckWarn
	}
	totalMB := totalKB / 1024
	availableMB := availableKB / 1024
	status := healthCheckPass
	message := "Memory availability is healthy"
	if totalKB > 0 {
		availablePct := (float64(availableKB) / float64(totalKB)) * 100
		if availablePct < 10 {
			status = healthCheckWarn
			message = "Low available memory"
		}
		return map[string]any{
			"status":       status,
			"message":      message,
			"totalMb":      totalMB,
			"availableMb":  availableMB,
			"availablePct": availablePct,
		}, status
	}
	return map[string]any{
		"status":      status,
		"message":     message,
		"totalMb":     totalMB,
		"availableMb": availableMB,
	}, status
}

func resolveBuildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	version := strings.TrimSpace(info.Main.Version)
	if version == "" || version == "(devel)" {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision := strings.TrimSpace(setting.Value)
				if len(revision) >= 7 {
					return revision[:7]
				}
				if revision != "" {
					return revision
				}
			}
		}
		return "unknown"
	}
	return version
}

func aggregateHealthStatuses(statuses ...string) string {
	hasWarn := false
	for _, status := range statuses {
		switch status {
		case healthCheckFail:
			return healthCheckFail
		case healthCheckWarn:
			hasWarn = true
		}
	}
	if hasWarn {
		return healthCheckWarn
	}
	return healthCheckPass
}

func readLoadAverages() []float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil
	}
	fields := strings.Fields(strings.TrimSpace(string(raw)))
	if len(fields) < 3 {
		return nil
	}
	values := make([]float64, 0, 3)
	for i := 0; i < 3; i++ {
		parsed, parseErr := strconv.ParseFloat(fields[i], 64)
		if parseErr != nil {
			return nil
		}
		values = append(values, parsed)
	}
	return values
}

func readMemoryKB() (uint64, uint64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()

	values := map[string]uint64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		fields := strings.Fields(strings.TrimSpace(parts[1]))
		if len(fields) == 0 {
			continue
		}
		parsed, parseErr := strconv.ParseUint(fields[0], 10, 64)
		if parseErr != nil {
			continue
		}
		values[key] = parsed
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}

	total := values["MemTotal"]
	available := values["MemAvailable"]
	if available == 0 {
		free := values["MemFree"]
		cached := values["Cached"]
		buffers := values["Buffers"]
		available = free + cached + buffers
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("MemTotal missing from /proc/meminfo")
	}
	return total, available, nil
}
