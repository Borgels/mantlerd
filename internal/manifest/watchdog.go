package manifest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/mantlerd/internal/runtime"
	"github.com/Borgels/mantlerd/internal/types"
)

type Watchdog struct {
	manifest       types.ResourceManifest
	machineID      string
	runtimeManager *runtime.Manager
	progress       func(msg string, eventType string)
	interval       time.Duration

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewWatchdog(
	manifestPayload types.ResourceManifest,
	machineID string,
	runtimeManager *runtime.Manager,
	progress func(string, string),
) *Watchdog {
	return &Watchdog{
		manifest:       manifestPayload,
		machineID:      machineID,
		runtimeManager: runtimeManager,
		progress:       progress,
		interval:       20 * time.Second,
	}
}

func (w *Watchdog) Start(ctx context.Context) {
	if w.cancel != nil {
		return
	}
	localCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run(localCtx)
	}()
}

func (w *Watchdog) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

func (w *Watchdog) localModels() []types.ManifestModel {
	result := make([]types.ManifestModel, 0)
	for _, model := range w.manifest.Models {
		if model.Source != "machine" {
			continue
		}
		if strings.TrimSpace(model.MachineID) != strings.TrimSpace(w.machineID) {
			continue
		}
		result = append(result, model)
	}
	return result
}

func (w *Watchdog) emit(msg string, eventType string) {
	if w.progress != nil {
		w.progress(msg, eventType)
	}
}

func (w *Watchdog) recoverModel(model types.ManifestModel, failure error) error {
	lower := strings.ToLower(failure.Error())
	if strings.Contains(lower, "out of memory") || strings.Contains(lower, "insufficient_memory") {
		w.emit(
			fmt.Sprintf("Watchdog: %s appears OOM. Restarting runtime and reloading model.", model.ModelID),
			"warning",
		)
		if strings.TrimSpace(model.Runtime) != "" {
			_ = w.runtimeManager.StopModelWithRuntime(model.ModelID, model.Runtime)
			if err := w.runtimeManager.RestartRuntimeNamed(model.Runtime); err != nil {
				return err
			}
			flags := &types.ModelFeatureFlags{
				Streaming: model.Capabilities.Streaming,
				Thinking:  model.Capabilities.Thinking,
			}
			return w.runtimeManager.StartModelWithRuntime(model.ModelID, model.Runtime, flags)
		}
	}

	w.emit(
		fmt.Sprintf("Watchdog: recovering model %s after health-check failure.", model.ModelID),
		"warning",
	)
	if strings.TrimSpace(model.Runtime) != "" {
		if err := w.runtimeManager.EnsureRuntime(model.Runtime); err != nil {
			return err
		}
		flags := &types.ModelFeatureFlags{
			Streaming: model.Capabilities.Streaming,
			Thinking:  model.Capabilities.Thinking,
		}
		return w.runtimeManager.StartModelWithRuntime(model.ModelID, model.Runtime, flags)
	}
	flags := &types.ModelFeatureFlags{
		Streaming: model.Capabilities.Streaming,
		Thinking:  model.Capabilities.Thinking,
	}
	return w.runtimeManager.StartModelWithFlags(model.ModelID, flags)
}

func (w *Watchdog) run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	failures := map[string]int{}
	models := w.localModels()
	if len(models) == 0 {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, model := range models {
				if strings.TrimSpace(model.Endpoint) == "" {
					continue
				}
				if err := probeModelEndpoint(model.Endpoint, model.Runtime); err != nil {
					failures[model.ID]++
					w.emit(
						fmt.Sprintf("Watchdog health failure for %s: %v", model.ModelID, err),
						"warning",
					)
					if recoverErr := w.recoverModel(model, err); recoverErr != nil {
						w.emit(
							fmt.Sprintf("Watchdog recovery failed for %s: %v", model.ModelID, recoverErr),
							"error",
						)
					}
					if failures[model.ID] >= 3 {
						w.emit(
							fmt.Sprintf("Watchdog critical: model %s failed health checks repeatedly.", model.ModelID),
							"error",
						)
					}
					continue
				}
				if failures[model.ID] > 0 {
					w.emit(
						fmt.Sprintf("Watchdog: model %s is healthy again.", model.ModelID),
						"content",
					)
				}
				failures[model.ID] = 0
			}
		}
	}
}
