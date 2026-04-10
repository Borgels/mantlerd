package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

type harnessLoginParams struct {
	HarnessType      string
	Method           string
	TransportCommand string
}

var (
	deviceAuthURLPattern  = regexp.MustCompile(`https?://\S+`)
	deviceAuthCodePattern = regexp.MustCompile(`\b[A-Z0-9]{3,8}(?:-[A-Z0-9]{3,8})\b`)
	ansiEscapePattern     = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

func parseHarnessLoginParams(params map[string]interface{}) (harnessLoginParams, error) {
	harnessType, err := stringParam(params, "harnessType")
	if err != nil {
		return harnessLoginParams{}, err
	}
	method := optionalStringParam(params, "method")
	if method == "" {
		method = "device_auth"
	}
	commandName := optionalStringParam(params, "transportCommand")
	if commandName == "" {
		commandName = defaultCodexCommand
	}
	return harnessLoginParams{
		HarnessType:      strings.TrimSpace(harnessType),
		Method:           strings.TrimSpace(method),
		TransportCommand: strings.TrimSpace(commandName),
	}, nil
}

func normalizeDeviceAuthURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimRight(trimmed, ".,);")
	return trimmed
}

func stripAnsi(value string) string {
	return ansiEscapePattern.ReplaceAllString(value, "")
}

func extractDeviceAuthURL(line string) string {
	match := deviceAuthURLPattern.FindString(stripAnsi(line))
	if match == "" {
		return ""
	}
	return normalizeDeviceAuthURL(match)
}

func extractDeviceAuthCode(line string) string {
	trimmed := strings.TrimSpace(stripAnsi(line))
	if trimmed == "" {
		return ""
	}
	upper := strings.ToUpper(trimmed)
	containsCodeHint := strings.Contains(strings.ToLower(trimmed), "code")
	match := deviceAuthCodePattern.FindString(upper)
	if match == "" {
		return ""
	}
	if !containsCodeHint && upper != match {
		return ""
	}
	return match
}

func scanHarnessLoginOutput(reader io.Reader, lines chan<- string) {
	scanner := bufio.NewScanner(reader)
	// Increase token size to avoid dropping long CLI lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(stripAnsi(scanner.Text()))
		if line != "" {
			lines <- line
		}
	}
}

func (e *Executor) emitHarnessLoginProgress(
	commandID string,
	deviceCodeURL string,
	userCode string,
	details string,
) {
	if e.progress == nil {
		return
	}
	payload := map[string]string{}
	if deviceCodeURL != "" {
		payload["deviceCodeUrl"] = deviceCodeURL
	}
	if userCode != "" {
		payload["userCode"] = userCode
	}
	if details != "" {
		payload["details"] = details
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		if details == "" {
			details = "Waiting for ChatGPT login in browser."
		}
		e.progress(types.AckRequest{
			CommandID: commandID,
			Status:    "in_progress",
			Details:   details,
		})
		return
	}
	e.progress(types.AckRequest{
		CommandID: commandID,
		Status:    "in_progress",
		Details:   string(raw),
	})
}

func (e *Executor) runHarnessLogin(command types.AgentCommand) (ExecutionResult, error) {
	params, err := parseHarnessLoginParams(command.Params)
	if err != nil {
		return ExecutionResult{}, err
	}
	if params.HarnessType != "codex_cli" {
		return ExecutionResult{}, fmt.Errorf("unsupported harness_login harness type: %s", params.HarnessType)
	}
	if params.Method != "device_auth" {
		return ExecutionResult{}, fmt.Errorf("unsupported harness_login method: %s", params.Method)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	loginCommand := fmt.Sprintf("%s login --device-auth", shellQuote(params.TransportCommand))
	cmd := exec.CommandContext(ctx, "script", "-q", "-c", loginCommand, "/dev/null")
	if _, lookErr := exec.LookPath("script"); lookErr != nil {
		// Fallback to direct execution when `script` is unavailable.
		cmd = exec.CommandContext(ctx, params.TransportCommand, "login", "--device-auth")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("open codex login stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("open codex login stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return ExecutionResult{}, fmt.Errorf("start codex login: %w", err)
	}

	lines := make(chan string, 64)
	var scanWG sync.WaitGroup
	scanWG.Add(2)
	go func() {
		defer scanWG.Done()
		scanHarnessLoginOutput(stdout, lines)
	}()
	go func() {
		defer scanWG.Done()
		scanHarnessLoginOutput(stderr, lines)
	}()
	go func() {
		scanWG.Wait()
		close(lines)
	}()

	waitErrCh := make(chan error, 1)
	go func() {
		waitErrCh <- cmd.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
	}()

	var (
		deviceCodeURL string
		userCode      string
		lastLine      string
		progressSent  bool
	)
	e.emitHarnessLoginProgress(command.ID, "", "", "Waiting for device code...")
	for line := range lines {
		lastLine = line
		if deviceCodeURL == "" {
			deviceCodeURL = extractDeviceAuthURL(line)
		}
		if userCode == "" {
			userCode = extractDeviceAuthCode(line)
		}
		if !progressSent && (deviceCodeURL != "" || userCode != "") {
			e.emitHarnessLoginProgress(command.ID, deviceCodeURL, userCode, "Complete login in your browser.")
			progressSent = true
		}
	}

	waitErr := <-waitErrCh
	if waitErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return ExecutionResult{}, fmt.Errorf("codex login timed out after 10 minutes")
		}
		if !progressSent {
			e.emitHarnessLoginProgress(command.ID, deviceCodeURL, userCode, lastLine)
		}
		if lastLine != "" {
			return ExecutionResult{}, fmt.Errorf("codex login failed: %s", lastLine)
		}
		return ExecutionResult{}, fmt.Errorf("codex login failed: %w", waitErr)
	}

	return ExecutionResult{
		Details: "ChatGPT login successful",
	}, nil
}
