package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

type ollamaDriver struct{}

func newOllamaDriver() Driver {
	return &ollamaDriver{}
}

func (d *ollamaDriver) Name() string { return "ollama" }

func (d *ollamaDriver) Install() error {
	return runCommand("sh", "-c", "curl -fsSL https://ollama.com/install.sh | sh")
}

func (d *ollamaDriver) Uninstall() error {
	_ = runCommand("systemctl", "stop", "ollama")
	_ = runCommand("systemctl", "disable", "ollama")
	_ = os.Remove("/etc/systemd/system/ollama.service")
	_ = runCommand("systemctl", "daemon-reload")
	for _, bin := range []string{"/usr/local/bin/ollama", "/usr/bin/ollama"} {
		_ = os.Remove(bin)
	}
	return nil
}

func (d *ollamaDriver) IsInstalled() bool {
	return runCommand("sh", "-c", "command -v ollama") == nil
}

func (d *ollamaDriver) IsReady() bool {
	if !d.IsInstalled() {
		return false
	}
	req, err := http.NewRequest(http.MethodGet, d.baseURL()+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (d *ollamaDriver) Version() string {
	cmd := exec.Command("ollama", "--version")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (d *ollamaDriver) PullModel(modelID string) error {
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	return runCommand("ollama", "pull", modelID)
}

func (d *ollamaDriver) ListModels() []string {
	cmd := exec.Command("ollama", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) <= 1 {
		return nil
	}

	models := make([]string, 0, len(lines)-1)
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		models = append(models, fields[0])
	}
	return models
}

func (d *ollamaDriver) HasModel(modelID string) bool {
	for _, model := range d.ListModels() {
		if model == modelID {
			return true
		}
	}
	return false
}

type ollamaGenerateRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Stream  bool           `json:"stream"`
	Options map[string]int `json:"options,omitempty"`
}

type ollamaGenerateResponse struct {
	TotalDuration      int64 `json:"total_duration"`
	LoadDuration       int64 `json:"load_duration"`
	PromptEvalCount    int64 `json:"prompt_eval_count"`
	PromptEvalDuration int64 `json:"prompt_eval_duration"`
	EvalCount          int64 `json:"eval_count"`
	EvalDuration       int64 `json:"eval_duration"`
}

func (d *ollamaDriver) BenchmarkModel(
	modelID string,
	samplePromptTokens int,
	sampleOutputTokens int,
	concurrency int,
	runs int,
	onProgress func(BenchmarkProgress),
) (BenchmarkResult, error) {
	if modelID == "" {
		return BenchmarkResult{}, fmt.Errorf("model ID is required")
	}
	if !d.HasModel(modelID) {
		return BenchmarkResult{}, fmt.Errorf("model not installed: %s", modelID)
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
	results := make([]ollamaGenerateResponse, 0, runs)
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

			resp, err := d.benchmarkOnce(modelID, prompt, sampleOutputTokens)
			var progress *BenchmarkProgress
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			} else {
				results = append(results, resp)
			}
			completedRuns := len(results) + len(errs)
			next := BenchmarkProgress{
				RunsCompleted:  completedRuns,
				RunsTotal:      runs,
				SuccessfulRuns: len(results),
				FailedRuns:     len(errs),
			}
			if err == nil {
				next.LastRunLatencyMs = roundTo(float64(resp.TotalDuration)/1_000_000.0, 2)
			}
			if len(results) > 0 {
				partial := summarizeOllamaBenchmarkResults(results)
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
	if len(results) == 0 {
		return BenchmarkResult{}, fmt.Errorf("benchmark produced no results")
	}

	final := summarizeOllamaBenchmarkResults(results)
	return final, nil
}

func summarizeOllamaBenchmarkResults(results []ollamaGenerateResponse) BenchmarkResult {
	ttftValues := make([]float64, 0, len(results))
	var sumTTFT, sumOutputTPS, sumPromptTPS, sumLatency float64
	for _, item := range results {
		ttftMs := float64(item.LoadDuration+item.PromptEvalDuration) / 1_000_000.0
		if ttftMs <= 0 {
			ttftMs = float64(item.TotalDuration) / 1_000_000.0
		}
		ttftValues = append(ttftValues, ttftMs)
		sumTTFT += ttftMs
		sumLatency += float64(item.TotalDuration) / 1_000_000.0

		promptSeconds := float64(item.PromptEvalDuration) / 1_000_000_000.0
		if promptSeconds > 0 {
			sumPromptTPS += float64(item.PromptEvalCount) / promptSeconds
		}

		outputSeconds := float64(item.EvalDuration) / 1_000_000_000.0
		if outputSeconds > 0 {
			sumOutputTPS += float64(item.EvalCount) / outputSeconds
		}
	}

	sort.Float64s(ttftValues)
	p95Index := int(math.Ceil(float64(len(ttftValues))*0.95)) - 1
	if p95Index < 0 {
		p95Index = 0
	}
	if p95Index >= len(ttftValues) {
		p95Index = len(ttftValues) - 1
	}
	count := float64(len(results))
	return BenchmarkResult{
		TTFTMs:                      roundTo(sumTTFT/count, 2),
		OutputTokensPerSec:          roundTo(sumOutputTPS/count, 2),
		TotalLatencyMs:              roundTo(sumLatency/count, 2),
		PromptTokensPerSec:          roundTo(sumPromptTPS/count, 2),
		P95TTFTMsAtSmallConcurrency: roundTo(ttftValues[p95Index], 2),
	}
}

func (d *ollamaDriver) benchmarkOnce(modelID string, prompt string, sampleOutputTokens int) (ollamaGenerateResponse, error) {
	requestBody := ollamaGenerateRequest{
		Model:  modelID,
		Prompt: prompt,
		Stream: false,
		Options: map[string]int{
			"num_predict": sampleOutputTokens,
		},
	}
	raw, err := json.Marshal(requestBody)
	if err != nil {
		return ollamaGenerateResponse{}, fmt.Errorf("encode benchmark request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, d.baseURL()+"/api/generate", bytes.NewReader(raw))
	if err != nil {
		return ollamaGenerateResponse{}, fmt.Errorf("create benchmark request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ollamaGenerateResponse{}, fmt.Errorf("benchmark request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return ollamaGenerateResponse{}, fmt.Errorf("ollama benchmark failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed ollamaGenerateResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ollamaGenerateResponse{}, fmt.Errorf("decode benchmark response: %w", err)
	}
	return parsed, nil
}

func (d *ollamaDriver) baseURL() string {
	base := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	if strings.Contains(base, "://0.0.0.0") {
		base = strings.Replace(base, "://0.0.0.0", "://127.0.0.1", 1)
	}
	return strings.TrimRight(base, "/")
}

func (d *ollamaDriver) PrepareModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	if !d.HasModel(modelID) {
		if err := d.PullModel(modelID); err != nil {
			return err
		}
	}
	if flags == nil {
		return nil
	}
	return d.upsertModelFlags(modelID, *flags)
}

func (d *ollamaDriver) StartModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	return d.PrepareModelWithFlags(modelID, flags)
}

func (d *ollamaDriver) StopModel(modelID string) error {
	return nil
}

func (d *ollamaDriver) EnsureModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	return d.StartModelWithFlags(modelID, flags)
}

func (d *ollamaDriver) RemoveModel(modelID string) error {
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	cmd := exec.Command("ollama", "rm", modelID)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	text := strings.ToLower(strings.TrimSpace(string(output)))
	if strings.Contains(text, "not found") || strings.Contains(text, "does not exist") {
		// Idempotent remove: treat already-missing models as removed.
		return nil
	}
	return fmt.Errorf("remove ollama model %q: %w (%s)", modelID, err, strings.TrimSpace(string(output)))
}

func (d *ollamaDriver) RestartRuntime() error {
	if err := runCommand("systemctl", "restart", "ollama"); err == nil {
		return nil
	}
	return runCommand("systemctl", "restart", "mantler-runtime")
}

func (d *ollamaDriver) modelFlagsPath() string {
	// Service-safe path: avoid relying on $HOME when systemd hardening
	// restricts /root access (e.g. ProtectHome=true).
	return filepath.Join("/etc", "mantler", "model-flags.json")
}

func (d *ollamaDriver) upsertModelFlags(modelID string, flags types.ModelFeatureFlags) error {
	path := d.modelFlagsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create model flags directory: %w", err)
	}

	current := map[string]types.ModelFeatureFlags{}
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &current)
	}

	if existing, ok := current[modelID]; ok && existing == flags {
		return nil
	}
	current[modelID] = flags

	payload, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Errorf("encode model flags: %w", err)
	}
	if err := os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
		return fmt.Errorf("write model flags: %w", err)
	}
	return nil
}
