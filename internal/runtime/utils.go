package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/Borgels/mantlerd/internal/discovery"
)

const modelFailReasonInsufficientMemory = "insufficient_memory"

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// runSystemctl runs a systemctl command, using sudo -n when not running as root.
// sudo -n fails immediately if a password would be required; install.sh sets up
// a NOPASSWD sudoers rule for the mantler group covering these operations.
func runSystemctl(args ...string) error {
	if os.Geteuid() == 0 {
		return runCommand("systemctl", args...)
	}
	return runCommand("sudo", append([]string{"-n", "systemctl"}, args...)...)
}

func isLikelyOutOfMemoryError(err error) bool {
	if err == nil {
		return false
	}
	return containsOOMSignal(err.Error())
}

func serviceLikelyOutOfMemory(serviceName string, cause error) bool {
	if isLikelyOutOfMemoryError(cause) {
		return true
	}
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		return false
	}
	out, err := NewServiceManager().Logs(serviceName, 120)
	if err != nil && strings.TrimSpace(out) == "" {
		return false
	}
	return containsOOMSignal(out)
}

func containsOOMSignal(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return false
	}
	signals := []string{
		"out of memory",
		"cuda out of memory",
		"torch.cuda.outofmemoryerror",
		"std::bad_alloc",
		"resourceexhausted",
		"resource exhausted",
		"not enough memory",
		"insufficient memory",
		"failed to allocate memory",
		"oom-kill",
		"killed process",
	}
	for _, signal := range signals {
		if strings.Contains(normalized, signal) {
			return true
		}
	}
	return false
}

func isSystemdServiceActive(serviceName string) (bool, error) {
	return NewServiceManager().IsActive(serviceName)
}

func isServiceListeningOnNonLoopback(port int) (bool, error) {
	if port <= 0 {
		return false, fmt.Errorf("invalid port: %d", port)
	}
	if runtime.GOOS == "darwin" {
		cmd := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN")
		output, err := cmd.CombinedOutput()
		if err != nil && len(strings.TrimSpace(string(output))) == 0 {
			return false, nil
		}
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(strings.ToUpper(line), "COMMAND ") {
				continue
			}
			if strings.Contains(line, "127.0.0.1:"+strconv.Itoa(port)) ||
				strings.Contains(line, "localhost:"+strconv.Itoa(port)) ||
				strings.Contains(line, "[::1]:"+strconv.Itoa(port)) {
				continue
			}
			if strings.Contains(line, ":"+strconv.Itoa(port)+" ") ||
				strings.HasSuffix(line, ":"+strconv.Itoa(port)) {
				return true, nil
			}
		}
		return false, nil
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

func flushUMAPageCache() error {
	if runtime.GOOS != "linux" || !discovery.IsDGXSpark() {
		return nil
	}
	if os.Geteuid() == 0 {
		return runCommand("sh", "-c", "sync; echo 3 > /proc/sys/vm/drop_caches")
	}
	return runCommand("sudo", "-n", "sh", "-c", "sync; echo 3 > /proc/sys/vm/drop_caches")
}
