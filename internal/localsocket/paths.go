// Package localsocket provides a lightweight Unix-socket control channel
// between the mantlerd daemon and the mantler CLI.  The daemon listens on the
// socket (root:mantler 0660) and the CLI connects to it when available,
// falling back to direct execution when the daemon is not running.
package localsocket

import (
	"os"
	"path/filepath"
	goruntime "runtime"
)

// SocketPath returns the path of the Unix domain socket.
//
//   - Linux / running as root:  /run/mantlerd/mantlerd.sock
//     (created under /run/mantlerd which is root:mantler 0750)
//   - macOS or non-root:        ~/.mantler/mantlerd.sock
func SocketPath() string {
	if goruntime.GOOS == "linux" && os.Geteuid() == 0 {
		return "/run/mantlerd/mantlerd.sock"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".mantler/mantlerd.sock"
	}
	return filepath.Join(home, ".mantler", "mantlerd.sock")
}

// Available returns true if the socket file exists (daemon is running).
func Available() bool {
	_, err := os.Stat(SocketPath())
	return err == nil
}
