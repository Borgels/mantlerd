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
	"strings"
	"sync"
	"time"

	"github.com/Borgels/clawcontrol-agent/internal/types"
)

const (
	vllmConfigPath            = "/etc/clawcontrol/vllm.json"
	vllmEnvPath               = "/etc/clawcontrol/vllm.env"
	vllmUnitPath              = "/etc/systemd/system/vllm.service"
	vllmVenvPath              = "/opt/clawcontrol/vllm-venv"
	vllmPythonPath            = "/opt/clawcontrol/vllm-venv/bin/python3"
	vllmContainerName         = "clawcontrol-vllm"
	vllmDefaultContainerImage = "nvcr.io/nvidia/vllm:26.02-py3"
	vllmReadyTimeout          = 180 * time.Second
	vllmRestartCooldown       = 90 * time.Second
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

func (d *vllmDriver) IsInstalled() bool {
	if d.shouldUseContainer() {
		return d.ensureDockerAvailable() == nil
	}
	if runCommand(vllmPythonPath, "-c", "import vllm") == nil {
		return true
	}
	// Backward compatibility for legacy installs outside the managed venv.
	return runCommand("python3", "-c", "import vllm") == nil
}

func (d *vllmDriver) IsReady() bool {
	if !d.IsInstalled() {
		return false
	}
	_, err := d.fetchRemoteModels()
	return err == nil
}

func (d *vllmDriver) Version() string {
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

func (d *vllmDriver) EnsureModelWithFlags(modelID string, _ *types.ModelFeatureFlags) error {
	if strings.TrimSpace(modelID) == "" {
		return fmt.Errorf("model ID is required")
	}
	if err := d.writeConfig(vllmConfig{Model: modelID, Port: 8000}); err != nil {
		return err
	}
	return d.startOrRestartService(modelID, 8000, false)
}

func (d *vllmDriver) ListModels() []string {
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

func (d *vllmDriver) InstalledModels() []types.InstalledModel {
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
				Runtime: types.RuntimeVLLM,
				Status:  types.ModelReady,
			})
		}
		if configuredModel != "" {
			if _, ok := seen[configuredModel]; !ok {
				models = append(models, types.InstalledModel{
					ModelID: configuredModel,
					Runtime: types.RuntimeVLLM,
					Status:  types.ModelInstalling,
				})
			}
		}
		return models
	}

	// Endpoint unreachable: preserve configured model but mark failed so server/UI
	// can show actionable state instead of a false-ready model.
	if configuredModel != "" {
		models = append(models, types.InstalledModel{
			ModelID: configuredModel,
			Runtime: types.RuntimeVLLM,
			Status:  types.ModelFailed,
		})
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
	cfg, cfgErr := d.readConfig()
	if cfgErr == nil && strings.TrimSpace(cfg.Model) != "" {
		port := cfg.Port
		if port <= 0 {
			port = 8000
		}
		return d.startOrRestartService(cfg.Model, port, true)
	}
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
ExecStart=/bin/sh -c 'LD_LIBRARY_PATH="${VLLM_LD_LIBRARY_PATH:-}${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}"; EXTRA_ARGS="${VLLM_EXTRA_ARGS:-}"; if [ "${VLLM_TRUST_REMOTE_CODE:-false}" = "true" ]; then EXTRA_ARGS="${EXTRA_ARGS} --trust-remote-code"; fi; exec ` + vllmPythonPath + ` -m vllm.entrypoints.openai.api_server --model "${VLLM_MODEL}" --host 0.0.0.0 --port "${VLLM_PORT:-8000}" --gpu-memory-utilization "${VLLM_GPU_MEMORY_UTILIZATION:-0.9}" ${EXTRA_ARGS}'
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
ExecStart=/bin/sh -c 'DOCKER_BIN="$(command -v docker)"; [ -n "$DOCKER_BIN" ] || exit 1; EXTRA_ARGS="${VLLM_EXTRA_ARGS:-}"; if [ "${VLLM_TRUST_REMOTE_CODE:-false}" = "true" ]; then EXTRA_ARGS="${EXTRA_ARGS} --trust-remote-code"; fi; exec "$DOCKER_BIN" run --rm --name ` + vllmContainerName + ` --gpus all --ipc=host --network host -e HF_TOKEN="${HF_TOKEN:-}" -e HUGGING_FACE_HUB_TOKEN="${HUGGING_FACE_HUB_TOKEN:-}" -e NVIDIA_VISIBLE_DEVICES=all -e NVIDIA_DRIVER_CAPABILITIES=compute,utility -v /root/.cache/huggingface:/root/.cache/huggingface "${VLLM_CONTAINER_IMAGE:-` + vllmDefaultContainerImage + `}" vllm serve "${VLLM_MODEL}" --host 0.0.0.0 --port "${VLLM_PORT:-8000}" --gpu-memory-utilization "${VLLM_GPU_MEMORY_UTILIZATION:-0.9}" ${EXTRA_ARGS}'
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
	extraArgs := strings.TrimSpace(existingEnv["VLLM_EXTRA_ARGS"])
	if existingLibrary := strings.TrimSpace(existingEnv["VLLM_LD_LIBRARY_PATH"]); existingLibrary != "" {
		libraryPath = existingLibrary
	}
	containerImage := strings.TrimSpace(existingEnv["VLLM_CONTAINER_IMAGE"])
	if containerImage == "" {
		containerImage = d.containerImage()
	}
	if d.shouldUseContainer() {
		if err := d.pullContainerImage(containerImage); err != nil {
			return err
		}
	}
	runtimeMode := d.runtimeMode()
	envContent := fmt.Sprintf("VLLM_MODEL=%q\nVLLM_PORT=%d\nVLLM_GPU_MEMORY_UTILIZATION=0.9\nVLLM_TRUST_REMOTE_CODE=%s\nVLLM_EXTRA_ARGS=%q\nVLLM_RUNTIME_MODE=%q\n",
		safeModelID,
		port,
		trustRemoteCode,
		extraArgs,
		runtimeMode,
	)
	if d.shouldUseContainer() {
		envContent += fmt.Sprintf("VLLM_CONTAINER_IMAGE=%q\n", containerImage)
	} else {
		envContent += fmt.Sprintf("VLLM_LD_LIBRARY_PATH=%q\n", libraryPath)
	}
	if err := os.WriteFile(vllmEnvPath, []byte(envContent), 0o600); err != nil {
		return fmt.Errorf("write vllm env config: %w", err)
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
	if err := d.waitForAPIReady(vllmReadyTimeout); err != nil {
		diagnostics := d.vllmDiagnosticsTail()
		if d.shouldAutoEnableTrustRemoteCode(diagnostics) {
			if trustErr := d.enableTrustRemoteCodeInEnv(); trustErr == nil {
				if restartErr := runCommand("systemctl", "restart", "vllm"); restartErr == nil {
					if readyErr := d.waitForAPIReady(vllmReadyTimeout); readyErr == nil {
						return nil
					}
					diagnostics = d.vllmDiagnosticsTail()
				}
			}
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

func (d *vllmDriver) shouldAutoEnableTrustRemoteCode(diagnostics string) bool {
	text := strings.ToLower(diagnostics)
	return strings.Contains(text, "trust_remote_code") ||
		strings.Contains(text, "trust-remote-code") ||
		strings.Contains(text, "please pass the argument `trust_remote_code=true`")
}

func (d *vllmDriver) enableTrustRemoteCodeInEnv() error {
	values := d.readEnvConfigMap()
	if strings.EqualFold(strings.TrimSpace(values["VLLM_TRUST_REMOTE_CODE"]), "true") {
		return nil
	}
	values["VLLM_TRUST_REMOTE_CODE"] = "true"

	order := []string{
		"VLLM_MODEL",
		"VLLM_PORT",
		"VLLM_GPU_MEMORY_UTILIZATION",
		"VLLM_TRUST_REMOTE_CODE",
		"VLLM_EXTRA_ARGS",
		"VLLM_RUNTIME_MODE",
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
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vllm models endpoint failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, _ := io.ReadAll(resp.Body)
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
	out, err := exec.Command("journalctl", "-u", "vllm", "-n", "25", "--no-pager").CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
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
			if (strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"")) ||
				(strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'")) {
				val = val[1 : len(val)-1]
			}
		}
		values[key] = val
	}
	return values
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
	case strings.Contains(text, "libcudart.so.12"):
		return "hint: CUDA runtime libraries are missing from loader path; install nvidia-cuda-runtime-cu12 and set VLLM_LD_LIBRARY_PATH"
	case strings.Contains(text, "libtorch_cuda.so"):
		return "hint: CUDA-enabled torch is missing/incompatible; ensure torch is not +cpu and matches vLLM wheel CUDA variant"
	case strings.Contains(text, "undefined symbol") && strings.Contains(text, "vllm/_c"):
		return "hint: torch/vllm ABI mismatch detected; reinstall matching torch and vllm versions from the same CUDA wheel family"
	case strings.Contains(text, "modelopt currently only supports") && strings.Contains(text, "mixed_precision"):
		return "hint: this model uses ModelOpt MIXED_PRECISION not supported by current vLLM build; upgrade vLLM container/wheel variant"
	default:
		return ""
	}
}
