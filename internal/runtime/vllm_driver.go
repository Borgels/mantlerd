package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

const (
	vllmConfigPath            = "/etc/mantler/vllm.json"
	vllmEnvPath               = "/etc/mantler/vllm.env"
	vllmDockerEnvPath         = "/etc/mantler/vllm-docker.env"
	vllmUnitPath              = "/etc/systemd/system/vllm.service"
	vllmVenvPath              = "/opt/mantler/vllm-venv"
	vllmPythonPath            = "/opt/mantler/vllm-venv/bin/python3"
	vllmContainerName         = "mantler-vllm"
	vllmDefaultContainerImage = "nvcr.io/nvidia/vllm:26.02-py3"
	vllmReadyTimeout          = 15 * time.Minute
	vllmRapidFailureWindow    = 45 * time.Second
	vllmStartupGraceWindow    = 20 * time.Minute
	vllmRestartCooldown       = 90 * time.Second
	nemotronSuper120BNVFP4    = "nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4"
	vllmMaxHTTPBodyBytes      = 1 << 20
)

type vllmConfig struct {
	Model string `json:"model,omitempty"`
	Port  int    `json:"port,omitempty"`
}

type vllmDriver struct{}

var (
	vllmRestartMu     sync.Mutex
	lastVLLMRestartAt time.Time
)

func newVLLMDriver() Driver {
	return &vllmDriver{}
}

func (d *vllmDriver) Name() string { return "vllm" }

func (d *vllmDriver) Install() error {
	if d.shouldUseContainer() {
		if err := d.ensureDockerAvailable(); err != nil {
			return err
		}
		if err := d.ensureServiceUnit(); err != nil {
			return err
		}
		image := d.containerImage()
		if err := d.pullContainerImage(image); err != nil {
			return err
		}
		cfg, err := d.readConfig()
		if err != nil {
			return nil
		}
		if strings.TrimSpace(cfg.Model) == "" {
			return nil
		}
		port := cfg.Port
		if port <= 0 {
			port = 8000
		}
		if err := d.startOrRestartService(cfg.Model, port, false); err != nil {
			return fmt.Errorf("vllm container install completed but configured model failed to start: %w", err)
		}
		return nil
	}
	if err := d.ensureVirtualEnv(); err != nil {
		return err
	}
	if err := runCommand(vllmPythonPath, "-m", "pip", "install", "--upgrade", "pip"); err != nil {
		return err
	}
	if err := runCommand(vllmPythonPath, "-m", "pip", "install", "--upgrade", "vllm"); err != nil {
		// Best-effort recovery: recreate the managed venv and retry once.
		_ = os.RemoveAll(vllmVenvPath)
		if retryErr := d.ensureVirtualEnv(); retryErr != nil {
			return fmt.Errorf("install vllm failed and venv recovery failed: %w", retryErr)
		}
		if retryErr := runCommand(vllmPythonPath, "-m", "pip", "install", "--upgrade", "pip"); retryErr != nil {
			return retryErr
		}
		if retryErr := runCommand(vllmPythonPath, "-m", "pip", "install", "--upgrade", "vllm"); retryErr != nil {
			return retryErr
		}
	}
	_ = d.ensureCudaRuntimeLibraries()
	if err := d.validateVLLMRuntimeCompatibility(); err != nil {
		return err
	}
	if err := d.ensureServiceUnit(); err != nil {
		return err
	}

	// If a model is already configured, treat runtime install as an end-to-end
	// recovery action and verify the API is actually serving afterwards.
	cfg, err := d.readConfig()
	if err != nil {
		return nil
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil
	}
	port := cfg.Port
	if port <= 0 {
		port = 8000
	}
	if err := d.startOrRestartService(cfg.Model, port, false); err != nil {
		return fmt.Errorf("vllm install completed but configured model failed to start: %w", err)
	}
	return nil
}

func (d *vllmDriver) Uninstall() error {
	_ = runCommand("systemctl", "stop", "vllm")
	_ = runCommand("systemctl", "disable", "vllm")
	_ = os.Remove(vllmUnitPath)
	_ = runCommand("systemctl", "daemon-reload")
	if d.shouldUseContainer() {
		_ = exec.Command("docker", "rm", "-f", vllmContainerName).Run()
		image := d.containerImage()
		if image != "" {
			_ = exec.Command("docker", "rmi", image).Run()
		}
	} else {
		_ = os.RemoveAll(vllmVenvPath)
	}
	_ = os.Remove(vllmConfigPath)
	_ = os.Remove(vllmEnvPath)
	return nil
}

func (d *vllmDriver) IsInstalled() bool {
	hasServiceUnit := fileExists(vllmUnitPath)
	hasConfig := fileExists(vllmConfigPath)
	hasEnv := fileExists(vllmEnvPath)
	if d.shouldUseContainer() {
		return inferVLLMInstalled(false, hasServiceUnit, hasConfig, hasEnv)
	}
	hasNativeImport := runCommand(vllmPythonPath, "-c", "import vllm") == nil ||
		// Backward compatibility for legacy installs outside the managed venv.
		runCommand("python3", "-c", "import vllm") == nil
	return inferVLLMInstalled(hasNativeImport, hasServiceUnit, hasConfig, hasEnv)
}

func (d *vllmDriver) IsReady() bool {
	if !d.IsInstalled() {
		return false
	}
	if configuredModel, known := d.configuredModelState(); known {
		if strings.TrimSpace(configuredModel) == "" {
			// Installed runtime with no configured model is considered idle-ready.
			_ = runCommand("systemctl", "stop", "vllm")
			_ = runCommand("systemctl", "reset-failed", "vllm")
			return true
		}
		if incompatibility := d.knownModelImageIncompatibility(configuredModel, d.effectiveContainerImage()); incompatibility != "" {
			// Self-heal known terminal startup mismatch into idle-ready state.
			d.disarmConfiguredModel()
			d.stopVLLMServiceForKnownIncompatibility()
			return true
		}
	}
	if _, known := d.configuredModelState(); !known && d.serviceIsInactive() {
		// Non-root CLI may not be able to read /etc/mantler files; treat
		// inactive service as idle-ready in that restricted visibility mode.
		return true
	}
	_, err := d.fetchRemoteModels()
	return err == nil
}

func (d *vllmDriver) Version() string {
	if !d.IsInstalled() {
		return ""
	}
	if d.shouldUseContainer() {
		return "container:" + d.containerImage()
	}
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

func (d *vllmDriver) RuntimeConfig() map[string]any {
	config := map[string]any{
		"version":        d.Version(),
		"runtimeMode":    d.runtimeMode(),
		"containerImage": d.effectiveContainerImage(),
	}

	env := d.readEnvConfigMap()
	if gpuMemoryUtilization := strings.TrimSpace(env["VLLM_GPU_MEMORY_UTILIZATION"]); gpuMemoryUtilization != "" {
		if parsed, err := strconv.ParseFloat(gpuMemoryUtilization, 64); err == nil {
			config["gpuMemoryUtilization"] = parsed
		} else {
			config["gpuMemoryUtilization"] = gpuMemoryUtilization
		}
	}
	if extraArgs := strings.TrimSpace(env["VLLM_EXTRA_ARGS"]); extraArgs != "" {
		config["extraArgs"] = extraArgs
	}

	return config
}

func (d *vllmDriver) PrepareModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	return d.PrepareModelWithFlagsCtx(context.Background(), modelID, flags)
}

// PrepareModelWithFlagsCtx downloads model weights with cancellation support.
func (d *vllmDriver) PrepareModelWithFlagsCtx(ctx context.Context, modelID string, _ *types.ModelFeatureFlags) error {
	trimmedModel := strings.TrimSpace(modelID)
	if trimmedModel == "" {
		return fmt.Errorf("model ID is required")
	}
	if err := d.downloadModelSnapshotCtx(ctx, trimmedModel); err != nil {
		return err
	}
	return d.markPreparedModel(trimmedModel)
}

func (d *vllmDriver) StartModelWithFlags(modelID string, _ *types.ModelFeatureFlags) error {
	trimmedModel := strings.TrimSpace(modelID)
	if trimmedModel == "" {
		return fmt.Errorf("model ID is required")
	}
	if err := d.PrepareModelWithFlags(trimmedModel, nil); err != nil {
		return err
	}
	containerImage := d.effectiveContainerImage()
	if incompatibility := d.knownModelImageIncompatibility(trimmedModel, containerImage); incompatibility != "" {
		d.disarmConfiguredModel()
		d.stopVLLMServiceForKnownIncompatibility()
		return fmt.Errorf(incompatibility)
	}
	if err := d.writeConfig(vllmConfig{Model: trimmedModel, Port: 8000}); err != nil {
		return err
	}
	return d.startOrRestartService(trimmedModel, 8000, false)
}

func (d *vllmDriver) StopModel(modelID string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	if configuredModel, known := d.configuredModelState(); known && strings.EqualFold(strings.TrimSpace(configuredModel), modelID) {
		d.disarmConfiguredModel()
	}
	_ = runCommand("systemctl", "stop", "vllm")
	_ = runCommand("systemctl", "reset-failed", "vllm")
	return nil
}

func (d *vllmDriver) EnsureModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	trimmedModel := strings.TrimSpace(modelID)
	if trimmedModel == "" {
		return fmt.Errorf("model ID is required")
	}
	remoteModels, err := d.fetchRemoteModels()
	if err == nil {
		for _, remoteModel := range remoteModels {
			if strings.EqualFold(strings.TrimSpace(remoteModel), trimmedModel) {
				return nil
			}
		}
	}
	return d.StartModelWithFlags(trimmedModel, flags)
}

func (d *vllmDriver) ListModels() []string {
	set := map[string]struct{}{}
	for _, model := range d.preparedModels() {
		if strings.TrimSpace(model) != "" {
			set[model] = struct{}{}
		}
	}
	remoteModels, _ := d.fetchRemoteModels()
	for _, model := range remoteModels {
		if strings.TrimSpace(model) != "" {
			set[model] = struct{}{}
		}
	}

	models := make([]string, 0, len(set))
	for model := range set {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func (d *vllmDriver) InstalledModels() []types.InstalledModel {
	models := make([]types.InstalledModel, 0)
	seen := map[string]struct{}{}
	indexByModelID := map[string]int{}
	addModel := func(modelID string, status types.ModelInstallStatus, failReason string) {
		trimmed := strings.TrimSpace(modelID)
		if trimmed == "" {
			return
		}
		if _, exists := seen[trimmed]; exists {
			return
		}
		seen[trimmed] = struct{}{}
		indexByModelID[trimmed] = len(models)
		models = append(models, types.InstalledModel{
			ModelID:    trimmed,
			Runtime:    types.RuntimeVLLM,
			Status:     status,
			FailReason: failReason,
		})
	}

	for _, prepared := range d.preparedModels() {
		addModel(prepared, types.ModelDownloaded, "")
	}

	cfg, _ := d.readConfig()
	configuredModel := strings.TrimSpace(cfg.Model)
	remoteModels, err := d.fetchRemoteModels()
	if err == nil {
		for _, modelID := range remoteModels {
			trimmed := strings.TrimSpace(modelID)
			if trimmed == "" {
				continue
			}
			if idx, ok := indexByModelID[trimmed]; ok {
				models[idx].Status = types.ModelReady
				models[idx].FailReason = ""
				continue
			}
			addModel(trimmed, types.ModelReady, "")
		}
		if configuredModel != "" {
			if _, ok := seen[configuredModel]; !ok {
				addModel(configuredModel, types.ModelStarting, "")
			}
		}
		return models
	}

	if configuredModel != "" {
		status := types.ModelFailed
		failReason := ""
		if d.isLikelyServiceWarmup(err) {
			status = types.ModelStarting
		}
		if status == types.ModelFailed && serviceLikelyOutOfMemory("vllm", err) {
			failReason = modelFailReasonInsufficientMemory
		}
		if _, ok := seen[configuredModel]; ok {
			for i := range models {
				if models[i].ModelID == configuredModel {
					models[i].Status = status
					models[i].FailReason = failReason
					break
				}
			}
		} else {
			addModel(configuredModel, status, failReason)
		}
	}
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
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	_ = d.StopModel(modelID)
	_ = d.unmarkPreparedModel(modelID)
	return nil
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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, vllmMaxHTTPBodyBytes))
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

func (d *vllmDriver) CompletePrompt(
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
		return PromptCompletionResult{}, fmt.Errorf("encode vllm completion request: %w", err)
	}

	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, d.baseURL()+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return PromptCompletionResult{}, fmt.Errorf("create vllm completion request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return PromptCompletionResult{}, fmt.Errorf("vllm completion request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, vllmMaxHTTPBodyBytes))
	if resp.StatusCode >= 400 {
		return PromptCompletionResult{}, fmt.Errorf("vllm completion failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
			PromptTokens     int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return PromptCompletionResult{}, fmt.Errorf("decode vllm completion response: %w", err)
	}
	elapsedMs := float64(time.Since(start).Milliseconds())
	output := ""
	if len(parsed.Choices) > 0 {
		output = strings.TrimSpace(parsed.Choices[0].Message.Content)
	}
	outputTokens := parsed.Usage.CompletionTokens
	tokensPerSec := 0.0
	if elapsedMs > 0 && outputTokens > 0 {
		tokensPerSec = float64(outputTokens) / (elapsedMs / 1000.0)
	}
	return PromptCompletionResult{
		Output:       output,
		LatencyMs:    elapsedMs,
		TTFTMs:       elapsedMs,
		TokensPerSec: roundTo(tokensPerSec, 2),
		OutputTokens: outputTokens,
	}, nil
}

func (d *vllmDriver) RestartRuntime() error {
	cfg, cfgErr := d.readConfig()
	if cfgErr == nil {
		if strings.TrimSpace(cfg.Model) != "" {
			if incompatibility := d.knownModelImageIncompatibility(cfg.Model, d.effectiveContainerImage()); incompatibility != "" {
				d.disarmConfiguredModel()
				d.stopVLLMServiceForKnownIncompatibility()
				_ = runCommand("systemctl", "reset-failed", "vllm")
				return nil
			}
			port := cfg.Port
			if port <= 0 {
				port = 8000
			}
			return d.startOrRestartService(cfg.Model, port, true)
		}
		// No configured model: keep runtime idle instead of crash-looping a blank service.
		_ = runCommand("systemctl", "stop", "vllm")
		_ = runCommand("systemctl", "reset-failed", "vllm")
		return nil
	}
	if os.IsNotExist(cfgErr) {
		_ = runCommand("systemctl", "stop", "vllm")
		_ = runCommand("systemctl", "reset-failed", "vllm")
		return nil
	}
	if err := runCommand("systemctl", "restart", "vllm"); err == nil {
		return nil
	}
	return runCommand("systemctl", "restart", "mantler-runtime")
}

func (d *vllmDriver) baseURL() string {
	cfg, err := d.readConfig()
	port := 8000
	if err == nil && cfg.Port > 0 {
		port = cfg.Port
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func (d *vllmDriver) configuredModelState() (string, bool) {
	cfg, err := d.readConfig()
	if err == nil {
		return strings.TrimSpace(cfg.Model), true
	}
	values := d.readEnvConfigMap()
	if envModel := strings.TrimSpace(values["VLLM_MODEL"]); envModel != "" {
		return envModel, true
	}
	if os.IsNotExist(err) {
		return "", true
	}
	return "", false
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
ExecStart=/bin/sh -c 'LD_LIBRARY_PATH="${VLLM_LD_LIBRARY_PATH:-}${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}"; exec ` + vllmPythonPath + ` -m vllm.entrypoints.openai.api_server --model "${VLLM_MODEL}" --host 0.0.0.0 --port "${VLLM_PORT:-8000}" --gpu-memory-utilization "${VLLM_GPU_MEMORY_UTILIZATION:-0.9}" ${VLLM_EXTRA_ARGS:-}'
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
`
	if d.shouldUseContainer() {
		unit = `[Unit]
Description=vLLM OpenAI API Server (Container)
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-` + vllmEnvPath + `
ExecStartPre=-/bin/sh -c 'DOCKER_BIN="$(command -v docker)"; if [ -n "$DOCKER_BIN" ]; then "$DOCKER_BIN" rm -f ` + vllmContainerName + ` >/dev/null 2>&1 || true; fi'
ExecStartPre=-/bin/sh -c 'mkdir -p /opt/mantler/vllm-app'
ExecStartPre=-/bin/sh -c 'mkdir -p ` + filepath.Dir(vllmDockerEnvPath) + ` && touch ` + vllmDockerEnvPath + `'
ExecStart=/bin/sh -c 'DOCKER_BIN="$(command -v docker)"; [ -n "$DOCKER_BIN" ] || exit 1; GPU_FLAGS="--gpus all"; if ! "$DOCKER_BIN" info --format "{{json .Runtimes}}" 2>/dev/null | tr -d " " | tr -d "\n" | grep -q "\"nvidia\":"; then if [ -f /etc/cdi/nvidia.yaml ] || [ -f /var/run/cdi/nvidia.yaml ] || [ -f /etc/cdi/nvidia.json ] || [ -f /var/run/cdi/nvidia.json ]; then GPU_FLAGS="--device nvidia.com/gpu=all"; fi; fi; exec "$DOCKER_BIN" run --rm --name ` + vllmContainerName + ` $$GPU_FLAGS --ipc=host --network host --env-file ` + vllmDockerEnvPath + ` -e HF_TOKEN="${HF_TOKEN:-}" -e HUGGING_FACE_HUB_TOKEN="${HUGGING_FACE_HUB_TOKEN:-}" -e NVIDIA_VISIBLE_DEVICES=all -e NVIDIA_DRIVER_CAPABILITIES=compute,utility -v /root/.cache/huggingface:/root/.cache/huggingface -v /opt/mantler/vllm-app:/app --entrypoint python3 "${VLLM_CONTAINER_IMAGE:-` + vllmDefaultContainerImage + `}" -m vllm.entrypoints.openai.api_server --model "${VLLM_MODEL}" --host 0.0.0.0 --port "${VLLM_PORT:-8000}" --gpu-memory-utilization "${VLLM_GPU_MEMORY_UTILIZATION:-0.9}" ${VLLM_EXTRA_ARGS:-}'
ExecStop=-/bin/sh -c 'DOCKER_BIN="$(command -v docker)"; if [ -n "$DOCKER_BIN" ]; then "$DOCKER_BIN" stop ` + vllmContainerName + ` >/dev/null 2>&1 || true; fi'
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
`
	}
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
		// Debian/Ubuntu often needs python3-venv explicitly installed.
		if os.Geteuid() == 0 {
			_ = runCommand(
				"sh",
				"-c",
				"DEBIAN_FRONTEND=noninteractive apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y python3-venv python3-pip",
			)
			if retryErr := runCommand("python3", "-m", "venv", vllmVenvPath); retryErr == nil {
				return nil
			}
		}
		return fmt.Errorf("create vllm virtualenv: %w", err)
	}
	return nil
}

func (d *vllmDriver) startOrRestartService(modelID string, port int, force bool) error {
	if d.shouldUseContainer() {
		if err := d.ensureDockerAvailable(); err != nil {
			return err
		}
	} else {
		if err := d.validateVLLMRuntimeCompatibility(); err != nil {
			return err
		}
	}
	if err := d.ensureServiceUnit(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(vllmEnvPath), 0o755); err != nil {
		return fmt.Errorf("create vllm env directory: %w", err)
	}
	safeModelID := strings.ReplaceAll(strings.TrimSpace(modelID), "\n", " ")
	libraryPath := d.detectVLLMLibraryPath()
	existingEnv := d.readEnvConfigMap()
	trustRemoteCode := strings.TrimSpace(existingEnv["VLLM_TRUST_REMOTE_CODE"])
	if trustRemoteCode == "" {
		trustRemoteCode = "false"
	}
	hfToken := strings.TrimSpace(existingEnv["HF_TOKEN"])
	if hfToken == "" {
		hfToken = strings.TrimSpace(os.Getenv("HF_TOKEN"))
	}
	hfHubToken := strings.TrimSpace(existingEnv["HUGGING_FACE_HUB_TOKEN"])
	if hfHubToken == "" {
		hfHubToken = strings.TrimSpace(os.Getenv("HUGGING_FACE_HUB_TOKEN"))
	}
	if hfHubToken == "" {
		// Keep both env var names aligned for runtimes that only read one.
		hfHubToken = hfToken
	}
	if hfToken == "" {
		hfToken = hfHubToken
	}
	extraArgs := strings.TrimSpace(existingEnv["VLLM_EXTRA_ARGS"])
	if strings.EqualFold(strings.TrimSpace(trustRemoteCode), "true") &&
		!strings.Contains(strings.ToLower(extraArgs), "--trust-remote-code") {
		if extraArgs == "" {
			extraArgs = "--trust-remote-code"
		} else {
			extraArgs = strings.TrimSpace(extraArgs + " --trust-remote-code")
		}
	}
	if existingLibrary := strings.TrimSpace(existingEnv["VLLM_LD_LIBRARY_PATH"]); existingLibrary != "" {
		libraryPath = existingLibrary
	}
	containerImage := strings.TrimSpace(existingEnv["VLLM_CONTAINER_IMAGE"])
	if containerImage == "" {
		containerImage = d.containerImage()
	}
	if incompatibility := d.knownModelImageIncompatibility(safeModelID, containerImage); incompatibility != "" {
		d.disarmConfiguredModel()
		d.stopVLLMServiceForKnownIncompatibility()
		return fmt.Errorf(incompatibility)
	}
	if d.shouldUseContainer() {
		// Runtime control actions should not force image upgrades implicitly.
		// Pull only when the configured image is missing locally.
		if !d.containerImageExists(containerImage) {
			if err := d.pullContainerImage(containerImage); err != nil {
				return err
			}
		}
	}
	runtimeMode := d.runtimeMode()
	values := d.readEnvConfigMap()
	values["VLLM_MODEL"] = safeModelID
	values["VLLM_PORT"] = strconv.Itoa(port)
	values["VLLM_GPU_MEMORY_UTILIZATION"] = "0.9"
	values["VLLM_TRUST_REMOTE_CODE"] = trustRemoteCode
	values["VLLM_EXTRA_ARGS"] = extraArgs
	values["VLLM_RUNTIME_MODE"] = runtimeMode
	if hfToken != "" {
		values["HF_TOKEN"] = hfToken
	}
	if hfHubToken != "" {
		values["HUGGING_FACE_HUB_TOKEN"] = hfHubToken
	}
	if d.shouldUseContainer() {
		values["VLLM_CONTAINER_IMAGE"] = containerImage
		delete(values, "VLLM_LD_LIBRARY_PATH")
	} else {
		values["VLLM_LD_LIBRARY_PATH"] = libraryPath
		delete(values, "VLLM_CONTAINER_IMAGE")
	}
	if err := d.writeEnvConfigMap(values); err != nil {
		return fmt.Errorf("write vllm env config: %w", err)
	}
	if d.shouldUseContainer() {
		if err := d.writeDockerEnvConfig(values); err != nil {
			return fmt.Errorf("write vllm docker env config: %w", err)
		}
	}
	if err := runCommand("systemctl", "enable", "vllm"); err != nil {
		return err
	}
	if !force {
		if throttled, remaining := throttleVLLMRestart(); throttled {
			// Avoid restart storms when the endpoint is repeatedly unavailable.
			if err := d.waitForAPIReady(6 * time.Second); err == nil {
				return nil
			}
			diagnostics := d.vllmDiagnosticsTail()
			if diagnostics == "" {
				return fmt.Errorf("skipping vllm restart due to cooldown (%s remaining) and API is still not reachable", remaining.Round(time.Second))
			}
			return fmt.Errorf("skipping vllm restart due to cooldown (%s remaining) and API is still not reachable; %s", remaining.Round(time.Second), diagnostics)
		}
	}
	markVLLMRestart()
	if err := runCommand("systemctl", "restart", "vllm"); err != nil {
		return err
	}
	if err := d.waitForAPIReady(vllmRapidFailureWindow); err != nil {
		diagnostics := d.vllmDiagnosticsTail()
		if d.shouldAutoEnableTrustRemoteCode(diagnostics) {
			if trustErr := d.enableTrustRemoteCodeInEnv(); trustErr == nil {
				if restartErr := runCommand("systemctl", "restart", "vllm"); restartErr == nil {
					if readyErr := d.waitForAPIReady(vllmReadyTimeout); readyErr == nil {
						return nil
					}
					diagnostics = d.vllmDiagnosticsTail()
				} else {
					diagnostics = strings.TrimSpace(strings.Join([]string{
						diagnostics,
						"trust-remote-code auto-restart failed: " + restartErr.Error(),
					}, " | "))
				}
			} else {
				diagnostics = strings.TrimSpace(strings.Join([]string{
					diagnostics,
					"failed to persist VLLM_TRUST_REMOTE_CODE=true: " + trustErr.Error(),
				}, " | "))
			}
		} else {
			remaining := vllmReadyTimeout - vllmRapidFailureWindow
			if remaining < 10*time.Second {
				remaining = 10 * time.Second
			}
			if readyErr := d.waitForAPIReady(remaining); readyErr == nil {
				return nil
			}
			diagnostics = d.vllmDiagnosticsTail()
		}
		if hint := d.classifyKnownStartupFailure(diagnostics); hint != "" {
			return fmt.Errorf("vllm service restarted but API not reachable yet: %w; %s; %s", err, diagnostics, hint)
		}
		if diagnostics == "" {
			return fmt.Errorf("vllm service restarted but API not reachable yet: %w", err)
		}
		return fmt.Errorf("vllm service restarted but API not reachable yet: %w; %s", err, diagnostics)
	}
	isExternal, listenErr := isServiceListeningOnNonLoopback(port)
	if listenErr != nil {
		return listenErr
	}
	if !isExternal {
		return fmt.Errorf("vllm server is only listening on localhost; expected 0.0.0.0 or non-loopback interface")
	}
	return nil
}

func (d *vllmDriver) effectiveContainerImage() string {
	existingEnv := d.readEnvConfigMap()
	containerImage := strings.TrimSpace(existingEnv["VLLM_CONTAINER_IMAGE"])
	if containerImage == "" {
		containerImage = d.containerImage()
	}
	return containerImage
}

func (d *vllmDriver) knownModelImageIncompatibility(modelID string, containerImage string) string {
	trimmedModel := strings.TrimSpace(modelID)
	trimmedImage := strings.TrimSpace(containerImage)
	if strings.EqualFold(trimmedModel, nemotronSuper120BNVFP4) &&
		strings.EqualFold(trimmedImage, vllmDefaultContainerImage) {
		return "known incompatibility: nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4 cannot start on nvcr.io/nvidia/vllm:26.02-py3 (vLLM 0.15.1, ModelOpt MIXED_PRECISION unsupported); use a newer compatible vLLM container or select another model"
	}
	return ""
}

func (d *vllmDriver) stopVLLMServiceForKnownIncompatibility() {
	if err := runCommand("systemctl", "stop", "vllm"); err != nil {
		return
	}
	_ = runCommand("systemctl", "reset-failed", "vllm")
}

func (d *vllmDriver) serviceIsInactive() bool {
	out, err := exec.Command("systemctl", "is-active", "vllm").CombinedOutput()
	if err != nil && len(out) == 0 {
		return false
	}
	return strings.TrimSpace(string(out)) == "inactive"
}

func (d *vllmDriver) disarmConfiguredModel() {
	port := 8000
	if cfg, err := d.readConfig(); err == nil && cfg.Port > 0 {
		port = cfg.Port
	}
	values := d.readEnvConfigMap()
	if envPort := strings.TrimSpace(values["VLLM_PORT"]); envPort != "" {
		if parsedPort, convErr := strconv.Atoi(envPort); convErr == nil && parsedPort > 0 {
			port = parsedPort
		}
	}
	if port <= 0 {
		port = 8000
	}
	_ = d.writeConfig(vllmConfig{Port: port})

	// Clear model from env as well; service ExecStart reads VLLM_MODEL here.
	delete(values, "VLLM_MODEL")
	if _, ok := values["VLLM_PORT"]; !ok {
		values["VLLM_PORT"] = fmt.Sprintf("%d", port)
	}
	order := []string{
		"VLLM_MODEL",
		"VLLM_PORT",
		"VLLM_GPU_MEMORY_UTILIZATION",
		"VLLM_TRUST_REMOTE_CODE",
		"VLLM_EXTRA_ARGS",
		"VLLM_RUNTIME_MODE",
		"HF_TOKEN",
		"HUGGING_FACE_HUB_TOKEN",
		"VLLM_CONTAINER_IMAGE",
		"VLLM_LD_LIBRARY_PATH",
	}
	lines := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, key := range order {
		if value, ok := values[key]; ok {
			lines = append(lines, fmt.Sprintf("%s=%q", key, value))
			seen[key] = struct{}{}
		}
	}
	for key, value := range values {
		if _, ok := seen[key]; ok {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s=%q", key, value))
	}
	payload := strings.Join(lines, "\n")
	if payload != "" {
		payload += "\n"
	}
	_ = os.WriteFile(vllmEnvPath, []byte(payload), 0o600)
}

func (d *vllmDriver) shouldAutoEnableTrustRemoteCode(diagnostics string) bool {
	text := strings.ToLower(diagnostics)
	if strings.Contains(text, "modelopt currently only supports") {
		return false
	}
	return strings.Contains(text, "trust_remote_code") ||
		strings.Contains(text, "trust-remote-code") ||
		strings.Contains(text, "please pass the argument `trust_remote_code=true`") ||
		strings.Contains(text, "contains custom code which must be executed") ||
		strings.Contains(text, "allow custom code to be run")
}

func (d *vllmDriver) enableTrustRemoteCodeInEnv() error {
	values := d.readEnvConfigMap()
	values["VLLM_TRUST_REMOTE_CODE"] = "true"
	extraArgs := strings.TrimSpace(values["VLLM_EXTRA_ARGS"])
	if !strings.Contains(strings.ToLower(extraArgs), "--trust-remote-code") {
		if extraArgs == "" {
			values["VLLM_EXTRA_ARGS"] = "--trust-remote-code"
		} else {
			values["VLLM_EXTRA_ARGS"] = strings.TrimSpace(extraArgs + " --trust-remote-code")
		}
	}

	order := []string{
		"VLLM_MODEL",
		"VLLM_PORT",
		"VLLM_GPU_MEMORY_UTILIZATION",
		"VLLM_TRUST_REMOTE_CODE",
		"VLLM_EXTRA_ARGS",
		"VLLM_RUNTIME_MODE",
		"HF_TOKEN",
		"HUGGING_FACE_HUB_TOKEN",
		"VLLM_CONTAINER_IMAGE",
		"VLLM_LD_LIBRARY_PATH",
	}
	lines := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, key := range order {
		if value, ok := values[key]; ok {
			lines = append(lines, fmt.Sprintf("%s=%q", key, value))
			seen[key] = struct{}{}
		}
	}
	for key, value := range values {
		if _, ok := seen[key]; ok {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s=%q", key, value))
	}
	payload := strings.Join(lines, "\n")
	if payload != "" {
		payload += "\n"
	}
	if err := os.MkdirAll(filepath.Dir(vllmEnvPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(vllmEnvPath, []byte(payload), 0o600)
}

func (d *vllmDriver) pullContainerImage(image string) error {
	image = strings.TrimSpace(image)
	if image == "" {
		return fmt.Errorf("vllm container image is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		if d.containerImageExists(image) {
			return nil
		}
		return fmt.Errorf("timed out pulling vllm container image %q and no local image fallback", image)
	}
	if d.containerImageExists(image) {
		return nil
	}
	return fmt.Errorf("pull vllm container image %q failed: %w (%s)", image, err, strings.TrimSpace(string(output)))
}

func (d *vllmDriver) containerImageExists(image string) bool {
	image = strings.TrimSpace(image)
	if image == "" {
		return false
	}
	cmd := exec.Command("docker", "image", "inspect", image)
	return cmd.Run() == nil
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

func (d *vllmDriver) fetchRemoteModels() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, d.baseURL()+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("create vllm models request: %w", err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, vllmMaxHTTPBodyBytes))
		return nil, fmt.Errorf("vllm models endpoint failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, vllmMaxHTTPBodyBytes))
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode vllm models response: %w", err)
	}
	models := make([]string, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		if strings.TrimSpace(item.ID) != "" {
			models = append(models, item.ID)
		}
	}
	return models, nil
}

func (d *vllmDriver) waitForAPIReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		_, err := d.fetchRemoteModels()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out waiting for vllm API")
	}
	return lastErr
}

func (d *vllmDriver) systemdUnitStatusTail() string {
	out, err := exec.Command("systemctl", "--no-pager", "--full", "status", "vllm").CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > 8 {
		lines = lines[:8]
	}
	return strings.Join(lines, " | ")
}

func (d *vllmDriver) systemdUnitJournalTail() string {
	out, err := exec.Command("journalctl", "-u", "vllm", "-n", "120", "--no-pager").CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, " | ")
}

func (d *vllmDriver) vllmDiagnosticsTail() string {
	parts := make([]string, 0, 2)
	if status := d.systemdUnitStatusTail(); status != "" {
		parts = append(parts, "systemd: "+status)
	}
	if journal := d.systemdUnitJournalTail(); journal != "" {
		parts = append(parts, "journal: "+journal)
	}
	return strings.Join(parts, "; ")
}

func throttleVLLMRestart() (bool, time.Duration) {
	vllmRestartMu.Lock()
	defer vllmRestartMu.Unlock()
	if lastVLLMRestartAt.IsZero() {
		return false, 0
	}
	elapsed := time.Since(lastVLLMRestartAt)
	if elapsed >= vllmRestartCooldown {
		return false, 0
	}
	return true, vllmRestartCooldown - elapsed
}

func markVLLMRestart() {
	vllmRestartMu.Lock()
	lastVLLMRestartAt = time.Now()
	vllmRestartMu.Unlock()
}

func (d *vllmDriver) detectVLLMLibraryPath() string {
	pySnippet := strings.Join([]string{
		"import glob",
		"import os",
		"import site",
		"paths = []",
		"for base in site.getsitepackages():",
		"    for pat in ('nvidia/*/lib', 'torch/lib'):",
		"        for p in glob.glob(os.path.join(base, pat)):",
		"            if os.path.isdir(p):",
		"                paths.append(p)",
		"seen = set()",
		"ordered = []",
		"for p in paths:",
		"    if p in seen:",
		"        continue",
		"    seen.add(p)",
		"    ordered.append(p)",
		"print(':'.join(ordered))",
	}, "\n")
	out, err := exec.Command(vllmPythonPath, "-c", pySnippet).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (d *vllmDriver) readEnvConfigMap() map[string]string {
	values := map[string]string{}
	raw, err := os.ReadFile(vllmEnvPath)
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if key == "" {
			continue
		}
		if len(val) >= 2 {
			if strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"") {
				if unquoted, err := strconv.Unquote(val); err == nil {
					val = unquoted
				} else {
					val = val[1 : len(val)-1]
				}
			} else if strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'") {
				val = val[1 : len(val)-1]
			}
		}
		values[key] = val
	}
	return values
}

func (d *vllmDriver) writeEnvConfigMap(values map[string]string) error {
	lines := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	order := []string{
		"VLLM_MODEL",
		"VLLM_PORT",
		"VLLM_GPU_MEMORY_UTILIZATION",
		"VLLM_TRUST_REMOTE_CODE",
		"VLLM_EXTRA_ARGS",
		"VLLM_RUNTIME_MODE",
		"HF_TOKEN",
		"HUGGING_FACE_HUB_TOKEN",
		"VLLM_CONTAINER_IMAGE",
		"VLLM_LD_LIBRARY_PATH",
	}
	for _, key := range order {
		value, ok := values[key]
		if !ok {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s=%q", key, value))
		seen[key] = struct{}{}
	}
	extraKeys := make([]string, 0, len(values))
	for key := range values {
		if _, ok := seen[key]; ok {
			continue
		}
		extraKeys = append(extraKeys, key)
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		lines = append(lines, fmt.Sprintf("%s=%q", key, values[key]))
	}
	payload := strings.Join(lines, "\n")
	if payload != "" {
		payload += "\n"
	}
	return os.WriteFile(vllmEnvPath, []byte(payload), 0o600)
}

func (d *vllmDriver) writeDockerEnvConfig(values map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(vllmDockerEnvPath), 0o755); err != nil {
		return err
	}
	excluded := map[string]struct{}{
		"VLLM_MODEL":                  {},
		"VLLM_PORT":                   {},
		"VLLM_GPU_MEMORY_UTILIZATION": {},
		"VLLM_EXTRA_ARGS":             {},
		"VLLM_RUNTIME_MODE":           {},
		"VLLM_CONTAINER_IMAGE":        {},
		"VLLM_LD_LIBRARY_PATH":        {},
		"VLLM_TRUST_REMOTE_CODE":      {},
	}
	keys := make([]string, 0, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, skip := excluded[key]; skip {
			continue
		}
		if strings.TrimSpace(value) == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s=%s", key, values[key]))
	}
	payload := strings.Join(lines, "\n")
	if payload != "" {
		payload += "\n"
	}
	return os.WriteFile(vllmDockerEnvPath, []byte(payload), 0o600)
}

func (d *vllmDriver) hasLibcudart() bool {
	pySnippet := strings.Join([]string{
		"import ctypes",
		"import sys",
		"try:",
		"    ctypes.CDLL('libcudart.so.12')",
		"    sys.exit(0)",
		"except Exception:",
		"    sys.exit(1)",
	}, "\n")
	return exec.Command(vllmPythonPath, "-c", pySnippet).Run() == nil
}

func (d *vllmDriver) ensureCudaRuntimeLibraries() error {
	if d.hasLibcudart() {
		return nil
	}
	// Some systems (including ARM builds) may miss CUDA runtime shared libs
	// even when vLLM itself installs successfully.
	if err := runCommand(vllmPythonPath, "-m", "pip", "install", "--upgrade", "nvidia-cuda-runtime-cu12"); err != nil {
		return err
	}
	return nil
}

func (d *vllmDriver) validateVLLMRuntimeCompatibility() error {
	if d.shouldUseContainer() {
		return nil
	}
	pySnippet := strings.Join([]string{
		"import json",
		"import sys",
		"out = {}",
		"try:",
		"    import torch",
		"    out['torchVersion'] = getattr(torch, '__version__', '')",
		"    out['torchCudaVersion'] = getattr(getattr(torch, 'version', None), 'cuda', None)",
		"except Exception as e:",
		"    out['torchError'] = str(e)",
		"try:",
		"    import vllm._C  # noqa: F401",
		"    out['vllmImportOk'] = True",
		"except Exception as e:",
		"    out['vllmImportOk'] = False",
		"    out['vllmImportError'] = str(e)",
		"print(json.dumps(out))",
	}, "\n")
	out, err := exec.Command(vllmPythonPath, "-c", pySnippet).CombinedOutput()
	raw := strings.TrimSpace(string(out))
	if err != nil {
		if strings.Contains(raw, "\"torchCudaVersion\": null") ||
			strings.Contains(strings.ToLower(raw), "+cpu") {
			return fmt.Errorf(
				"incompatible torch build for vllm (cpu-only torch detected). install a CUDA-enabled torch matching vllm; details: %s",
				raw,
			)
		}
		if raw != "" {
			return fmt.Errorf("vllm runtime compatibility check failed: %s", raw)
		}
		return fmt.Errorf("vllm runtime compatibility check failed: %w", err)
	}
	if strings.Contains(raw, "\"vllmImportOk\": false") {
		if strings.Contains(raw, "undefined symbol") {
			return fmt.Errorf(
				"vllm binary compatibility check failed (likely torch/vllm ABI mismatch). reinstall matching torch+vllm builds; details: %s",
				raw,
			)
		}
		return fmt.Errorf("vllm import check failed: %s", raw)
	}
	return nil
}

func (d *vllmDriver) runtimeMode() string {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("VLLM_RUNTIME_MODE")))
	switch mode {
	case "native", "container":
		return mode
	}
	envCfg := d.readEnvConfigMap()
	mode = strings.ToLower(strings.TrimSpace(envCfg["VLLM_RUNTIME_MODE"]))
	switch mode {
	case "native", "container":
		return mode
	}
	if _, err := exec.LookPath("docker"); err == nil {
		return "container"
	}
	if goruntime.GOARCH == "arm64" || goruntime.GOARCH == "aarch64" {
		return "container"
	}
	return "native"
}

func (d *vllmDriver) shouldUseContainer() bool {
	return d.runtimeMode() == "container"
}

func vllmPreparedModelsDir() string {
	return "/var/lib/mantler/models/vllm"
}

func safeModelPathSegment(modelID string) string {
	replacer := strings.NewReplacer("/", "--", "\\", "--", ":", "_", " ", "_")
	return replacer.Replace(strings.TrimSpace(modelID))
}

func (d *vllmDriver) preparedModels() []string {
	dir := vllmPreparedModelsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	models := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			continue
		}
		modelID := strings.TrimSpace(string(raw))
		if modelID != "" {
			models = append(models, modelID)
		}
	}
	sort.Strings(models)
	return models
}

func (d *vllmDriver) markPreparedModel(modelID string) error {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return nil
	}
	dir := vllmPreparedModelsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, safeModelPathSegment(trimmed)+".model")
	return os.WriteFile(path, []byte(trimmed+"\n"), 0o600)
}

func (d *vllmDriver) unmarkPreparedModel(modelID string) error {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return nil
	}
	path := filepath.Join(vllmPreparedModelsDir(), safeModelPathSegment(trimmed)+".model")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (d *vllmDriver) downloadModelSnapshot(modelID string) error {
	return d.downloadModelSnapshotCtx(context.Background(), modelID)
}

// downloadModelSnapshotCtx downloads HuggingFace model weights with cancellation support.
func (d *vllmDriver) downloadModelSnapshotCtx(ctx context.Context, modelID string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	pythonCandidates := []string{vllmPythonPath, "python3"}
	script := "from huggingface_hub import snapshot_download; snapshot_download(repo_id='" + strings.ReplaceAll(modelID, "'", "\\'") + "', cache_dir='/root/.cache/huggingface', resume_download=True)"
	for _, python := range pythonCandidates {
		if strings.TrimSpace(python) == "" {
			continue
		}
		if _, err := exec.LookPath(python); err != nil && python != vllmPythonPath {
			continue
		}
		cmd := exec.CommandContext(ctx, python, "-c", script)
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		return nil
	}
	if d.shouldUseContainer() {
		if err := d.ensureDockerAvailable(); err != nil {
			return err
		}
		image := d.effectiveContainerImage()
		if !d.containerImageExists(image) {
			if err := d.pullContainerImageCtx(ctx, image); err != nil {
				return err
			}
		}
		cmd := exec.CommandContext(
			ctx,
			"docker",
			"run",
			"--rm",
			"--network",
			"host",
			"--entrypoint",
			"python3",
			"-v",
			"/root/.cache/huggingface:/root/.cache/huggingface",
			image,
			"-c",
			script,
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("download model snapshot in container: %w (%s)", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	return fmt.Errorf("failed to download model snapshot for %s (huggingface_hub unavailable)", modelID)
}

// pullContainerImageCtx pulls a container image with cancellation support.
func (d *vllmDriver) pullContainerImageCtx(ctx context.Context, image string) error {
	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("pull container image %s: %w (%s)", image, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func inferVLLMInstalled(hasNativeImport bool, hasServiceUnit bool, hasConfig bool, hasEnv bool) bool {
	if hasNativeImport {
		return true
	}
	// Managed installs should leave one or more local artifacts behind.
	return hasServiceUnit || hasConfig || hasEnv
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (d *vllmDriver) containerImage() string {
	image := strings.TrimSpace(os.Getenv("VLLM_CONTAINER_IMAGE"))
	if image == "" {
		image = vllmDefaultContainerImage
	}
	return image
}

func (d *vllmDriver) ensureDockerAvailable() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is required for containerized vllm mode")
	}
	return nil
}

func (d *vllmDriver) classifyKnownStartupFailure(diagnostics string) string {
	text := strings.ToLower(diagnostics)
	switch {
	case strings.Contains(text, "401") && (strings.Contains(text, "huggingface") ||
		strings.Contains(text, "gated") ||
		strings.Contains(text, "authentication") ||
		strings.Contains(text, "authorization")):
		return "hint: model download needs Hugging Face auth; set HF_TOKEN (or HUGGING_FACE_HUB_TOKEN) in /etc/mantler/vllm.env so vLLM can access gated repos"
	case strings.Contains(text, "libcudart.so.12"):
		return "hint: CUDA runtime libraries are missing from loader path; install nvidia-cuda-runtime-cu12 and set VLLM_LD_LIBRARY_PATH"
	case strings.Contains(text, "libtorch_cuda.so"):
		return "hint: CUDA-enabled torch is missing/incompatible; ensure torch is not +cpu and matches vLLM wheel CUDA variant"
	case strings.Contains(text, "undefined symbol") && strings.Contains(text, "vllm/_c"):
		return "hint: torch/vllm ABI mismatch detected; reinstall matching torch and vllm versions from the same CUDA wheel family"
	case strings.Contains(text, "modelopt currently only supports") && strings.Contains(text, "mixed_precision"):
		return "hint: this model uses ModelOpt MIXED_PRECISION not supported by current vLLM build; upgrade vLLM container/wheel variant"
	case strings.Contains(text, "modelopt currently only supports"):
		return "hint: this model's ModelOpt quantization format is incompatible with nvcr.io/nvidia/vllm:26.02-py3 (vLLM 0.15.1); use a container build that includes mixed-precision ModelOpt support or select a compatible model"
	case strings.Contains(text, "trust_remote_code") || strings.Contains(text, "trust-remote-code"):
		return "hint: this model requires remote code; set VLLM_TRUST_REMOTE_CODE=true (or MANTLER_VLLM_TRUST_REMOTE_CODE=true at install time)"
	default:
		return ""
	}
}

func (d *vllmDriver) isLikelyServiceWarmup(endpointErr error) bool {
	if endpointErr == nil {
		return false
	}
	if !d.isTransientEndpointError(endpointErr) {
		return false
	}
	out, err := exec.Command(
		"systemctl",
		"show",
		"--property=ActiveState",
		"--property=SubState",
		"--property=ActiveEnterTimestamp",
		"vllm",
	).Output()
	if err != nil {
		return false
	}
	activeState := ""
	subState := ""
	activeSinceRaw := ""
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "ActiveState":
			activeState = strings.TrimSpace(parts[1])
		case "SubState":
			subState = strings.TrimSpace(parts[1])
		case "ActiveEnterTimestamp":
			activeSinceRaw = strings.TrimSpace(parts[1])
		}
	}
	if activeState != "active" {
		return false
	}
	if subState != "running" && subState != "start" && subState != "start-post" {
		return false
	}
	if activeSinceRaw == "" || activeSinceRaw == "n/a" {
		return true
	}
	// Example: "Sat 2026-04-04 18:08:52 CEST"
	activeSince, parseErr := time.Parse("Mon 2006-01-02 15:04:05 MST", activeSinceRaw)
	if parseErr != nil {
		return true
	}
	return time.Since(activeSince) <= vllmStartupGraceWindow
}

func (d *vllmDriver) isTransientEndpointError(err error) bool {
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" {
		return false
	}
	return strings.Contains(text, "connection refused") ||
		strings.Contains(text, "i/o timeout") ||
		strings.Contains(text, "context deadline exceeded") ||
		strings.Contains(text, "eof") ||
		strings.Contains(text, "connection reset by peer") ||
		strings.Contains(text, "service unavailable") ||
		strings.Contains(text, "bad gateway") ||
		strings.Contains(text, "temporarily unavailable")
}
