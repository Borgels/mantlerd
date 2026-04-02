package runtime

import (
	"fmt"
	"os/exec"
	"strings"
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

func (m *Manager) PullModel(modelID string) error {
	if modelID == "" {
		return fmt.Errorf("model ID is required")
	}
	return m.run("ollama", "pull", modelID)
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
