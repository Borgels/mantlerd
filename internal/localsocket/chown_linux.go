package localsocket

import (
	"os/user"
	"strconv"
	"syscall"
)

// chownSocket attempts to set the socket file ownership to root:mantler so that
// members of the mantler group can connect without root.  Non-fatal on error.
func chownSocket(path string) {
	g, err := user.LookupGroup("mantler")
	if err != nil {
		return
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return
	}
	_ = syscall.Chown(path, 0, gid)
}
