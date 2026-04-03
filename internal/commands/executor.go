package commands

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Borgels/clawcontrol-agent/internal/config"
	"github.com/Borgels/clawcontrol-agent/internal/runtime"
	"github.com/Borgels/clawcontrol-agent/internal/types"
)

type Executor struct {
	runtimeManager *runtime.Manager
	cfg            config.Config
	progress       func(commandID string, details string)
}

func NewExecutor(runtimeManager *runtime.Manager, cfg config.Config, progress func(commandID string, details string)) *Executor {
	return &Executor{
		runtimeManager: runtimeManager,
		cfg:            cfg,
		progress:       progress,
	}
}

func (e *Executor) Execute(command types.AgentCommand) (string, error) {
	switch command.Type {
	case "install_runtime":
		rawRuntime, ok := command.Params["runtime"]
		if !ok {
			return "", fmt.Errorf("missing runtime param")
		}
		runtimeName, ok := rawRuntime.(string)
		if !ok || runtimeName == "" {
			return "", fmt.Errorf("invalid runtime param")
		}
		return "", e.runtimeManager.InstallRuntime(runtimeName)
	case "pull_model":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return "", err
		}
		flags := modelFeatureFlagsParam(command.Params)
		runtimeName := optionalStringParam(command.Params, "runtime")
		return "", e.runtimeManager.EnsureModelWithRuntime(modelID, runtimeName, flags)
	case "remove_model":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return "", err
		}
		runtimeName := optionalStringParam(command.Params, "runtime")
		return "", e.runtimeManager.RemoveModelWithRuntime(modelID, runtimeName)
	case "health_check":
		scope, _ := command.Params["scope"].(string)
		if scope != "model_benchmark" {
			return "", nil
		}
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return "", err
		}
		samplePromptTokens := intParam(command.Params, "samplePromptTokens", 640)
		sampleOutputTokens := intParam(command.Params, "sampleOutputTokens", 256)
		concurrency := intParam(command.Params, "concurrency", 2)
		runs := intParam(command.Params, "runs", 8)
		metrics, err := e.runtimeManager.BenchmarkModel(
			modelID,
			samplePromptTokens,
			sampleOutputTokens,
			concurrency,
			runs,
			func(progress runtime.BenchmarkProgress) {
				if e.progress == nil {
					return
				}
				payload := map[string]any{
					"progress": map[string]any{
						"scope":            "model_benchmark",
						"runsCompleted":    progress.RunsCompleted,
						"runsTotal":        progress.RunsTotal,
						"successfulRuns":   progress.SuccessfulRuns,
						"failedRuns":       progress.FailedRuns,
						"lastRunLatencyMs": progress.LastRunLatencyMs,
					},
				}
				if progress.Benchmark != nil {
					payload["progress"] = map[string]any{
						"scope":                       "model_benchmark",
						"runsCompleted":               progress.RunsCompleted,
						"runsTotal":                   progress.RunsTotal,
						"successfulRuns":              progress.SuccessfulRuns,
						"failedRuns":                  progress.FailedRuns,
						"lastRunLatencyMs":            progress.LastRunLatencyMs,
						"ttftMs":                      progress.Benchmark.TTFTMs,
						"outputTokensPerSec":          progress.Benchmark.OutputTokensPerSec,
						"totalLatencyMs":              progress.Benchmark.TotalLatencyMs,
						"promptTokensPerSec":          progress.Benchmark.PromptTokensPerSec,
						"p95TtftMsAtSmallConcurrency": progress.Benchmark.P95TTFTMsAtSmallConcurrency,
					}
				}
				raw, err := json.Marshal(payload)
				if err != nil {
					return
				}
				e.progress(command.ID, string(raw))
			},
		)
		if err != nil {
			return "", err
		}
		details, err := json.Marshal(map[string]any{
			"benchmark": types.ModelBenchmarkMetrics{
				TTFTMs:                      metrics.TTFTMs,
				OutputTokensPerSec:          metrics.OutputTokensPerSec,
				TotalLatencyMs:              metrics.TotalLatencyMs,
				PromptTokensPerSec:          metrics.PromptTokensPerSec,
				P95TTFTMsAtSmallConcurrency: metrics.P95TTFTMsAtSmallConcurrency,
			},
		})
		if err != nil {
			return "", fmt.Errorf("encode benchmark metrics: %w", err)
		}
		return string(details), nil
	case "restart_runtime":
		return "", e.runtimeManager.RestartRuntime()
	case "update_agent":
		version := "latest"
		if rawVersion, ok := command.Params["version"]; ok {
			if parsedVersion, ok := rawVersion.(string); ok && strings.TrimSpace(parsedVersion) != "" {
				version = strings.TrimSpace(parsedVersion)
			}
		}
		if err := e.startAgentUpdate(version); err != nil {
			return "", err
		}
		return fmt.Sprintf("Agent update started (target version: %s)", version), nil
	default:
		return "", fmt.Errorf("unsupported command type: %s", command.Type)
	}
}

func stringParam(params map[string]interface{}, key string) (string, error) {
	raw, ok := params[key]
	if !ok {
		return "", fmt.Errorf("missing %s param", key)
	}
	value, ok := raw.(string)
	if !ok || value == "" {
		return "", fmt.Errorf("invalid %s param", key)
	}
	return value, nil
}

func modelFeatureFlagsParam(params map[string]interface{}) *types.ModelFeatureFlags {
	raw, ok := params["featureFlags"]
	if !ok {
		return nil
	}
	obj, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	flags := &types.ModelFeatureFlags{
		Streaming: true,
		Thinking:  false,
	}
	if streaming, ok := obj["streaming"].(bool); ok {
		flags.Streaming = streaming
	}
	if thinking, ok := obj["thinking"].(bool); ok {
		flags.Thinking = thinking
	}
	return flags
}

func intParam(params map[string]interface{}, key string, fallback int) int {
	raw, ok := params[key]
	if !ok {
		return fallback
	}
	switch value := raw.(type) {
	case float64:
		if value <= 0 {
			return fallback
		}
		return int(value)
	case int:
		if value <= 0 {
			return fallback
		}
		return value
	default:
		return fallback
	}
}

func optionalStringParam(params map[string]interface{}, key string) string {
	raw, ok := params[key]
	if !ok {
		return ""
	}
	value, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func (e *Executor) startAgentUpdate(version string) error {
	installer := "https://raw.githubusercontent.com/Borgels/clawcontrol-agent/master/scripts/install.sh"
	commandParts := []string{
		"curl", "-fsSL", shellQuote(installer), "|", "sh", "-s", "--",
		"--token", shellQuote(e.cfg.Token),
		"--machine", shellQuote(e.cfg.MachineID),
		"--server", shellQuote(e.cfg.ServerURL),
		"--version", shellQuote(version),
	}
	if e.cfg.Insecure {
		commandParts = append(commandParts, "--insecure")
	}

	commandLine := strings.Join(commandParts, " ")
	cmd := exec.Command("sh", "-c", commandLine)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent update: %w", err)
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
