package manifest

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/Borgels/clawcontrol-agent/internal/types"
)

type MemoryEstimate struct {
	RequiredMB   int  `json:"requiredMb"`
	AvailableMB  int  `json:"availableMb"`
	FitsInMemory bool `json:"fitsInMemory"`
}

type LoadPlan struct {
	EjectModelIDs []string `json:"ejectModelIds"`
	LoadModelIDs  []string `json:"loadModelIds"`
	Sequential    bool     `json:"sequential"`
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
	case "lmstudio":
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

func PlanModelLoading(
	manifest types.ResourceManifest,
	localMachineID string,
	currentlyLoaded []string,
	totalMB int,
	usedMB int,
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
	requiredMB := 0
	for _, model := range desired {
		desiredIDs[model.ModelID] = struct{}{}
		requiredMB += EstimateModelVRAM(model.ModelID, model.Runtime, model.ParameterCount)
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

	availableMB := totalMB - usedMB
	sequential := requiredMB > availableMB && availableMB > 0
	if availableMB <= 0 {
		sequential = true
	}

	return LoadPlan{
		EjectModelIDs: ejectModelIDs,
		LoadModelIDs:  loadModelIDs,
		Sequential:    sequential,
	}
}
