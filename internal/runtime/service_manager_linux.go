package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type systemdServiceManager struct{}

func (m *systemdServiceManager) Install(name string, execStart string, env map[string]string) error {
	unitPath := filepath.Join("/etc/systemd/system", name+".service")
	if os.Geteuid() != 0 {
		if fileExists(unitPath) {
			return nil
		}
		return fmt.Errorf("write systemd unit %s: requires root (run `mantler runtime install %s` as root first)", unitPath, name)
	}
	lines := []string{
		"[Unit]",
		"Description=" + name,
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		"ExecStart=" + strings.TrimSpace(execStart),
		"Restart=always",
		"RestartSec=5",
	}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for key := range env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lines = append(lines, fmt.Sprintf(`Environment="%s=%s"`, key, strings.ReplaceAll(env[key], `"`, `\"`)))
		}
	}
	lines = append(lines, "", "[Install]", "WantedBy=multi-user.target", "")
	if err := os.WriteFile(unitPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return fmt.Errorf("write systemd unit %s: %w", unitPath, err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	return runSystemctl("enable", name)
}

func (m *systemdServiceManager) Start(name string) error   { return runSystemctl("start", name) }
func (m *systemdServiceManager) Stop(name string) error    { return runSystemctl("stop", name) }
func (m *systemdServiceManager) Restart(name string) error { return runSystemctl("restart", name) }

func (m *systemdServiceManager) IsActive(name string) (bool, error) {
	cmd := exec.Command("systemctl", "is-active", name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		state := strings.TrimSpace(string(output))
		if state == "inactive" || state == "failed" || state == "deactivating" {
			return false, nil
		}
		return false, fmt.Errorf("systemctl is-active %s failed: %w (%s)", name, err, state)
	}
	return strings.TrimSpace(string(output)) == "active", nil
}

func (m *systemdServiceManager) Uninstall(name string) error {
	_ = runSystemctl("stop", name)
	_ = runSystemctl("disable", name)
	_ = os.Remove(filepath.Join("/etc/systemd/system", name+".service"))
	return runSystemctl("daemon-reload")
}

func (m *systemdServiceManager) Logs(name string, lines int) (string, error) {
	if lines <= 0 {
		lines = 120
	}
	cmd := exec.Command("journalctl", "-u", name, "-n", fmt.Sprintf("%d", lines), "--no-pager")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("journalctl logs for %s: %w", name, err)
	}
	return string(output), nil
}
