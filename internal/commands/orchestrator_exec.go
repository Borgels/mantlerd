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

	"github.com/Borgels/clawcontrol-agent/internal/types"
)

const orchestratorScannerMaxTokenSize = 1024 * 1024

type orchestratorExecParams struct {
	OrchestratorID   string
	OrchestratorType string
	Command          string
	Args             []string
	WorkingDir       string
	Task             map[string]interface{}
	Skills           []map[string]interface{}
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
		case "autogen":
			cmdName = "autogen"
		default:
			return ExecutionResult{}, fmt.Errorf("missing orchestrator command for type %s", params.OrchestratorType)
		}
	}

	commandPath, err := resolveOrchestratorExecutable(params.OrchestratorType, cmdName)
	if err != nil {
		return ExecutionResult{}, err
	}

	cmd := exec.CommandContext(ctx, commandPath, cmdArgs...)
	if params.WorkingDir != "" {
		workingDir, err := filepath.Abs(params.WorkingDir)
		if err != nil {
			return ExecutionResult{}, fmt.Errorf("resolve orchestrator workingDir: %w", err)
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

	cmd.Env = append(os.Environ(),
		"CLAWCONTROL_ORCHESTRATOR_ID="+params.OrchestratorID,
		"CLAWCONTROL_ORCHESTRATOR_TYPE="+params.OrchestratorType,
		"CLAWCONTROL_TASK_FILE="+taskFile,
		"CLAWCONTROL_SKILLS_FILE="+skillsFile,
	)
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
		return ExecutionResult{Details: lastLine}, fmt.Errorf("orchestrator execution failed: %w", err)
	}

	summary := lastLine
	if summary == "" {
		summary = fmt.Sprintf("%s completed successfully.", params.OrchestratorType)
	}

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
	if path, err := resolveExecutableWithUserPath(commandName); err == nil {
		return path, nil
	}
	if err := autoInstallOrchestratorBinary(orchestratorType); err != nil {
		return "", fmt.Errorf("orchestrator command %q not found and auto-install failed: %w", commandName, err)
	}
	if path, err := resolveExecutableWithUserPath(commandName); err == nil {
		return path, nil
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

func autoInstallOrchestratorBinary(orchestratorType string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	type installStep struct {
		binary string
		args   []string
	}
	pkg := ""
	commandName := ""
	switch strings.ToLower(strings.TrimSpace(orchestratorType)) {
	case "crewai":
		pkg = "crewai"
		commandName = "crewai"
	case "langgraph":
		pkg = "langgraph-cli"
		commandName = "langgraph"
	case "autogen":
		pkg = "pyautogen"
		commandName = "autogen"
	default:
		return fmt.Errorf("unsupported orchestrator type: %s", orchestratorType)
	}

	steps := []installStep{
		{binary: "pipx", args: []string{"install", "--force", pkg}},
		{binary: "uv", args: []string{"tool", "install", "--force", pkg}},
		{binary: "python3", args: []string{"-m", "pip", "install", "--user", "--upgrade", "--break-system-packages", pkg}},
	}

	var lastErr error
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
		if _, err := resolveExecutableWithUserPath(commandName); err == nil {
			return nil
		}
	}

	if err := installOrchestratorViaVenv(ctx, orchestratorType, pkg, commandName); err == nil {
		return nil
	} else {
		lastErr = err
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no installer available")
}

func installOrchestratorViaVenv(ctx context.Context, orchestratorType, pkg, commandName string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	venvDir := filepath.Join(home, ".local", "share", "clawcontrol", "orchestrators", strings.ToLower(orchestratorType))
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
		return fmt.Errorf("venv installed package but command %s not found", target)
	}
	shim := filepath.Join(localBinDir, commandName)
	_ = os.Remove(shim)
	if err := os.Symlink(target, shim); err != nil {
		return fmt.Errorf("create shim %s -> %s: %w", shim, target, err)
	}
	return nil
}

func writeOrchestratorPayloadFile(prefix string, payload any) (string, error) {
	file, err := os.CreateTemp("", "clawcontrol-orchestrator-"+prefix+"-*.json")
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
		OrchestratorID:   optionalStringParam(params, "orchestratorId"),
		OrchestratorType: optionalStringParam(params, "orchestratorType"),
		Command:          optionalStringParam(params, "command"),
		WorkingDir:       optionalStringParam(params, "workingDir"),
		Task:             map[string]interface{}{},
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
	return result, nil
}
