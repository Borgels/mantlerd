package commands

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/mantlerd/internal/netutil"
	"github.com/Borgels/mantlerd/internal/types"
)

const (
	defaultCodexCommand       = "codex"
	defaultGooseCommand       = "goosed"
	defaultGooseDaemonBaseURL = "https://127.0.0.1:3000"
)

var gooseDaemonRegistry = struct {
	mu        sync.Mutex
	processes map[string]*gooseManagedDaemon
}{
	processes: map[string]*gooseManagedDaemon{},
}

type harnessExecMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type codexEvent struct {
	Type string `json:"type"`
	Item *struct {
		ID               string `json:"id"`
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		Server           string `json:"server"`
		Tool             string `json:"tool"`
		Status           string `json:"status"`
		ExitCode         *int   `json:"exit_code"`
		AggregatedOutput string `json:"aggregated_output"`
	} `json:"item"`
	ThreadID string `json:"thread_id"`
	Usage    *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type harnessExecParams struct {
	HarnessID           string
	HarnessType         string
	DirectTargetID      string
	TaskID              string
	CompatibilityPlanID string
	MantleFingerprint   string
	BaseFingerprint     string
	Model               string
	Messages            []harnessExecMessage
	PreferredRepo       string
	HarnessSessionID    string
	TransportKind       string
	TransportBaseURL    string
	TransportEndpoint   string
	TransportCommand    string
	TransportArgs       []string
	CredentialEnv       map[string]string
}

type codexExecutionState struct {
	commandID   string
	workingDir  string
	threadID    string
	lastMessage string
	lastDetail  string
	usage       *types.CommandStreamUsage
	toolActions []string
	mu          sync.Mutex
}

type gooseExecutionState struct {
	lastAssistantText string
	lastDetail        string
	usage             *types.CommandStreamUsage
	toolActions       []string
	mu                sync.Mutex
}

type gooseManagedDaemon struct {
	harnessID  string
	baseURL    string
	secret     string
	command    string
	workingDir string
	cmd        *exec.Cmd
	stderrMu   sync.Mutex
	stderr     []string
}

func (e *Executor) runHarnessExec(command types.AgentCommand) (ExecutionResult, error) {
	params, err := parseHarnessExecParams(command.Params)
	if err != nil {
		return ExecutionResult{}, err
	}

	switch normalizeHarnessTransportKind(params.TransportKind) {
	case "cli":
		switch params.HarnessType {
		case "codex_cli":
			return e.runCodexExec(command.ID, params)
		default:
			return ExecutionResult{}, fmt.Errorf("unsupported CLI harness type: %s", params.HarnessType)
		}
	case "daemon", "session_http":
		switch params.HarnessType {
		case "goose":
			return e.runGooseExec(command.ID, params)
		default:
			return ExecutionResult{}, fmt.Errorf("unsupported daemon/session harness type: %s", params.HarnessType)
		}
	default:
		return ExecutionResult{}, fmt.Errorf(
			"unsupported harness transport %q for harness type %s",
			params.TransportKind,
			params.HarnessType,
		)
	}
}

func parseHarnessExecParams(params map[string]interface{}) (harnessExecParams, error) {
	result := harnessExecParams{
		HarnessID:           optionalStringParam(params, "harnessId"),
		HarnessType:         optionalStringParam(params, "harnessType"),
		DirectTargetID:      optionalStringParam(params, "directTargetId"),
		TaskID:              optionalStringParam(params, "taskId"),
		CompatibilityPlanID: optionalStringParam(params, "compatibilityPlanId"),
		MantleFingerprint:   optionalStringParam(params, "mantleFingerprint"),
		BaseFingerprint:     optionalStringParam(params, "baseFingerprint"),
		Model:               optionalStringParam(params, "model"),
		PreferredRepo:       optionalStringParam(params, "preferredRepository"),
		HarnessSessionID:    optionalStringParam(params, "harnessSessionId"),
		TransportKind:       optionalStringParam(params, "transportKind"),
		TransportBaseURL:    optionalStringParam(params, "transportBaseUrl"),
		TransportEndpoint:   optionalStringParam(params, "transportEndpointPath"),
		TransportCommand:    optionalStringParam(params, "transportCommand"),
	}
	if result.HarnessID == "" {
		return result, fmt.Errorf("missing harnessId param")
	}
	if result.HarnessType == "" {
		return result, fmt.Errorf("missing harnessType param")
	}

	if rawArgs, ok := params["transportArgs"]; ok {
		switch value := rawArgs.(type) {
		case []interface{}:
			for _, entry := range value {
				text, ok := entry.(string)
				if !ok {
					continue
				}
				text = strings.TrimSpace(text)
				if text != "" {
					result.TransportArgs = append(result.TransportArgs, text)
				}
			}
		case []string:
			for _, entry := range value {
				entry = strings.TrimSpace(entry)
				if entry != "" {
					result.TransportArgs = append(result.TransportArgs, entry)
				}
			}
		}
	}

	if rawCredentialEnv, ok := params["credentialEnv"]; ok {
		result.CredentialEnv = map[string]string{}
		switch value := rawCredentialEnv.(type) {
		case map[string]interface{}:
			for key, raw := range value {
				name := strings.TrimSpace(key)
				if name == "" {
					continue
				}
				text, ok := raw.(string)
				if !ok {
					continue
				}
				text = strings.TrimSpace(text)
				if text == "" {
					continue
				}
				result.CredentialEnv[name] = text
			}
		case map[string]string:
			for key, raw := range value {
				name := strings.TrimSpace(key)
				text := strings.TrimSpace(raw)
				if name == "" || text == "" {
					continue
				}
				result.CredentialEnv[name] = text
			}
		}
	}

	rawMessages, ok := params["messages"].([]interface{})
	if !ok || len(rawMessages) == 0 {
		return result, fmt.Errorf("missing messages param")
	}
	for _, raw := range rawMessages {
		obj, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := obj["role"].(string)
		content, _ := obj["content"].(string)
		role = strings.TrimSpace(role)
		if role == "" {
			continue
		}
		result.Messages = append(result.Messages, harnessExecMessage{
			Role:    role,
			Content: content,
		})
	}
	if len(result.Messages) == 0 {
		return result, fmt.Errorf("messages param contained no usable chat messages")
	}

	return result, nil
}

func usageToOutcomeTokenUsage(usage *types.CommandStreamUsage) *types.OutcomeTokenUsage {
	if usage == nil {
		return nil
	}
	return &types.OutcomeTokenUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
	}
}

func normalizeHarnessTransportKind(kind string) string {
	trimmed := strings.TrimSpace(strings.ToLower(kind))
	if trimmed == "" {
		return "cli"
	}
	return trimmed
}

func (e *Executor) emitHarnessProgress(commandID string, detail string, event *types.CommandStreamEvent) {
	if e.progress == nil {
		return
	}
	payload := types.AckRequest{
		CommandID: commandID,
		Status:    "in_progress",
		Details:   detail,
	}
	if event != nil {
		payload.StreamEvents = []types.CommandStreamEvent{*event}
	}
	e.progress(payload)
}

func (e *Executor) runCodexExec(commandID string, params harnessExecParams) (ExecutionResult, error) {
	startedAt := time.Now()
	commandName := params.TransportCommand
	if commandName == "" {
		commandName = defaultCodexCommand
	}
	baseArgs := append([]string{}, params.TransportArgs...)
	if len(baseArgs) == 0 {
		baseArgs = []string{"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox"}
	}

	prompt := buildCodexPrompt(params.Messages)
	workingDir := resolveHarnessWorkingDir(params.PreferredRepo)
	args := buildCodexArgs(baseArgs, params.Model, workingDir, prompt)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	cmd := exec.Command(commandName, args...)
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	if len(params.CredentialEnv) > 0 {
		cmd.Env = withCredentialEnv(os.Environ(), params.CredentialEnv)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("open codex stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("open codex stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return ExecutionResult{}, fmt.Errorf("start codex exec: %w", err)
	}

	state := &codexExecutionState{
		commandID:  commandID,
		workingDir: workingDir,
	}
	secretValues := credentialSecretValues(params.CredentialEnv)
	var stderrLines []string
	var stderrMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		consumeCodexStdout(stdout, state, func(event types.CommandStreamEvent) {
			redactedEvent := redactStreamEvent(event, secretValues)
			e.emitHarnessProgress(commandID, redactSecrets(state.currentDetail(), secretValues), &redactedEvent)
		})
	}()

	go func() {
		defer wg.Done()
		stderrLines = readCommandStderr(stderr, &stderrMu, secretValues)
	}()

	waitErr := cmd.Wait()
	wg.Wait()

	if errors := strings.TrimSpace(strings.Join(stderrLines, "\n")); errors != "" && state.lastMessage == "" {
		state.lastDetail = redactSecrets(errors, secretValues)
	}

	result := ExecutionResult{
		Details: redactSecrets(state.finalDetail(), secretValues),
		ResultPayload: map[string]interface{}{
			"harnessId":      params.HarnessID,
			"harnessType":    params.HarnessType,
			"directTargetId": params.DirectTargetID,
			"threadId":       state.threadID,
			"workingDir":     workingDir,
		},
	}

	if state.usage != nil {
		payload := result.ResultPayload.(map[string]interface{})
		payload["usage"] = state.usage
	}
	if len(stderrLines) > 0 {
		payload := result.ResultPayload.(map[string]interface{})
		payload["stderr"] = stderrLines
	}

	if waitErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			if result.Details == "" {
				result.Details = "Codex execution timed out after 20 minutes."
			}
			e.emitOutcome(types.OutcomeEvent{
				TaskID:            params.TaskID,
				PlanID:            params.CompatibilityPlanID,
				MantleFingerprint: params.MantleFingerprint,
				BaseFingerprint:   params.BaseFingerprint,
				EventType:         "task_failure",
				DurationMs:        time.Since(startedAt).Milliseconds(),
				TokenUsage:        usageToOutcomeTokenUsage(state.usage),
				Detail:            result.Details,
				Timestamp:         time.Now().UTC().Format(time.RFC3339),
			})
			return result, fmt.Errorf("codex exec timed out")
		}
		if result.Details == "" {
			result.Details = fmt.Sprintf("codex exec failed: %v", waitErr)
		}
		e.emitOutcome(types.OutcomeEvent{
			TaskID:            params.TaskID,
			PlanID:            params.CompatibilityPlanID,
			MantleFingerprint: params.MantleFingerprint,
			BaseFingerprint:   params.BaseFingerprint,
			EventType:         "task_failure",
			DurationMs:        time.Since(startedAt).Milliseconds(),
			TokenUsage:        usageToOutcomeTokenUsage(state.usage),
			Detail:            result.Details,
			Timestamp:         time.Now().UTC().Format(time.RFC3339),
		})
		return result, fmt.Errorf("codex exec failed: %w", waitErr)
	}

	if result.Details == "" {
		result.Details = "Codex execution completed."
	}
	e.emitOutcome(types.OutcomeEvent{
		TaskID:            params.TaskID,
		PlanID:            params.CompatibilityPlanID,
		MantleFingerprint: params.MantleFingerprint,
		BaseFingerprint:   params.BaseFingerprint,
		EventType:         "task_success",
		DurationMs:        time.Since(startedAt).Milliseconds(),
		TokenUsage:        usageToOutcomeTokenUsage(state.usage),
		Detail:            result.Details,
		Timestamp:         time.Now().UTC().Format(time.RFC3339),
	})
	return result, nil
}

func (e *Executor) runGooseExec(commandID string, params harnessExecParams) (ExecutionResult, error) {
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	workingDir := resolveHarnessWorkingDir(params.PreferredRepo)
	daemon, err := ensureGooseDaemon(ctx, params, workingDir)
	if err != nil {
		return ExecutionResult{}, err
	}

	sessionID, reusedSession, err := ensureGooseSession(ctx, daemon, params.HarnessSessionID, workingDir)
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("prepare Goose session: %w", err)
	}

	state := &gooseExecutionState{}
	secretValues := credentialSecretValues(params.CredentialEnv)
	replyRequest := buildGooseReplyRequest(sessionID, params.Messages)
	finalUsage, err := e.streamGooseReply(ctx, commandID, daemon, replyRequest, state, secretValues)
	if err != nil {
		result := ExecutionResult{
			Details: redactSecrets(state.finalDetail("Goose execution failed."), secretValues),
			ResultPayload: map[string]interface{}{
				"harnessId":        params.HarnessID,
				"harnessType":      params.HarnessType,
				"directTargetId":   params.DirectTargetID,
				"sessionId":        sessionID,
				"workingDir":       workingDir,
				"sessionReused":    reusedSession,
				"daemonBaseUrl":    daemon.baseURL,
				"daemonManaged":    daemon.secret != "",
				"daemonStderrTail": daemon.stderrTail(),
			},
		}
		e.emitOutcome(types.OutcomeEvent{
			TaskID:            params.TaskID,
			PlanID:            params.CompatibilityPlanID,
			MantleFingerprint: params.MantleFingerprint,
			BaseFingerprint:   params.BaseFingerprint,
			EventType:         "task_failure",
			DurationMs:        time.Since(startedAt).Milliseconds(),
			TokenUsage:        usageToOutcomeTokenUsage(finalUsage),
			Detail:            result.Details,
			Timestamp:         time.Now().UTC().Format(time.RFC3339),
		})
		return result, err
	}

	resultPayload := map[string]interface{}{
		"harnessId":      params.HarnessID,
		"harnessType":    params.HarnessType,
		"directTargetId": params.DirectTargetID,
		"sessionId":      sessionID,
		"workingDir":     workingDir,
		"sessionReused":  reusedSession,
		"daemonBaseUrl":  daemon.baseURL,
		"daemonManaged":  daemon.secret != "",
	}
	if finalUsage != nil {
		resultPayload["usage"] = finalUsage
	}

	result := ExecutionResult{
		Details:       redactSecrets(state.finalDetail("Goose execution completed."), secretValues),
		ResultPayload: resultPayload,
	}
	e.emitOutcome(types.OutcomeEvent{
		TaskID:            params.TaskID,
		PlanID:            params.CompatibilityPlanID,
		MantleFingerprint: params.MantleFingerprint,
		BaseFingerprint:   params.BaseFingerprint,
		EventType:         "task_success",
		DurationMs:        time.Since(startedAt).Milliseconds(),
		TokenUsage:        usageToOutcomeTokenUsage(finalUsage),
		Detail:            result.Details,
		Timestamp:         time.Now().UTC().Format(time.RFC3339),
	})
	return result, nil
}

type gooseDaemonHandle struct {
	baseURL    string
	secret     string
	stderrTail func() []string
}

func ensureGooseDaemon(ctx context.Context, params harnessExecParams, workingDir string) (*gooseDaemonHandle, error) {
	baseURL := strings.TrimSpace(params.TransportBaseURL)
	if baseURL == "" {
		baseURL = defaultGooseDaemonBaseURL
	}

	if daemon := lookupManagedGooseDaemon(params.HarnessID); daemon != nil {
		if err := waitForGooseStatus(ctx, daemon.baseURL, daemon.secret); err == nil {
			return &gooseDaemonHandle{
				baseURL: daemon.baseURL,
				secret:  daemon.secret,
				stderrTail: func() []string {
					return daemon.stderrTail()
				},
			}, nil
		}
	}

	if err := waitForGooseStatus(ctx, baseURL, ""); err == nil {
		return &gooseDaemonHandle{
			baseURL: baseURL,
			stderrTail: func() []string {
				return nil
			},
		}, nil
	}

	commandName := strings.TrimSpace(params.TransportCommand)
	if commandName == "" {
		commandName = defaultGooseCommand
	}
	daemon, err := startManagedGooseDaemon(ctx, params.HarnessID, baseURL, commandName, params.TransportArgs, workingDir, params.CredentialEnv)
	if err != nil {
		return nil, err
	}

	return &gooseDaemonHandle{
		baseURL: daemon.baseURL,
		secret:  daemon.secret,
		stderrTail: func() []string {
			return daemon.stderrTail()
		},
	}, nil
}

func lookupManagedGooseDaemon(harnessID string) *gooseManagedDaemon {
	gooseDaemonRegistry.mu.Lock()
	defer gooseDaemonRegistry.mu.Unlock()
	daemon := gooseDaemonRegistry.processes[harnessID]
	if daemon == nil || daemon.cmd == nil || daemon.cmd.Process == nil || daemon.cmd.ProcessState != nil {
		return nil
	}
	return daemon
}

func startManagedGooseDaemon(
	ctx context.Context,
	harnessID string,
	baseURL string,
	commandName string,
	transportArgs []string,
	workingDir string,
	credentialEnv map[string]string,
) (*gooseManagedDaemon, error) {
	parsedURL, err := normalizeGooseBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Goose base URL: %w", err)
	}

	port := parsedURL.Port()
	if port == "" {
		port = "3000"
	}
	secret := randomHex(24)
	args := append([]string{}, transportArgs...)
	if len(args) == 0 {
		args = []string{"agent"}
	}

	cmd := exec.CommandContext(ctx, commandName, args...)
	cmd.Dir = workingDir
	cmd.Env = append(os.Environ(),
		"GOOSE_PORT="+port,
		"GOOSE_SERVER__SECRET_KEY="+secret,
	)
	if len(credentialEnv) > 0 {
		cmd.Env = withCredentialEnv(cmd.Env, credentialEnv)
	}
	if host := parsedURL.Hostname(); host != "" {
		cmd.Env = append(cmd.Env, "GOOSE_HOST="+host)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open Goose daemon stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open Goose daemon stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Goose daemon: %w", err)
	}

	daemon := &gooseManagedDaemon{
		harnessID:  harnessID,
		baseURL:    parsedURL.String(),
		secret:     secret,
		command:    commandName,
		workingDir: workingDir,
		cmd:        cmd,
	}

	go consumeGooseDaemonLogs(stdout, daemon, "stdout")
	go consumeGooseDaemonLogs(stderr, daemon, "stderr")
	go func() {
		_ = cmd.Wait()
		gooseDaemonRegistry.mu.Lock()
		if gooseDaemonRegistry.processes[harnessID] == daemon {
			delete(gooseDaemonRegistry.processes, harnessID)
		}
		gooseDaemonRegistry.mu.Unlock()
	}()

	gooseDaemonRegistry.mu.Lock()
	gooseDaemonRegistry.processes[harnessID] = daemon
	gooseDaemonRegistry.mu.Unlock()

	if err := waitForGooseStatus(ctx, daemon.baseURL, daemon.secret); err != nil {
		return nil, fmt.Errorf("wait for Goose daemon readiness: %w", err)
	}

	return daemon, nil
}

func consumeGooseDaemonLogs(r io.Reader, daemon *gooseManagedDaemon, stream string) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 32*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		daemon.stderrMu.Lock()
		daemon.stderr = append(daemon.stderr, fmt.Sprintf("%s: %s", stream, line))
		if len(daemon.stderr) > 40 {
			daemon.stderr = daemon.stderr[len(daemon.stderr)-40:]
		}
		daemon.stderrMu.Unlock()
	}
}

func (d *gooseManagedDaemon) stderrTail() []string {
	d.stderrMu.Lock()
	defer d.stderrMu.Unlock()
	return append([]string{}, d.stderr...)
}

func normalizeGooseBaseURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = defaultGooseDaemonBaseURL
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, err
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("missing host")
	}
	if parsed.Scheme == "" {
		parsed.Scheme = "https"
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

func waitForGooseStatus(ctx context.Context, baseURL string, secret string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	for {
		req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/status", nil)
		if err != nil {
			return err
		}
		if strings.TrimSpace(secret) != "" {
			req.Header.Set("X-Secret-Key", secret)
		}
		resp, err := gooseHTTPClient(req.URL.String()).Do(req)
		if err == nil && resp != nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if checkCtx.Err() != nil {
			if err != nil {
				return err
			}
			return fmt.Errorf("Goose status endpoint did not become ready")
		}
		select {
		case <-checkCtx.Done():
			if err != nil {
				return err
			}
			return fmt.Errorf("Goose status endpoint did not become ready")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func ensureGooseSession(
	ctx context.Context,
	daemon *gooseDaemonHandle,
	existingSessionID string,
	workingDir string,
) (string, bool, error) {
	trimmedSessionID := strings.TrimSpace(existingSessionID)
	if trimmedSessionID != "" {
		if err := gooseOpenSession(ctx, daemon, trimmedSessionID, workingDir); err == nil {
			return trimmedSessionID, true, nil
		}
	}
	sessionID, err := gooseCreateSession(ctx, daemon, workingDir)
	if err != nil {
		return "", false, err
	}
	return sessionID, false, nil
}

func gooseCreateSession(ctx context.Context, daemon *gooseDaemonHandle, workingDir string) (string, error) {
	payload := map[string]interface{}{
		"working_dir": workingDir,
	}
	var session map[string]interface{}
	if err := gooseJSONRequest(ctx, daemon, http.MethodPost, "/sessions", payload, &session); err == nil {
		if id := optionalStringFromMap(session, "id"); id != "" {
			return id, nil
		}
	}
	if err := gooseJSONRequest(ctx, daemon, http.MethodPost, "/agent/start", payload, &session); err != nil {
		return "", err
	}
	if id := optionalStringFromMap(session, "id"); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("Goose create session response did not include an id")
}

func gooseOpenSession(ctx context.Context, daemon *gooseDaemonHandle, sessionID string, workingDir string) error {
	payload := map[string]interface{}{
		"working_dir":               workingDir,
		"load_model_and_extensions": true,
	}
	if err := gooseJSONRequest(ctx, daemon, http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/open", payload, nil); err == nil {
		return nil
	}
	return gooseJSONRequest(ctx, daemon, http.MethodPost, "/agent/resume", map[string]interface{}{
		"session_id":                sessionID,
		"load_model_and_extensions": true,
	}, nil)
}

func (e *Executor) streamGooseReply(
	ctx context.Context,
	commandID string,
	daemon *gooseDaemonHandle,
	replyRequest map[string]interface{},
	state *gooseExecutionState,
	secrets []string,
) (*types.CommandStreamUsage, error) {
	resp, err := gooseStreamRequest(ctx, daemon, "/reply", replyRequest)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("Goose reply failed: %s", strings.TrimSpace(string(body)))
	}

	var finalUsage *types.CommandStreamUsage
	err = consumeGooseReplyStream(resp.Body, state, func(event types.CommandStreamEvent) {
		if event.Type == "usage" && event.Usage != nil {
			usageCopy := *event.Usage
			finalUsage = &usageCopy
		}
		redactedEvent := redactStreamEvent(event, secrets)
		e.emitHarnessProgress(commandID, redactSecrets(state.currentDetail(), secrets), &redactedEvent)
	})
	if err != nil {
		return finalUsage, err
	}
	return finalUsage, nil
}

func buildGooseReplyRequest(sessionID string, messages []harnessExecMessage) map[string]interface{} {
	conversation := make([]map[string]interface{}, 0, len(messages))
	for _, message := range messages {
		if converted := toGooseMessage(message, false); converted != nil {
			conversation = append(conversation, converted)
		}
	}
	if len(conversation) == 0 {
		return map[string]interface{}{
			"session_id": sessionID,
			"user_message": map[string]interface{}{
				"role":     "user",
				"content":  []map[string]interface{}{{"type": "text", "text": "(empty)"}},
				"created":  time.Now().UnixMilli(),
				"metadata": map[string]bool{"agentVisible": true, "userVisible": true},
			},
		}
	}

	userIndex := len(conversation) - 1
	for i := len(conversation) - 1; i >= 0; i-- {
		if conversation[i]["role"] == "user" {
			userIndex = i
			break
		}
	}

	userMessage := conversation[userIndex]
	overrideConversation := append([]map[string]interface{}{}, conversation[:userIndex]...)
	if userMessage["role"] != "user" {
		userMessage = toGooseMessage(messages[min(userIndex, len(messages)-1)], true)
	}

	request := map[string]interface{}{
		"session_id":   sessionID,
		"user_message": userMessage,
	}
	if len(overrideConversation) > 0 {
		request["override_conversation"] = overrideConversation
	}
	return request
}

func toGooseMessage(message harnessExecMessage, forceUser bool) map[string]interface{} {
	role := strings.TrimSpace(strings.ToLower(message.Role))
	content := strings.TrimSpace(message.Content)
	if content == "" {
		content = "(empty)"
	}
	switch role {
	case "assistant":
		if forceUser {
			content = "Conversation context from the assistant:\n" + content
			role = "user"
		}
	case "user":
		role = "user"
	case "system":
		content = "System instructions:\n" + content
		role = "user"
	default:
		content = strings.ToUpper(role) + ":\n" + content
		role = "user"
	}
	return map[string]interface{}{
		"role": role,
		"content": []map[string]interface{}{
			{
				"type": "text",
				"text": content,
			},
		},
		"created":  time.Now().UnixMilli(),
		"metadata": map[string]bool{"agentVisible": true, "userVisible": true},
	}
}

func consumeGooseReplyStream(stdout io.Reader, state *gooseExecutionState, emit func(types.CommandStreamEvent)) error {
	reader := bufio.NewReader(stdout)
	var dataLines []string

	flushData := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = nil
		if strings.TrimSpace(payload) == "" {
			return nil
		}
		return handleGooseSSEPayload(payload, state, emit)
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		} else if trimmed == "" {
			if flushErr := flushData(); flushErr != nil {
				return flushErr
			}
		}
		if err == io.EOF {
			return flushData()
		}
	}
}

func handleGooseSSEPayload(payload string, state *gooseExecutionState, emit func(types.CommandStreamEvent)) error {
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil
	}

	switch strings.TrimSpace(optionalStringFromMap(event, "type")) {
	case "Message":
		message, _ := event["message"].(map[string]interface{})
		if message == nil {
			return nil
		}
		if text := gooseAssistantText(message); text != "" {
			state.mu.Lock()
			delta := gooseContentDelta(state.lastAssistantText, text)
			state.lastAssistantText = text
			state.lastDetail = text
			state.mu.Unlock()
			if delta != "" {
				emit(newContentEvent(delta))
			}
		}
		if nextActions := gooseToolActions(message); len(nextActions) > 0 {
			state.mu.Lock()
			for _, action := range nextActions {
				state.toolActions = appendToolAction(state.toolActions, action)
			}
			actions := append([]string{}, state.toolActions...)
			state.mu.Unlock()
			emit(newToolActionsEvent(actions))
		}
		if usage := gooseUsageFromEvent(event); usage != nil {
			state.mu.Lock()
			state.usage = usage
			state.mu.Unlock()
		}
	case "Finish":
		if usage := gooseUsageFromEvent(event); usage != nil {
			state.mu.Lock()
			state.usage = usage
			state.mu.Unlock()
			emit(newUsageEvent(*usage))
		}
	case "Error":
		detail := strings.TrimSpace(optionalStringFromMap(event, "error"))
		if detail != "" {
			state.mu.Lock()
			state.lastDetail = detail
			state.mu.Unlock()
			emit(newErrorEvent(detail))
		}
	}
	return nil
}

func gooseAssistantText(message map[string]interface{}) string {
	role := strings.TrimSpace(optionalStringFromMap(message, "role"))
	if role != "assistant" {
		return ""
	}
	rawContent, _ := message["content"].([]interface{})
	if len(rawContent) == 0 {
		return ""
	}
	parts := make([]string, 0, len(rawContent))
	for _, raw := range rawContent {
		item, _ := raw.(map[string]interface{})
		if item == nil {
			continue
		}
		switch strings.TrimSpace(optionalStringFromMap(item, "type")) {
		case "text":
			if text := strings.TrimSpace(optionalStringFromMap(item, "text")); text != "" {
				parts = append(parts, text)
			}
		case "reasoning":
			if text := strings.TrimSpace(optionalStringFromMap(item, "reasoning")); text != "" {
				parts = append(parts, text)
			}
		case "thinking":
			if text := strings.TrimSpace(optionalStringFromMap(item, "thinking")); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func gooseToolActions(message map[string]interface{}) []string {
	rawContent, _ := message["content"].([]interface{})
	if len(rawContent) == 0 {
		return nil
	}
	actions := make([]string, 0, len(rawContent))
	for _, raw := range rawContent {
		item, _ := raw.(map[string]interface{})
		if item == nil {
			continue
		}
		switch strings.TrimSpace(optionalStringFromMap(item, "type")) {
		case "toolRequest":
			toolCall, _ := item["toolCall"].(map[string]interface{})
			name := strings.TrimSpace(optionalStringFromMap(toolCall, "name"))
			if name == "" {
				name = strings.TrimSpace(optionalStringFromMap(item, "id"))
			}
			if name != "" {
				actions = append(actions, "tool: "+name)
			}
		case "toolResponse":
			toolResult, _ := item["toolResult"].(map[string]interface{})
			name := strings.TrimSpace(optionalStringFromMap(toolResult, "name"))
			if name == "" {
				name = strings.TrimSpace(optionalStringFromMap(item, "id"))
			}
			if name != "" {
				actions = append(actions, "tool_result: "+name)
			}
		case "actionRequired":
			actions = append(actions, "action_required")
		}
	}
	return actions
}

func gooseUsageFromEvent(event map[string]interface{}) *types.CommandStreamUsage {
	tokenState, _ := event["token_state"].(map[string]interface{})
	if tokenState == nil {
		return nil
	}
	total := int(numberFromMap(tokenState, "accumulatedTotalTokens"))
	if total == 0 {
		total = int(numberFromMap(tokenState, "totalTokens"))
	}
	prompt := int(numberFromMap(tokenState, "accumulatedInputTokens"))
	if prompt == 0 {
		prompt = int(numberFromMap(tokenState, "inputTokens"))
	}
	completion := int(numberFromMap(tokenState, "accumulatedOutputTokens"))
	if completion == 0 {
		completion = int(numberFromMap(tokenState, "outputTokens"))
	}
	if total == 0 && prompt == 0 && completion == 0 {
		return nil
	}
	return &types.CommandStreamUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
	}
}

func gooseContentDelta(previous string, next string) string {
	previous = strings.TrimSpace(previous)
	next = strings.TrimSpace(next)
	if next == "" {
		return ""
	}
	if previous == "" {
		return next
	}
	if strings.HasPrefix(next, previous) {
		return strings.TrimPrefix(next, previous)
	}
	if next == previous {
		return ""
	}
	return next
}

func gooseJSONRequest(
	ctx context.Context,
	daemon *gooseDaemonHandle,
	method string,
	path string,
	payload interface{},
	dest interface{},
) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(encoded))
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(daemon.baseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(daemon.secret) != "" {
		req.Header.Set("X-Secret-Key", daemon.secret)
	}

	resp, err := gooseHTTPClient(req.URL.String()).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("Goose request failed (%s %s): %s", method, path, strings.TrimSpace(string(raw)))
	}
	if dest == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(dest)
}

func gooseStreamRequest(
	ctx context.Context,
	daemon *gooseDaemonHandle,
	path string,
	payload interface{},
) (*http.Response, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(daemon.baseURL, "/")+path, strings.NewReader(string(encoded)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if strings.TrimSpace(daemon.secret) != "" {
		req.Header.Set("X-Secret-Key", daemon.secret)
	}
	return gooseHTTPClient(req.URL.String()).Do(req)
}

func gooseHTTPClient(targetURL string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: netutil.ShouldSkipTLSVerifyForURL(targetURL)},
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2: true,
		},
	}
}

func (s *codexExecutionState) currentDetail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentDetailLocked()
}

func (s *codexExecutionState) currentDetailLocked() string {
	if s.lastMessage != "" {
		return s.lastMessage
	}
	return s.lastDetail
}

func (s *codexExecutionState) finalDetail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastMessage != "" {
		return s.lastMessage
	}
	return s.lastDetail
}

func (s *gooseExecutionState) currentDetail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastAssistantText != "" {
		return s.lastAssistantText
	}
	return s.lastDetail
}

func (s *gooseExecutionState) finalDetail(fallback string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastAssistantText != "" {
		return s.lastAssistantText
	}
	if s.lastDetail != "" {
		return s.lastDetail
	}
	return fallback
}

func consumeCodexStdout(stdout io.Reader, state *codexExecutionState, emit func(types.CommandStreamEvent)) {
	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event codexEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			state.mu.Lock()
			state.lastDetail = line
			state.mu.Unlock()
			continue
		}

		switch event.Type {
		case "thread.started":
			state.mu.Lock()
			state.threadID = event.ThreadID
			state.mu.Unlock()
		case "item.started":
			if event.Item == nil {
				continue
			}
			switch event.Item.Type {
			case "command_execution":
				state.mu.Lock()
				state.toolActions = appendToolAction(state.toolActions, fmt.Sprintf("command: %s", strings.TrimSpace(event.Item.Command)))
				actions := append([]string{}, state.toolActions...)
				state.lastDetail = strings.TrimSpace(event.Item.Command)
				state.mu.Unlock()
				emit(newToolActionsEvent(actions))
			case "mcp_tool_call":
				label := strings.TrimSpace(event.Item.Server + "/" + event.Item.Tool)
				if label == "/" {
					label = strings.TrimSpace(event.Item.Tool)
				}
				state.mu.Lock()
				state.toolActions = appendToolAction(state.toolActions, fmt.Sprintf("tool: %s", label))
				actions := append([]string{}, state.toolActions...)
				state.lastDetail = label
				state.mu.Unlock()
				emit(newToolActionsEvent(actions))
			}
		case "item.completed":
			if event.Item == nil {
				continue
			}
			switch event.Item.Type {
			case "agent_message":
				text := strings.TrimSpace(event.Item.Text)
				if text == "" {
					continue
				}
				state.mu.Lock()
				state.lastMessage = text
				state.mu.Unlock()
				emit(newContentEvent(text))
			case "command_execution":
				detail := strings.TrimSpace(event.Item.Command)
				if event.Item.ExitCode != nil {
					detail = fmt.Sprintf("%s (exit %d)", detail, *event.Item.ExitCode)
				}
				if detail == "" {
					continue
				}
				state.mu.Lock()
				state.lastDetail = detail
				state.mu.Unlock()
			case "mcp_tool_call":
				detail := strings.TrimSpace(event.Item.Server + "/" + event.Item.Tool)
				if detail == "/" {
					detail = strings.TrimSpace(event.Item.Tool)
				}
				if detail == "" {
					continue
				}
				state.mu.Lock()
				state.lastDetail = detail
				state.mu.Unlock()
			}
		case "turn.completed":
			if event.Usage == nil {
				continue
			}
			usage := &types.CommandStreamUsage{
				PromptTokens:     event.Usage.InputTokens,
				CompletionTokens: event.Usage.OutputTokens,
				TotalTokens:      event.Usage.InputTokens + event.Usage.OutputTokens,
			}
			state.mu.Lock()
			state.usage = usage
			state.mu.Unlock()
			emit(newUsageEvent(*usage))
		}
	}
}

func readCommandStderr(stderr io.Reader, mu *sync.Mutex, secrets []string) []string {
	lines := make([]string, 0, 8)
	scanner := bufio.NewScanner(stderr)
	buf := make([]byte, 0, 32*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		mu.Lock()
		lines = append(lines, redactSecrets(line, secrets))
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string{}, lines...)
}

func buildCodexArgs(baseArgs []string, model string, workingDir string, prompt string) []string {
	args := append([]string{}, baseArgs...)
	if !containsArg(args, "--json") {
		args = append(args, "--json")
	}
	if !containsArg(args, "--skip-git-repo-check") {
		args = append(args, "--skip-git-repo-check")
	}
	if !hasCodexAutomationFlag(args) {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	if workingDir != "" && !containsArg(args, "--cd") && !containsArg(args, "-C") {
		args = append(args, "--cd", workingDir)
	}
	if shouldPassCodexModel(model) && !containsArg(args, "--model") && !containsArg(args, "-m") {
		args = append(args, "--model", strings.TrimSpace(model))
	}
	args = append(args, prompt)
	return args
}

func shouldPassCodexModel(model string) bool {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return false
	}
	return !strings.EqualFold(trimmed, "codex")
}

func containsArg(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func buildCodexPrompt(messages []harnessExecMessage) string {
	var builder strings.Builder
	builder.WriteString("You are continuing a Mantler direct chat session on this machine.\n")
	builder.WriteString("Use the conversation history below as context and provide the next assistant response.\n")
	builder.WriteString("When interacting with the local workspace, prefer the repository requested by the user if one is implied.\n\n")
	builder.WriteString("Conversation history:\n")
	for _, message := range messages {
		role := strings.ToUpper(strings.TrimSpace(message.Role))
		content := strings.TrimSpace(message.Content)
		if role == "" {
			continue
		}
		builder.WriteString("\n[")
		builder.WriteString(role)
		builder.WriteString("]\n")
		if content == "" {
			builder.WriteString("(empty)\n")
		} else {
			builder.WriteString(content)
			builder.WriteString("\n")
		}
	}
	return strings.TrimSpace(builder.String())
}

func resolveHarnessWorkingDir(preferredRepository string) string {
	trimmed := strings.TrimSpace(preferredRepository)
	if trimmed != "" {
		if filepath.IsAbs(trimmed) {
			if stat, err := os.Stat(trimmed); err == nil && stat.IsDir() {
				return trimmed
			}
		}
		for _, candidateName := range repositoryLookupCandidates(trimmed) {
			for _, root := range candidateRepoRoots() {
				candidate := filepath.Join(root, candidateName)
				if stat, err := os.Stat(candidate); err == nil && stat.IsDir() {
					return candidate
				}
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

func candidateRepoRoots() []string {
	roots := make([]string, 0, 8)
	if cwd, err := os.Getwd(); err == nil {
		roots = append(roots, cwd, filepath.Join(cwd, "repos"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, home, filepath.Join(home, "repos"), filepath.Join(home, "src"), filepath.Join(home, "workspace"))
	}
	roots = append(roots, "/repos", "/workspaces", "/workspace")

	seen := map[string]struct{}{}
	deduped := make([]string, 0, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		deduped = append(deduped, root)
	}
	return deduped
}

func repositoryLookupCandidates(value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	candidates := []string{trimmed}
	base := filepath.Base(trimmed)
	if base != "." && base != string(filepath.Separator) && base != trimmed {
		candidates = append(candidates, base)
	}
	seen := map[string]struct{}{}
	deduped := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		deduped = append(deduped, candidate)
	}
	return deduped
}

func hasCodexAutomationFlag(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--dangerously-bypass-approvals-and-sandbox", "--full-auto", "--sandbox", "-s":
			return true
		}
	}
	return false
}

func appendToolAction(actions []string, next string) []string {
	next = strings.TrimSpace(next)
	if next == "" {
		return actions
	}
	if len(actions) > 0 && actions[len(actions)-1] == next {
		return actions
	}
	return append(actions, next)
}

func optionalStringFromMap(value map[string]interface{}, key string) string {
	raw, _ := value[key].(string)
	return strings.TrimSpace(raw)
}

func numberFromMap(value map[string]interface{}, key string) float64 {
	switch v := value[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	default:
		return 0
	}
}

func randomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func withCredentialEnv(base []string, credentialEnv map[string]string) []string {
	next := append([]string{}, base...)
	for key, value := range credentialEnv {
		k := strings.TrimSpace(key)
		v := strings.TrimSpace(value)
		if k == "" || v == "" {
			continue
		}
		next = append(next, fmt.Sprintf("%s=%s", k, v))
	}
	return next
}

func credentialSecretValues(credentialEnv map[string]string) []string {
	if len(credentialEnv) == 0 {
		return nil
	}
	values := make([]string, 0, len(credentialEnv))
	for _, value := range credentialEnv {
		trimmed := strings.TrimSpace(value)
		if len(trimmed) < 4 {
			continue
		}
		values = append(values, trimmed)
	}
	return values
}

func redactSecrets(input string, secrets []string) string {
	output := input
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		output = strings.ReplaceAll(output, secret, "[REDACTED]")
	}
	return output
}

func redactStreamEvent(event types.CommandStreamEvent, secrets []string) types.CommandStreamEvent {
	next := event
	if next.Content != "" {
		next.Content = redactSecrets(next.Content, secrets)
	}
	if next.Detail != "" {
		next.Detail = redactSecrets(next.Detail, secrets)
	}
	if len(next.Actions) > 0 {
		actions := make([]string, 0, len(next.Actions))
		for _, action := range next.Actions {
			actions = append(actions, redactSecrets(action, secrets))
		}
		next.Actions = actions
	}
	return next
}

func newContentEvent(content string) types.CommandStreamEvent {
	return types.CommandStreamEvent{
		ID:        fmt.Sprintf("evt-%d", time.Now().UnixNano()),
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Type:      "content",
		Content:   content,
	}
}

func newToolActionsEvent(actions []string) types.CommandStreamEvent {
	return types.CommandStreamEvent{
		ID:        fmt.Sprintf("evt-%d", time.Now().UnixNano()),
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Type:      "tool_actions",
		Actions:   actions,
	}
}

func newUsageEvent(usage types.CommandStreamUsage) types.CommandStreamEvent {
	return types.CommandStreamEvent{
		ID:        fmt.Sprintf("evt-%d", time.Now().UnixNano()),
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Type:      "usage",
		Usage:     &usage,
	}
}

func newErrorEvent(detail string) types.CommandStreamEvent {
	return types.CommandStreamEvent{
		ID:        fmt.Sprintf("evt-%d", time.Now().UnixNano()),
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Type:      "error",
		Detail:    detail,
	}
}
