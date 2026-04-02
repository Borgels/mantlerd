package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

func (m *Manager) HasModel(modelID string) bool {
	for _, model := range m.ListModels() {
		if model == modelID {
			return true
		}
	}
	return false
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
