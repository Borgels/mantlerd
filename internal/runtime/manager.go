package runtime

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Borgels/clawcontrol-agent/internal/types"
)

type Manager struct {
	drivers map[string]Driver
}

func NewManager() *Manager {
	drivers := map[string]Driver{
		"ollama": newOllamaDriver(),
		"vllm":   newVLLMDriver(),
	}
	return &Manager{drivers: drivers}
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

func (m *Manager) InstallRuntime(runtimeName string) error {
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return err
	}
	return driver.Install()
}

func (m *Manager) IsRuntimeInstalled(runtimeName string) bool {
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return false
	}
	return driver.IsInstalled()
}

func (m *Manager) EnsureRuntime(runtimeName string) error {
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return err
	}
	if driver.IsInstalled() {
		return nil
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
	driver, err := m.preferredDriverForModel(modelID)
	if err != nil {
		return err
	}
	return driver.EnsureModelWithFlags(modelID, flags)
}

func (m *Manager) EnsureModelWithRuntime(modelID string, runtimeName string, flags *types.ModelFeatureFlags) error {
	if strings.TrimSpace(runtimeName) == "" {
		return m.EnsureModelWithFlags(modelID, flags)
	}
	driver, err := m.driverFor(runtimeName)
	if err != nil {
		return err
	}
	return driver.EnsureModelWithFlags(modelID, flags)
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
