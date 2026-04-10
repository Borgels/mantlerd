package manifest

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/Borgels/mantlerd/internal/discovery"
	"github.com/Borgels/mantlerd/internal/types"
)

type MemoryEstimate struct {
	RequiredMB   int  `json:"requiredMb"`
	AvailableMB  int  `json:"availableMb"`
	FitsInMemory bool `json:"fitsInMemory"`
	HeadroomMB   int  `json:"headroomMb,omitempty"`
}

type LoadPlan struct {
	EjectModelIDs   []string `json:"ejectModelIds"`
	LoadModelIDs    []string `json:"loadModelIds"`
	Sequential      bool     `json:"sequential"`
	ProjectedUsedMB int      `json:"projectedUsedMb,omitempty"`
	ProjectedFreeMB int      `json:"projectedFreeMb,omitempty"`
	HeadroomMB      int      `json:"headroomMb,omitempty"`
	SteadyStateFits bool     `json:"steadyStateFits,omitempty"`
	MemorySource    string   `json:"memorySource,omitempty"`
}

type MemorySnapshot struct {
	TotalMB  int
	UsedMB   int
	Source   string
	Unified  bool
	Known    bool
	QueryErr error
}

func parseParameterCountInBillions(value string) float64 {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return 0
	}
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9':
			return r
		case r == '.':
			return r
		default:
			return -1
		}
	}, trimmed)
	if clean == "" {
		return 0
	}
	parsed, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return 0
	}
	switch {
	case strings.Contains(trimmed, "b"):
		return parsed
	case strings.Contains(trimmed, "m"):
		return parsed / 1000
	default:
		// Assume billions when no unit is present.
		return parsed
	}
}

func EstimateModelVRAM(modelID string, runtime string, parameterCount string) int {
	// Heuristic:
	// - Prefer explicit parameter count when present
	// - Assume ~2 bytes per parameter (fp16-ish effective memory footprint)
	// - Add baseline runtime overhead per backend
	overheadMB := 2048
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "vllm":
		overheadMB = 3072
	case "tensorrt":
		overheadMB = 4096
	case "llamacpp":
		overheadMB = 2048
	case "ollama":
		overheadMB = 1536
	}

	billions := parseParameterCountInBillions(parameterCount)
	if billions <= 0 {
		// Conservative fallback when model size is unknown.
		if strings.Contains(strings.ToLower(modelID), "70b") {
			billions = 70
		} else if strings.Contains(strings.ToLower(modelID), "34b") {
			billions = 34
		} else if strings.Contains(strings.ToLower(modelID), "13b") {
			billions = 13
		} else if strings.Contains(strings.ToLower(modelID), "8b") {
			billions = 8
		} else {
			billions = 7
		}
	}

	modelMB := int(billions * 1900) // ~1.9 GB / 1B params (quantized-ish, conservative)
	return modelMB + overheadMB
}

func QueryGPUUtilization() (totalMB int, usedMB int, err error) {
	out, cmdErr := exec.Command(
		"nvidia-smi",
		"--query-gpu=memory.total,memory.used",
		"--format=csv,noheader,nounits",
	).Output()
	if cmdErr != nil {
		return 0, 0, fmt.Errorf("query gpu utilization: %w", cmdErr)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		total, totalErr := strconv.Atoi(strings.TrimSpace(parts[0]))
		used, usedErr := strconv.Atoi(strings.TrimSpace(parts[1]))
		if totalErr != nil || usedErr != nil {
			continue
		}
		if total > 0 {
			totalMB += total
		}
		if used > 0 {
			usedMB += used
		}
	}
	if totalMB <= 0 {
		return 0, 0, fmt.Errorf("no GPU memory data available")
	}
	return totalMB, usedMB, nil
}

func querySystemMemoryUtilization() (totalMB int, usedMB int, err error) {
	if runtime.GOOS == "darwin" {
		return querySystemMemoryUtilizationDarwin()
	}

	file, openErr := os.Open("/proc/meminfo")
	if openErr != nil {
		return 0, 0, fmt.Errorf("open /proc/meminfo: %w", openErr)
	}
	defer file.Close()

	var totalKiB int
	var availableKiB int
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			parsed, parseErr := strconv.Atoi(fields[1])
			if parseErr == nil {
				totalKiB = parsed
			}
		case "MemAvailable:":
			parsed, parseErr := strconv.Atoi(fields[1])
			if parseErr == nil {
				availableKiB = parsed
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("scan /proc/meminfo: %w", err)
	}
	if totalKiB <= 0 {
		return 0, 0, fmt.Errorf("MemTotal unavailable")
	}
	totalMB = totalKiB / 1024
	if availableKiB <= 0 {
		return totalMB, 0, nil
	}
	availableMB := availableKiB / 1024
	usedMB = totalMB - availableMB
	if usedMB < 0 {
		usedMB = 0
	}
	return totalMB, usedMB, nil
}

func querySystemMemoryUtilizationDarwin() (totalMB int, usedMB int, err error) {
	memOut, memErr := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if memErr != nil {
		return 0, 0, fmt.Errorf("query hw.memsize: %w", memErr)
	}
	memBytes, parseErr := strconv.ParseInt(strings.TrimSpace(string(memOut)), 10, 64)
	if parseErr != nil || memBytes <= 0 {
		return 0, 0, fmt.Errorf("parse hw.memsize: %w", parseErr)
	}
	totalMB = int(memBytes / (1024 * 1024))
	if totalMB <= 0 {
		return 0, 0, fmt.Errorf("hw.memsize unavailable")
	}

	pageOut, pageErr := exec.Command("sysctl", "-n", "hw.pagesize").Output()
	if pageErr != nil {
		return totalMB, 0, nil
	}
	pageSizeBytes, parsePageErr := strconv.ParseInt(strings.TrimSpace(string(pageOut)), 10, 64)
	if parsePageErr != nil || pageSizeBytes <= 0 {
		return totalMB, 0, nil
	}

	vmOut, vmErr := exec.Command("vm_stat").Output()
	if vmErr != nil {
		return totalMB, 0, nil
	}

	freePages := int64(0)
	scanner := bufio.NewScanner(strings.NewReader(string(vmOut)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if !strings.HasPrefix(lower, "pages free") && !strings.HasPrefix(lower, "pages speculative") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		value := strings.TrimSuffix(parts[len(parts)-1], ".")
		parsed, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || parsed < 0 {
			continue
		}
		freePages += parsed
	}

	freeMB := int((freePages * pageSizeBytes) / (1024 * 1024))
	usedMB = totalMB - freeMB
	if usedMB < 0 {
		usedMB = 0
	}
	return totalMB, usedMB, nil
}

func QueryMemorySnapshot() MemorySnapshot {
	unified := discovery.DetectUnifiedMemory()
	if unified != nil && *unified {
		totalMB, usedMB, err := querySystemMemoryUtilization()
		if err == nil && totalMB > 0 {
			return MemorySnapshot{TotalMB: totalMB, UsedMB: usedMB, Source: "system_ram", Unified: true, Known: true}
		}
		return MemorySnapshot{Source: "system_ram", Unified: true, QueryErr: err}
	}

	if totalMB, usedMB, err := QueryGPUUtilization(); err == nil && totalMB > 0 {
		return MemorySnapshot{TotalMB: totalMB, UsedMB: usedMB, Source: "gpu_vram", Known: true}
	}

	totalMB, usedMB, err := querySystemMemoryUtilization()
	if err == nil && totalMB > 0 {
		return MemorySnapshot{TotalMB: totalMB, UsedMB: usedMB, Source: "system_ram", Known: true}
	}
	return MemorySnapshot{Source: "unknown", QueryErr: err}
}

func estimateHeadroomMB(totalMB int, unified bool) int {
	if totalMB <= 0 {
		if unified {
			return 4096
		}
		return 2048
	}
	minimum := 2048
	ratio := 0.10
	if unified {
		minimum = 4096
		ratio = 0.15
	}
	headroom := int(float64(totalMB) * ratio)
	if headroom < minimum {
		headroom = minimum
	}
	return headroom
}

func PlanModelLoading(
	manifest types.ResourceManifest,
	localMachineID string,
	currentlyLoaded []string,
	totalMB int,
	usedMB int,
) LoadPlan {
	return PlanModelLoadingWithSnapshot(manifest, localMachineID, currentlyLoaded, MemorySnapshot{
		TotalMB: totalMB,
		UsedMB:  usedMB,
		Known:   totalMB > 0,
		Source:  "gpu_vram",
	})
}

func PlanModelLoadingWithSnapshot(
	manifest types.ResourceManifest,
	localMachineID string,
	currentlyLoaded []string,
	snapshot MemorySnapshot,
) LoadPlan {
	desired := make([]types.ManifestModel, 0)
	for _, model := range manifest.Models {
		if model.Source != "machine" {
			continue
		}
		if strings.TrimSpace(model.MachineID) != strings.TrimSpace(localMachineID) {
			continue
		}
		desired = append(desired, model)
	}

	desiredIDs := make(map[string]struct{}, len(desired))
	desiredModelByID := make(map[string]types.ManifestModel, len(desired))
	for _, model := range desired {
		desiredIDs[model.ModelID] = struct{}{}
		desiredModelByID[model.ModelID] = model
	}

	loadModelIDs := make([]string, 0, len(desired))
	loadedSet := make(map[string]struct{}, len(currentlyLoaded))
	for _, modelID := range currentlyLoaded {
		loadedSet[strings.TrimSpace(modelID)] = struct{}{}
	}
	for _, model := range desired {
		if _, ok := loadedSet[model.ModelID]; !ok {
			loadModelIDs = append(loadModelIDs, model.ModelID)
		}
	}

	ejectModelIDs := make([]string, 0)
	for _, modelID := range currentlyLoaded {
		if _, keep := desiredIDs[strings.TrimSpace(modelID)]; !keep {
			ejectModelIDs = append(ejectModelIDs, modelID)
		}
	}
	sort.Strings(ejectModelIDs)
	sort.Strings(loadModelIDs)

	headroomMB := estimateHeadroomMB(snapshot.TotalMB, snapshot.Unified)
	reclaimableMB := 0
	for _, modelID := range ejectModelIDs {
		reclaimableMB += EstimateModelVRAM(modelID, "", "")
	}
	loadRequiredMB := 0
	maxSingleLoadMB := 0
	for _, modelID := range loadModelIDs {
		model := desiredModelByID[modelID]
		estimate := EstimateModelVRAM(model.ModelID, model.Runtime, model.ParameterCount)
		loadRequiredMB += estimate
		if estimate > maxSingleLoadMB {
			maxSingleLoadMB = estimate
		}
	}

	projectedUsedMB := snapshot.UsedMB - reclaimableMB + loadRequiredMB
	if projectedUsedMB < 0 {
		projectedUsedMB = 0
	}
	projectedFreeMB := snapshot.TotalMB - projectedUsedMB
	if projectedFreeMB < 0 {
		projectedFreeMB = 0
	}
	steadyStateFits := !snapshot.Known || projectedUsedMB+headroomMB <= snapshot.TotalMB
	sequential := snapshot.Known && maxSingleLoadMB > 0 && !steadyStateFits && (snapshot.UsedMB-reclaimableMB+maxSingleLoadMB+headroomMB <= snapshot.TotalMB)

	return LoadPlan{
		EjectModelIDs:   ejectModelIDs,
		LoadModelIDs:    loadModelIDs,
		Sequential:      sequential,
		ProjectedUsedMB: projectedUsedMB,
		ProjectedFreeMB: projectedFreeMB,
		HeadroomMB:      headroomMB,
		SteadyStateFits: steadyStateFits,
		MemorySource:    snapshot.Source,
	}
}
