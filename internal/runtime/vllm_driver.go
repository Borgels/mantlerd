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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/clawcontrol-agent/internal/types"
)

const (
	vllmConfigPath = "/etc/clawcontrol/vllm.json"
	vllmEnvPath    = "/etc/clawcontrol/vllm.env"
	vllmUnitPath   = "/etc/systemd/system/vllm.service"
	vllmVenvPath   = "/opt/clawcontrol/vllm-venv"
	vllmPythonPath = "/opt/clawcontrol/vllm-venv/bin/python3"
)

type vllmConfig struct {
	Model string `json:"model,omitempty"`
	Port  int    `json:"port,omitempty"`
}

type vllmDriver struct{}

func newVLLMDriver() Driver {
	return &vllmDriver{}
}

func (d *vllmDriver) Name() string { return "vllm" }

func (d *vllmDriver) Install() error {
	if err := d.ensureVirtualEnv(); err != nil {
		return err
	}
	if err := runCommand(vllmPythonPath, "-m", "pip", "install", "--upgrade", "pip"); err != nil {
		return err
	}
	if err := runCommand(vllmPythonPath, "-m", "pip", "install", "--upgrade", "vllm"); err != nil {
		return err
	}
	return d.ensureServiceUnit()
}

func (d *vllmDriver) IsInstalled() bool {
	if runCommand(vllmPythonPath, "-c", "import vllm") == nil {
		return true
	}
	// Backward compatibility for legacy installs outside the managed venv.
	return runCommand("python3", "-c", "import vllm") == nil
}

func (d *vllmDriver) Version() string {
	for _, pythonPath := range []string{vllmPythonPath, "python3"} {
		cmd := exec.Command(
			pythonPath,
			"-c",
			"import importlib.metadata as m; print(m.version('vllm'))",
		)
		output, err := cmd.Output()
		if err != nil {
			continue
		}
		version := strings.TrimSpace(string(output))
		if version != "" {
			return version
		}
	}
	return ""
}

func (d *vllmDriver) EnsureModelWithFlags(modelID string, _ *types.ModelFeatureFlags) error {
	if strings.TrimSpace(modelID) == "" {
		return fmt.Errorf("model ID is required")
	}
	if err := d.writeConfig(vllmConfig{Model: modelID, Port: 8000}); err != nil {
		return err
	}
	return d.startOrRestartService(modelID, 8000)
}

func (d *vllmDriver) ListModels() []string {
	set := map[string]struct{}{}
	cfg, err := d.readConfig()
	if err == nil && cfg.Model != "" {
		set[cfg.Model] = struct{}{}
	}

	req, err := http.NewRequest(http.MethodGet, d.baseURL()+"/v1/models", nil)
	if err == nil {
		resp, reqErr := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if reqErr == nil {
			defer resp.Body.Close()
			if resp.StatusCode < 400 {
				body, _ := io.ReadAll(resp.Body)
				var parsed struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if json.Unmarshal(body, &parsed) == nil {
					for _, item := range parsed.Data {
						if strings.TrimSpace(item.ID) != "" {
							set[item.ID] = struct{}{}
						}
					}
				}
			}
		}
	}

	models := make([]string, 0, len(set))
	for model := range set {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func (d *vllmDriver) HasModel(modelID string) bool {
	for _, model := range d.ListModels() {
		if model == modelID {
			return true
		}
	}
	return false
}

func (d *vllmDriver) RemoveModel(modelID string) error {
	cfg, err := d.readConfig()
	if err == nil && cfg.Model == modelID {
		if err := d.writeConfig(vllmConfig{Port: 8000}); err != nil {
			return err
		}
	}
	return runCommand("systemctl", "stop", "vllm")
}

func (d *vllmDriver) BenchmarkModel(
	modelID string,
	samplePromptTokens int,
	sampleOutputTokens int,
	concurrency int,
	runs int,
	onProgress func(BenchmarkProgress),
) (BenchmarkResult, error) {
	if strings.TrimSpace(modelID) == "" {
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

func (d *vllmDriver) benchmarkOnce(modelID string, prompt string, sampleOutputTokens int) (BenchmarkResult, error) {
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
		return BenchmarkResult{}, fmt.Errorf("encode vllm benchmark request: %w", err)
	}

	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, d.baseURL()+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("create vllm benchmark request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("vllm benchmark request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return BenchmarkResult{}, fmt.Errorf("vllm benchmark failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return BenchmarkResult{}, fmt.Errorf("decode vllm benchmark response: %w", err)
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

func (d *vllmDriver) RestartRuntime() error {
	if err := runCommand("systemctl", "restart", "vllm"); err == nil {
		return nil
	}
	return runCommand("systemctl", "restart", "clawcontrol-runtime")
}

func (d *vllmDriver) baseURL() string {
	cfg, err := d.readConfig()
	port := 8000
	if err == nil && cfg.Port > 0 {
		port = cfg.Port
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func (d *vllmDriver) ensureServiceUnit() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(vllmUnitPath), 0o755); err != nil {
		return fmt.Errorf("create vllm systemd directory: %w", err)
	}
	unit := `[Unit]
Description=vLLM OpenAI API Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-` + vllmEnvPath + `
ExecStart=/bin/sh -c '` + vllmPythonPath + ` -m vllm.entrypoints.openai.api_server --model "${VLLM_MODEL}" --host 0.0.0.0 --port "${VLLM_PORT:-8000}" --gpu-memory-utilization "${VLLM_GPU_MEMORY_UTILIZATION:-0.9}"'
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
`
	if err := os.WriteFile(vllmUnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write vllm service unit: %w", err)
	}
	return runCommand("systemctl", "daemon-reload")
}

func (d *vllmDriver) ensureVirtualEnv() error {
	if _, err := os.Stat(vllmPythonPath); err == nil {
		return nil
	}
	if err := os.MkdirAll(vllmVenvPath, 0o755); err != nil {
		return fmt.Errorf("create vllm virtualenv directory: %w", err)
	}
	if err := runCommand("python3", "-m", "venv", vllmVenvPath); err != nil {
		return fmt.Errorf("create vllm virtualenv: %w", err)
	}
	return nil
}

func (d *vllmDriver) startOrRestartService(modelID string, port int) error {
	if err := d.ensureServiceUnit(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(vllmEnvPath), 0o755); err != nil {
		return fmt.Errorf("create vllm env directory: %w", err)
	}
	envContent := fmt.Sprintf("VLLM_MODEL=%s\nVLLM_PORT=%d\nVLLM_GPU_MEMORY_UTILIZATION=0.9\n", modelID, port)
	if err := os.WriteFile(vllmEnvPath, []byte(envContent), 0o600); err != nil {
		return fmt.Errorf("write vllm env config: %w", err)
	}
	if err := runCommand("systemctl", "enable", "vllm"); err != nil {
		return err
	}
	return runCommand("systemctl", "restart", "vllm")
}

func (d *vllmDriver) readConfig() (vllmConfig, error) {
	raw, err := os.ReadFile(vllmConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return vllmConfig{}, nil
		}
		return vllmConfig{}, err
	}
	if len(raw) == 0 {
		return vllmConfig{}, nil
	}
	var cfg vllmConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return vllmConfig{}, err
	}
	return cfg, nil
}

func (d *vllmDriver) writeConfig(cfg vllmConfig) error {
	if cfg.Port <= 0 {
		cfg.Port = 8000
	}
	if err := os.MkdirAll(filepath.Dir(vllmConfigPath), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(vllmConfigPath, append(payload, '\n'), 0o600)
}
