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
	tensorrtConfigPath      = "/etc/clawcontrol/tensorrt.json"
	tensorrtEnvPath         = "/etc/clawcontrol/tensorrt.env"
	tensorrtUnitPath        = "/etc/systemd/system/tensorrt-llm.service"
	tensorrtContainerName   = "clawcontrol-tensorrt"
	tensorrtDefaultImage    = "nvcr.io/nvidia/tritonserver:25.03-trtllm-python-py3"
	tensorrtDefaultPort     = 8000
	tensorrtReadyTimeout    = 180 * time.Second
	tensorrtRestartCooldown = 90 * time.Second
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
		// Non-root CLI may not be able to read /etc/clawcontrol files; treat
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

func (d *tensorrtDriver) EnsureModelWithFlags(modelID string, _ *types.ModelFeatureFlags) error {
	if strings.TrimSpace(modelID) == "" {
		return fmt.Errorf("model ID is required")
	}
	if err := d.writeConfig(tensorrtConfig{Model: modelID, Port: tensorrtDefaultPort}); err != nil {
		return err
	}
	return d.startOrRestartService(modelID, tensorrtDefaultPort, false)
}

func (d *tensorrtDriver) ListModels() []string {
	set := map[string]struct{}{}
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
	cfg, _ := d.readConfig()
	configuredModel := strings.TrimSpace(cfg.Model)
	remoteModels, err := d.fetchRemoteModels()
	seen := map[string]struct{}{}
	if err == nil {
		for _, modelID := range remoteModels {
			trimmed := strings.TrimSpace(modelID)
			if trimmed == "" {
				continue
			}
			seen[trimmed] = struct{}{}
			models = append(models, types.InstalledModel{
				ModelID: trimmed,
				Runtime: types.RuntimeTensorRT,
				Status:  types.ModelReady,
			})
		}
		if configuredModel != "" {
			if _, ok := seen[configuredModel]; !ok {
				models = append(models, types.InstalledModel{
					ModelID: configuredModel,
					Runtime: types.RuntimeTensorRT,
					Status:  types.ModelInstalling,
				})
			}
		}
		return models
	}
	if configuredModel != "" {
		failReason := ""
		if serviceLikelyOutOfMemory("tensorrt-llm", err) {
			failReason = modelFailReasonInsufficientMemory
		}
		models = append(models, types.InstalledModel{
			ModelID:    configuredModel,
			Runtime:    types.RuntimeTensorRT,
			Status:     types.ModelFailed,
			FailReason: failReason,
		})
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
	cfg, err := d.readConfig()
	if err == nil && strings.EqualFold(strings.TrimSpace(cfg.Model), modelID) {
		if err := d.writeConfig(tensorrtConfig{Port: tensorrtDefaultPort}); err != nil {
			return err
		}
	}
	return runCommand("systemctl", "stop", "tensorrt-llm")
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
ExecStart=/bin/sh -c 'DOCKER_BIN="$(command -v docker)"; [ -n "$DOCKER_BIN" ] || exit 1; exec "$DOCKER_BIN" run --rm --name ` + tensorrtContainerName + ` --gpus all --ipc=host --network host -e HF_TOKEN="${HF_TOKEN:-}" -e HUGGING_FACE_HUB_TOKEN="${HUGGING_FACE_HUB_TOKEN:-}" -e NVIDIA_VISIBLE_DEVICES=all -e NVIDIA_DRIVER_CAPABILITIES=compute,utility -v /root/.cache/huggingface:/root/.cache/huggingface "${TENSORRT_CONTAINER_IMAGE:-` + tensorrtDefaultImage + `}" trtllm-serve "${TENSORRT_MODEL}" --host 0.0.0.0 --port "${TENSORRT_PORT:-8000}" ${TENSORRT_EXTRA_ARGS:-}'
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
