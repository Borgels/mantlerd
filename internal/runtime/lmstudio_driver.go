package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/clawcontrol-agent/internal/types"
)

const (
	lmsInstallScriptURL   = "https://lmstudio.ai/install.sh"
	lmsServiceUnitPath    = "/etc/systemd/system/lmstudio.service"
	lmsConfigPath         = "/etc/clawcontrol/lmstudio.json"
	lmsDefaultPort        = 1234
	lmsReadyTimeout       = 90 * time.Second
	lmsDefaultSystemdPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

type lmstudioConfig struct {
	Model string `json:"model"`
	Port  int    `json:"port"`
}

type lmstudioCommandContext struct {
	path string
	home string
}

type lmstudioDriver struct{}

func newLMStudioDriver() Driver {
	return &lmstudioDriver{}
}

func (d *lmstudioDriver) Name() string { return "lmstudio" }

func (d *lmstudioDriver) Install() error {
	if !d.IsInstalled() {
		if err := runCommand("sh", "-c", "curl -fsSL "+lmsInstallScriptURL+" | bash"); err != nil {
			return fmt.Errorf("install lmstudio cli: %w", err)
		}
	}
	if err := d.ensureServiceUnit(); err != nil {
		return err
	}
	if err := d.startOrRestartService(); err != nil {
		return err
	}
	cfg, err := d.readConfig()
	if err == nil && strings.TrimSpace(cfg.Model) != "" {
		resolvedModelID, err := d.loadModel(cfg.Model)
		if err != nil {
			return fmt.Errorf("load configured lmstudio model %q: %w", cfg.Model, err)
		}
		if resolvedModelID != "" && resolvedModelID != cfg.Model {
			cfg.Model = resolvedModelID
			_ = d.writeConfig(cfg)
		}
	}
	return nil
}

func (d *lmstudioDriver) Uninstall() error {
	_ = runCommand("systemctl", "stop", "lmstudio")
	_ = runCommand("systemctl", "disable", "lmstudio")
	_ = os.Remove(lmsServiceUnitPath)
	_ = runCommand("systemctl", "daemon-reload")
	path := d.resolveLMSPath()
	if path != "" {
		cmd := exec.Command(path, "daemon", "down")
		cmd.Env = d.envForPath(path)
		_ = cmd.Run()
	}
	_ = os.Remove(lmsConfigPath)
	return nil
}

func (d *lmstudioDriver) IsInstalled() bool {
	cliPath := strings.TrimSpace(d.resolveLMSPath())
	if cliPath != "" {
		return true
	}
	// Under hardened systemd settings (ProtectHome=true), the daemon may not
	// be able to stat /home/*/.lmstudio/bin/lms even when LM Studio is
	// installed and managed via systemd. Treat managed artifacts as installed.
	if _, err := os.Stat(lmsServiceUnitPath); err == nil {
		return true
	}
	if _, err := os.Stat(lmsConfigPath); err == nil {
		return true
	}
	return false
}

func (d *lmstudioDriver) IsReady() bool {
	// LM Studio's CLI often daemonizes/returns immediately, which can make the
	// systemd unit report transient "deactivating" states even while the API is
	// healthy. Treat API health as the source of truth for readiness.
	_, err := d.fetchRemoteModels()
	return err == nil
}

func (d *lmstudioDriver) Version() string {
	path := d.resolveLMSPath()
	if path == "" {
		return ""
	}
	cmd := exec.Command(path, "daemon", "status", "--json", "--quiet")
	cmd.Env = d.envForPath(path)
	output, err := cmd.Output()
	if err == nil {
		var payload struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(output, &payload) == nil && strings.TrimSpace(payload.Version) != "" {
			return strings.TrimSpace(payload.Version)
		}
	}
	cmd = exec.Command(path, "--version")
	cmd.Env = d.envForPath(path)
	output, err = cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (d *lmstudioDriver) daemonRunning(path string) (bool, bool) {
	cmd := exec.Command(path, "daemon", "status", "--json", "--quiet")
	cmd.Env = d.envForPath(path)
	output, err := cmd.Output()
	if err != nil {
		return false, false
	}
	var payload struct {
		Running *bool  `json:"running"`
		Status  string `json:"status"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal(output, &payload); err != nil {
		return false, false
	}
	if payload.Running != nil {
		return *payload.Running, true
	}
	state := strings.ToLower(strings.TrimSpace(payload.State))
	if state == "" {
		state = strings.ToLower(strings.TrimSpace(payload.Status))
	}
	if state == "" {
		return false, false
	}
	switch state {
	case "running", "active", "up":
		return true, true
	case "stopped", "inactive", "down":
		return false, true
	default:
		return false, false
	}
}

func (d *lmstudioDriver) EnsureModelWithFlags(modelID string, _ *types.ModelFeatureFlags) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	if err := d.Install(); err != nil {
		return err
	}
	normalizedModelID := normalizeLMStudioModelID(modelID)
	resolvedModelID := normalizedModelID
	if !d.HasModel(normalizedModelID) {
		loadedModelID, err := d.loadModel(modelID)
		if err != nil {
			return err
		}
		resolvedModelID = normalizeLMStudioModelID(loadedModelID)
	}
	cfg, _ := d.readConfig()
	cfg.Model = resolvedModelID
	if cfg.Port <= 0 {
		cfg.Port = lmsDefaultPort
	}
	if err := d.writeConfig(cfg); err != nil {
		return err
	}
	return nil
}

func (d *lmstudioDriver) ListModels() []string {
	models, err := d.fetchRemoteModels()
	if err != nil {
		return nil
	}
	return collapseLMStudioModelIDs(models)
}

func (d *lmstudioDriver) InstalledModels() []types.InstalledModel {
	result := make([]types.InstalledModel, 0)
	seen := map[string]struct{}{}
	cfg, _ := d.readConfig()
	configured := normalizeLMStudioModelID(strings.TrimSpace(cfg.Model))

	models, err := d.fetchRemoteModels()
	if err == nil {
		for _, model := range collapseLMStudioModelIDs(models) {
			if model == "" {
				continue
			}
			seen[strings.ToLower(model)] = struct{}{}
			result = append(result, types.InstalledModel{
				ModelID: model,
				Runtime: types.RuntimeLMStudio,
				Status:  types.ModelReady,
			})
		}
		if configured != "" {
			if _, ok := seen[strings.ToLower(configured)]; !ok {
				result = append(result, types.InstalledModel{
					ModelID: configured,
					Runtime: types.RuntimeLMStudio,
					Status:  types.ModelInstalling,
				})
			}
		}
		return result
	}

	if configured != "" {
		result = append(result, types.InstalledModel{
			ModelID: configured,
			Runtime: types.RuntimeLMStudio,
			Status:  types.ModelFailed,
		})
	}
	return result
}

func (d *lmstudioDriver) HasModel(modelID string) bool {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return false
	}
	for _, model := range d.ListModels() {
		if lmstudioModelIDsEquivalent(model, modelID) {
			return true
		}
	}
	return false
}

func (d *lmstudioDriver) RemoveModel(modelID string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	contexts := d.resolveLMSContexts()
	if len(contexts) == 0 {
		return fmt.Errorf("lmstudio cli not installed")
	}

	targets := []string{modelID}
	normalizedModelID := normalizeLMStudioModelID(modelID)
	if normalizedModelID != "" && !lmstudioModelIDsEquivalent(normalizedModelID, modelID) {
		targets = append(targets, normalizedModelID)
	}
	if remoteModels, err := d.fetchRemoteModels(); err == nil {
		for _, remoteModel := range remoteModels {
			if lmstudioModelIDsEquivalent(remoteModel, modelID) {
				targets = append(targets, strings.TrimSpace(remoteModel))
			}
		}
	}
	targets = uniqueStrings(targets)

	var hadSuccess bool
	var lastErr error
	for _, target := range targets {
		if err := d.unloadModelAcrossContexts(target, contexts); err != nil {
			lastErr = err
			continue
		}
		hadSuccess = true
	}
	if hadSuccess {
		d.clearConfiguredModel(modelID)
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("unload lmstudio model %q failed", modelID)
}

func (d *lmstudioDriver) BenchmarkModel(
	modelID string,
	samplePromptTokens int,
	sampleOutputTokens int,
	concurrency int,
	runs int,
	onProgress func(BenchmarkProgress),
) (BenchmarkResult, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return BenchmarkResult{}, fmt.Errorf("model ID is required")
	}
	if err := d.EnsureModelWithFlags(modelID, nil); err != nil {
		return BenchmarkResult{}, err
	}
	if samplePromptTokens <= 0 {
		samplePromptTokens = 640
	}
	if sampleOutputTokens <= 0 {
		sampleOutputTokens = 256
	}
	if concurrency <= 0 {
		concurrency = 2
	}
	if concurrency > 4 {
		concurrency = 4
	}
	if runs <= 0 {
		runs = concurrency * 4
	}
	if runs < 4 {
		runs = 4
	}
	if runs > 16 {
		runs = 16
	}

	prompt := makeBenchmarkPrompt(samplePromptTokens)
	results := make([]BenchmarkResult, 0, runs)
	errs := make([]error, 0)
	var mu sync.Mutex
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrency)

	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			one, err := d.benchmarkOnce(modelID, prompt, sampleOutputTokens)
			var progress *BenchmarkProgress
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			} else {
				results = append(results, one)
			}
			next := BenchmarkProgress{
				RunsCompleted:  len(results) + len(errs),
				RunsTotal:      runs,
				SuccessfulRuns: len(results),
				FailedRuns:     len(errs),
			}
			if err == nil {
				next.LastRunLatencyMs = one.TotalLatencyMs
			}
			if len(results) > 0 {
				partial := summarizeBenchmarkResults(results)
				next.Benchmark = &partial
			}
			progress = &next
			mu.Unlock()
			if onProgress != nil && progress != nil {
				onProgress(*progress)
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return BenchmarkResult{}, errs[0]
	}
	return summarizeBenchmarkResults(results), nil
}

func (d *lmstudioDriver) RestartRuntime() error {
	if err := d.Install(); err != nil {
		return err
	}
	return d.startOrRestartService()
}

func (d *lmstudioDriver) benchmarkOnce(modelID string, prompt string, sampleOutputTokens int) (BenchmarkResult, error) {
	reqBody := map[string]any{
		"model": modelID,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens": sampleOutputTokens,
		"stream":     false,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("encode lmstudio benchmark request: %w", err)
	}

	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, d.baseURL()+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("create lmstudio benchmark request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("lmstudio benchmark request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return BenchmarkResult{}, fmt.Errorf("lmstudio benchmark failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return BenchmarkResult{}, fmt.Errorf("decode lmstudio benchmark response: %w", err)
	}

	latencyMs := float64(time.Since(start).Milliseconds())
	seconds := latencyMs / 1000.0
	if seconds <= 0 {
		seconds = 0.001
	}
	return BenchmarkResult{
		TTFTMs:                      roundTo(latencyMs, 2),
		OutputTokensPerSec:          roundTo(float64(parsed.Usage.CompletionTokens)/seconds, 2),
		TotalLatencyMs:              roundTo(latencyMs, 2),
		PromptTokensPerSec:          roundTo(float64(parsed.Usage.PromptTokens)/seconds, 2),
		P95TTFTMsAtSmallConcurrency: roundTo(latencyMs, 2),
	}, nil
}

func (d *lmstudioDriver) loadModel(modelID string) (string, error) {
	path := d.resolveLMSPath()
	if path == "" {
		return "", fmt.Errorf("lmstudio cli not installed")
	}
	candidates := []string{modelID}
	if normalized := normalizeLMStudioModelID(modelID); normalized != modelID {
		candidates = []string{normalized, modelID}
	}
	var lastErr error
	for _, candidate := range candidates {
		loadCmd := exec.Command(path, "load", candidate)
		loadCmd.Env = d.envForPath(path)
		output, err := loadCmd.CombinedOutput()
		if err == nil {
			return candidate, nil
		}
		outText := strings.TrimSpace(string(output))
		if shouldAvoidInteractiveLMStudioGet(outText) {
			lastErr = fmt.Errorf("load lmstudio model %q failed: %w (%s)", candidate, err, outText)
			continue
		}

		getCmd := exec.Command(path, "get", candidate)
		getCmd.Env = d.envForPath(path)
		if getOut, getErr := getCmd.CombinedOutput(); getErr != nil {
			lastErr = fmt.Errorf(
				"load lmstudio model %q failed: %w (%s); download attempt failed: %w (%s)",
				candidate,
				err,
				outText,
				getErr,
				strings.TrimSpace(string(getOut)),
			)
			continue
		}
		retryCmd := exec.Command(path, "load", candidate)
		retryCmd.Env = d.envForPath(path)
		if retryOut, retryErr := retryCmd.CombinedOutput(); retryErr != nil {
			lastErr = fmt.Errorf("load lmstudio model %q after download: %w (%s)", candidate, retryErr, strings.TrimSpace(string(retryOut)))
			continue
		}
		return candidate, nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("failed to load lmstudio model %q", modelID)
}

var lmstudioNumericSuffixPattern = regexp.MustCompile(`^(.*):[0-9]+$`)
var lmstudioBinaryPathPattern = regexp.MustCompile(`(/[^'"\s;]+/lms)\b`)

func normalizeLMStudioModelID(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	matches := lmstudioNumericSuffixPattern.FindStringSubmatch(modelID)
	if len(matches) == 2 {
		trimmed := strings.TrimSpace(matches[1])
		if trimmed != "" {
			return trimmed
		}
	}
	return modelID
}

func collapseLMStudioModelIDs(models []string) []string {
	result := make([]string, 0, len(models))
	seen := map[string]struct{}{}
	for _, model := range models {
		normalized := normalizeLMStudioModelID(model)
		if normalized == "" {
			continue
		}
		key := strings.ToLower(normalized)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, normalized)
	}
	return result
}

func lmstudioModelIDsEquivalent(left string, right string) bool {
	l := normalizeLMStudioModelID(left)
	r := normalizeLMStudioModelID(right)
	if l == "" || r == "" {
		return false
	}
	return strings.EqualFold(l, r)
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func shouldAvoidInteractiveLMStudioGet(output string) bool {
	text := strings.ToLower(strings.TrimSpace(output))
	if text == "" {
		return false
	}
	return strings.Contains(text, "enotty") ||
		strings.Contains(text, "not a typewriter") ||
		strings.Contains(text, "please select one from the list below") ||
		strings.Contains(text, "cannot find a model matching the provided model key")
}

func lmstudioAuthPasskeyError(output string) bool {
	text := strings.ToLower(strings.TrimSpace(output))
	if text == "" {
		return false
	}
	return strings.Contains(text, "invalid passkey for lms cli client") ||
		strings.Contains(text, "using the lms shipped with lm studio")
}

func lmstudioModelAlreadyRemoved(output string) bool {
	text := strings.ToLower(strings.TrimSpace(output))
	if text == "" {
		return false
	}
	return strings.Contains(text, "model is not loaded") ||
		strings.Contains(text, "not currently loaded") ||
		strings.Contains(text, "already unloaded") ||
		strings.Contains(text, "no model is loaded") ||
		strings.Contains(text, "cannot find a model")
}

func (d *lmstudioDriver) unloadModelAcrossContexts(
	modelID string,
	contexts []lmstudioCommandContext,
) error {
	var sawPasskeyFailure bool
	var firstErr error
	for _, ctx := range contexts {
		cmd := exec.Command(ctx.path, "unload", modelID)
		cmd.Env = d.envForPathWithHome(ctx.path, ctx.home)
		output, err := cmd.CombinedOutput()
		outText := strings.TrimSpace(string(output))

		if err == nil || lmstudioModelAlreadyRemoved(outText) {
			return nil
		}
		if lmstudioAuthPasskeyError(outText) {
			sawPasskeyFailure = true
			continue
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("unload lmstudio model %q via %s (HOME=%s): %w (%s)", modelID, ctx.path, ctx.home, err, outText)
		}
	}
	if firstErr != nil {
		return firstErr
	}
	if sawPasskeyFailure {
		return fmt.Errorf("unload lmstudio model %q failed due to lmstudio CLI passkey mismatch; ensure agent uses the LM Studio-shipped lms binary", modelID)
	}
	return fmt.Errorf("unload lmstudio model %q failed", modelID)
}

func (d *lmstudioDriver) baseURL() string {
	cfg, err := d.readConfig()
	port := lmsDefaultPort
	if err == nil && cfg.Port > 0 {
		port = cfg.Port
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func (d *lmstudioDriver) ensureServiceUnit() error {
	path := d.resolveLMSPath()
	if path == "" {
		return fmt.Errorf("lmstudio cli not installed")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(lmsServiceUnitPath), 0o755); err != nil {
		return fmt.Errorf("create lmstudio systemd directory: %w", err)
	}
	home := d.homeForPath(path)
	binDir := filepath.Dir(path)
	pathEnv := binDir + ":" + lmsDefaultSystemdPath
	unit := `[Unit]
Description=LM Studio OpenAI API Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=HOME=` + home + `
Environment=PATH=` + pathEnv + `
Environment=LMS_SERVER_PORT=` + fmt.Sprintf("%d", lmsDefaultPort) + `
Environment=LMS_BIND_ADDRESS=0.0.0.0
ExecStart=/bin/sh -lc 'set -e; ` + path + ` daemon up --json >/dev/null 2>&1 || ` + path + ` daemon up >/dev/null 2>&1; HELP="$(` + path + ` server start --help 2>&1 || true)"; case "$HELP" in *"--bind"*) exec ` + path + ` server start --port "${LMS_SERVER_PORT:-1234}" --bind "${LMS_BIND_ADDRESS:-0.0.0.0}" ;; *"--host"*) exec ` + path + ` server start --port "${LMS_SERVER_PORT:-1234}" --host "${LMS_BIND_ADDRESS:-0.0.0.0}" ;; *) export LMS_SERVER_HOST="${LMS_BIND_ADDRESS:-0.0.0.0}"; exec ` + path + ` server start --port "${LMS_SERVER_PORT:-1234}" ;; esac'
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
`
	if err := os.WriteFile(lmsServiceUnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write lmstudio service unit: %w", err)
	}
	if err := runCommand("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd daemon: %w", err)
	}
	if err := runCommand("systemctl", "enable", "lmstudio"); err != nil {
		return fmt.Errorf("enable lmstudio service: %w", err)
	}
	return nil
}

func (d *lmstudioDriver) startOrRestartService() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not available")
	}
	if err := d.ensureServiceUnit(); err != nil {
		return err
	}
	if err := runCommand("systemctl", "restart", "lmstudio"); err != nil {
		return fmt.Errorf("restart lmstudio service: %w", err)
	}
	if err := d.waitForAPIReady(lmsReadyTimeout); err != nil {
		return err
	}
	cfg, cfgErr := d.readConfig()
	port := lmsDefaultPort
	if cfgErr == nil && cfg.Port > 0 {
		port = cfg.Port
	}
	isExternal, err := isServiceListeningOnNonLoopback(port)
	if err != nil {
		return err
	}
	if !isExternal {
		return fmt.Errorf("lmstudio server is only listening on localhost; expected 0.0.0.0 or non-loopback interface")
	}
	return nil
}

func (d *lmstudioDriver) waitForAPIReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	lastErr := ""
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, d.baseURL()+"/v1/models", nil)
		if err == nil {
			resp, reqErr := client.Do(req)
			if reqErr == nil {
				_, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
				lastErr = fmt.Sprintf("status %d", resp.StatusCode)
			} else {
				lastErr = reqErr.Error()
			}
		} else {
			lastErr = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	if strings.TrimSpace(lastErr) == "" {
		lastErr = "timed out waiting for /v1/models"
	}
	return fmt.Errorf("lmstudio api not ready: %s", lastErr)
}

func (d *lmstudioDriver) fetchRemoteModels() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, d.baseURL()+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("lmstudio models endpoint failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id != "" {
			models = append(models, id)
		}
	}
	return models, nil
}

func (d *lmstudioDriver) readConfig() (lmstudioConfig, error) {
	cfg := lmstudioConfig{Port: lmsDefaultPort}
	raw, err := os.ReadFile(lmsConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read lmstudio config: %w", err)
	}
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return lmstudioConfig{}, fmt.Errorf("parse lmstudio config: %w", err)
	}
	if cfg.Port <= 0 {
		cfg.Port = lmsDefaultPort
	}
	return cfg, nil
}

func (d *lmstudioDriver) writeConfig(cfg lmstudioConfig) error {
	if cfg.Port <= 0 {
		cfg.Port = lmsDefaultPort
	}
	if err := os.MkdirAll(filepath.Dir(lmsConfigPath), 0o755); err != nil {
		return fmt.Errorf("create lmstudio config directory: %w", err)
	}
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode lmstudio config: %w", err)
	}
	if err := os.WriteFile(lmsConfigPath, append(payload, '\n'), 0o600); err != nil {
		return fmt.Errorf("write lmstudio config: %w", err)
	}
	return nil
}

func (d *lmstudioDriver) resolveLMSPath() string {
	if path, err := exec.LookPath("lms"); err == nil {
		path = strings.TrimSpace(path)
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			return path
		}
	}
	candidates := []string{"/root/.lmstudio/bin/lms"}
	if paths, err := filepath.Glob("/home/*/.lmstudio/bin/lms"); err == nil {
		candidates = append(candidates, paths...)
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

func (d *lmstudioDriver) resolveLMSContexts() []lmstudioCommandContext {
	contexts := make([]lmstudioCommandContext, 0, 6)
	seen := map[string]struct{}{}
	add := func(path string, home string) {
		path = strings.TrimSpace(path)
		home = strings.TrimSpace(home)
		if path == "" {
			return
		}
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return
		}
		if home == "" {
			home = d.homeForPath(path)
		}
		key := path + "|" + home
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		contexts = append(contexts, lmstudioCommandContext{
			path: path,
			home: home,
		})
	}

	unitPath, unitHome := d.resolveLMSFromServiceUnit()
	add(unitPath, unitHome)
	add(d.resolveLMSPath(), "")

	candidates := []string{
		"/root/.lmstudio/bin/lms",
	}
	if paths, err := filepath.Glob("/home/*/.lmstudio/bin/lms"); err == nil {
		candidates = append(candidates, paths...)
	}
	for _, candidate := range candidates {
		add(candidate, "")
	}
	if path, err := exec.LookPath("lms"); err == nil {
		add(path, "")
	}
	return contexts
}

func (d *lmstudioDriver) resolveLMSFromServiceUnit() (string, string) {
	raw, err := os.ReadFile(lmsServiceUnitPath)
	if err != nil {
		return "", ""
	}
	unit := string(raw)
	home := ""
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Environment=HOME=") {
			home = strings.TrimSpace(strings.TrimPrefix(line, "Environment=HOME="))
			home = strings.Trim(home, `"`)
			break
		}
	}
	matches := lmstudioBinaryPathPattern.FindStringSubmatch(unit)
	if len(matches) < 2 {
		return "", home
	}
	return strings.TrimSpace(matches[1]), home
}

func (d *lmstudioDriver) clearConfiguredModel(modelID string) {
	cfg, err := d.readConfig()
	if err != nil {
		return
	}
	configured := strings.TrimSpace(cfg.Model)
	if configured == "" {
		return
	}
	if lmstudioModelIDsEquivalent(configured, modelID) {
		cfg.Model = ""
		_ = d.writeConfig(cfg)
	}
}

func (d *lmstudioDriver) homeForPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/root"
	}
	marker := "/.lmstudio/bin/lms"
	if strings.Contains(path, marker) {
		return strings.TrimSuffix(path, marker)
	}
	return "/root"
}

func (d *lmstudioDriver) envForPath(path string) []string {
	return d.envForPathWithHome(path, d.homeForPath(path))
}

func (d *lmstudioDriver) envForPathWithHome(path string, home string) []string {
	home = strings.TrimSpace(home)
	if home == "" {
		home = d.homeForPath(path)
	}
	binDir := filepath.Dir(path)
	env := os.Environ()
	env = append(env, "HOME="+home)
	env = append(env, "PATH="+binDir+":"+lmsDefaultSystemdPath)
	return env
}
