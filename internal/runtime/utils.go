package runtime

import (
	"fmt"
	"os/exec"
	"strings"
)

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func isSystemdServiceActive(serviceName string) (bool, error) {
	cmd := exec.Command("systemctl", "is-active", serviceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		state := strings.TrimSpace(string(output))
		if state == "inactive" || state == "failed" || state == "deactivating" {
			return false, nil
		}
		return false, fmt.Errorf("systemctl is-active %s failed: %w (%s)", serviceName, err, state)
	}
	return strings.TrimSpace(string(output)) == "active", nil
}
