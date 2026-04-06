package manifest

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Borgels/clawcontrol-agent/internal/runtime"
	"github.com/Borgels/clawcontrol-agent/internal/types"
)

type PreflightResult struct {
	Ready          bool           `json:"ready"`
	Issues         []string       `json:"issues,omitempty"`
	LoadedModels   []string       `json:"loadedModels,omitempty"`
	MemoryEstimate MemoryEstimate `json:"memoryEstimate"`
	LoadPlan       LoadPlan       `json:"loadPlan"`
}

func localManifestModels(manifest types.ResourceManifest, localMachineID string) []types.ManifestModel {
	result := make([]types.ManifestModel, 0)
	for _, model := range manifest.Models {
		if model.Source != "machine" {
			continue
		}
		if strings.TrimSpace(model.MachineID) != strings.TrimSpace(localMachineID) {
			continue
		}
		result = append(result, model)
	}
	return result
}

func endpointHealthPath(runtimeName string) string {
	switch strings.ToLower(strings.TrimSpace(runtimeName)) {
	case "ollama":
		return "/api/tags"
	case "vllm":
		return "/health"
	default:
		return "/health"
	}
}

func probeModelEndpoint(endpoint string, runtimeName string) error {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}
	parsed.Path = endpointHealthPath(runtimeName)
	req, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return fmt.Errorf("build endpoint probe request: %w", err)
	}
	client := &http.Client{
		Timeout: 4 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint returned status %d", resp.StatusCode)
	}
	return nil
}

func RunPreflight(
	ctx context.Context,
	manifest types.ResourceManifest,
	localMachineID string,
	runtimeManager *runtime.Manager,
	progress func(msg string),
) (*PreflightResult, error) {
	localModels := localManifestModels(manifest, localMachineID)
	if len(localModels) == 0 {
		return &PreflightResult{
			Ready: true,
			MemoryEstimate: MemoryEstimate{
				RequiredMB:   0,
				AvailableMB:  0,
				FitsInMemory: true,
			},
		}, nil
	}

	totalMB, usedMB, gpuErr := QueryGPUUtilization()
	if gpuErr != nil {
		// Non-fatal fallback for systems without nvidia-smi.
		totalMB = 0
		usedMB = 0
	}
	currentlyLoaded := runtimeManager.ListModels()
	loadPlan := PlanModelLoading(manifest, localMachineID, currentlyLoaded, totalMB, usedMB)

	requiredMB := 0
	for _, model := range localModels {
		requiredMB += EstimateModelVRAM(model.ModelID, model.Runtime, model.ParameterCount)
	}
	availableMB := totalMB - usedMB
	fits := totalMB == 0 || availableMB <= 0 || requiredMB <= availableMB || loadPlan.Sequential

	result := &PreflightResult{
		Ready: true,
		MemoryEstimate: MemoryEstimate{
			RequiredMB:   requiredMB,
			AvailableMB:  availableMB,
			FitsInMemory: fits,
		},
		LoadPlan: loadPlan,
	}
	if !fits {
		result.Ready = false
		result.Issues = append(
			result.Issues,
			fmt.Sprintf("manifest requires ~%dMB but only ~%dMB appears available", requiredMB, availableMB),
		)
		return result, nil
	}

	for _, model := range localModels {
		if progress != nil {
			progress(fmt.Sprintf("Preflight loading model %s (%s)...", model.ModelID, model.Runtime))
		}
		flags := &types.ModelFeatureFlags{
			Streaming: model.Capabilities.Streaming,
			Thinking:  model.Capabilities.Thinking,
		}
		var loadErr error
		if strings.TrimSpace(model.Runtime) != "" {
			loadErr = runtimeManager.StartModelWithRuntime(model.ModelID, model.Runtime, flags)
		} else {
			loadErr = runtimeManager.StartModelWithFlags(model.ModelID, flags)
		}
		if loadErr != nil {
			result.Ready = false
			result.Issues = append(result.Issues, fmt.Sprintf("failed to load model %s: %v", model.ModelID, loadErr))
			continue
		}
		result.LoadedModels = append(result.LoadedModels, model.ModelID)
	}

	for _, model := range localModels {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if strings.TrimSpace(model.Endpoint) == "" {
			result.Ready = false
			result.Issues = append(result.Issues, fmt.Sprintf("model %s is missing endpoint URL", model.ModelID))
			continue
		}
		if progress != nil {
			progress(fmt.Sprintf("Preflight probing endpoint for %s...", model.ModelID))
		}
		if err := probeModelEndpoint(model.Endpoint, model.Runtime); err != nil {
			result.Ready = false
			result.Issues = append(result.Issues, fmt.Sprintf("endpoint check failed for %s: %v", model.ModelID, err))
		}
	}

	return result, nil
}
