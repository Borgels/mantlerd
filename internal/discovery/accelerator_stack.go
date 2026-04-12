package discovery

import (
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/Borgels/mantlerd/internal/types"
)

var versionRegex = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)

func CollectAcceleratorStack(gpuVendor string) *types.AcceleratorStackReport {
	stack := &types.AcceleratorStackReport{}
	vendor := strings.ToLower(strings.TrimSpace(gpuVendor))

	if vendor == "nvidia" || vendor == "mixed" {
		stack.NvidiaDriverVersion = commandVersionField("nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader")
		stack.CudaToolkitVersion = parseNvccVersion(readNvccVersion())
		stack.CudaRuntimeVersion = parseCudaRuntimeVersion(commandOutput("nvidia-smi"))
		stack.CudnnVersion = commandVersionField("bash", "-lc", "ldconfig -p 2>/dev/null | rg libcudnn | head -n 1")
		stack.NcclVersion = commandVersionField("bash", "-lc", "ldconfig -p 2>/dev/null | rg libnccl | head -n 1")
		stack.NvidiaToolkit = commandVersionField("nvidia-ctk", "--version")
	}

	if vendor == "amd" || vendor == "mixed" {
		stack.RocmVersion = firstVersion(commandOutput("rocminfo"))
		stack.HipRuntimeVersion = firstVersion(commandOutput("hipcc", "--version"))
		stack.MiopenVersion = commandVersionField("bash", "-lc", "ldconfig -p 2>/dev/null | rg libMIOpen | head -n 1")
		stack.RcclVersion = commandVersionField("bash", "-lc", "ldconfig -p 2>/dev/null | rg librccl | head -n 1")
	}

	stack.ContainerRuntime = strings.TrimSpace(commandOutput("docker", "--version"))
	if isEmptyAcceleratorStack(stack) {
		return nil
	}
	return stack
}

func isEmptyAcceleratorStack(stack *types.AcceleratorStackReport) bool {
	if stack == nil {
		return true
	}
	return strings.TrimSpace(stack.NvidiaDriverVersion) == "" &&
		strings.TrimSpace(stack.CudaToolkitVersion) == "" &&
		strings.TrimSpace(stack.CudaRuntimeVersion) == "" &&
		strings.TrimSpace(stack.CudnnVersion) == "" &&
		strings.TrimSpace(stack.NcclVersion) == "" &&
		strings.TrimSpace(stack.RocmVersion) == "" &&
		strings.TrimSpace(stack.HipRuntimeVersion) == "" &&
		strings.TrimSpace(stack.MiopenVersion) == "" &&
		strings.TrimSpace(stack.RcclVersion) == "" &&
		strings.TrimSpace(stack.ContainerRuntime) == "" &&
		strings.TrimSpace(stack.NvidiaToolkit) == ""
}

func commandOutput(command string, args ...string) string {
	if _, err := exec.LookPath(command); err != nil {
		return ""
	}
	output, err := exec.Command(command, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func readNvccVersion() string {
	if output := commandOutput("nvcc", "--version"); strings.TrimSpace(output) != "" {
		return output
	}
	candidates := []string{
		"/usr/local/cuda/bin/nvcc",
		"/opt/cuda/bin/nvcc",
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			if output := commandOutput(candidate, "--version"); strings.TrimSpace(output) != "" {
				return output
			}
		}
	}
	return ""
}

func commandVersionField(command string, args ...string) string {
	output := commandOutput(command, args...)
	if output == "" {
		return ""
	}
	return normalizeFirstLine(output)
}

func normalizeFirstLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstVersion(value string) string {
	match := versionRegex.FindString(value)
	return strings.TrimSpace(match)
}

func parseNvccVersion(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if !strings.Contains(strings.ToLower(line), "release") {
			continue
		}
		if idx := strings.Index(strings.ToLower(line), "release"); idx >= 0 {
			return strings.TrimSpace(strings.TrimPrefix(line[idx:], "release"))
		}
	}
	return firstVersion(value)
}

func parseCudaRuntimeVersion(value string) string {
	for _, line := range strings.Split(value, "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "cuda version") {
			continue
		}
		if idx := strings.Index(lower, "cuda version"); idx >= 0 {
			segment := line[idx:]
			if match := versionRegex.FindString(segment); match != "" {
				return match
			}
		}
	}
	return ""
}
