package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// runtimeConfigDir returns the directory that stores runtime config files.
//
// On Linux as root the canonical system-wide path /etc/mantler is used, which
// is managed by the installer and owned root:mantler.  On macOS (where the
// daemon runs as the login user via LaunchAgent) and on Linux when running as
// a non-root user, the per-user ~/.mantler directory is used instead so that
// no elevated privileges are required for everyday CLI operations.
func runtimeConfigDir() string {
	if goruntime.GOOS == "linux" && os.Geteuid() == 0 {
		return "/etc/mantler"
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ".mantler"
	}
	return filepath.Join(home, ".mantler")
}

// runtimeConfigFile returns the full path for a named runtime config file,
// using runtimeConfigDir() as the base directory.
func runtimeConfigFile(name string) string {
	return filepath.Join(runtimeConfigDir(), name)
}

// runtimeStateDir returns the directory for runtime state files (prepared-model
// markers, etc.).  On Linux/root this is under /etc/mantler/runtimes; elsewhere
// it is under ~/.mantler/runtimes.
func runtimeStateDir() string {
	return filepath.Join(runtimeConfigDir(), "runtimes")
}
