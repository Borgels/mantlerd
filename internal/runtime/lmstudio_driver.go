package runtime

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/mantlerd/internal/discovery"
	"github.com/Borgels/mantlerd/internal/types"
)

const (
	llamaCppServiceUnitPath  = "/etc/systemd/system/llamacpp.service"
	llamaCppConfigPath       = "/etc/mantler/llamacpp.json"
	llamaCppInstallDir       = "/opt/mantler/llamacpp"
	llamaCppBinaryPath       = "/opt/mantler/llamacpp/llama-server"
	llamaCppModelsDir        = "/var/lib/mantler/models/llamacpp"
	llamaCppDefaultPort      = 1234
	llamaCppReadyTimeout     = 90 * time.Second
	llamaCppMaxHTTPBodyBytes = 1 << 20
)

type llamaCppConfig struct {
	Model       string `json:"model"`
	Port        int    `json:"port"`
	Backend     string `json:"backend,omitempty"`
	NGPULayers  int    `json:"nGpuLayers,omitempty"`
	ContextSize int    `json:"contextSize,omitempty"`
}

type llamaCppDriver struct{}

func newLMStudioDriver() Driver {
	return &llamaCppDriver{}
}

func newLlamaCppDriver() Driver {
	return &llamaCppDriver{}
}

func (d *llamaCppDriver) Name() string { return "llamacpp" }

func (d *llamaCppDriver) Install() error {
	cfg, err := d.readConfig()
	if err != nil {
		return err
	}
	if cfg.Port <= 0 {
		cfg.Port = llamaCppDefaultPort
	}
	availablePort, err := resolveLlamaCppPort(cfg.Port)
	if err != nil {
		return err
	}
	cfg.Port = availablePort
	if cfg.Backend == "" {
		cfg.Backend = detectBestBackend()
	}
	if cfg.ContextSize <= 0 {
		cfg.ContextSize = 8192
	}
	if cfg.NGPULayers == 0 {
		cfg.NGPULayers = -1
	}

	if !d.IsInstalled() {
		if discovery.IsDGXSpark() {
			if err := buildLlamaCppFromSourceForDGXSpark(); err != nil {
				return err
			}
		} else {
			assetURL, err := resolveLlamaCppReleaseAssetURL(cfg.Backend)
			if err != nil {
				return fmt.Errorf("resolve llama.cpp release asset: %w", err)
			}
			if err := installLlamaCppFromAsset(assetURL); err != nil {
				return err
			}
		}
		if err := verifyLlamaCppBinary(); err != nil {
			_ = os.Remove(llamaCppBinaryPath)
			return err
		}
	}

	if err := d.writeConfig(cfg); err != nil {
		return err
	}
	if err := d.ensureServiceUnit(cfg); err != nil {
		return err
	}
	if err := runSystemctl("enable", "llamacpp"); err != nil {
		return fmt.Errorf("enable llamacpp service: %w", err)
	}
	if err := runSystemctl("restart", "llamacpp"); err != nil {
		return fmt.Errorf("restart llamacpp service: %w", err)
	}
	if err := d.waitForReady(llamaCppReadyTimeout); err != nil {
		return err
	}
	isExternal, err := isServiceListeningOnNonLoopback(cfg.Port)
	if err != nil {
		return err
	}
	if !isExternal {
		return fmt.Errorf("llamacpp server is only listening on localhost; expected non-loopback bind")
	}
	return nil
}

func (d *llamaCppDriver) Uninstall() error {
	_ = runSystemctl("stop", "llamacpp")
	_ = runSystemctl("disable", "llamacpp")
	_ = os.Remove(llamaCppServiceUnitPath)
	_ = runSystemctl("daemon-reload")
	_ = os.RemoveAll(llamaCppInstallDir)
	_ = os.Remove(llamaCppConfigPath)
	return nil
}

func (d *llamaCppDriver) IsInstalled() bool {
	info, err := os.Stat(llamaCppBinaryPath)
	return err == nil && !info.IsDir()
}

func (d *llamaCppDriver) IsReady() bool {
	_, err := d.fetchRemoteModels()
	return err == nil
}

func (d *llamaCppDriver) Version() string {
	output, err := exec.Command(llamaCppBinaryPath, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (d *llamaCppDriver) EnsureModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	return d.StartModelWithFlags(modelID, flags)
}

func (d *llamaCppDriver) PrepareModelWithFlags(modelID string, _ *types.ModelFeatureFlags) error {
	modelPath, err := d.resolveModelPath(modelID)
	if err != nil {
		return err
	}
	cfg, err := d.readConfig()
	if err != nil {
		return err
	}
	cfg.Model = modelPath
	if cfg.Port <= 0 {
		cfg.Port = llamaCppDefaultPort
	}
	availablePort, err := resolveLlamaCppPort(cfg.Port)
	if err != nil {
		return err
	}
	cfg.Port = availablePort
	if cfg.Backend == "" {
		cfg.Backend = detectBestBackend()
	}
	if cfg.ContextSize <= 0 {
		cfg.ContextSize = 8192
	}
	if cfg.NGPULayers == 0 {
		cfg.NGPULayers = -1
	}
	if err := d.writeConfig(cfg); err != nil {
		return err
	}
	if err := d.ensureServiceUnit(cfg); err != nil {
		return err
	}
	if err := runSystemctl("restart", "llamacpp"); err != nil {
		return fmt.Errorf("restart llamacpp service: %w", err)
	}
	return d.waitForReady(llamaCppReadyTimeout)
}

func (d *llamaCppDriver) StartModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	return d.PrepareModelWithFlags(modelID, flags)
}

func (d *llamaCppDriver) StopModel(modelID string) error {
	_ = modelID
	return runSystemctl("stop", "llamacpp")
}

func (d *llamaCppDriver) ListModels() []string {
	models, err := d.fetchRemoteModels()
	if err != nil {
		return nil
	}
	return models
}

func (d *llamaCppDriver) HasModel(modelID string) bool {
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

func (d *llamaCppDriver) RemoveModel(modelID string) error {
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
	return runSystemctl("restart", "llamacpp")
}

func (d *llamaCppDriver) BenchmarkModel(
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

func (d *llamaCppDriver) RestartRuntime() error {
	cfg, err := d.readConfig()
	if err != nil {
		return err
	}
	if cfg.Port <= 0 {
		cfg.Port = llamaCppDefaultPort
	}
	availablePort, err := resolveLlamaCppPort(cfg.Port)
	if err != nil {
		return err
	}
	cfg.Port = availablePort
	if cfg.Backend == "" {
		cfg.Backend = detectBestBackend()
	}
	if cfg.ContextSize <= 0 {
		cfg.ContextSize = 8192
	}
	if cfg.NGPULayers == 0 {
		cfg.NGPULayers = -1
	}
	if err := d.writeConfig(cfg); err != nil {
		return err
	}
	if err := d.ensureServiceUnit(cfg); err != nil {
		return err
	}
	if err := runSystemctl("restart", "llamacpp"); err != nil {
		return fmt.Errorf("restart llamacpp service: %w", err)
	}
	return d.waitForReady(llamaCppReadyTimeout)
}

func (d *llamaCppDriver) RuntimeConfig() map[string]any {
	cfg, err := d.readConfig()
	if err != nil {
		return nil
	}
	return map[string]any{
		"backend":     cfg.Backend,
		"nGpuLayers":  cfg.NGPULayers,
		"contextSize": cfg.ContextSize,
		"version":     strings.TrimSpace(d.Version()),
	}
}

func (d *llamaCppDriver) benchmarkOnce(modelID string, prompt string, sampleOutputTokens int) (BenchmarkResult, error) {
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
		return BenchmarkResult{}, fmt.Errorf("encode llamacpp benchmark request: %w", err)
	}

	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, d.baseURL()+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("create llamacpp benchmark request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("llamacpp benchmark request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, llamaCppMaxHTTPBodyBytes))
	if resp.StatusCode >= 400 {
		return BenchmarkResult{}, fmt.Errorf("llamacpp benchmark failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return BenchmarkResult{}, fmt.Errorf("decode llamacpp benchmark response: %w", err)
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

func (d *llamaCppDriver) CompletePrompt(
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
		return PromptCompletionResult{}, fmt.Errorf("encode llamacpp completion request: %w", err)
	}
	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, d.baseURL()+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return PromptCompletionResult{}, fmt.Errorf("create llamacpp completion request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return PromptCompletionResult{}, fmt.Errorf("llamacpp completion request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, llamaCppMaxHTTPBodyBytes))
	if resp.StatusCode >= 400 {
		return PromptCompletionResult{}, fmt.Errorf("llamacpp completion failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
		return PromptCompletionResult{}, fmt.Errorf("decode llamacpp completion response: %w", err)
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

func (d *llamaCppDriver) resolveModelPath(modelID string) (string, error) {
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
		target := filepath.Join(llamaCppModelsDir, name)
		if err := os.MkdirAll(llamaCppModelsDir, 0o755); err != nil {
			return "", err
		}
		if err := downloadFile(modelID, target); err != nil {
			return "", err
		}
		return target, nil
	}

	// Hugging Face repo id fallback.
	if err := os.MkdirAll(llamaCppModelsDir, 0o755); err != nil {
		return "", err
	}
	dir := filepath.Join(llamaCppModelsDir, strings.ReplaceAll(modelID, "/", "__"))
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

func (d *llamaCppDriver) fetchRemoteModels() ([]string, error) {
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, llamaCppMaxHTTPBodyBytes))
		return nil, fmt.Errorf("llamacpp models endpoint failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
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

func (d *llamaCppDriver) baseURL() string {
	cfg, err := d.readConfig()
	port := llamaCppDefaultPort
	if err == nil && cfg.Port > 0 {
		port = cfg.Port
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func (d *llamaCppDriver) waitForReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	lastErr := ""
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, d.baseURL()+"/v1/models", nil)
		if err == nil {
			resp, reqErr := client.Do(req)
			if reqErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, llamaCppMaxHTTPBodyBytes))
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
	return fmt.Errorf("llamacpp api not ready: %s", lastErr)
}

func (d *llamaCppDriver) configFilePath() string { return runtimeConfigFile("llamacpp.json") }

func (d *llamaCppDriver) readConfig() (llamaCppConfig, error) {
	cfg := llamaCppConfig{
		Port:        llamaCppDefaultPort,
		Backend:     detectBestBackend(),
		NGPULayers:  -1,
		ContextSize: 8192,
	}
	raw, err := os.ReadFile(d.configFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read llamacpp config: %w", err)
	}
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse llamacpp config: %w", err)
	}
	if cfg.Port <= 0 {
		cfg.Port = llamaCppDefaultPort
	}
	if strings.TrimSpace(cfg.Backend) == "" {
		cfg.Backend = detectBestBackend()
	}
	if cfg.ContextSize <= 0 {
		cfg.ContextSize = 8192
	}
	if cfg.NGPULayers == 0 {
		cfg.NGPULayers = -1
	}
	return cfg, nil
}

func (d *llamaCppDriver) writeConfig(cfg llamaCppConfig) error {
	if cfg.Port <= 0 {
		cfg.Port = llamaCppDefaultPort
	}
	if cfg.Backend == "" {
		cfg.Backend = detectBestBackend()
	}
	if cfg.ContextSize <= 0 {
		cfg.ContextSize = 8192
	}
	if cfg.NGPULayers == 0 {
		cfg.NGPULayers = -1
	}
	p := d.configFilePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("create llamacpp config directory: %w", err)
	}
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode llamacpp config: %w", err)
	}
	if err := os.WriteFile(p, append(payload, '\n'), 0o664); err != nil {
		return fmt.Errorf("write llamacpp config: %w", err)
	}
	return nil
}

func (d *llamaCppDriver) ensureServiceUnit(cfg llamaCppConfig) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	if os.Geteuid() != 0 {
		if fileExists(llamaCppServiceUnitPath) {
			return nil
		}
		return fmt.Errorf("write service unit: requires root (run `mantler runtime install llamacpp` as root first)")
	}
	if err := os.MkdirAll(filepath.Dir(llamaCppServiceUnitPath), 0o755); err != nil {
		return fmt.Errorf("create systemd directory: %w", err)
	}
	execStart := fmt.Sprintf(
		"%s --host 0.0.0.0 --port %d -ngl %d -c %d",
		llamaCppBinaryPath,
		cfg.Port,
		cfg.NGPULayers,
		cfg.ContextSize,
	)
	if model := strings.TrimSpace(cfg.Model); model != "" {
		execStart += " --model " + shellQuote(model)
	}
	unit := `[Unit]
Description=llama.cpp server managed by mantlerd
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
	if err := os.WriteFile(llamaCppServiceUnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write service unit: %w", err)
	}
	return runSystemctl("daemon-reload")
}

func detectBestBackend() string {
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		return "cuda"
	}
	if runtime.GOOS == "darwin" {
		return "metal"
	}
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/kfd"); err == nil {
			return "rocm"
		}
		if _, err := exec.LookPath("vulkaninfo"); err == nil {
			return "vulkan"
		}
		if _, err := os.Stat("/usr/share/vulkan/icd.d"); err == nil {
			return "vulkan"
		}
	}
	return "cpu"
}

func resolveLlamaCppReleaseAssetURL(backend string) (string, error) {
	backend = strings.ToLower(strings.TrimSpace(backend))
	if backend == "" {
		backend = detectBestBackend()
	}
	if runtime.GOOS == "darwin" {
		if runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
			return "", fmt.Errorf("unsupported darwin arch: %s", runtime.GOARCH)
		}
		if runtime.GOARCH == "arm64" {
			return "https://github.com/ggerganov/llama.cpp/releases/latest/download/llama-b8708-bin-macos-arm64.tar.gz", nil
		}
		return "https://github.com/ggerganov/llama.cpp/releases/latest/download/llama-b8708-bin-macos-x64.tar.gz", nil
	}
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("unsupported OS for automatic llama.cpp install: %s", runtime.GOOS)
	}
	if runtime.GOARCH == "arm64" {
		switch backend {
		case "vulkan":
			return "https://github.com/ggerganov/llama.cpp/releases/latest/download/llama-b8708-bin-ubuntu-vulkan-arm64.tar.gz", nil
		default:
			return "https://github.com/ggerganov/llama.cpp/releases/latest/download/llama-b8708-bin-ubuntu-arm64.tar.gz", nil
		}
	}
	if runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("unsupported linux arch for automatic llama.cpp install: %s", runtime.GOARCH)
	}
	switch backend {
	case "cuda":
		return "https://github.com/ggerganov/llama.cpp/releases/latest/download/llama-b8708-bin-ubuntu-x64.tar.gz", nil
	case "vulkan":
		return "https://github.com/ggerganov/llama.cpp/releases/latest/download/llama-b8708-bin-ubuntu-vulkan-x64.tar.gz", nil
	case "rocm":
		return "https://github.com/ggerganov/llama.cpp/releases/latest/download/llama-b8708-bin-ubuntu-rocm-7.2-x64.tar.gz", nil
	default:
		return "https://github.com/ggerganov/llama.cpp/releases/latest/download/llama-b8708-bin-ubuntu-x64.tar.gz", nil
	}
}

func verifyLlamaCppBinary() error {
	info, err := os.Stat(llamaCppBinaryPath)
	if err != nil {
		return fmt.Errorf("verify llama.cpp binary: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("verify llama.cpp binary: %s is a directory", llamaCppBinaryPath)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("verify llama.cpp binary: %s is not executable", llamaCppBinaryPath)
	}
	output, err := exec.Command(llamaCppBinaryPath, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify llama.cpp binary execution: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func resolveLlamaCppPort(preferredPort int) (int, error) {
	if preferredPort <= 0 {
		preferredPort = llamaCppDefaultPort
	}
	if isTcpPortAvailable(preferredPort) {
		return preferredPort, nil
	}
	for candidate := preferredPort + 1; candidate <= preferredPort+50; candidate += 1 {
		if isTcpPortAvailable(candidate) {
			return candidate, nil
		}
	}
	return 0, fmt.Errorf("no available TCP port found for llama.cpp runtime near %d", preferredPort)
}

func isTcpPortAvailable(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func installLlamaCppFromAsset(assetURL string) error {
	tmpDir, err := os.MkdirTemp("", "llamacpp-install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, filepath.Base(assetURL))
	if err := downloadFile(assetURL, archivePath); err != nil {
		return err
	}
	extractDir := filepath.Join(tmpDir, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return err
	}
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		if err := unzipArchive(archivePath, extractDir); err != nil {
			return err
		}
	} else {
		if err := runCommand("tar", "-xzf", archivePath, "-C", extractDir); err != nil {
			return fmt.Errorf("extract llama.cpp archive: %w", err)
		}
	}
	binary, err := findExecutableNamed(extractDir, "llama-server")
	if err != nil {
		return err
	}
	_ = os.RemoveAll(llamaCppInstallDir)
	if err := os.MkdirAll(llamaCppInstallDir, 0o755); err != nil {
		return err
	}
	if err := copyLlamaCppArtifacts(filepath.Dir(binary), llamaCppInstallDir); err != nil {
		return err
	}
	return nil
}

func runCommandInDir(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func buildLlamaCppFromSourceForDGXSpark() error {
	requiredTools := []string{"git", "cmake", "make", "nvcc"}
	for _, tool := range requiredTools {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("building llama.cpp for DGX Spark requires %s in PATH", tool)
		}
	}

	tmpDir, err := os.MkdirTemp("", "llamacpp-dgx-build-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	sourceDir := filepath.Join(tmpDir, "llama.cpp")
	if err := runCommand("git", "clone", "--depth", "1", "https://github.com/ggml-org/llama.cpp", sourceDir); err != nil {
		return fmt.Errorf("clone llama.cpp: %w", err)
	}
	buildDir := filepath.Join(sourceDir, "build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return err
	}
	if err := runCommandInDir(
		buildDir,
		"cmake",
		"..",
		"-DGGML_CUDA=ON",
		"-DCMAKE_CUDA_ARCHITECTURES=121",
		"-DLLAMA_CURL=OFF",
	); err != nil {
		return fmt.Errorf("configure llama.cpp: %w", err)
	}
	if err := runCommandInDir(buildDir, "make", "-j8"); err != nil {
		return fmt.Errorf("build llama.cpp: %w", err)
	}

	binDir := filepath.Join(buildDir, "bin")
	binaryPath := filepath.Join(binDir, "llama-server")
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("llama.cpp build did not produce llama-server: %w", err)
	}

	_ = os.RemoveAll(llamaCppInstallDir)
	if err := os.MkdirAll(llamaCppInstallDir, 0o755); err != nil {
		return err
	}
	if err := copyLlamaCppArtifacts(binDir, llamaCppInstallDir); err != nil {
		return err
	}
	return nil
}

func copyLlamaCppArtifacts(sourceDir string, targetDir string) error {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return fmt.Errorf("read llama.cpp artifacts: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		sourcePath := filepath.Join(sourceDir, entry.Name())
		info, err := os.Stat(sourcePath)
		if err != nil {
			return fmt.Errorf("stat llama.cpp artifact %s: %w", entry.Name(), err)
		}
		data, err := os.ReadFile(sourcePath)
		if err != nil {
			return fmt.Errorf("read llama.cpp artifact %s: %w", entry.Name(), err)
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(filepath.Join(targetDir, entry.Name()), data, mode); err != nil {
			return fmt.Errorf("write llama.cpp artifact %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func downloadFile(url string, destination string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "mantlerd")
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("download failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, resp.Body)
	return err
}

func unzipArchive(zipPath string, destDir string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer reader.Close()
	cleanDest := filepath.Clean(destDir)
	prefix := cleanDest + string(os.PathSeparator)
	for _, file := range reader.File {
		targetPath := filepath.Join(destDir, file.Name)
		cleanTarget := filepath.Clean(targetPath)
		if cleanTarget != cleanDest && !strings.HasPrefix(cleanTarget, prefix) {
			return fmt.Errorf("unsafe zip path: %s", file.Name)
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(cleanTarget, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(cleanTarget), 0o755); err != nil {
			return err
		}
		src, err := file.Open()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(cleanTarget, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, file.Mode())
		if err != nil {
			src.Close()
			return err
		}
		_, copyErr := io.Copy(dst, src)
		_ = dst.Close()
		_ = src.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	return nil
}

func findExecutableNamed(root string, name string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Name() != name {
			return nil
		}
		found = path
		return io.EOF
	})
	if err != nil && err != io.EOF {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("%s not found in extracted archive", name)
	}
	return found, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

var lmstudioNumericSuffixPattern = regexp.MustCompile(`^(.*):[0-9]+$`)

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

func llamacppModelIDsEquivalent(left string, right string) bool {
	l := normalizeLMStudioModelID(left)
	r := normalizeLMStudioModelID(right)
	if l == "" || r == "" {
		return false
	}
	return strings.EqualFold(l, r)
}

func llamacppAuthPasskeyError(output string) bool {
	text := strings.ToLower(strings.TrimSpace(output))
	if text == "" {
		return false
	}
	return strings.Contains(text, "invalid passkey for lms cli client") ||
		strings.Contains(text, "using the bundled lms binary")
}

func llamacppModelAlreadyRemoved(output string) bool {
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
