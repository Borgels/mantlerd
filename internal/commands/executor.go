package commands

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	progress       func(payload types.AckRequest)
}

type ExecutionResult struct {
	Details       string
	ResultPayload interface{}
}

const gooseInstallScriptURL = "https://github.com/block/goose/raw/main/download_cli.sh"

func NewExecutor(runtimeManager *runtime.Manager, cfg config.Config, progress func(payload types.AckRequest)) *Executor {
	return &Executor{
		runtimeManager: runtimeManager,
		cfg:            cfg,
		progress:       progress,
	}
}

func (e *Executor) Execute(command types.AgentCommand) (ExecutionResult, error) {
	switch command.Type {
	case "install_runtime":
		rawRuntime, ok := command.Params["runtime"]
		if !ok {
			return ExecutionResult{}, fmt.Errorf("missing runtime param")
		}
		runtimeName, ok := rawRuntime.(string)
		if !ok || runtimeName == "" {
			return ExecutionResult{}, fmt.Errorf("invalid runtime param")
		}
		if strings.EqualFold(strings.TrimSpace(runtimeName), "vllm") {
			return ExecutionResult{}, withVLLMRuntimeEnv(command.Params, func() error {
				return e.runtimeManager.InstallRuntime(runtimeName)
			})
		}
		return ExecutionResult{}, e.runtimeManager.InstallRuntime(runtimeName)
	case "pull_model":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return ExecutionResult{}, err
		}
		flags := modelFeatureFlagsParam(command.Params)
		runtimeName := optionalStringParam(command.Params, "runtime")
		return ExecutionResult{}, e.runtimeManager.EnsureModelWithRuntime(modelID, runtimeName, flags)
	case "remove_model":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return ExecutionResult{}, err
		}
		runtimeName := optionalStringParam(command.Params, "runtime")
		return ExecutionResult{}, e.runtimeManager.RemoveModelWithRuntime(modelID, runtimeName)
	case "health_check":
		scope, _ := command.Params["scope"].(string)
		if scope != "model_benchmark" {
			return ExecutionResult{}, nil
		}
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return ExecutionResult{}, err
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
				e.progress(types.AckRequest{
					CommandID: command.ID,
					Status:    "in_progress",
					Details:   string(raw),
				})
			},
		)
		if err != nil {
			return ExecutionResult{}, err
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
			return ExecutionResult{}, fmt.Errorf("encode benchmark metrics: %w", err)
		}
		return ExecutionResult{Details: string(details)}, nil
	case "uninstall_runtime":
		rawRuntime, ok := command.Params["runtime"]
		if !ok {
			return ExecutionResult{}, fmt.Errorf("missing runtime param")
		}
		runtimeName, ok := rawRuntime.(string)
		if !ok || runtimeName == "" {
			return ExecutionResult{}, fmt.Errorf("invalid runtime param")
		}
		return ExecutionResult{}, e.runtimeManager.UninstallRuntime(runtimeName)
	case "restart_runtime":
		runtimeName := optionalStringParam(command.Params, "runtime")
		if runtimeName == "" {
			return ExecutionResult{}, e.runtimeManager.RestartRuntime()
		}
		if strings.EqualFold(strings.TrimSpace(runtimeName), "vllm") {
			return ExecutionResult{}, withVLLMRuntimeEnv(command.Params, func() error {
				return e.runtimeManager.RestartRuntimeNamed(runtimeName)
			})
		}
		return ExecutionResult{}, e.runtimeManager.RestartRuntimeNamed(runtimeName)
	case "update_agent":
		version := "latest"
		if rawVersion, ok := command.Params["version"]; ok {
			if parsedVersion, ok := rawVersion.(string); ok && strings.TrimSpace(parsedVersion) != "" {
				version = strings.TrimSpace(parsedVersion)
			}
		}
		details, err := e.startAgentUpdate(version)
		if err != nil {
			return ExecutionResult{Details: details}, err
		}
		return ExecutionResult{Details: details}, nil
	case "run_harness_exec":
		return e.runHarnessExec(command)
	case "run_orchestrator_exec":
		return e.runOrchestratorExec(command)
	case "sync_harnesses":
		desiredHarnesses, err := desiredHarnessesParam(command.Params)
		if err != nil {
			return ExecutionResult{}, err
		}
		if e.progress != nil {
			e.progress(types.AckRequest{
				CommandID: command.ID,
				Status:    "in_progress",
				Details:   fmt.Sprintf("Checking %d harness(es)...", len(desiredHarnesses)),
			})
		}
		e.prepareHarnessesForSync(desiredHarnesses, command.ID)
		installedHarnesses := probeInstalledHarnesses(desiredHarnesses)
		readyCount := 0
		offlineCount := 0
		failedCount := 0
		for _, harness := range installedHarnesses {
			switch harness.Status {
			case "ready":
				readyCount++
			case "offline":
				offlineCount++
			case "failed":
				failedCount++
			}
		}
		return ExecutionResult{
			Details: fmt.Sprintf(
				"Harness check complete (%d ready, %d offline, %d failed).",
				readyCount,
				offlineCount,
				failedCount,
			),
			ResultPayload: map[string]any{
				"installedHarnesses": installedHarnesses,
			},
		}, nil
	default:
		return ExecutionResult{}, fmt.Errorf("unsupported command type: %s", command.Type)
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

func desiredHarnessesParam(params map[string]interface{}) ([]types.DesiredHarness, error) {
	raw, ok := params["harnesses"]
	if !ok {
		return nil, fmt.Errorf("missing harnesses param")
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal harnesses param: %w", err)
	}
	var harnesses []types.DesiredHarness
	if err := json.Unmarshal(payload, &harnesses); err != nil {
		return nil, fmt.Errorf("decode harnesses param: %w", err)
	}
	return harnesses, nil
}

func probeInstalledHarnesses(desired []types.DesiredHarness) []types.InstalledHarness {
	result := make([]types.InstalledHarness, 0, len(desired))
	for _, harness := range desired {
		if strings.TrimSpace(harness.Type) == "" {
			continue
		}
		item := types.InstalledHarness{
			ID:             harness.ID,
			Name:           harness.Name,
			Type:           harness.Type,
			Status:         "configuring",
			ModelSelection: harness.ModelSelection,
			ManagedModelID: harness.ManagedModelID,
			Capabilities:   harness.Capabilities,
		}

		switch harness.Type {
		case "codex_cli":
			commandName := strings.TrimSpace(harness.Transport.Command)
			if commandName == "" {
				commandName = "codex"
			}
			args := append([]string{}, harness.Transport.Args...)
			if len(args) == 0 {
				args = []string{"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox"}
			}
			item.Transport = &types.HarnessTransportConfig{Kind: "cli", Command: commandName, Args: args}

			path, err := resolveCommandPath(commandName)
			if err != nil {
				item.Status = "offline"
				item.Detail = fmt.Sprintf("%s was not found in PATH", commandName)
				result = append(result, item)
				continue
			}
			item.Transport.Command = path

			item.Status = "ready"
			item.ExecutablePath = path
			item.Version = probeHarnessVersion(path)
			if item.Version != "" {
				item.Detail = "Detected " + item.Version
			} else {
				item.Detail = "Detected executable at " + path
			}
			result = append(result, item)
		case "goose":
			baseURL := strings.TrimSpace(harness.Transport.BaseURL)
			if baseURL == "" {
				baseURL = "https://127.0.0.1:3000"
			}
			commandName := strings.TrimSpace(harness.Transport.Command)
			if commandName == "" {
				commandName = "goosed"
			}
			args := append([]string{}, harness.Transport.Args...)
			if len(args) == 0 {
				args = []string{"agent"}
			}
			item.Transport = &types.HarnessTransportConfig{
				Kind:    "daemon",
				BaseURL: baseURL,
				Command: commandName,
				Args:    args,
			}

			if gooseDaemonReachable(baseURL) {
				item.Status = "ready"
				item.Detail = "Goose daemon is reachable at " + baseURL
				result = append(result, item)
				continue
			}

			path, err := resolveCommandPath(commandName)
			if err != nil {
				item.Status = "offline"
				item.Detail = fmt.Sprintf("Goose daemon not reachable at %s and %s was not found in PATH", baseURL, commandName)
				result = append(result, item)
				continue
			}
			item.Transport.Command = path

			item.Status = "ready"
			item.ExecutablePath = path
			item.Version = probeHarnessVersion(path)
			if item.Version != "" {
				item.Detail = fmt.Sprintf("Detected %s. Daemon is not currently reachable, but %s can be started on demand.", item.Version, commandName)
			} else {
				item.Detail = fmt.Sprintf("Detected executable at %s. Daemon will be started on demand if needed.", path)
			}
			result = append(result, item)
		case "custom_openai":
			baseURL := strings.TrimSpace(harness.Transport.BaseURL)
			endpointPath := strings.TrimSpace(harness.Transport.EndpointPath)
			if endpointPath == "" {
				endpointPath = "/v1/chat/completions"
			}
			item.Transport = &types.HarnessTransportConfig{
				Kind:         "openai_http",
				BaseURL:      baseURL,
				EndpointPath: endpointPath,
			}
			if baseURL == "" {
				item.Status = "failed"
				item.Detail = "Missing base URL for OpenAI-compatible endpoint."
			} else {
				item.Status = "ready"
				item.Detail = fmt.Sprintf("Configured endpoint %s%s", strings.TrimRight(baseURL, "/"), endpointPath)
			}
			result = append(result, item)
		default:
			commandName := strings.TrimSpace(harness.Transport.Command)
			if commandName == "" {
				commandName = defaultCLIHarnessCommand(harness.Type)
			}
			item.Transport = &types.HarnessTransportConfig{
				Kind:    "cli",
				Command: commandName,
				Args:    append([]string{}, harness.Transport.Args...),
			}
			if commandName == "" {
				item.Status = "failed"
				item.Detail = fmt.Sprintf("No default command is defined for harness type %s.", harness.Type)
				result = append(result, item)
				continue
			}
			path, err := resolveCommandPath(commandName)
			if err != nil {
				item.Status = "offline"
				item.Detail = fmt.Sprintf("%s was not found in PATH", commandName)
				result = append(result, item)
				continue
			}
			item.Transport.Command = path

			item.ExecutablePath = path
			item.Version = probeHarnessVersion(path)
			if supportsAgentHarnessExecution(harness.Type) {
				item.Status = "ready"
				if item.Version != "" {
					item.Detail = "Detected " + item.Version
				} else {
					item.Detail = "Detected executable at " + path
				}
			} else {
				item.Status = "failed"
				if item.Version != "" {
					item.Detail = fmt.Sprintf(
						"Detected %s, but this agent build cannot execute %s harness jobs yet.",
						item.Version,
						harness.Type,
					)
				} else {
					item.Detail = fmt.Sprintf(
						"Detected executable at %s, but this agent build cannot execute %s harness jobs yet.",
						path,
						harness.Type,
					)
				}
			}
			result = append(result, item)
		}
	}
	return result
}

func defaultCLIHarnessCommand(harnessType string) string {
	switch harnessType {
	case "opencode":
		return "opencode"
	case "claude_code":
		return "claude"
	case "openharness":
		return "openharness"
	case "aider":
		return "aider"
	case "open_interpreter":
		return "interpreter"
	default:
		return ""
	}
}

func supportsAgentHarnessExecution(harnessType string) bool {
	return harnessType == "codex_cli" || harnessType == "goose"
}

func (e *Executor) prepareHarnessesForSync(desired []types.DesiredHarness, commandID string) {
	for _, harness := range desired {
		if harness.Type != "goose" {
			continue
		}
		if err := e.prepareGooseHarness(harness); err != nil {
			if e.progress != nil {
				e.progress(types.AckRequest{
					CommandID: commandID,
					Status:    "in_progress",
					Details:   fmt.Sprintf("Goose setup check: %v", err),
				})
			}
			continue
		}
		if e.progress != nil {
			e.progress(types.AckRequest{
				CommandID: commandID,
				Status:    "in_progress",
				Details:   "Goose setup verified.",
			})
		}
	}
}

func (e *Executor) prepareGooseHarness(harness types.DesiredHarness) error {
	baseURL := strings.TrimSpace(harness.Transport.BaseURL)
	if baseURL == "" {
		baseURL = "https://127.0.0.1:3000"
	}
	if gooseDaemonReachable(baseURL) {
		return nil
	}
	commandName := strings.TrimSpace(harness.Transport.Command)
	if commandName == "" {
		commandName = "goosed"
	}
	if _, err := resolveCommandPath(commandName); err == nil {
		return nil
	}
	if err := installGooseCLI(); err != nil {
		return fmt.Errorf("unable to bootstrap Goose CLI: %w", err)
	}
	if _, err := resolveCommandPath(commandName); err != nil {
		return fmt.Errorf("Goose CLI bootstrap completed but %s is still unavailable", commandName)
	}
	return nil
}

func installGooseCLI() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := fmt.Sprintf("set -euo pipefail; tmp_script=$(mktemp); trap 'rm -f \"$tmp_script\"' EXIT; curl -fsSL %s -o \"$tmp_script\"; chmod +x \"$tmp_script\"; CONFIGURE=false \"$tmp_script\"", shellQuote(gooseInstallScriptURL))
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, trimmed)
	}
	return nil
}

func resolveCommandPath(commandName string) (string, error) {
	commandName = strings.TrimSpace(commandName)
	if commandName == "" {
		return "", fmt.Errorf("empty command")
	}
	if path, err := exec.LookPath(commandName); err == nil {
		return path, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", commandName),
		filepath.Join("/usr/local/bin", commandName),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&0111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%s not found", commandName)
}

func gooseDaemonReachable(baseURL string) bool {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return false
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get(baseURL + "/status")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func probeHarnessVersion(commandName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, commandName, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
