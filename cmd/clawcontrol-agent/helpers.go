package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Borgels/clawcontrol-agent/internal/client"
	"github.com/Borgels/clawcontrol-agent/internal/runtime"
	"github.com/Borgels/clawcontrol-agent/internal/types"
)

const desiredConfigCachePath = "/etc/clawcontrol/desired-config.json"
const lmstudioKeepWarmEnv = "CLAWCONTROL_LMSTUDIO_KEEP_WARM"

func enforceDesiredConfig(runtimeManager *runtime.Manager, desired types.DesiredConfig) {
	ejectLMStudio := shouldAutoEjectLMStudio(desired)
	for _, runtimeType := range desired.Runtimes {
		if err := runtimeManager.EnsureRuntime(string(runtimeType)); err != nil {
			log.Printf("failed to ensure runtime %s: %v", runtimeType, err)
		}
	}

	modelsHandled := map[string]bool{}
	for _, target := range desired.ModelTargets {
		modelsHandled[target.ModelID] = true
		runtimeName := strings.ToLower(strings.TrimSpace(string(target.Runtime)))
		if ejectLMStudio && runtimeName == string(types.RuntimeLMStudio) {
			log.Printf(
				"skipping lmstudio ensure for %s due to idle-eject policy; set %s=true to keep warm",
				strings.TrimSpace(target.ModelID),
				lmstudioKeepWarmEnv,
			)
			continue
		}
		flags := target.FeatureFlags
		if err := runtimeManager.PrepareModelWithRuntime(target.ModelID, string(target.Runtime), &flags); err != nil {
			log.Printf("failed to prepare model target %s: %v", target.ModelID, err)
		}
	}
	for _, modelID := range desired.Models {
		if modelsHandled[modelID] {
			continue
		}
		if err := runtimeManager.PrepareModelWithFlags(modelID, nil); err != nil {
			log.Printf("failed to prepare model %s: %v", modelID, err)
		}
	}
}

func reconcileStaleModels(runtimeManager *runtime.Manager, desired types.DesiredConfig) {
	ejectLMStudio := shouldAutoEjectLMStudio(desired)
	desiredGlobal := map[string]struct{}{}
	desiredByRuntime := map[string]map[string]struct{}{}

	addRuntimeDesired := func(runtimeName string, modelID string) {
		runtimeName = strings.ToLower(strings.TrimSpace(runtimeName))
		modelID = strings.TrimSpace(modelID)
		if runtimeName == "" || modelID == "" {
			return
		}
		if _, ok := desiredByRuntime[runtimeName]; !ok {
			desiredByRuntime[runtimeName] = map[string]struct{}{}
		}
		desiredByRuntime[runtimeName][modelID] = struct{}{}
	}

	for _, target := range desired.ModelTargets {
		modelID := strings.TrimSpace(target.ModelID)
		if modelID == "" {
			continue
		}
		desiredGlobal[modelID] = struct{}{}
		addRuntimeDesired(string(target.Runtime), modelID)
	}

	for _, modelID := range desired.Models {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		desiredGlobal[modelID] = struct{}{}
	}

	for _, runtimeName := range runtimeManager.InstalledRuntimes() {
		driver, err := runtimeManager.DriverFor(runtimeName)
		if err != nil {
			log.Printf("failed to inspect runtime %s during stale reconciliation: %v", runtimeName, err)
			continue
		}

		models := driver.ListModels()
		runtimeDesired := desiredByRuntime[runtimeName]
		for _, modelID := range models {
			modelID = strings.TrimSpace(modelID)
			if modelID == "" {
				continue
			}
			if !(ejectLMStudio && runtimeName == string(types.RuntimeLMStudio)) {
				if _, ok := desiredGlobal[modelID]; ok {
					continue
				}
				if _, ok := runtimeDesired[modelID]; ok {
					continue
				}
			}
			if err := runtimeManager.RemoveModelWithRuntime(modelID, runtimeName); err != nil {
				log.Printf(
					"failed to reconcile stale model %s on runtime %s: %v",
					modelID,
					runtimeName,
					err,
				)
				continue
			}
			log.Printf("reconciled stale model %s on runtime %s", modelID, runtimeName)
		}
	}
}

func shouldAutoEjectLMStudio(_ types.DesiredConfig) bool {
	return !lmstudioKeepWarmEnabled()
}

func lmstudioKeepWarmEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(lmstudioKeepWarmEnv)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
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

func toInstalledHarnesses(desired types.DesiredConfig) []types.InstalledHarness {
	result := make([]types.InstalledHarness, 0, len(desired.Harnesses))
	for _, harness := range desired.Harnesses {
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

			path, err := exec.LookPath(commandName)
			if err != nil {
				item.Status = "offline"
				item.Detail = fmt.Sprintf("%s was not found in PATH", commandName)
				result = append(result, item)
				continue
			}

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

			if ok := gooseDaemonReachable(baseURL); ok {
				item.Status = "ready"
				item.Detail = "Goose daemon is reachable at " + baseURL
				result = append(result, item)
				continue
			}

			path, err := exec.LookPath(commandName)
			if err != nil {
				item.Status = "offline"
				item.Detail = fmt.Sprintf("Goose daemon not reachable at %s and %s was not found in PATH", baseURL, commandName)
				result = append(result, item)
				continue
			}

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
			args := append([]string{}, harness.Transport.Args...)
			item.Transport = &types.HarnessTransportConfig{
				Kind:    "cli",
				Command: commandName,
				Args:    args,
			}
			if commandName == "" {
				item.Status = "failed"
				item.Detail = fmt.Sprintf("No default command is defined for harness type %s.", harness.Type)
				result = append(result, item)
				continue
			}

			path, err := exec.LookPath(commandName)
			if err != nil {
				item.Status = "offline"
				item.Detail = fmt.Sprintf("%s was not found in PATH", commandName)
				result = append(result, item)
				continue
			}

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

func probeHarnessVersion(commandPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, commandPath, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func toInstalledOrchestrators(desired types.DesiredConfig) []types.InstalledOrchestrator {
	result := make([]types.InstalledOrchestrator, 0, len(desired.Orchestrators))
	for _, orchestrator := range desired.Orchestrators {
		if strings.TrimSpace(orchestrator.Type) == "" {
			continue
		}
		item := types.InstalledOrchestrator{
			ID:           orchestrator.ID,
			Name:         orchestrator.Name,
			Type:         orchestrator.Type,
			Status:       "configuring",
			Capabilities: orchestrator.Capabilities,
		}

		switch orchestrator.Type {
		case "builtin":
			if item.Capabilities == nil {
				item.Capabilities = defaultOrchestratorCapabilities(orchestrator.Type)
			}
			item.Status = "ready"
			item.Detail = "Built-in orchestrator is managed by ClawControl."
		case "crewai", "langgraph", "autogen":
			if item.Capabilities == nil {
				item.Capabilities = defaultOrchestratorCapabilities(orchestrator.Type)
			}
			commandName := firstNonEmpty(orchestrator.Command, defaultOrchestratorCommand(orchestrator.Type))
			path, detail, err := ensureOrchestratorExecutable(orchestrator.Type, commandName)
			if err != nil {
				item.Status = "offline"
				item.Detail = detail
			} else {
				item.Status = "ready"
				item.Version = probeHarnessVersion(path)
				if item.Version != "" {
					item.Detail = executableDetail(path, item.Version)
				} else {
					item.Detail = detail
				}
			}
		default:
			item.Status = "failed"
			item.Detail = fmt.Sprintf("Unknown orchestrator type %s", orchestrator.Type)
		}

		result = append(result, item)
	}
	return result
}

func defaultOrchestratorCapabilities(orchestratorType string) *types.OrchestratorCapabilities {
	switch orchestratorType {
	case "builtin":
		return &types.OrchestratorCapabilities{
			SupportsQualityGates:     boolPtr(true),
			SupportsSkillInjection:   boolPtr(true),
			SupportsSubTasks:         boolPtr(true),
			SupportsConcurrentAgents: boolPtr(false),
		}
	case "crewai", "langgraph", "autogen":
		return &types.OrchestratorCapabilities{
			SupportsQualityGates:     boolPtr(true),
			SupportsSkillInjection:   boolPtr(true),
			SupportsSubTasks:         boolPtr(true),
			SupportsConcurrentAgents: boolPtr(true),
		}
	default:
		return nil
	}
}

func boolPtr(value bool) *bool {
	return &value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func defaultOrchestratorCommand(orchestratorType string) string {
	switch strings.ToLower(strings.TrimSpace(orchestratorType)) {
	case "crewai":
		return "crewai"
	case "langgraph":
		return "langgraph"
	case "autogen":
		return "autogen"
	default:
		return ""
	}
}

func resolveExecutableWithUserPath(command string) (string, error) {
	if path, err := exec.LookPath(command); err == nil {
		return path, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", command)
		if stat, statErr := os.Stat(candidate); statErr == nil && !stat.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s executable was not found in PATH", command)
}

func ensureOrchestratorExecutable(orchestratorType string, commandName string) (string, string, error) {
	commandName = strings.TrimSpace(commandName)
	if commandName == "" {
		return "", "No orchestrator command configured.", fmt.Errorf("missing command")
	}
	if path, err := resolveExecutableWithUserPath(commandName); err == nil {
		return path, fmt.Sprintf("Detected executable at %s", path), nil
	}

	if err := autoInstallOrchestrator(orchestratorType); err != nil {
		return "", fmt.Sprintf("%s not found and auto-install failed: %v", commandName, err), err
	}
	if path, err := resolveExecutableWithUserPath(commandName); err == nil {
		return path, fmt.Sprintf("Installed and detected executable at %s", path), nil
	}
	return "", fmt.Sprintf("%s still not found after auto-install", commandName), fmt.Errorf("executable missing after install")
}

func autoInstallOrchestrator(orchestratorType string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	type installStep struct {
		binary string
		args   []string
	}
	var steps []installStep
	switch strings.ToLower(strings.TrimSpace(orchestratorType)) {
	case "crewai":
		steps = []installStep{{binary: "python3", args: []string{"-m", "pip", "install", "--user", "--upgrade", "crewai"}}}
	case "langgraph":
		steps = []installStep{{binary: "python3", args: []string{"-m", "pip", "install", "--user", "--upgrade", "langgraph-cli"}}}
	case "autogen":
		steps = []installStep{{binary: "python3", args: []string{"-m", "pip", "install", "--user", "--upgrade", "pyautogen"}}}
	default:
		return fmt.Errorf("unsupported orchestrator type: %s", orchestratorType)
	}

	var lastErr error
	for _, step := range steps {
		if _, err := exec.LookPath(step.binary); err != nil {
			lastErr = err
			continue
		}
		cmd := exec.CommandContext(ctx, step.binary, step.args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			lastErr = fmt.Errorf("%s %s: %w (%s)", step.binary, strings.Join(step.args, " "), err, strings.TrimSpace(string(output)))
			continue
		}
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no installer available")
}

func executableDetail(path string, version string) string {
	if version != "" {
		return "Detected " + version
	}
	return "Detected executable at " + path
}

func harnessReportsDiffer(a []types.InstalledHarness, b []types.InstalledHarness) bool {
	if len(a) != len(b) {
		return true
	}
	if len(a) == 0 {
		return false
	}

	type comparableHarness struct {
		ID             string
		Name           string
		Type           string
		Status         string
		Version        string
		ExecutablePath string
		Detail         string
		Command        string
		Args           string
	}

	toMap := func(items []types.InstalledHarness) map[string]comparableHarness {
		result := make(map[string]comparableHarness, len(items))
		for _, item := range items {
			key := strings.TrimSpace(item.ID)
			if key == "" {
				key = strings.TrimSpace(item.Type + "::" + item.Name)
			}
			result[key] = comparableHarness{
				ID:             item.ID,
				Name:           item.Name,
				Type:           item.Type,
				Status:         item.Status,
				Version:        item.Version,
				ExecutablePath: item.ExecutablePath,
				Detail:         item.Detail,
				Command: func() string {
					if item.Transport != nil {
						return item.Transport.Command
					}
					return ""
				}(),
				Args: func() string {
					if item.Transport != nil {
						return strings.Join(item.Transport.Args, "\x00")
					}
					return ""
				}(),
			}
		}
		return result
	}

	left := toMap(a)
	right := toMap(b)
	if len(left) != len(right) {
		return true
	}
	for key, value := range left {
		if right[key] != value {
			return true
		}
	}
	return false
}

func orchestratorReportsDiffer(a []types.InstalledOrchestrator, b []types.InstalledOrchestrator) bool {
	if len(a) != len(b) {
		return true
	}
	if len(a) == 0 {
		return false
	}

	type comparableOrchestrator struct {
		ID      string
		Name    string
		Type    string
		Status  string
		Version string
		Detail  string
	}

	toMap := func(items []types.InstalledOrchestrator) map[string]comparableOrchestrator {
		result := make(map[string]comparableOrchestrator, len(items))
		for _, item := range items {
			key := strings.TrimSpace(item.ID)
			if key == "" {
				key = strings.TrimSpace(item.Type + "::" + item.Name)
			}
			result[key] = comparableOrchestrator{
				ID:      item.ID,
				Name:    item.Name,
				Type:    item.Type,
				Status:  item.Status,
				Version: item.Version,
				Detail:  item.Detail,
			}
		}
		return result
	}

	left := toMap(a)
	right := toMap(b)
	if len(left) != len(right) {
		return true
	}
	for key, value := range left {
		if right[key] != value {
			return true
		}
	}
	return false
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
