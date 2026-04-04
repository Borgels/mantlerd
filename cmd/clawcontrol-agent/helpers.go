package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Borgels/clawcontrol-agent/internal/client"
	"github.com/Borgels/clawcontrol-agent/internal/runtime"
	"github.com/Borgels/clawcontrol-agent/internal/types"
)

const desiredConfigCachePath = "/etc/clawcontrol/desired-config.json"

func enforceDesiredConfig(runtimeManager *runtime.Manager, desired types.DesiredConfig) {
	for _, runtimeType := range desired.Runtimes {
		if err := runtimeManager.EnsureRuntime(string(runtimeType)); err != nil {
			log.Printf("failed to ensure runtime %s: %v", runtimeType, err)
		}
	}

	modelsHandled := map[string]bool{}
	for _, target := range desired.ModelTargets {
		modelsHandled[target.ModelID] = true
		flags := target.FeatureFlags
		if err := runtimeManager.EnsureModelWithRuntime(target.ModelID, string(target.Runtime), &flags); err != nil {
			log.Printf("failed to ensure model target %s: %v", target.ModelID, err)
		}
	}
	for _, modelID := range desired.Models {
		if modelsHandled[modelID] {
			continue
		}
		if err := runtimeManager.EnsureModelWithFlags(modelID, nil); err != nil {
			log.Printf("failed to ensure model %s: %v", modelID, err)
		}
	}
}

func loadCachedDesiredConfig() types.DesiredConfig {
	raw, err := os.ReadFile(desiredConfigCachePath)
	if err != nil {
		return types.DesiredConfig{}
	}
	var desired types.DesiredConfig
	if err := json.Unmarshal(raw, &desired); err != nil {
		log.Printf("failed to parse desired config cache: %v", err)
		return types.DesiredConfig{}
	}
	return desired
}

func saveCachedDesiredConfig(desired types.DesiredConfig) error {
	if err := os.MkdirAll(filepath.Dir(desiredConfigCachePath), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(desired, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(desiredConfigCachePath, append(payload, '\n'), 0o600)
}

func toRuntimeTypes(values []string) []types.RuntimeType {
	result := make([]types.RuntimeType, 0, len(values))
	for _, value := range values {
		result = append(result, types.RuntimeType(value))
	}
	return result
}

func toInstalledModels(runtimeManager *runtime.Manager) []types.InstalledModel {
	result := make([]types.InstalledModel, 0)
	seen := map[string]struct{}{}
	type installedModelsProvider interface {
		InstalledModels() []types.InstalledModel
	}
	for _, runtimeName := range runtimeManager.InstalledRuntimes() {
		driver, err := runtimeManager.DriverFor(runtimeName)
		if err != nil {
			continue
		}
		if provider, ok := driver.(installedModelsProvider); ok {
			for _, model := range provider.InstalledModels() {
				modelID := strings.TrimSpace(model.ModelID)
				if modelID == "" {
					continue
				}
				key := runtimeName + "::" + modelID
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
				result = append(result, model)
			}
			continue
		}
		for _, modelID := range driver.ListModels() {
			modelID = strings.TrimSpace(modelID)
			if modelID == "" {
				continue
			}
			key := runtimeName + "::" + modelID
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, types.InstalledModel{
				ModelID: modelID,
				Runtime: types.RuntimeType(runtimeName),
				Status:  types.ModelReady,
			})
		}
	}
	return result
}

func ackCommandWithRetry(cl *client.Client, payload types.AckRequest) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := cl.Ack(ackCtx, payload)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	return lastErr
}
