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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

const (
	tensorrtConfigPath      = "/etc/mantler/tensorrt.json"
	tensorrtEnvPath         = "/etc/mantler/tensorrt.env"
	tensorrtUnitPath        = "/etc/systemd/system/tensorrt-llm.service"
	tensorrtContainerName   = "mantler-tensorrt"
	tensorrtDefaultImage    = "nvcr.io/nvidia/tritonserver:25.03-trtllm-python-py3"
	tensorrtDefaultPort     = 8000
	tensorrtReadyTimeout    = 180 * time.Second
	tensorrtRestartCooldown = 90 * time.Second
	tensorrtEnginesDir      = "/var/lib/mantler/trt-engines"
)

type tensorrtConfig struct {
	Model string `json:"model,omitempty"`
	Port  int    `json:"port,omitempty"`
}

type tensorrtDriver struct{}

var (
	tensorrtRestartMu     sync.Mutex
	lastTensorRTRestartAt time.Time
)

func newTensorRTDriver() Driver {
	return &tensorrtDriver{}
}

func (d *tensorrtDriver) Name() string { return "tensorrt" }

func (d *tensorrtDriver) Install() error {
	if err := d.ensureDockerAvailable(); err != nil {
		return err
	}
	image := d.containerImage()
	if err := d.pullContainerImage(image); err != nil {
		return err
	}
	if err := d.ensureServiceUnit(); err != nil {
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
		port = tensorrtDefaultPort
	}
	if err := d.startOrRestartService(cfg.Model, port, false); err != nil {
		return fmt.Errorf("tensorrt install completed but configured model failed to start: %w", err)
	}
	return nil
}

func (d *tensorrtDriver) Uninstall() error {
	_ = runCommand("systemctl", "stop", "tensorrt-llm")
	_ = runCommand("systemctl", "disable", "tensorrt-llm")
	_ = os.Remove(tensorrtUnitPath)
	_ = runCommand("systemctl", "daemon-reload")
	_ = exec.Command("docker", "rm", "-f", tensorrtContainerName).Run()
	_ = os.Remove(tensorrtConfigPath)
	_ = os.Remove(tensorrtEnvPath)
	return nil
}

func (d *tensorrtDriver) IsInstalled() bool {
	hasNativeBinary := d.hasTrtllmServe()
	hasServiceUnit := d.fileExists(tensorrtUnitPath)
	hasConfig := d.fileExists(tensorrtConfigPath)
	hasEnv := d.fileExists(tensorrtEnvPath)
	return inferTensorRTInstalled(hasNativeBinary, hasServiceUnit, hasConfig, hasEnv)
}

func (d *tensorrtDriver) IsReady() bool {
	if !d.IsInstalled() {
		return false
	}
	if configuredModel, known := d.configuredModelState(); known && strings.TrimSpace(configuredModel) == "" {
		// Installed runtime with no configured model is considered idle-ready.
		_ = runCommand("systemctl", "stop", "tensorrt-llm")
		_ = runCommand("systemctl", "reset-failed", "tensorrt-llm")
		return true
	}
	if _, known := d.configuredModelState(); !known && d.serviceIsInactive() {
		// Non-root CLI may not be able to read /etc/mantler files; treat
		// inactive service as idle-ready in that restricted visibility mode.
		return true
	}
	_, err := d.fetchRemoteModels()
	return err == nil
}

func (d *tensorrtDriver) Version() string {
	if !d.IsInstalled() {
		return ""
	}
	image := d.containerImage()
	if d.containerImageExists(image) {
		return "container:" + image
	}
	out, err := exec.Command("trtllm-serve", "--version").CombinedOutput()
	if err == nil {
		v := strings.TrimSpace(string(out))
		if v != "" {
			return v
		}
	}
	return ""
}

func (d *tensorrtDriver) RuntimeConfig() map[string]any {
	env := d.readEnvConfigMap()
	config := map[string]any{
		"version":        d.Version(),
		"containerImage": d.containerImage(),
	}
	if extraArgs := strings.TrimSpace(env["TENSORRT_EXTRA_ARGS"]); extraArgs != "" {
		config["extraArgs"] = extraArgs
	}

	modelID := ""
	if cfg, err := d.readConfig(); err == nil {
		modelID = strings.TrimSpace(cfg.Model)
	}
	if modelID == "" {
		modelID = strings.TrimSpace(env["TENSORRT_MODEL"])
	}
	if modelID == "" {
		return config
	}

	if enginePath, ok := d.BuiltEnginePath(modelID); ok {
		config["enginePath"] = enginePath
		if buildConfig := d.readBuiltEngineConfig(modelID); len(buildConfig) > 0 {
			if quantization, ok := buildConfig["quantization"].(string); ok && strings.TrimSpace(quantization) != "" {
				config["quantization"] = quantization
			}
			if tpSize, ok := buildConfig["tpSize"].(float64); ok && tpSize > 0 {
				config["tpSize"] = int(tpSize)
			}
		}
	}

	return config
}

func (d *tensorrtDriver) PrepareModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	return d.PrepareModelWithFlagsCtx(context.Background(), modelID, flags)
}

// PrepareModelWithFlagsCtx downloads model weights with cancellation support.
func (d *tensorrtDriver) PrepareModelWithFlagsCtx(ctx context.Context, modelID string, _ *types.ModelFeatureFlags) error {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return fmt.Errorf("model ID is required")
	}
	if err := d.downloadModelSnapshotCtx(ctx, trimmed); err != nil {
		return err
	}
	return d.markPreparedModel(trimmed)
}

func (d *tensorrtDriver) StartModelWithFlags(modelID string, _ *types.ModelFeatureFlags) error {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return fmt.Errorf("model ID is required")
	}
	if err := d.PrepareModelWithFlags(trimmed, nil); err != nil {
		return err
	}
	if err := d.writeConfig(tensorrtConfig{Model: trimmed, Port: tensorrtDefaultPort}); err != nil {
		return err
	}
	return d.startOrRestartService(trimmed, tensorrtDefaultPort, false)
}

func (d *tensorrtDriver) StopModel(modelID string) error {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return fmt.Errorf("model ID is required")
	}
	cfg, err := d.readConfig()
	if err == nil && strings.EqualFold(strings.TrimSpace(cfg.Model), trimmed) {
		_ = d.writeConfig(tensorrtConfig{Port: tensorrtDefaultPort})
	}
	_ = runCommand("systemctl", "stop", "tensorrt-llm")
	_ = runCommand("systemctl", "reset-failed", "tensorrt-llm")
	return nil
}

func (d *tensorrtDriver) EnsureModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	return d.StartModelWithFlags(modelID, flags)
}

func (d *tensorrtDriver) ListModels() []string {
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

func (d *tensorrtDriver) InstalledModels() []types.InstalledModel {
	models := make([]types.InstalledModel, 0)
	seen := map[string]struct{}{}
	addModel := func(modelID string, status types.ModelInstallStatus, failReason string) {
		trimmed := strings.TrimSpace(modelID)
		if trimmed == "" {
			return
		}
		if _, exists := seen[trimmed]; exists {
			return
		}
		seen[trimmed] = struct{}{}
		models = append(models, types.InstalledModel{
			ModelID:    trimmed,
			Runtime:    types.RuntimeTensorRT,
			Status:     status,
			FailReason: failReason,
		})
	}
	// Report built engines first (highest priority status)
	for _, built := range d.builtModels() {
		addModel(built, types.ModelBuilt, "")
	}
	// Report downloaded but not-yet-built models
	for _, prepared := range d.preparedModels() {
		addModel(prepared, types.ModelDownloaded, "")
	}
	cfg, _ := d.readConfig()
	configuredModel := strings.TrimSpace(cfg.Model)
	remoteModels, err := d.fetchRemoteModels()
	if err == nil {
		for _, modelID := range remoteModels {
			addModel(modelID, types.ModelReady, "")
		}
		if configuredModel != "" {
			if _, ok := seen[configuredModel]; !ok {
				addModel(configuredModel, types.ModelStarting, "")
			}
		}
		return models
	}
	if configuredModel != "" {
		failReason := ""
		if serviceLikelyOutOfMemory("tensorrt-llm", err) {
			failReason = modelFailReasonInsufficientMemory
		}
		if _, ok := seen[configuredModel]; ok {
			for i := range models {
				if models[i].ModelID == configuredModel {
					models[i].Status = types.ModelFailed
					models[i].FailReason = failReason
					break
				}
			}
		} else {
			addModel(configuredModel, types.ModelFailed, failReason)
		}
	}
	return models
}

func (d *tensorrtDriver) HasModel(modelID string) bool {
	for _, model := range d.ListModels() {
		if model == modelID {
			return true
		}
	}
	return false
}

func (d *tensorrtDriver) RemoveModel(modelID string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	_ = d.StopModel(modelID)
	_ = d.unmarkPreparedModel(modelID)
	_ = d.removeBuiltEngine(modelID)
	return nil
}

func (d *tensorrtDriver) BenchmarkModel(
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

func (d *tensorrtDriver) benchmarkOnce(modelID string, prompt string, sampleOutputTokens int) (BenchmarkResult, error) {
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
		return BenchmarkResult{}, fmt.Errorf("encode tensorrt benchmark request: %w", err)
	}

	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, d.baseURL()+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("create tensorrt benchmark request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("tensorrt benchmark request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return BenchmarkResult{}, fmt.Errorf("tensorrt benchmark failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return BenchmarkResult{}, fmt.Errorf("decode tensorrt benchmark response: %w", err)
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

func (d *tensorrtDriver) RestartRuntime() error {
	cfg, cfgErr := d.readConfig()
	if cfgErr == nil {
		if strings.TrimSpace(cfg.Model) != "" {
			port := cfg.Port
			if port <= 0 {
				port = tensorrtDefaultPort
			}
			return d.startOrRestartService(cfg.Model, port, true)
		}
		// No configured model: keep runtime idle instead of crash-looping.
		_ = runCommand("systemctl", "stop", "tensorrt-llm")
		_ = runCommand("systemctl", "reset-failed", "tensorrt-llm")
		return nil
	}
	if os.IsNotExist(cfgErr) {
		_ = runCommand("systemctl", "stop", "tensorrt-llm")
		_ = runCommand("systemctl", "reset-failed", "tensorrt-llm")
		return nil
	}
	return runCommand("systemctl", "restart", "tensorrt-llm")
}

// -------------------------------------------------------------------------
// Internal helpers
// -------------------------------------------------------------------------

func (d *tensorrtDriver) baseURL() string {
	cfg, err := d.readConfig()
	port := tensorrtDefaultPort
	if err == nil && cfg.Port > 0 {
		port = cfg.Port
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func (d *tensorrtDriver) configuredModelState() (string, bool) {
	cfg, err := d.readConfig()
	if err == nil {
		return strings.TrimSpace(cfg.Model), true
	}
	if os.IsNotExist(err) {
		return "", true
	}
	return "", false
}

func (d *tensorrtDriver) serviceIsInactive() bool {
	out, err := exec.Command("systemctl", "is-active", "tensorrt-llm").CombinedOutput()
	if err != nil && len(out) == 0 {
		return false
	}
	return strings.TrimSpace(string(out)) == "inactive"
}

func (d *tensorrtDriver) hasTrtllmServe() bool {
	_, err := exec.LookPath("trtllm-serve")
	return err == nil
}

func inferTensorRTInstalled(hasNativeBinary bool, hasServiceUnit bool, hasConfig bool, hasEnv bool) bool {
	if hasNativeBinary {
		return true
	}
	// Managed installs should leave one or more local artifacts behind.
	return hasServiceUnit || hasConfig || hasEnv
}

func (d *tensorrtDriver) fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (d *tensorrtDriver) ensureDockerAvailable() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is required for tensorrt-llm container mode")
	}
	return nil
}

func (d *tensorrtDriver) containerImage() string {
	image := strings.TrimSpace(os.Getenv("TENSORRT_CONTAINER_IMAGE"))
	if image != "" {
		return image
	}
	envCfg := d.readEnvConfigMap()
	if img := strings.TrimSpace(envCfg["TENSORRT_CONTAINER_IMAGE"]); img != "" {
		return img
	}
	return tensorrtDefaultImage
}

func (d *tensorrtDriver) pullContainerImage(image string) error {
	image = strings.TrimSpace(image)
	if image == "" {
		return fmt.Errorf("tensorrt container image is required")
	}
	cmd := exec.Command("docker", "pull", image)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if d.containerImageExists(image) {
		return nil
	}
	return fmt.Errorf("pull tensorrt container image %q failed: %w (%s)", image, err, strings.TrimSpace(string(output)))
}

func (d *tensorrtDriver) containerImageExists(image string) bool {
	image = strings.TrimSpace(image)
	if image == "" {
		return false
	}
	return exec.Command("docker", "image", "inspect", image).Run() == nil
}

func (d *tensorrtDriver) ensureServiceUnit() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(tensorrtUnitPath), 0o755); err != nil {
		return fmt.Errorf("create tensorrt systemd directory: %w", err)
	}

	var unit string
	if d.hasTrtllmServe() {
		unit = `[Unit]
Description=TensorRT-LLM OpenAI API Server (native)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-` + tensorrtEnvPath + `
ExecStart=/bin/sh -c 'exec trtllm-serve "${TENSORRT_MODEL}" --host 0.0.0.0 --port "${TENSORRT_PORT:-8000}" ${TENSORRT_EXTRA_ARGS:-}'
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
`
	} else {
		unit = `[Unit]
Description=TensorRT-LLM OpenAI API Server (Container)
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-` + tensorrtEnvPath + `
ExecStartPre=-/bin/sh -c 'DOCKER_BIN="$(command -v docker)"; if [ -n "$DOCKER_BIN" ]; then "$DOCKER_BIN" rm -f ` + tensorrtContainerName + ` >/dev/null 2>&1 || true; fi'
ExecStartPre=-/bin/sh -c 'mkdir -p /opt/mantler/tensorrt-app'
ExecStart=/bin/sh -c 'DOCKER_BIN="$(command -v docker)"; [ -n "$DOCKER_BIN" ] || exit 1; exec "$DOCKER_BIN" run --rm --name ` + tensorrtContainerName + ` --gpus all --ipc=host --network host -e HF_TOKEN="${HF_TOKEN:-}" -e HUGGING_FACE_HUB_TOKEN="${HUGGING_FACE_HUB_TOKEN:-}" -e NVIDIA_VISIBLE_DEVICES=all -e NVIDIA_DRIVER_CAPABILITIES=compute,utility -v /root/.cache/huggingface:/root/.cache/huggingface -v /opt/mantler/tensorrt-app:/app "${TENSORRT_CONTAINER_IMAGE:-` + tensorrtDefaultImage + `}" trtllm-serve "${TENSORRT_MODEL}" --host 0.0.0.0 --port "${TENSORRT_PORT:-8000}" ${TENSORRT_EXTRA_ARGS:-}'
ExecStop=-/bin/sh -c 'DOCKER_BIN="$(command -v docker)"; if [ -n "$DOCKER_BIN" ]; then "$DOCKER_BIN" stop ` + tensorrtContainerName + ` >/dev/null 2>&1 || true; fi'
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
`
	}

	if err := os.WriteFile(tensorrtUnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write tensorrt service unit: %w", err)
	}
	return runCommand("systemctl", "daemon-reload")
}

func (d *tensorrtDriver) startOrRestartService(modelID string, port int, force bool) error {
	if err := d.ensureServiceUnit(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tensorrtEnvPath), 0o755); err != nil {
		return fmt.Errorf("create tensorrt env directory: %w", err)
	}
	safeModelID := strings.ReplaceAll(strings.TrimSpace(modelID), "\n", " ")
	existingEnv := d.readEnvConfigMap()
	extraArgs := strings.TrimSpace(existingEnv["TENSORRT_EXTRA_ARGS"])
	containerImage := strings.TrimSpace(existingEnv["TENSORRT_CONTAINER_IMAGE"])
	if containerImage == "" {
		containerImage = d.containerImage()
	}

	if !d.hasTrtllmServe() {
		if err := d.ensureDockerAvailable(); err != nil {
			return err
		}
		if err := d.pullContainerImage(containerImage); err != nil {
			return err
		}
	}

	envContent := fmt.Sprintf("TENSORRT_MODEL=%q\nTENSORRT_PORT=%d\nTENSORRT_EXTRA_ARGS=%q\nTENSORRT_CONTAINER_IMAGE=%q\n",
		safeModelID,
		port,
		extraArgs,
		containerImage,
	)
	if err := os.WriteFile(tensorrtEnvPath, []byte(envContent), 0o600); err != nil {
		return fmt.Errorf("write tensorrt env config: %w", err)
	}
	if err := runCommand("systemctl", "enable", "tensorrt-llm"); err != nil {
		return err
	}

	if !force {
		if throttled, remaining := throttleTensorRTRestart(); throttled {
			if err := d.waitForAPIReady(6 * time.Second); err == nil {
				return nil
			}
			return fmt.Errorf("skipping tensorrt restart due to cooldown (%s remaining)", remaining.Round(time.Second))
		}
	}

	markTensorRTRestart()
	if err := runCommand("systemctl", "restart", "tensorrt-llm"); err != nil {
		return err
	}
	if err := d.waitForAPIReady(tensorrtReadyTimeout); err != nil {
		return fmt.Errorf("tensorrt service restarted but API not reachable yet: %w", err)
	}
	return nil
}

func (d *tensorrtDriver) fetchRemoteModels() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, d.baseURL()+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("create tensorrt models request: %w", err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tensorrt models endpoint failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode tensorrt models response: %w", err)
	}
	models := make([]string, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		if strings.TrimSpace(item.ID) != "" {
			models = append(models, item.ID)
		}
	}
	return models, nil
}

func (d *tensorrtDriver) waitForAPIReady(timeout time.Duration) error {
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
		lastErr = fmt.Errorf("timed out waiting for tensorrt API")
	}
	return lastErr
}

func (d *tensorrtDriver) readConfig() (tensorrtConfig, error) {
	raw, err := os.ReadFile(tensorrtConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return tensorrtConfig{}, nil
		}
		return tensorrtConfig{}, err
	}
	if len(raw) == 0 {
		return tensorrtConfig{}, nil
	}
	var cfg tensorrtConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return tensorrtConfig{}, err
	}
	return cfg, nil
}

func (d *tensorrtDriver) writeConfig(cfg tensorrtConfig) error {
	if cfg.Port <= 0 {
		cfg.Port = tensorrtDefaultPort
	}
	if err := os.MkdirAll(filepath.Dir(tensorrtConfigPath), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tensorrtConfigPath, append(payload, '\n'), 0o600)
}

func tensorrtPreparedModelsDir() string {
	return "/var/lib/mantler/models/tensorrt"
}

func (d *tensorrtDriver) preparedModels() []string {
	dir := tensorrtPreparedModelsDir()
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

func (d *tensorrtDriver) markPreparedModel(modelID string) error {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return nil
	}
	dir := tensorrtPreparedModelsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, safeModelPathSegment(trimmed)+".model")
	return os.WriteFile(path, []byte(trimmed+"\n"), 0o600)
}

func (d *tensorrtDriver) unmarkPreparedModel(modelID string) error {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return nil
	}
	path := filepath.Join(tensorrtPreparedModelsDir(), safeModelPathSegment(trimmed)+".model")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (d *tensorrtDriver) downloadModelSnapshot(modelID string) error {
	return d.downloadModelSnapshotCtx(context.Background(), modelID)
}

// downloadModelSnapshotCtx downloads HuggingFace model weights with cancellation support.
func (d *tensorrtDriver) downloadModelSnapshotCtx(ctx context.Context, modelID string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	script := "from huggingface_hub import snapshot_download; snapshot_download(repo_id='" + strings.ReplaceAll(modelID, "'", "\\'") + "', cache_dir='/root/.cache/huggingface', resume_download=True)"
	for _, python := range []string{"python3"} {
		cmd := exec.CommandContext(ctx, python, "-c", script)
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		return nil
	}
	if err := d.ensureDockerAvailable(); err != nil {
		return err
	}
	image := d.containerImage()
	if !d.containerImageExists(image) {
		if err := d.pullContainerImageCtx(ctx, image); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "host", "-v", "/root/.cache/huggingface:/root/.cache/huggingface", image, "python3", "-c", script)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("download model snapshot in tensorrt container: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// pullContainerImageCtx pulls a container image with cancellation support.
func (d *tensorrtDriver) pullContainerImageCtx(ctx context.Context, image string) error {
	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("pull tensorrt container image %s: %w (%s)", image, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d *tensorrtDriver) readEnvConfigMap() map[string]string {
	values := map[string]string{}
	raw, err := os.ReadFile(tensorrtEnvPath)
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
			if (strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"")) ||
				(strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'")) {
				val = val[1 : len(val)-1]
			}
		}
		values[key] = val
	}
	return values
}

func throttleTensorRTRestart() (bool, time.Duration) {
	tensorrtRestartMu.Lock()
	defer tensorrtRestartMu.Unlock()
	if lastTensorRTRestartAt.IsZero() {
		return false, 0
	}
	elapsed := time.Since(lastTensorRTRestartAt)
	if elapsed >= tensorrtRestartCooldown {
		return false, 0
	}
	return true, tensorrtRestartCooldown - elapsed
}

func markTensorRTRestart() {
	tensorrtRestartMu.Lock()
	lastTensorRTRestartAt = time.Now()
	tensorrtRestartMu.Unlock()
}

// ---------------------------------------------------------------------------
// TensorRT Engine Build Support (BuildableDriver interface)
// ---------------------------------------------------------------------------

func tensorrtEngineDir(modelID string) string {
	return filepath.Join(tensorrtEnginesDir, safeModelPathSegment(modelID))
}

// IsModelBuilt checks if a TensorRT engine exists for the given model.
func (d *tensorrtDriver) IsModelBuilt(modelID string) bool {
	engineDir := tensorrtEngineDir(modelID)
	// Check for engine file presence (config.json indicates completed build)
	configPath := filepath.Join(engineDir, "config.json")
	_, err := os.Stat(configPath)
	return err == nil
}

// BuiltEnginePath returns the path to a built TensorRT engine directory.
func (d *tensorrtDriver) BuiltEnginePath(modelID string) (string, bool) {
	if !d.IsModelBuilt(modelID) {
		return "", false
	}
	return tensorrtEngineDir(modelID), true
}

// BuildModel compiles a TensorRT-LLM engine from downloaded HuggingFace weights.
func (d *tensorrtDriver) BuildModel(ctx context.Context, modelID string, opts BuildOptions) error {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return fmt.Errorf("model ID is required")
	}

	// 1. Ensure weights are downloaded
	if err := d.PrepareModelWithFlagsCtx(ctx, trimmed, nil); err != nil {
		return fmt.Errorf("download model weights: %w", err)
	}

	// 2. Check if already built
	if d.IsModelBuilt(trimmed) {
		return nil
	}

	// 3. Create engine output directory
	engineDir := tensorrtEngineDir(trimmed)
	if err := os.MkdirAll(engineDir, 0o755); err != nil {
		return fmt.Errorf("create engine directory: %w", err)
	}

	// 4. Determine build parameters
	quant := opts.Quantization
	if quant == "" {
		quant = "none"
	}
	tpSize := opts.TPSize
	if tpSize <= 0 {
		tpSize = 1
	}
	maxBatchSize := opts.MaxBatchSize
	if maxBatchSize <= 0 {
		maxBatchSize = 4
	}
	maxSeqLen := opts.MaxSeqLen
	if maxSeqLen <= 0 {
		maxSeqLen = 8192
	}

	// 5. Build the engine using trtllm-build in container
	if err := d.ensureDockerAvailable(); err != nil {
		return err
	}
	image := d.containerImage()
	if !d.containerImageExists(image) {
		if err := d.pullContainerImageCtx(ctx, image); err != nil {
			return err
		}
	}

	// Build arguments for trtllm-build
	buildArgs := []string{
		"--model_dir", "/root/.cache/huggingface/hub/models--" + strings.ReplaceAll(trimmed, "/", "--") + "/snapshots",
		"--output_dir", "/output",
		"--tp_size", fmt.Sprintf("%d", tpSize),
		"--max_batch_size", fmt.Sprintf("%d", maxBatchSize),
		"--max_seq_len", fmt.Sprintf("%d", maxSeqLen),
	}
	if quant != "none" {
		buildArgs = append(buildArgs, "--quantization", quant)
	}

	// Run trtllm-build inside container
	dockerArgs := []string{
		"run", "--rm",
		"--gpus", "all",
		"--ipc=host",
		"-v", "/root/.cache/huggingface:/root/.cache/huggingface",
		"-v", engineDir + ":/output",
		image,
		"trtllm-build",
	}
	dockerArgs = append(dockerArgs, buildArgs...)

	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Clean up failed build directory
		_ = os.RemoveAll(engineDir)
		return fmt.Errorf("trtllm-build failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}

	// 6. Write build config for tracking
	configPath := filepath.Join(engineDir, "config.json")
	configData := map[string]interface{}{
		"modelId":      trimmed,
		"quantization": quant,
		"tpSize":       tpSize,
		"maxBatchSize": maxBatchSize,
		"maxSeqLen":    maxSeqLen,
		"builtAt":      time.Now().UTC().Format(time.RFC3339),
	}
	configJSON, _ := json.MarshalIndent(configData, "", "  ")
	if err := os.WriteFile(configPath, configJSON, 0o644); err != nil {
		return fmt.Errorf("write engine config: %w", err)
	}

	return nil
}

// builtModels returns a list of models with built TensorRT engines.
func (d *tensorrtDriver) builtModels() []string {
	entries, err := os.ReadDir(tensorrtEnginesDir)
	if err != nil {
		return nil
	}
	models := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		configPath := filepath.Join(tensorrtEnginesDir, entry.Name(), "config.json")
		raw, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}
		var cfg map[string]interface{}
		if json.Unmarshal(raw, &cfg) != nil {
			continue
		}
		if modelID, ok := cfg["modelId"].(string); ok && modelID != "" {
			models = append(models, modelID)
		}
	}
	sort.Strings(models)
	return models
}

func (d *tensorrtDriver) readBuiltEngineConfig(modelID string) map[string]any {
	if strings.TrimSpace(modelID) == "" {
		return nil
	}
	configPath := filepath.Join(tensorrtEngineDir(modelID), "config.json")
	raw, err := os.ReadFile(configPath)
	if err != nil || len(raw) == 0 {
		return nil
	}
	parsed := map[string]any{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	return parsed
}

// removeBuiltEngine removes a built TensorRT engine.
func (d *tensorrtDriver) removeBuiltEngine(modelID string) error {
	engineDir := tensorrtEngineDir(modelID)
	return os.RemoveAll(engineDir)
}
