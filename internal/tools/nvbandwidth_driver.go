package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type nvbandwidthDriver struct{}

func newNvBandwidthDriver() Driver { return &nvbandwidthDriver{} }

func (d *nvbandwidthDriver) Name() string { return "nvbandwidth" }

func (d *nvbandwidthDriver) Install() error {
	if d.IsInstalled() {
		return nil
	}
	nvccBinary := resolveNvccBinary()
	if nvccBinary == "" {
		return fmt.Errorf("nvcc is required to build nvbandwidth")
	}
	if !commandExists("git") || !commandExists("cmake") {
		return fmt.Errorf("git and cmake are required to build nvbandwidth")
	}

	installScript := fmt.Sprintf(`
set -euo pipefail
export PATH="%s:$PATH"
export CUDACXX="%s"
if ! dpkg -s libboost-program-options-dev >/dev/null 2>&1; then
  if [ "$(id -u)" -eq 0 ]; then
    apt-get update
    apt-get install -y libboost-program-options-dev
  elif command -v sudo >/dev/null 2>&1; then
    sudo apt-get update
    sudo apt-get install -y libboost-program-options-dev
  else
    echo "libboost-program-options-dev is missing and sudo is unavailable"
    exit 1
  fi
fi
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
git clone --depth 1 https://github.com/NVIDIA/nvbandwidth.git "$tmpdir/nvbandwidth"
cmake -S "$tmpdir/nvbandwidth" -B "$tmpdir/nvbandwidth/build" -DCMAKE_BUILD_TYPE=Release
cmake --build "$tmpdir/nvbandwidth/build" -j"$(nproc)"
install -Dm755 "$tmpdir/nvbandwidth/build/nvbandwidth" "$HOME/.local/bin/nvbandwidth"
`, filepath.Dir(nvccBinary), nvccBinary)

	if _, err := commandOutput(12*time.Minute, "bash", "-lc", installScript); err != nil {
		return fmt.Errorf("install nvbandwidth: %w", err)
	}
	if !d.IsInstalled() {
		return fmt.Errorf("nvbandwidth install completed but binary was not found")
	}
	return nil
}

func (d *nvbandwidthDriver) Uninstall() error {
	localPath := nvbandwidthUserBinaryPath()
	if binaryLooksUsable(localPath) {
		if err := os.Remove(localPath); err != nil {
			return fmt.Errorf("remove %s: %w", localPath, err)
		}
		return nil
	}
	if commandExists("nvbandwidth") {
		return fmt.Errorf("%w: nvbandwidth exists outside ~/.local/bin and is not managed by mantlerd", ErrNotImplemented)
	}
	return nil
}

func (d *nvbandwidthDriver) IsInstalled() bool {
	if commandExists("nvbandwidth") {
		return true
	}
	return binaryLooksUsable(nvbandwidthUserBinaryPath())
}

func (d *nvbandwidthDriver) IsReady() bool { return d.IsInstalled() }

func (d *nvbandwidthDriver) Version() string {
	binary := resolveNvBandwidthBinary()
	if binary == "" {
		return ""
	}
	version := commandVersion(binary, "--version")
	if strings.TrimSpace(version) != "" {
		return version
	}
	return commandVersion(binary)
}

func (d *nvbandwidthDriver) RunDiagnostic(level string) (DiagnosticResult, error) {
	binary := resolveNvBandwidthBinary()
	if binary == "" {
		return DiagnosticResult{}, fmt.Errorf("nvbandwidth is not installed")
	}
	commands := [][]string{
		{"-j", "-t", "device_to_device_memcpy_read_ce"},
		{"-j"},
	}
	for _, args := range commands {
		output, err := commandOutput(2*time.Minute, binary, args...)
		if err != nil {
			continue
		}
		value := parseBandwidthFromJSON(output)
		if value <= 0 {
			value = parseMaxBandwidthFromText(output)
		}
		if value <= 0 {
			continue
		}
		return DiagnosticResult{
			MemoryBandwidthGBps: value,
			Detail:              "bandwidth measured via nvbandwidth",
			Source:              "measured_nvbandwidth",
		}, nil
	}
	return DiagnosticResult{}, fmt.Errorf("nvbandwidth output did not include bandwidth")
}

func (d *nvbandwidthDriver) Configure(config map[string]any) error { return nil }

func resolveNvBandwidthBinary() string {
	if commandExists("nvbandwidth") {
		return "nvbandwidth"
	}
	localPath := nvbandwidthUserBinaryPath()
	if binaryLooksUsable(localPath) {
		return localPath
	}
	return ""
}

func nvbandwidthUserBinaryPath() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "bin", "nvbandwidth")
	}
	return filepath.Join(homeDir, ".local", "bin", "nvbandwidth")
}
