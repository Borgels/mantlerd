package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/mantlerd/internal/manifest"
	"github.com/Borgels/mantlerd/internal/types"
)

const orchestratorScannerMaxTokenSize = 1024 * 1024

type orchestratorExecParams struct {
	OrchestratorID      string
	OrchestratorType    string
	CompatibilityPlanID string
	MantleFingerprint   string
	Command             string
	Args                []string
	WorkingDir          string
	Task                map[string]interface{}
	Skills              []map[string]interface{}
	ResourceManifest    *types.ResourceManifest
}

type orchestratorExecState struct {
	lastLine string
	mu       sync.Mutex
}

func (e *Executor) runOrchestratorExec(command types.AgentCommand) (ExecutionResult, error) {
	params, err := parseOrchestratorExecParams(command.Params)
	if err != nil {
		return ExecutionResult{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmdName := params.Command
	cmdArgs := append([]string{}, params.Args...)
	if cmdName == "" {
		switch params.OrchestratorType {
		case "crewai":
			cmdName = "crewai"
			cmdArgs = append(cmdArgs, "run")
		case "langgraph":
			cmdName = "langgraph"
			cmdArgs = append(cmdArgs, "dev")
		case "autogen", "ag2":
			cmdName = "ag2"
		case "semantic_kernel":
			cmdName = "semantic-kernel"
		case "haystack":
			cmdName = "haystack"
		case "mastra":
			cmdName = "mastra"
		default:
			return ExecutionResult{}, fmt.Errorf("missing orchestrator command for type %s", params.OrchestratorType)
		}
	}

	commandPath, err := resolveOrchestratorExecutable(params.OrchestratorType, cmdName)
	if err != nil {
		return ExecutionResult{}, err
	}
	if err := validateOrchestratorArgs(params.OrchestratorType, cmdArgs); err != nil {
		return ExecutionResult{}, err
	}

	cmd := exec.CommandContext(ctx, commandPath, cmdArgs...)
	if params.WorkingDir != "" {
		workingDir, err := filepath.Abs(params.WorkingDir)
		if err != nil {
			return ExecutionResult{}, fmt.Errorf("resolve orchestrator workingDir: %w", err)
		}
		workingDir, err = sanitizeOrchestratorWorkingDir(workingDir)
		if err != nil {
			return ExecutionResult{}, err
		}
		info, err := os.Stat(workingDir)
		if err != nil {
			return ExecutionResult{}, fmt.Errorf("stat orchestrator workingDir: %w", err)
		}
		if !info.IsDir() {
			return ExecutionResult{}, fmt.Errorf("orchestrator workingDir is not a directory: %s", workingDir)
		}
		cmd.Dir = workingDir
		params.WorkingDir = workingDir
	}

	taskFile, err := writeOrchestratorPayloadFile("task", params.Task)
	if err != nil {
		return ExecutionResult{}, err
	}
	defer os.Remove(taskFile)

	skillsFile, err := writeOrchestratorPayloadFile("skills", params.Skills)
	if err != nil {
		return ExecutionResult{}, err
	}
	defer os.Remove(skillsFile)

	manifestFile := ""
	var watchdog *manifest.Watchdog
	if params.ResourceManifest != nil {
		e.registerManifest(command.ID, params.ResourceManifest)
		defer e.unregisterManifest(command.ID)
		preflightStarted := time.Now()
		preflight, preflightErr := manifest.RunPreflight(
			ctx,
			*params.ResourceManifest,
			e.cfg.MachineID,
			e.runtimeManager,
			func(msg string) {
				e.emitHarnessProgress(command.ID, msg, &types.CommandStreamEvent{
					Type:      "content",
					Content:   msg,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
				})
			},
		)
		if preflightErr != nil {
			e.emitOutcome(types.OutcomeEvent{
				PlanID:            params.CompatibilityPlanID,
				MantleFingerprint: params.MantleFingerprint,
				EventType:         "startup_failure",
				DurationMs:        time.Since(preflightStarted).Milliseconds(),
				CrashSignature:    "manifest_preflight_failed",
				Detail:            preflightErr.Error(),
				Timestamp:         time.Now().UTC().Format(time.RFC3339),
			})
			return ExecutionResult{}, fmt.Errorf("manifest preflight failed: %w", preflightErr)
		}
		if preflight != nil && !preflight.Ready {
			detail := "Orchestrator preflight failed."
			if len(preflight.Issues) > 0 {
				detail = strings.Join(preflight.Issues, " ")
			}
			e.emitOutcome(types.OutcomeEvent{
				PlanID:            params.CompatibilityPlanID,
				MantleFingerprint: params.MantleFingerprint,
				EventType:         "startup_failure",
				DurationMs:        time.Since(preflightStarted).Milliseconds(),
				CrashSignature:    "manifest_preflight_not_ready",
				Detail:            detail,
				Timestamp:         time.Now().UTC().Format(time.RFC3339),
			})
			return ExecutionResult{Details: detail}, fmt.Errorf(detail)
		}
		e.emitOutcome(types.OutcomeEvent{
			PlanID:            params.CompatibilityPlanID,
			MantleFingerprint: params.MantleFingerprint,
			EventType:         "readiness",
			DurationMs:        time.Since(preflightStarted).Milliseconds(),
			Detail:            "manifest preflight passed",
			Timestamp:         time.Now().UTC().Format(time.RFC3339),
		})
		manifestFile, err = writeOrchestratorPayloadFile("manifest", params.ResourceManifest)
		if err != nil {
			return ExecutionResult{}, err
		}
		defer os.Remove(manifestFile)
		watchdog = manifest.NewWatchdog(
			*params.ResourceManifest,
			e.cfg.MachineID,
			e.runtimeManager,
			func(msg string, eventType string) {
				event := &types.CommandStreamEvent{
					Type:      eventType,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
				}
				if eventType == "error" {
					event.Detail = msg
				} else {
					event.Content = msg
				}
				e.emitHarnessProgress(command.ID, msg, event)
				if eventType == "error" {
					e.emitOutcome(types.OutcomeEvent{
						PlanID:            params.CompatibilityPlanID,
						MantleFingerprint: params.MantleFingerprint,
						EventType:         "crash",
						CrashSignature:    "watchdog_error",
						Detail:            msg,
						Timestamp:         time.Now().UTC().Format(time.RFC3339),
					})
				}
			},
		)
	}

	cmd.Env = append(os.Environ(),
		"MANTLER_ORCHESTRATOR_ID="+params.OrchestratorID,
		"MANTLER_ORCHESTRATOR_TYPE="+params.OrchestratorType,
		"MANTLER_TASK_FILE="+taskFile,
		"MANTLER_SKILLS_FILE="+skillsFile,
	)
	if manifestFile != "" {
		cmd.Env = append(cmd.Env, "MANTLER_MANIFEST_FILE="+manifestFile)
	}
	if description, ok := params.Task["description"].(string); ok && strings.TrimSpace(description) != "" {
		cmd.Stdin = strings.NewReader(description)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("stderr pipe: %w", err)
	}

	state := &orchestratorExecState{}
	relay := func(prefix string, scanner *bufio.Scanner) {
		scanner.Buffer(make([]byte, 0, 64*1024), orchestratorScannerMaxTokenSize)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			state.mu.Lock()
			state.lastLine = line
			state.mu.Unlock()
			e.emitHarnessProgress(command.ID, prefix+line, &types.CommandStreamEvent{
				Type:      "content",
				Content:   prefix + line,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
		}
		if err := scanner.Err(); err != nil {
			state.mu.Lock()
			state.lastLine = prefix + err.Error()
			state.mu.Unlock()
			e.emitHarnessProgress(command.ID, prefix+err.Error(), &types.CommandStreamEvent{
				Type:      "error",
				Detail:    prefix + err.Error(),
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
		}
	}

	if err := cmd.Start(); err != nil {
		return ExecutionResult{}, fmt.Errorf("start orchestrator: %w", err)
	}
	if watchdog != nil {
		watchdog.Start(ctx)
		defer watchdog.Stop()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		relay("", bufio.NewScanner(stdout))
	}()
	go func() {
		defer wg.Done()
		relay("[stderr] ", bufio.NewScanner(stderr))
	}()

	err = cmd.Wait()
	wg.Wait()

	state.mu.Lock()
	lastLine := state.lastLine
	state.mu.Unlock()

	if err != nil {
		if lastLine == "" {
			lastLine = err.Error()
		}
		exitCode := 1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		e.emitOutcome(types.OutcomeEvent{
			PlanID:            params.CompatibilityPlanID,
			MantleFingerprint: params.MantleFingerprint,
			EventType:         "startup_failure",
			ExitCode:          exitCode,
			CrashSignature:    "orchestrator_exec_failed",
			Detail:            lastLine,
			Timestamp:         time.Now().UTC().Format(time.RFC3339),
		})
		return ExecutionResult{Details: lastLine}, fmt.Errorf("orchestrator execution failed: %w", err)
	}

	summary := lastLine
	if summary == "" {
		summary = fmt.Sprintf("%s completed successfully.", params.OrchestratorType)
	}

	e.emitOutcome(types.OutcomeEvent{
		PlanID:            params.CompatibilityPlanID,
		MantleFingerprint: params.MantleFingerprint,
		EventType:         "startup_success",
		ExitCode:          0,
		Detail:            summary,
		Timestamp:         time.Now().UTC().Format(time.RFC3339),
	})

	return ExecutionResult{
		Details: summary,
		ResultPayload: map[string]any{
			"summary":          summary,
			"orchestratorId":   params.OrchestratorID,
			"orchestratorType": params.OrchestratorType,
			"workingDir":       filepath.Clean(params.WorkingDir),
		},
	}, nil
}

func resolveOrchestratorExecutable(orchestratorType string, commandName string) (string, error) {
	candidates := orchestratorCommandCandidates(orchestratorType, commandName)
	for _, candidate := range candidates {
		if path, err := resolveExecutableWithUserPath(candidate); err == nil {
			return path, nil
		}
	}
	if err := autoInstallOrchestratorBinary(orchestratorType); err != nil {
		return "", fmt.Errorf("orchestrator command %q not found and auto-install failed: %w", commandName, err)
	}
	for _, candidate := range candidates {
		if path, err := resolveExecutableWithUserPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("orchestrator command %q still not found after auto-install", commandName)
}

func resolveExecutableWithUserPath(commandName string) (string, error) {
	if path, err := exec.LookPath(commandName); err == nil {
		return path, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", commandName)
		if stat, statErr := os.Stat(candidate); statErr == nil && !stat.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found in PATH", commandName)
}

func defaultOrchestratorCommand(orchestratorType string) string {
	switch strings.ToLower(strings.TrimSpace(orchestratorType)) {
	case "crewai":
		return "crewai"
	case "langgraph":
		return "langgraph"
	case "autogen", "ag2":
		return "ag2"
	case "semantic_kernel":
		return "semantic-kernel"
	case "haystack":
		return "haystack"
	case "mastra":
		return "mastra"
	default:
		return ""
	}
}

func orchestratorCommandCandidates(orchestratorType string, commandName string) []string {
	preferred := strings.TrimSpace(commandName)
	seen := map[string]struct{}{}
	result := make([]string, 0, 3)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}

	add(preferred)
	switch strings.ToLower(strings.TrimSpace(orchestratorType)) {
	case "autogen", "ag2":
		add("ag2")
		add("autogen")
	case "semantic_kernel":
		add("semantic-kernel")
		add("semantic_kernel")
	case "haystack":
		add("haystack")
	case "mastra":
		add("mastra")
	}
	return result
}

func orchestratorPackageCandidates(orchestratorType string) []string {
	switch strings.ToLower(strings.TrimSpace(orchestratorType)) {
	case "crewai":
		return []string{"crewai"}
	case "langgraph":
		return []string{"langgraph-cli"}
	case "autogen", "ag2":
		return []string{"ag2", "pyautogen"}
	case "semantic_kernel":
		return []string{"semantic-kernel"}
	case "haystack":
		return []string{"haystack-ai", "haystack"}
	case "mastra":
		return []string{"mastra"}
	default:
		return nil
	}
}

func autoInstallOrchestratorBinary(orchestratorType string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	type installStep struct {
		binary string
		args   []string
	}
	packages := orchestratorPackageCandidates(orchestratorType)
	if len(packages) == 0 {
		return fmt.Errorf("unsupported orchestrator type: %s", orchestratorType)
	}
	commandCandidates := orchestratorCommandCandidates(orchestratorType, defaultOrchestratorCommand(orchestratorType))
	if len(commandCandidates) == 0 {
		return fmt.Errorf("unsupported orchestrator command for type: %s", orchestratorType)
	}

	var lastErr error
	for _, pkg := range packages {
		if strings.EqualFold(strings.TrimSpace(orchestratorType), "mastra") {
			if err := installOrchestratorViaNode(ctx, pkg, commandCandidates); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
		steps := []installStep{
			{binary: "pipx", args: []string{"install", "--force", pkg}},
			{binary: "uv", args: []string{"tool", "install", "--force", pkg}},
			{binary: "python3", args: []string{"-m", "pip", "install", "--user", "--upgrade", "--break-system-packages", pkg}},
		}

		for _, step := range steps {
			if _, err := exec.LookPath(step.binary); err != nil {
				lastErr = err
				continue
			}
			cmd := exec.CommandContext(ctx, step.binary, step.args...)
			if output, err := cmd.CombinedOutput(); err != nil {
				lastErr = fmt.Errorf("%s %s failed: %w (%s)", step.binary, strings.Join(step.args, " "), err, strings.TrimSpace(string(output)))
				continue
			}
			for _, candidate := range commandCandidates {
				if _, err := resolveExecutableWithUserPath(candidate); err == nil {
					return nil
				}
			}
		}

		for _, candidate := range commandCandidates {
			if err := installOrchestratorViaVenv(ctx, orchestratorType, pkg, candidate); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no installer available")
}

func installOrchestratorViaNode(ctx context.Context, pkg string, commandCandidates []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("npm"); err != nil {
		return err
	}
	prefixDir := filepath.Join(home, ".local")
	cmd := exec.CommandContext(ctx, "npm", "install", "--global", "--prefix", prefixDir, pkg)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("npm install --global --prefix %s %s failed: %w (%s)", prefixDir, pkg, err, strings.TrimSpace(string(output)))
	}
	for _, candidate := range commandCandidates {
		if _, err := resolveExecutableWithUserPath(candidate); err == nil {
			return nil
		}
	}
	return fmt.Errorf("node package installed but no executable detected for %s", pkg)
}

func installOrchestratorViaVenv(ctx context.Context, orchestratorType, pkg, commandName string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	venvDir := filepath.Join(home, ".local", "share", "mantler", "orchestrators", strings.ToLower(orchestratorType))
	if err := os.MkdirAll(filepath.Dir(venvDir), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(venvDir, "bin", "python")); err != nil {
		if output, venvErr := exec.CommandContext(ctx, "python3", "-m", "venv", venvDir).CombinedOutput(); venvErr != nil {
			return fmt.Errorf("python3 -m venv %s: %w (%s)", venvDir, venvErr, strings.TrimSpace(string(output)))
		}
	}
	pythonBin := filepath.Join(venvDir, "bin", "python")
	if output, pipErr := exec.CommandContext(ctx, pythonBin, "-m", "pip", "install", "--upgrade", pkg).CombinedOutput(); pipErr != nil {
		return fmt.Errorf("%s -m pip install --upgrade %s: %w (%s)", pythonBin, pkg, pipErr, strings.TrimSpace(string(output)))
	}
	localBinDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBinDir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(venvDir, "bin", commandName)
	if _, err := os.Stat(target); err != nil {
		if strings.EqualFold(strings.TrimSpace(orchestratorType), "autogen") || strings.EqualFold(strings.TrimSpace(orchestratorType), "ag2") {
			pythonBin := filepath.Join(venvDir, "bin", "python")
			shimScript := "#!/usr/bin/env bash\nexec \"" + pythonBin + "\" -m autogen \"$@\"\n"
			if writeErr := os.WriteFile(target, []byte(shimScript), 0o755); writeErr != nil {
				return fmt.Errorf("venv installed package but command %s not found", target)
			}
		} else {
			return fmt.Errorf("venv installed package but command %s not found", target)
		}
	}
	shim := filepath.Join(localBinDir, commandName)
	_ = os.Remove(shim)
	if err := os.Symlink(target, shim); err != nil {
		return fmt.Errorf("create shim %s -> %s: %w", shim, target, err)
	}
	return nil
}

func writeOrchestratorPayloadFile(prefix string, payload any) (string, error) {
	file, err := os.CreateTemp("", "mantler-orchestrator-"+prefix+"-*.json")
	if err != nil {
		return "", fmt.Errorf("create orchestrator %s payload file: %w", prefix, err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		return "", fmt.Errorf("write orchestrator %s payload file: %w", prefix, err)
	}
	return file.Name(), nil
}

func parseOrchestratorExecParams(params map[string]interface{}) (orchestratorExecParams, error) {
	result := orchestratorExecParams{
		OrchestratorID:      optionalStringParam(params, "orchestratorId"),
		OrchestratorType:    optionalStringParam(params, "orchestratorType"),
		CompatibilityPlanID: optionalStringParam(params, "compatibilityPlanId"),
		MantleFingerprint:   optionalStringParam(params, "mantleFingerprint"),
		Command:             optionalStringParam(params, "command"),
		WorkingDir:          optionalStringParam(params, "workingDir"),
		Task:                map[string]interface{}{},
	}
	if result.OrchestratorType == "" {
		return result, fmt.Errorf("missing orchestratorType param")
	}
	if rawArgs, ok := params["args"].([]interface{}); ok {
		for _, entry := range rawArgs {
			text, ok := entry.(string)
			if ok && strings.TrimSpace(text) != "" {
				result.Args = append(result.Args, strings.TrimSpace(text))
			}
		}
	}
	if rawTask, ok := params["task"].(map[string]interface{}); ok {
		result.Task = rawTask
	}
	if rawSkills, ok := params["skills"].([]interface{}); ok {
		for _, item := range rawSkills {
			if skill, ok := item.(map[string]interface{}); ok {
				result.Skills = append(result.Skills, skill)
			}
		}
	}
	if rawManifest, ok := params["resourceManifest"].(map[string]interface{}); ok {
		payload, marshalErr := json.Marshal(rawManifest)
		if marshalErr == nil {
			var manifestPayload types.ResourceManifest
			if unmarshalErr := json.Unmarshal(payload, &manifestPayload); unmarshalErr == nil {
				result.ResourceManifest = &manifestPayload
			}
		}
	}
	return result, nil
}

func validateOrchestratorArgs(orchestratorType string, args []string) error {
	allowedFlagPrefixes := map[string][]string{
		"crewai":          {"--help", "--version", "--verbose", "--quiet", "--config", "--port", "--host"},
		"langgraph":       {"--help", "--version", "--verbose", "--quiet", "--config", "--port", "--host"},
		"autogen":         {"--help", "--version", "--verbose", "--quiet", "--config", "--port", "--host"},
		"ag2":             {"--help", "--version", "--verbose", "--quiet", "--config", "--port", "--host"},
		"semantic_kernel": {"--help", "--version", "--verbose", "--quiet", "--config", "--port", "--host"},
		"haystack":        {"--help", "--version", "--verbose", "--quiet", "--config", "--port", "--host"},
		"mastra":          {"--help", "--version", "--verbose", "--quiet", "--config", "--port", "--host"},
		"custom":          {"--help", "--version", "--verbose", "--quiet", "--config", "--port", "--host"},
	}
	normalizedType := strings.ToLower(strings.TrimSpace(orchestratorType))
	allowed := allowedFlagPrefixes[normalizedType]
	for _, arg := range args {
		trimmed := strings.TrimSpace(arg)
		if trimmed == "" || !strings.HasPrefix(trimmed, "-") {
			continue
		}
		permitted := false
		for _, prefix := range allowed {
			if strings.HasPrefix(trimmed, prefix) {
				permitted = true
				break
			}
		}
		if !permitted {
			return fmt.Errorf("orchestrator argument %q is not allowed for type %s", arg, orchestratorType)
		}
	}
	return nil
}

func sanitizeOrchestratorWorkingDir(path string) (string, error) {
	cleanPath := filepath.Clean(strings.TrimSpace(path))
	if cleanPath == "." || cleanPath == "" {
		return "", fmt.Errorf("orchestrator workingDir is empty")
	}
	if !filepath.IsAbs(cleanPath) {
		return "", fmt.Errorf("orchestrator workingDir must be absolute")
	}
	allowedRoots := []string{"/var/lib/mantler"}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		allowedRoots = append(allowedRoots, filepath.Clean(home))
	}
	for _, root := range allowedRoots {
		if cleanPath == root || strings.HasPrefix(cleanPath, root+string(os.PathSeparator)) {
			return cleanPath, nil
		}
	}
	return "", fmt.Errorf("orchestrator workingDir %q must be under approved roots", cleanPath)
}
