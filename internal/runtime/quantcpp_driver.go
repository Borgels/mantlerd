package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Borgels/mantlerd/internal/types"
)

const (
	quantCppServiceUnitPath = "/etc/systemd/system/quantcpp.service"
	quantCppConfigPath      = "/etc/mantler/quantcpp.json"
	quantCppInstallDir      = "/opt/mantler/quantcpp"
	quantCppBinaryPath      = "/opt/mantler/quantcpp/quant-server"
	quantCppModelsDir       = "/var/lib/mantler/models/quantcpp"
	quantCppDefaultPort     = 8080
	quantCppReadyTimeout    = 90 * time.Second
)

type quantCppConfig struct {
	Model          string `json:"model,omitempty"`
	Port           int    `json:"port,omitempty"`
	KeyQuantType   string `json:"keyQuantType,omitempty"`
	ValueQuantType string `json:"valueQuantType,omitempty"`
	Threads        int    `json:"threads,omitempty"`
	ContextSize    int    `json:"contextSize,omitempty"`
}

type quantCppDriver struct{}

func newQuantCppDriver() Driver {
	return &quantCppDriver{}
}

func (d *quantCppDriver) Name() string { return "quantcpp" }

func (d *quantCppDriver) Install() error {
	cfg, err := d.readConfig()
	if err != nil {
		return err
	}
	if !d.IsInstalled() {
		assetURL, err := resolveQuantCppReleaseAssetURL()
		if err != nil {
			return fmt.Errorf("resolve quant.cpp release asset: %w", err)
		}
		if err := installQuantCppFromAsset(assetURL); err != nil {
			return err
		}
	}
	if err := d.writeConfig(cfg); err != nil {
		return err
	}
	if err := d.ensureServiceUnit(cfg); err != nil {
		return err
	}
	if err := runCommand("systemctl", "enable", "quantcpp"); err != nil {
		return fmt.Errorf("enable quantcpp service: %w", err)
	}
	if err := runCommand("systemctl", "restart", "quantcpp"); err != nil {
		return fmt.Errorf("restart quantcpp service: %w", err)
	}
	if err := d.waitForReady(quantCppReadyTimeout); err != nil {
		return err
	}
	isExternal, err := isServiceListeningOnNonLoopback(cfg.Port)
	if err != nil {
		return err
	}
	if !isExternal {
		return fmt.Errorf("quantcpp server is only listening on localhost; expected non-loopback bind")
	}
	return nil
}

func (d *quantCppDriver) Uninstall() error {
	_ = runCommand("systemctl", "stop", "quantcpp")
	_ = runCommand("systemctl", "disable", "quantcpp")
	_ = os.Remove(quantCppServiceUnitPath)
	_ = runCommand("systemctl", "daemon-reload")
	_ = os.RemoveAll(quantCppInstallDir)
	_ = os.Remove(quantCppConfigPath)
	return nil
}

func (d *quantCppDriver) IsInstalled() bool {
	info, err := os.Stat(quantCppBinaryPath)
	return err == nil && !info.IsDir()
}

func (d *quantCppDriver) IsReady() bool {
	_, err := d.fetchRemoteModels()
	return err == nil
}

func (d *quantCppDriver) Version() string {
	output, err := exec.Command(quantCppBinaryPath, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (d *quantCppDriver) EnsureModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	return d.StartModelWithFlags(modelID, flags)
}

func (d *quantCppDriver) PrepareModelWithFlags(modelID string, _ *types.ModelFeatureFlags) error {
	modelPath, err := d.resolveModelPath(modelID)
	if err != nil {
		return err
	}
	cfg, err := d.readConfig()
	if err != nil {
		return err
	}
	cfg.Model = modelPath
	if err := d.writeConfig(cfg); err != nil {
		return err
	}
	if err := d.ensureServiceUnit(cfg); err != nil {
		return err
	}
	if err := runCommand("systemctl", "restart", "quantcpp"); err != nil {
		return fmt.Errorf("restart quantcpp service: %w", err)
	}
	return d.waitForReady(quantCppReadyTimeout)
}

func (d *quantCppDriver) StartModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	return d.PrepareModelWithFlags(modelID, flags)
}

func (d *quantCppDriver) StopModel(modelID string) error {
	_ = modelID
	return runCommand("systemctl", "stop", "quantcpp")
}

func (d *quantCppDriver) ListModels() []string {
	models, err := d.fetchRemoteModels()
	if err != nil {
		return nil
	}
	return models
}

func (d *quantCppDriver) HasModel(modelID string) bool {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return false
	}
	for _, model := range d.ListModels() {
		if strings.EqualFold(strings.TrimSpace(model), modelID) {
			return true
		}
	}
	return false
}

func (d *quantCppDriver) RemoveModel(modelID string) error {
	cfg, err := d.readConfig()
	if err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Model), strings.TrimSpace(modelID)) {
		cfg.Model = ""
		if err := d.writeConfig(cfg); err != nil {
			return err
		}
	}
	return runCommand("systemctl", "restart", "quantcpp")
}

func (d *quantCppDriver) BenchmarkModel(
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

func (d *quantCppDriver) RestartRuntime() error {
	cfg, err := d.readConfig()
	if err != nil {
		return err
	}
	if err := d.writeConfig(cfg); err != nil {
		return err
	}
	if err := d.ensureServiceUnit(cfg); err != nil {
		return err
	}
	if err := runCommand("systemctl", "restart", "quantcpp"); err != nil {
		return fmt.Errorf("restart quantcpp service: %w", err)
	}
	return d.waitForReady(quantCppReadyTimeout)
}

func (d *quantCppDriver) RuntimeConfig() map[string]any {
	cfg, err := d.readConfig()
	if err != nil {
		return nil
	}
	return map[string]any{
		"keyQuantType":   cfg.KeyQuantType,
		"valueQuantType": cfg.ValueQuantType,
		"threads":        cfg.Threads,
		"contextSize":    cfg.ContextSize,
		"version":        strings.TrimSpace(d.Version()),
	}
}

func (d *quantCppDriver) benchmarkOnce(modelID string, prompt string, sampleOutputTokens int) (BenchmarkResult, error) {
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
		return BenchmarkResult{}, fmt.Errorf("encode quantcpp benchmark request: %w", err)
	}

	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, d.baseURL()+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("create quantcpp benchmark request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("quantcpp benchmark request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return BenchmarkResult{}, fmt.Errorf("quantcpp benchmark failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return BenchmarkResult{}, fmt.Errorf("decode quantcpp benchmark response: %w", err)
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

func (d *quantCppDriver) CompletePrompt(
	modelID string,
	systemPrompt string,
	prompt string,
	maxTokens int,
) (PromptCompletionResult, error) {
	if strings.TrimSpace(modelID) == "" {
		return PromptCompletionResult{}, fmt.Errorf("model ID is required")
	}
	if maxTokens <= 0 {
		maxTokens = 128
	}
	if err := d.EnsureModelWithFlags(modelID, nil); err != nil {
		return PromptCompletionResult{}, err
	}
	messages := []map[string]string{}
	if strings.TrimSpace(systemPrompt) != "" {
		messages = append(messages, map[string]string{"role": "system", "content": systemPrompt})
	}
	messages = append(messages, map[string]string{"role": "user", "content": prompt})
	reqBody := map[string]any{
		"model":      modelID,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     false,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return PromptCompletionResult{}, fmt.Errorf("encode quantcpp completion request: %w", err)
	}
	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, d.baseURL()+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return PromptCompletionResult{}, fmt.Errorf("create quantcpp completion request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return PromptCompletionResult{}, fmt.Errorf("quantcpp completion request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return PromptCompletionResult{}, fmt.Errorf("quantcpp completion failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return PromptCompletionResult{}, fmt.Errorf("decode quantcpp completion response: %w", err)
	}
	latencyMs := float64(time.Since(start).Milliseconds())
	seconds := latencyMs / 1000.0
	if seconds <= 0 {
		seconds = 0.001
	}
	output := ""
	if len(parsed.Choices) > 0 {
		output = strings.TrimSpace(parsed.Choices[0].Message.Content)
	}
	return PromptCompletionResult{
		Output:       output,
		LatencyMs:    roundTo(latencyMs, 2),
		TTFTMs:       roundTo(latencyMs, 2),
		TokensPerSec: roundTo(float64(parsed.Usage.CompletionTokens)/seconds, 2),
		OutputTokens: parsed.Usage.CompletionTokens,
	}, nil
}

func (d *quantCppDriver) resolveModelPath(modelID string) (string, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "", fmt.Errorf("model ID is required")
	}
	if strings.HasPrefix(modelID, "/") {
		if _, err := os.Stat(modelID); err != nil {
			return "", fmt.Errorf("model path not found: %w", err)
		}
		return modelID, nil
	}
	if strings.HasPrefix(modelID, "http://") || strings.HasPrefix(modelID, "https://") {
		if !strings.HasSuffix(strings.ToLower(modelID), ".gguf") {
			return "", fmt.Errorf("remote model URL must point to .gguf")
		}
		name := filepath.Base(modelID)
		target := filepath.Join(quantCppModelsDir, name)
		if err := os.MkdirAll(quantCppModelsDir, 0o755); err != nil {
			return "", err
		}
		if err := downloadFile(modelID, target); err != nil {
			return "", err
		}
		return target, nil
	}

	if err := os.MkdirAll(quantCppModelsDir, 0o755); err != nil {
		return "", err
	}
	dir := filepath.Join(quantCppModelsDir, strings.ReplaceAll(modelID, "/", "__"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	hfPath, err := exec.LookPath("hf")
	if err == nil {
		cmd := exec.Command(hfPath, "download", modelID, "--include", "*.gguf", "--local-dir", dir)
		if output, cmdErr := cmd.CombinedOutput(); cmdErr != nil {
			return "", fmt.Errorf("hf download %s failed: %w (%s)", modelID, cmdErr, strings.TrimSpace(string(output)))
		}
		matches, globErr := filepath.Glob(filepath.Join(dir, "*.gguf"))
		if globErr == nil && len(matches) > 0 {
			slices.Sort(matches)
			return matches[0], nil
		}
	}
	return "", fmt.Errorf("unable to resolve model %q to a local GGUF file", modelID)
}

func (d *quantCppDriver) fetchRemoteModels() ([]string, error) {
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
		return nil, fmt.Errorf("quantcpp models endpoint failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
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

func (d *quantCppDriver) baseURL() string {
	cfg, err := d.readConfig()
	port := quantCppDefaultPort
	if err == nil && cfg.Port > 0 {
		port = cfg.Port
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func (d *quantCppDriver) waitForReady(timeout time.Duration) error {
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
	return fmt.Errorf("quantcpp api not ready: %s", lastErr)
}

func (d *quantCppDriver) readConfig() (quantCppConfig, error) {
	cfg := quantCppConfig{
		Port:           quantCppDefaultPort,
		KeyQuantType:   "uniform_4b",
		ValueQuantType: "q4",
		Threads:        max(1, goruntime.NumCPU()),
		ContextSize:    8192,
	}
	raw, err := os.ReadFile(quantCppConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read quantcpp config: %w", err)
	}
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse quantcpp config: %w", err)
	}
	if cfg.Port <= 0 {
		cfg.Port = quantCppDefaultPort
	}
	if strings.TrimSpace(cfg.KeyQuantType) == "" {
		cfg.KeyQuantType = "uniform_4b"
	}
	if !isSafeQuantToken(cfg.KeyQuantType) {
		cfg.KeyQuantType = "uniform_4b"
	}
	if strings.TrimSpace(cfg.ValueQuantType) == "" {
		cfg.ValueQuantType = "q4"
	}
	if !isSafeQuantToken(cfg.ValueQuantType) {
		cfg.ValueQuantType = "q4"
	}
	if cfg.Threads <= 0 {
		cfg.Threads = max(1, goruntime.NumCPU())
	}
	if cfg.ContextSize <= 0 {
		cfg.ContextSize = 8192
	}
	return cfg, nil
}

func (d *quantCppDriver) writeConfig(cfg quantCppConfig) error {
	if cfg.Port <= 0 {
		cfg.Port = quantCppDefaultPort
	}
	if strings.TrimSpace(cfg.KeyQuantType) == "" {
		cfg.KeyQuantType = "uniform_4b"
	}
	if !isSafeQuantToken(cfg.KeyQuantType) {
		return fmt.Errorf("invalid quantcpp key quant type: %q", cfg.KeyQuantType)
	}
	if strings.TrimSpace(cfg.ValueQuantType) == "" {
		cfg.ValueQuantType = "q4"
	}
	if !isSafeQuantToken(cfg.ValueQuantType) {
		return fmt.Errorf("invalid quantcpp value quant type: %q", cfg.ValueQuantType)
	}
	if cfg.Threads <= 0 {
		cfg.Threads = max(1, goruntime.NumCPU())
	}
	if cfg.ContextSize <= 0 {
		cfg.ContextSize = 8192
	}
	if err := os.MkdirAll(filepath.Dir(quantCppConfigPath), 0o755); err != nil {
		return fmt.Errorf("create quantcpp config directory: %w", err)
	}
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode quantcpp config: %w", err)
	}
	if err := os.WriteFile(quantCppConfigPath, append(payload, '\n'), 0o600); err != nil {
		return fmt.Errorf("write quantcpp config: %w", err)
	}
	return nil
}

func (d *quantCppDriver) ensureServiceUnit(cfg quantCppConfig) error {
	if err := os.MkdirAll(filepath.Dir(quantCppServiceUnitPath), 0o755); err != nil {
		return fmt.Errorf("create systemd directory: %w", err)
	}
	args := []string{}
	if model := strings.TrimSpace(cfg.Model); model != "" {
		args = append(args, shellQuote(model))
	}
	args = append(
		args,
		"--host", "0.0.0.0",
		"-p", fmt.Sprintf("%d", cfg.Port),
		"-k", shellQuote(cfg.KeyQuantType),
		"-v", shellQuote(cfg.ValueQuantType),
		"-j", fmt.Sprintf("%d", cfg.Threads),
		"-c", fmt.Sprintf("%d", cfg.ContextSize),
	)
	execStart := quantCppBinaryPath + " " + strings.Join(args, " ")
	unit := `[Unit]
Description=quant.cpp server managed by mantlerd
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/bin/sh -lc '` + execStart + `'
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
`
	if err := os.WriteFile(quantCppServiceUnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write service unit: %w", err)
	}
	return runCommand("systemctl", "daemon-reload")
}

func resolveQuantCppReleaseAssetURL() (string, error) {
	if override := strings.TrimSpace(os.Getenv("QUANTCPP_ASSET_URL")); override != "" {
		return override, nil
	}
	repo := strings.TrimSpace(os.Getenv("QUANTCPP_REPO"))
	if repo == "" {
		repo = "quantumaikr/TurboQuant.cpp"
	}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mantlerd-quantcpp-installer")
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch github release metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("github release metadata request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode github release metadata: %w", err)
	}
	osToken := strings.ToLower(goruntime.GOOS)
	archToken := strings.ToLower(goruntime.GOARCH)
	for _, asset := range payload.Assets {
		name := strings.ToLower(asset.Name)
		if !strings.Contains(name, osToken) || !strings.Contains(name, archToken) {
			continue
		}
		if !(strings.Contains(name, "quant") || strings.Contains(name, "server")) {
			continue
		}
		if strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
			return asset.BrowserDownloadURL, nil
		}
	}
	for _, asset := range payload.Assets {
		name := strings.ToLower(asset.Name)
		if strings.Contains(name, "quant-server") &&
			(strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz")) {
			return asset.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("unable to find quantcpp release asset for %s/%s", goruntime.GOOS, goruntime.GOARCH)
}

func installQuantCppFromAsset(assetURL string) error {
	if err := os.MkdirAll(quantCppInstallDir, 0o755); err != nil {
		return fmt.Errorf("create install directory: %w", err)
	}
	tempDir, err := os.MkdirTemp("", "quantcpp-install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	archivePath := filepath.Join(tempDir, "asset")
	if err := downloadFile(assetURL, archivePath); err != nil {
		return err
	}
	lowerURL := strings.ToLower(assetURL)
	switch {
	case strings.HasSuffix(lowerURL, ".zip"):
		if err := unzipArchive(archivePath, tempDir); err != nil {
			return fmt.Errorf("extract quantcpp archive: %w", err)
		}
	case strings.HasSuffix(lowerURL, ".tar.gz"), strings.HasSuffix(lowerURL, ".tgz"):
		if err := extractTarGzArchive(archivePath, tempDir); err != nil {
			return fmt.Errorf("extract quantcpp archive: %w", err)
		}
	default:
		// Treat unknown extension as direct executable download.
		if err := os.Chmod(archivePath, 0o755); err != nil {
			return fmt.Errorf("make downloaded binary executable: %w", err)
		}
		raw, err := os.ReadFile(archivePath)
		if err != nil {
			return fmt.Errorf("read downloaded quantcpp binary: %w", err)
		}
		if err := os.WriteFile(quantCppBinaryPath, raw, 0o755); err != nil {
			return fmt.Errorf("install quantcpp binary: %w", err)
		}
		return nil
	}
	quantServerPath, err := findExecutableNamed(tempDir, "quant-server")
	if err != nil {
		return err
	}
	contents, err := os.ReadFile(quantServerPath)
	if err != nil {
		return fmt.Errorf("read extracted quantcpp binary: %w", err)
	}
	if err := os.WriteFile(quantCppBinaryPath, contents, 0o755); err != nil {
		return fmt.Errorf("write quantcpp binary: %w", err)
	}
	return nil
}

func extractTarGzArchive(archivePath string, destDir string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, header.Name)
		cleanDest := filepath.Clean(destDir)
		cleanTarget := filepath.Clean(target)
		prefix := cleanDest + string(os.PathSeparator)
		if cleanTarget != cleanDest && !strings.HasPrefix(cleanTarget, prefix) {
			return fmt.Errorf("unsafe tar path: %s", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(dst, tr); err != nil {
				dst.Close()
				return err
			}
			if err := dst.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

func isSafeQuantToken(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	for _, ch := range trimmed {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_' || ch == '-' || ch == '.' {
			continue
		}
		return false
	}
	return true
}
