package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

type Manager struct {
	drivers map[string]Driver
	outcome func(event types.OutcomeEvent)

	outcomeContextMu        sync.Mutex
	activePlanID            string
	activeMantleFingerprint string
}

func NewManager() *Manager {
	return &Manager{drivers: NewDriverRegistry()}
}

func (m *Manager) SetOutcomeReporter(reporter func(event types.OutcomeEvent)) {
	m.outcome = reporter
}

func (m *Manager) SetActiveContext(planID string, mantleFingerprint string) {
	m.outcomeContextMu.Lock()
	m.activePlanID = strings.TrimSpace(planID)
	m.activeMantleFingerprint = strings.TrimSpace(mantleFingerprint)
	m.outcomeContextMu.Unlock()
}

func (m *Manager) ClearActiveContext() {
	m.outcomeContextMu.Lock()
	m.activePlanID = ""
	m.activeMantleFingerprint = ""
	m.outcomeContextMu.Unlock()
}

func (m *Manager) emitOutcome(event types.OutcomeEvent) {
	if m.outcome == nil || strings.TrimSpace(event.EventType) == "" {
		return
	}
	m.outcomeContextMu.Lock()
	planID := m.activePlanID
	mantleFingerprint := m.activeMantleFingerprint
	m.outcomeContextMu.Unlock()
	if strings.TrimSpace(event.PlanID) == "" {
		event.PlanID = planID
	}
	if strings.TrimSpace(event.MantleFingerprint) == "" {
		event.MantleFingerprint = mantleFingerprint
	}
	if strings.TrimSpace(event.Timestamp) == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	m.outcome(event)
}

func (m *Manager) DriverFor(runtimeName string) (Driver, error) {
	return m.driverFor(runtimeName)
}

func (m *Manager) driverFor(runtimeName string) (Driver, error) {
	driver, ok := m.drivers[strings.ToLower(strings.TrimSpace(runtimeName))]
	if !ok {
		return nil, fmt.Errorf("unsupported runtime: %s", runtimeName)
	}
	return driver, nil
}

func (m *Manager) InstalledRuntimes() []string {
	runtimes := make([]string, 0, len(m.drivers))
	for name, driver := range m.drivers {
		if driver.IsInstalled() {
			runtimes = append(runtimes, name)
		}
	}
	sort.Strings(runtimes)
	return runtimes
}

func (m *Manager) ReadyRuntimes() []string {
	runtimes := make([]string, 0, len(m.drivers))
	for name, driver := range m.drivers {
		if !driver.IsInstalled() {
			continue
		}
		if driver.IsReady() {
			runtimes = append(runtimes, name)
		}
	}
	sort.Strings(runtimes)
	return runtimes
}

func (m *Manager) RuntimeVersion(runtimeName string) string {
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return ""
	}
	return driver.Version()
}

func (m *Manager) RuntimeVersions() map[types.RuntimeType]string {
	versions := map[types.RuntimeType]string{}
	for runtimeName, driver := range m.drivers {
		if !driver.IsInstalled() {
			continue
		}
		version := strings.TrimSpace(driver.Version())
		if version == "" {
			continue
		}
		versions[types.RuntimeType(runtimeName)] = version
	}
	return versions
}

func (m *Manager) RuntimeConfigs() map[types.RuntimeType]map[string]any {
	configs := map[types.RuntimeType]map[string]any{}
	for runtimeName, driver := range m.drivers {
		configurable, ok := driver.(ConfigurableDriver)
		if !ok {
			continue
		}
		cfg := configurable.RuntimeConfig()
		if len(cfg) == 0 {
			continue
		}
		configs[types.RuntimeType(runtimeName)] = cfg
	}
	return configs
}

func (m *Manager) InstallRuntime(runtimeName string) error {
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return err
	}
	return driver.Install()
}

func (m *Manager) UninstallRuntime(runtimeName string) error {
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return err
	}
	return driver.Uninstall()
}

func (m *Manager) IsRuntimeInstalled(runtimeName string) bool {
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return false
	}
	return driver.IsInstalled()
}

func (m *Manager) IsRuntimeReady(runtimeName string) bool {
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return false
	}
	if !driver.IsInstalled() {
		return false
	}
	return driver.IsReady()
}

func (m *Manager) EnsureRuntime(runtimeName string) error {
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return err
	}
	if driver.IsInstalled() {
		if driver.IsReady() {
			return nil
		}
		return driver.RestartRuntime()
	}
	return driver.Install()
}

func (m *Manager) preferredDriverForModel(modelID string) (Driver, error) {
	trimmedModel := strings.TrimSpace(modelID)
	if trimmedModel == "" {
		return nil, fmt.Errorf("model ID is required")
	}

	// HuggingFace-style IDs are usually served by vLLM.
	if strings.Contains(trimmedModel, "/") && m.IsRuntimeInstalled("vllm") {
		return m.driverFor("vllm")
	}
	// Ollama tags often contain ":".
	if strings.Contains(trimmedModel, ":") && m.IsRuntimeInstalled("ollama") {
		return m.driverFor("ollama")
	}

	// Fallback to first installed runtime.
	installed := m.InstalledRuntimes()
	if len(installed) > 0 {
		return m.driverFor(installed[0])
	}
	return nil, fmt.Errorf("no installed runtime available")
}

func (m *Manager) EnsureModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	// Backward-compatible behavior: ensure means prepare+start.
	return m.StartModelWithFlags(modelID, flags)
}

func (m *Manager) EnsureModelWithRuntime(modelID string, runtimeName string, flags *types.ModelFeatureFlags) error {
	// Backward-compatible behavior: ensure means prepare+start.
	return m.StartModelWithRuntime(modelID, runtimeName, flags)
}

func (m *Manager) PrepareModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	trimmedModel := strings.TrimSpace(modelID)
	if trimmedModel == "" {
		return fmt.Errorf("model ID is required")
	}

	if strings.Contains(trimmedModel, "/") {
		if err := m.EnsureRuntime("vllm"); err != nil {
			return fmt.Errorf("ensure vllm runtime: %w", err)
		}
		driver, err := m.driverFor("vllm")
		if err != nil {
			return err
		}
		return driver.PrepareModelWithFlags(trimmedModel, flags)
	}
	if strings.Contains(trimmedModel, ":") {
		if err := m.EnsureRuntime("ollama"); err == nil {
			driver, drvErr := m.driverFor("ollama")
			if drvErr == nil {
				return driver.PrepareModelWithFlags(trimmedModel, flags)
			}
		}
	}

	driver, err := m.preferredDriverForModel(trimmedModel)
	if err != nil {
		return err
	}
	return driver.PrepareModelWithFlags(trimmedModel, flags)
}

func (m *Manager) PrepareModelWithRuntime(modelID string, runtimeName string, flags *types.ModelFeatureFlags) error {
	return m.PrepareModelWithRuntimeCtx(context.Background(), modelID, runtimeName, flags)
}

// PrepareModelWithRuntimeCtx prepares a model with cancellation support.
func (m *Manager) PrepareModelWithRuntimeCtx(ctx context.Context, modelID string, runtimeName string, flags *types.ModelFeatureFlags) error {
	if strings.TrimSpace(runtimeName) == "" {
		return m.PrepareModelWithFlagsCtx(ctx, modelID, flags)
	}
	normalizedRuntime := strings.ToLower(strings.TrimSpace(runtimeName))
	if err := m.EnsureRuntime(normalizedRuntime); err != nil {
		return fmt.Errorf("ensure runtime %s: %w", normalizedRuntime, err)
	}
	driver, err := m.driverFor(normalizedRuntime)
	if err != nil {
		return err
	}
	// Use context-aware method if available
	if cancellable, ok := driver.(CancellableDriver); ok {
		return cancellable.PrepareModelWithFlagsCtx(ctx, modelID, flags)
	}
	return driver.PrepareModelWithFlags(modelID, flags)
}

// PrepareModelWithFlagsCtx prepares a model with cancellation support (no explicit runtime).
func (m *Manager) PrepareModelWithFlagsCtx(ctx context.Context, modelID string, flags *types.ModelFeatureFlags) error {
	trimmedModel := strings.TrimSpace(modelID)
	if trimmedModel == "" {
		return fmt.Errorf("model ID is required")
	}

	if strings.Contains(trimmedModel, "/") {
		if err := m.EnsureRuntime("vllm"); err != nil {
			return fmt.Errorf("ensure vllm runtime: %w", err)
		}
		driver, err := m.driverFor("vllm")
		if err != nil {
			return err
		}
		if cancellable, ok := driver.(CancellableDriver); ok {
			return cancellable.PrepareModelWithFlagsCtx(ctx, trimmedModel, flags)
		}
		return driver.PrepareModelWithFlags(trimmedModel, flags)
	}
	if strings.Contains(trimmedModel, ":") {
		if err := m.EnsureRuntime("ollama"); err == nil {
			driver, drvErr := m.driverFor("ollama")
			if drvErr == nil {
				if cancellable, ok := driver.(CancellableDriver); ok {
					return cancellable.PrepareModelWithFlagsCtx(ctx, trimmedModel, flags)
				}
				return driver.PrepareModelWithFlags(trimmedModel, flags)
			}
		}
	}

	driver, err := m.preferredDriverForModel(trimmedModel)
	if err != nil {
		return err
	}
	if cancellable, ok := driver.(CancellableDriver); ok {
		return cancellable.PrepareModelWithFlagsCtx(ctx, trimmedModel, flags)
	}
	return driver.PrepareModelWithFlags(trimmedModel, flags)
}

func (m *Manager) StartModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	startedAt := time.Now()
	if err := m.PrepareModelWithFlags(modelID, flags); err != nil {
		m.emitOutcome(types.OutcomeEvent{
			EventType:      "startup_failure",
			DurationMs:     time.Since(startedAt).Milliseconds(),
			CrashSignature: "prepare_failed",
			Detail:         err.Error(),
		})
		return err
	}
	trimmedModel := strings.TrimSpace(modelID)
	driver, err := m.preferredDriverForModel(trimmedModel)
	if err != nil {
		m.emitOutcome(types.OutcomeEvent{
			EventType:      "startup_failure",
			DurationMs:     time.Since(startedAt).Milliseconds(),
			CrashSignature: "driver_not_found",
			Detail:         err.Error(),
		})
		return err
	}
	startErr := driver.StartModelWithFlags(trimmedModel, flags)
	m.emitOutcome(types.OutcomeEvent{
		EventType:  map[bool]string{true: "startup_success", false: "startup_failure"}[startErr == nil],
		DurationMs: time.Since(startedAt).Milliseconds(),
		Detail: func() string {
			if startErr != nil {
				return startErr.Error()
			}
			return "model startup succeeded"
		}(),
	})
	return startErr
}

func (m *Manager) StartModelWithRuntime(modelID string, runtimeName string, flags *types.ModelFeatureFlags) error {
	startedAt := time.Now()
	if strings.TrimSpace(runtimeName) == "" {
		return m.StartModelWithFlags(modelID, flags)
	}
	normalizedRuntime := strings.ToLower(strings.TrimSpace(runtimeName))
	if err := m.EnsureRuntime(normalizedRuntime); err != nil {
		m.emitOutcome(types.OutcomeEvent{
			EventType:      "startup_failure",
			DurationMs:     time.Since(startedAt).Milliseconds(),
			CrashSignature: "runtime_not_ready",
			Detail:         err.Error(),
		})
		return fmt.Errorf("ensure runtime %s: %w", normalizedRuntime, err)
	}
	driver, err := m.driverFor(normalizedRuntime)
	if err != nil {
		m.emitOutcome(types.OutcomeEvent{
			EventType:      "startup_failure",
			DurationMs:     time.Since(startedAt).Milliseconds(),
			CrashSignature: "driver_not_found",
			Detail:         err.Error(),
		})
		return err
	}
	if err := driver.PrepareModelWithFlags(modelID, flags); err != nil {
		m.emitOutcome(types.OutcomeEvent{
			EventType:      "startup_failure",
			DurationMs:     time.Since(startedAt).Milliseconds(),
			CrashSignature: "prepare_failed",
			Detail:         err.Error(),
		})
		return err
	}
	startErr := driver.StartModelWithFlags(modelID, flags)
	m.emitOutcome(types.OutcomeEvent{
		EventType:  map[bool]string{true: "startup_success", false: "startup_failure"}[startErr == nil],
		DurationMs: time.Since(startedAt).Milliseconds(),
		Detail: func() string {
			if startErr != nil {
				return startErr.Error()
			}
			return "model startup succeeded"
		}(),
	})
	return startErr
}

func (m *Manager) ListModels() []string {
	set := map[string]struct{}{}
	for _, runtimeName := range m.InstalledRuntimes() {
		driver, err := m.driverFor(runtimeName)
		if err != nil {
			continue
		}
		for _, model := range driver.ListModels() {
			if strings.TrimSpace(model) != "" {
				set[model] = struct{}{}
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

func (m *Manager) RemoveModel(modelID string) error {
	driver, err := m.preferredDriverForModel(modelID)
	if err != nil {
		return err
	}
	return driver.RemoveModel(modelID)
}

func (m *Manager) RemoveModelWithRuntime(modelID string, runtimeName string) error {
	if strings.TrimSpace(runtimeName) == "" {
		return m.RemoveModel(modelID)
	}
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return err
	}
	return driver.RemoveModel(modelID)
}

func (m *Manager) StopModelWithRuntime(modelID string, runtimeName string) error {
	if strings.TrimSpace(runtimeName) == "" {
		driver, err := m.preferredDriverForModel(modelID)
		if err != nil {
			return err
		}
		return driver.StopModel(modelID)
	}
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return err
	}
	return driver.StopModel(modelID)
}

// BuildModel compiles a TensorRT engine from downloaded weights.
// Only works with runtimes that implement BuildableDriver (currently tensorrt).
func (m *Manager) BuildModel(ctx context.Context, modelID string, opts BuildOptions) error {
	// Build is only supported for TensorRT runtime
	driver, err := m.driverFor("tensorrt")
	if err != nil {
		return fmt.Errorf("tensorrt runtime not available: %w", err)
	}
	buildable, ok := driver.(BuildableDriver)
	if !ok {
		return fmt.Errorf("tensorrt driver does not support build operations")
	}
	return buildable.BuildModel(ctx, modelID, opts)
}

// IsModelBuilt checks if a TensorRT engine exists for the given model.
func (m *Manager) IsModelBuilt(modelID string) bool {
	driver, err := m.driverFor("tensorrt")
	if err != nil {
		return false
	}
	buildable, ok := driver.(BuildableDriver)
	if !ok {
		return false
	}
	return buildable.IsModelBuilt(modelID)
}

func (m *Manager) BenchmarkModel(
	modelID string,
	samplePromptTokens int,
	sampleOutputTokens int,
	concurrency int,
	runs int,
	onProgress func(BenchmarkProgress),
) (BenchmarkResult, error) {
	driver, err := m.preferredDriverForModel(modelID)
	if err != nil {
		return BenchmarkResult{}, err
	}
	return driver.BenchmarkModel(modelID, samplePromptTokens, sampleOutputTokens, concurrency, runs, onProgress)
}

func (m *Manager) RestartRuntime() error {
	var lastErr error
	restarted := false
	for _, runtimeName := range m.InstalledRuntimes() {
		driver, err := m.driverFor(runtimeName)
		if err != nil {
			lastErr = err
			continue
		}
		if err := driver.RestartRuntime(); err != nil {
			lastErr = err
			continue
		}
		restarted = true
	}
	if restarted {
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no installed runtime to restart")
}

func (m *Manager) RestartRuntimeNamed(runtimeName string) error {
	normalizedRuntime := strings.ToLower(strings.TrimSpace(runtimeName))
	if normalizedRuntime == "" {
		return fmt.Errorf("runtime name is required")
	}
	driver, err := m.driverFor(normalizedRuntime)
	if err != nil {
		return err
	}
	if !driver.IsInstalled() {
		if err := driver.Install(); err != nil {
			return fmt.Errorf("ensure runtime %s: %w", normalizedRuntime, err)
		}
	}
	return driver.RestartRuntime()
}
