package manifest

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/netutil"
	"github.com/Borgels/mantlerd/internal/runtime"
	"github.com/Borgels/mantlerd/internal/types"
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
			TLSClientConfig: &tls.Config{InsecureSkipVerify: netutil.IsLoopbackHost(parsed.Hostname())},
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

	snapshot := QueryMemorySnapshot()
	currentlyLoaded := runtimeManager.ListModels()
	loadPlan := PlanModelLoadingWithSnapshot(manifest, localMachineID, currentlyLoaded, snapshot)

	requiredMB := 0
	for _, model := range localModels {
		requiredMB += EstimateModelVRAM(model.ModelID, model.Runtime, model.ParameterCount)
	}
	availableMB := snapshot.TotalMB - snapshot.UsedMB - loadPlan.HeadroomMB
	if availableMB < 0 {
		availableMB = 0
	}
	fits := loadPlan.SteadyStateFits
	if !snapshot.Known {
		fits = true
	}

	result := &PreflightResult{
		Ready: true,
		MemoryEstimate: MemoryEstimate{
			RequiredMB:   requiredMB,
			AvailableMB:  availableMB,
			FitsInMemory: fits,
			HeadroomMB:   loadPlan.HeadroomMB,
		},
		LoadPlan: loadPlan,
	}
	if snapshot.QueryErr != nil && progress != nil {
		progress(fmt.Sprintf("Preflight memory check fell back to %s: %v", loadPlan.MemorySource, snapshot.QueryErr))
	}
	if !fits {
		result.Ready = false
		result.Issues = append(
			result.Issues,
			fmt.Sprintf(
				"projected steady-state memory use is too high: need ~%dMB with ~%dMB headroom, projected used ~%dMB of %dMB total",
				requiredMB,
				loadPlan.HeadroomMB,
				loadPlan.ProjectedUsedMB,
				snapshot.TotalMB,
			),
		)
		return result, nil
	}

	for _, modelID := range loadPlan.EjectModelIDs {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if progress != nil {
			progress(fmt.Sprintf("Preflight ejecting no-longer-needed model %s...", modelID))
		}
		if err := runtimeManager.StopModelWithRuntime(modelID, ""); err != nil {
			result.Issues = append(result.Issues, fmt.Sprintf("failed to eject model %s before load: %v", modelID, err))
		}
	}

	for _, model := range localModels {
		if _, alreadyLoaded := containsModelID(currentlyLoaded, model.ModelID); alreadyLoaded {
			result.LoadedModels = append(result.LoadedModels, model.ModelID)
			continue
		}
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

func containsModelID(items []string, target string) (string, bool) {
	trimmedTarget := strings.TrimSpace(target)
	for _, item := range items {
		if strings.TrimSpace(item) == trimmedTarget {
			return item, true
		}
	}
	return "", false
}
