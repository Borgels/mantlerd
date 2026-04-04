package runtime

import (
	"fmt"
	"os/exec"
	"strconv"
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

func isServiceListeningOnNonLoopback(port int) (bool, error) {
	if port <= 0 {
		return false, fmt.Errorf("invalid port: %d", port)
	}
	cmd := exec.Command("sh", "-c", "ss -ltnH '( sport = :"+strconv.Itoa(port)+" )' || true")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("check listen sockets on port %d: %w (%s)", port, err, strings.TrimSpace(string(output)))
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	seenAny := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		localAddr := fields[3]
		if localAddr == "" {
			continue
		}
		seenAny = true
		if !isLoopbackSocketAddress(localAddr) {
			return true, nil
		}
	}
	if !seenAny {
		return false, nil
	}
	return false, nil
}

func isLoopbackSocketAddress(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "127.0.0.1:") || strings.HasPrefix(value, "[::1]:")
}
