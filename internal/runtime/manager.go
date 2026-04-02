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

type Manager struct{}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) InstallRuntime(runtimeName string) error {
	switch runtimeName {
	case "vllm":
		return m.run("python3", "-m", "pip", "install", "--upgrade", "vllm")
	case "ollama":
		return m.run("sh", "-c", "curl -fsSL https://ollama.com/install.sh | sh")
	default:
		return fmt.Errorf("unsupported runtime: %s", runtimeName)
	}
}

func (m *Manager) IsRuntimeInstalled(runtimeName string) bool {
	switch runtimeName {
	case "vllm":
		return m.run("python3", "-m", "pip", "show", "vllm") == nil
	case "ollama":
		return m.run("sh", "-c", "command -v ollama") == nil
	default:
		return false
	}
}

func (m *Manager) EnsureRuntime(runtimeName string) error {
	if m.IsRuntimeInstalled(runtimeName) {
		return nil
	}
	return m.InstallRuntime(runtimeName)
}

func (m *Manager) InstalledRuntimes() []string {
	runtimes := make([]string, 0, 2)
	if m.IsRuntimeInstalled("vllm") {
		runtimes = append(runtimes, "vllm")
	}
	if m.IsRuntimeInstalled("ollama") {
		runtimes = append(runtimes, "ollama")
	}
	return runtimes
}

func (m *Manager) RuntimeVersion(runtimeName string) string {
	switch runtimeName {
	case "ollama":
		cmd := exec.Command("ollama", "--version")
		output, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(output))
	case "vllm":
		cmd := exec.Command("python3", "-m", "pip", "show", "vllm")
		output, err := cmd.Output()
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(strings.ToLower(line), "version:") {
				return strings.TrimSpace(strings.TrimPrefix(line, "Version:"))
			}
		}
	}
	return ""
}

func (m *Manager) PullModel(modelID string) error {
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	return m.run("ollama", "pull", modelID)
}

func (m *Manager) ListModels() []string {
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

type BenchmarkResult struct {
	TTFTMs                      float64
	OutputTokensPerSec          float64
	TotalLatencyMs              float64
	PromptTokensPerSec          float64
	P95TTFTMsAtSmallConcurrency float64
}

type BenchmarkProgress struct {
	RunsCompleted   int
	RunsTotal       int
	SuccessfulRuns  int
	FailedRuns      int
	LastRunLatencyMs float64
	Benchmark       *BenchmarkResult
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

func (m *Manager) HasModel(modelID string) bool {
	for _, model := range m.ListModels() {
		if model == modelID {
			return true
		}
	}
	return false
}

func (m *Manager) BenchmarkModel(
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
	if !m.HasModel(modelID) {
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

			resp, err := m.benchmarkOnce(modelID, prompt, sampleOutputTokens)
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
	if len(results) == 0 {
		return BenchmarkResult{}, fmt.Errorf("benchmark produced no results")
	}

	final := summarizeBenchmarkResults(results)
	return final, nil
}

func summarizeBenchmarkResults(results []ollamaGenerateResponse) BenchmarkResult {
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
		TTFTMs:                      roundTo(sumTTFT / count, 2),
		OutputTokensPerSec:          roundTo(sumOutputTPS / count, 2),
		TotalLatencyMs:              roundTo(sumLatency / count, 2),
		PromptTokensPerSec:          roundTo(sumPromptTPS / count, 2),
		P95TTFTMsAtSmallConcurrency: roundTo(ttftValues[p95Index], 2),
	}
}

func (m *Manager) benchmarkOnce(modelID string, prompt string, sampleOutputTokens int) (ollamaGenerateResponse, error) {
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
	req, err := http.NewRequest(http.MethodPost, m.ollamaBaseURL()+"/api/generate", bytes.NewReader(raw))
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

func (m *Manager) ollamaBaseURL() string {
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

func makeBenchmarkPrompt(tokenCount int) string {
	if tokenCount < 32 {
		tokenCount = 32
	}
	var builder strings.Builder
	builder.WriteString("Summarize this synthetic benchmark context in concise bullet points.\n")
	for i := 0; i < tokenCount; i++ {
		builder.WriteString("token ")
	}
	return builder.String()
}

func roundTo(value float64, decimals int) float64 {
	pow := math.Pow10(decimals)
	return math.Round(value*pow) / pow
}

func (m *Manager) EnsureModel(modelID string) error {
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	if m.HasModel(modelID) {
		return nil
	}
	return m.PullModel(modelID)
}

func (m *Manager) EnsureModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	if err := m.EnsureModel(modelID); err != nil {
		return err
	}
	if flags == nil {
		return nil
	}
	return m.upsertModelFlags(modelID, *flags)
}

func (m *Manager) RemoveModel(modelID string) error {
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	return m.run("ollama", "rm", modelID)
}

func (m *Manager) RestartRuntime() error {
	// Best-effort; deployments can override service names.
	if err := m.run("systemctl", "restart", "ollama"); err == nil {
		return nil
	}
	return m.run("systemctl", "restart", "clawcontrol-runtime")
}

func (m *Manager) run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (m *Manager) modelFlagsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join("/var/lib/clawcontrol-agent", "model-flags.json")
	}
	return filepath.Join(home, ".config", "clawcontrol-agent", "model-flags.json")
}

func (m *Manager) upsertModelFlags(modelID string, flags types.ModelFeatureFlags) error {
	path := m.modelFlagsPath()
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
