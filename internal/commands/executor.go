package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
		if strings.EqualFold(strings.TrimSpace(runtimeName), "vllm") {
			return "", withVLLMRuntimeEnv(command.Params, func() error {
				return e.runtimeManager.InstallRuntime(runtimeName)
			})
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
						"scope":           "model_benchmark",
						"runsCompleted":   progress.RunsCompleted,
						"runsTotal":       progress.RunsTotal,
						"successfulRuns":  progress.SuccessfulRuns,
						"failedRuns":      progress.FailedRuns,
						"lastRunLatencyMs": progress.LastRunLatencyMs,
					},
				}
				if progress.Benchmark != nil {
					payload["progress"] = map[string]any{
						"scope":                         "model_benchmark",
						"runsCompleted":                 progress.RunsCompleted,
						"runsTotal":                     progress.RunsTotal,
						"successfulRuns":                progress.SuccessfulRuns,
						"failedRuns":                    progress.FailedRuns,
						"lastRunLatencyMs":              progress.LastRunLatencyMs,
						"ttftMs":                        progress.Benchmark.TTFTMs,
						"outputTokensPerSec":            progress.Benchmark.OutputTokensPerSec,
						"totalLatencyMs":                progress.Benchmark.TotalLatencyMs,
						"promptTokensPerSec":            progress.Benchmark.PromptTokensPerSec,
						"p95TtftMsAtSmallConcurrency":   progress.Benchmark.P95TTFTMsAtSmallConcurrency,
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
		runtimeName := optionalStringParam(command.Params, "runtime")
		if runtimeName == "" {
			return "", e.runtimeManager.RestartRuntime()
		}
		if strings.EqualFold(strings.TrimSpace(runtimeName), "vllm") {
			return "", withVLLMRuntimeEnv(command.Params, func() error {
				return e.runtimeManager.RestartRuntimeNamed(runtimeName)
			})
		}
		return "", e.runtimeManager.RestartRuntimeNamed(runtimeName)
	case "update_agent":
		version := "latest"
		if rawVersion, ok := command.Params["version"]; ok {
			if parsedVersion, ok := rawVersion.(string); ok && strings.TrimSpace(parsedVersion) != "" {
				version = strings.TrimSpace(parsedVersion)
			}
		}
		details, err := e.startAgentUpdate(version)
		if err != nil {
			return details, err
		}
		return details, nil
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

func withVLLMRuntimeEnv(params map[string]interface{}, fn func() error) error {
	mode := strings.ToLower(optionalStringParam(params, "runtimeMode"))
	image := optionalStringParam(params, "containerImage")

	var restoreMode *string
	var restoreImage *string

	if mode != "" {
		if mode != "container" && mode != "native" {
			return fmt.Errorf("invalid runtimeMode param for vllm: %s", mode)
		}
		if current, ok := os.LookupEnv("VLLM_RUNTIME_MODE"); ok {
			tmp := current
			restoreMode = &tmp
		}
		_ = os.Setenv("VLLM_RUNTIME_MODE", mode)
	}
	if image != "" {
		if current, ok := os.LookupEnv("VLLM_CONTAINER_IMAGE"); ok {
			tmp := current
			restoreImage = &tmp
		}
		_ = os.Setenv("VLLM_CONTAINER_IMAGE", image)
	}
	if mode != "" || image != "" {
		if err := persistVLLMEnvOverrides(mode, image); err != nil {
			return err
		}
	}
	defer func() {
		if mode != "" {
			if restoreMode != nil {
				_ = os.Setenv("VLLM_RUNTIME_MODE", *restoreMode)
			} else {
				_ = os.Unsetenv("VLLM_RUNTIME_MODE")
			}
		}
		if image != "" {
			if restoreImage != nil {
				_ = os.Setenv("VLLM_CONTAINER_IMAGE", *restoreImage)
			} else {
				_ = os.Unsetenv("VLLM_CONTAINER_IMAGE")
			}
		}
	}()

	return fn()
}

func persistVLLMEnvOverrides(mode string, image string) error {
	const vllmEnvPath = "/etc/clawcontrol/vllm.env"
	if mode == "" && image == "" {
		return nil
	}
	values := map[string]string{}
	order := make([]string, 0, 8)
	addKey := func(key, value string) {
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	if raw, err := os.ReadFile(vllmEnvPath); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			if key == "" {
				continue
			}
			val = strings.Trim(val, "\"")
			addKey(key, val)
		}
	}
	if mode != "" {
		addKey("VLLM_RUNTIME_MODE", mode)
	}
	if image != "" {
		addKey("VLLM_CONTAINER_IMAGE", image)
	}
	if err := os.MkdirAll(filepath.Dir(vllmEnvPath), 0o755); err != nil {
		return fmt.Errorf("create vllm env directory: %w", err)
	}
	lines := make([]string, 0, len(order))
	for _, key := range order {
		lines = append(lines, fmt.Sprintf("%s=%s", key, values[key]))
	}
	payload := strings.Join(lines, "\n")
	if payload != "" {
		payload += "\n"
	}
	if err := os.WriteFile(vllmEnvPath, []byte(payload), 0o600); err != nil {
		return fmt.Errorf("persist vllm runtime settings: %w", err)
	}
	return nil
}

func (e *Executor) startAgentUpdate(version string) (string, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", commandLine)
	cmd.Env = append(os.Environ(), "CLAWCONTROL_AGENT_SELF_UPDATE=true")
	output, err := cmd.CombinedOutput()
	details := strings.TrimSpace(string(output))
	if details == "" {
		details = fmt.Sprintf("Agent update completed (target version: %s)", version)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return details, fmt.Errorf("agent update timed out after 5m")
		}
		if isSignalTermination(err) {
			return "Agent service restart triggered during update. Update likely applied; waiting for service to check in again.", nil
		}
		return details, fmt.Errorf("agent update failed: %w", err)
	}
	return details, nil
}

func isSignalTermination(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return false
	}
	switch status.Signal() {
	case syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGKILL:
		return true
	default:
		return false
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
