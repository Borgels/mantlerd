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
		if err := d.loadModel(cfg.Model); err != nil {
			return fmt.Errorf("load configured lmstudio model %q: %w", cfg.Model, err)
		}
	}
	return nil
}

func (d *lmstudioDriver) IsInstalled() bool {
	return strings.TrimSpace(d.resolveLMSPath()) != ""
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

func (d *lmstudioDriver) EnsureModelWithFlags(modelID string, _ *types.ModelFeatureFlags) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	if err := d.Install(); err != nil {
		return err
	}
	if err := d.loadModel(modelID); err != nil {
		return err
	}
	cfg, _ := d.readConfig()
	cfg.Model = modelID
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
	return models
}

func (d *lmstudioDriver) InstalledModels() []types.InstalledModel {
	result := make([]types.InstalledModel, 0)
	seen := map[string]struct{}{}
	cfg, _ := d.readConfig()
	configured := strings.TrimSpace(cfg.Model)

	models, err := d.fetchRemoteModels()
	if err == nil {
		for _, model := range models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			seen[model] = struct{}{}
			result = append(result, types.InstalledModel{
				ModelID: model,
				Runtime: types.RuntimeLMStudio,
				Status:  types.ModelReady,
			})
		}
		if configured != "" {
			if _, ok := seen[configured]; !ok {
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
		if model == modelID {
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
	path := d.resolveLMSPath()
	if path == "" {
		return fmt.Errorf("lmstudio cli not installed")
	}
	cmd := exec.Command(path, "unload", modelID)
	cmd.Env = d.envForPath(path)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("unload lmstudio model %q: %w (%s)", modelID, err, strings.TrimSpace(string(output)))
	}
	cfg, err := d.readConfig()
	if err == nil && strings.EqualFold(strings.TrimSpace(cfg.Model), modelID) {
		cfg.Model = ""
		_ = d.writeConfig(cfg)
	}
	return nil
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

func (d *lmstudioDriver) loadModel(modelID string) error {
	path := d.resolveLMSPath()
	if path == "" {
		return fmt.Errorf("lmstudio cli not installed")
	}
	loadCmd := exec.Command(path, "load", modelID)
	loadCmd.Env = d.envForPath(path)
	if output, err := loadCmd.CombinedOutput(); err != nil {
		getCmd := exec.Command(path, "get", modelID)
		getCmd.Env = d.envForPath(path)
		if getOut, getErr := getCmd.CombinedOutput(); getErr != nil {
			return fmt.Errorf(
				"load lmstudio model %q failed: %w (%s); download attempt failed: %w (%s)",
				modelID,
				err,
				strings.TrimSpace(string(output)),
				getErr,
				strings.TrimSpace(string(getOut)),
			)
		}
		retryCmd := exec.Command(path, "load", modelID)
		retryCmd.Env = d.envForPath(path)
		if retryOut, retryErr := retryCmd.CombinedOutput(); retryErr != nil {
			return fmt.Errorf("load lmstudio model %q after download: %w (%s)", modelID, retryErr, strings.TrimSpace(string(retryOut)))
		}
	}
	return nil
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
	isExternal, err := d.isListeningOnNonLoopback(port)
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

func (d *lmstudioDriver) isListeningOnNonLoopback(port int) (bool, error) {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("ss -ltnH '( sport = :%d )' || true", port))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("check lmstudio listen sockets: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	seenAny := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		localAddr := fields[3]
		if localAddr == "" {
			continue
		}
		seenAny = true
		if !isLoopbackSocket(localAddr) {
			return true, nil
		}
	}
	if !seenAny {
		return false, nil
	}
	return false, nil
}

func isLoopbackSocket(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "127.0.0.1:") || strings.HasPrefix(value, "[::1]:")
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
		return path
	}
	candidates := []string{
		"/root/.lmstudio/bin/lms",
		filepath.Join(os.Getenv("HOME"), ".lmstudio", "bin", "lms"),
	}
	if paths, err := filepath.Glob("/home/*/.lmstudio/bin/lms"); err == nil {
		candidates = append(candidates, paths...)
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
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
	home := d.homeForPath(path)
	binDir := filepath.Dir(path)
	env := os.Environ()
	env = append(env, "HOME="+home)
	env = append(env, "PATH="+binDir+":"+lmsDefaultSystemdPath)
	return env
}
