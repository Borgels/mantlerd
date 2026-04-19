// Package policy enforces which remote commands the agent will execute based
// on the locally configured trust mode.
//
// Trust modes
//
//	"managed"    (default) – all command types are permitted; existing behaviour.
//	"restricted" – commands that fall into the Destructive category are denied
//	               unless the operator has explicitly added them to AllowedCommands
//	               in the agent config file.
//
// Destructive commands are those that can significantly alter system state,
// execute third-party code, or affect machine availability.
package policy

import (
	"fmt"
	"strings"
)

// TrustMode mirrors config.TrustMode to avoid a circular import.
type TrustMode string

const (
	TrustModeManaged    TrustMode = "managed"
	TrustModeRestricted TrustMode = "restricted"
)

// destructiveCommands is the set of command types that require explicit local
// opt-in when running in "restricted" trust mode.
var destructiveCommands = map[string]struct{}{
	"install_runtime":     {},
	"uninstall_runtime":   {},
	"restart_runtime":     {},
	"install_tool":        {},
	"uninstall_tool":      {},
	"install_trainer":     {},
	"uninstall_trainer":   {},
	"run_harness_exec":    {},
	"harness_login":       {},
	"run_orchestrator_exec": {},
	"sync_harnesses":      {},
	"update_agent":        {},
	"self_shutdown":       {},
	"uninstall_agent":     {},
	"nccl_test":           {},
}

// IsDestructive returns true when a command type is in the destructive
// category regardless of trust mode.
func IsDestructive(commandType string) bool {
	_, ok := destructiveCommands[strings.TrimSpace(commandType)]
	return ok
}

// Allowed reports whether a command of the given type may execute under the
// provided trust mode and operator allowlist.
//
// In "managed" mode every command is allowed.
// In "restricted" mode a destructive command is allowed only when its type
// appears in the allowedCommands slice.
// Unknown trust modes are treated as "restricted" for safety.
func Allowed(mode TrustMode, commandType string, allowedCommands []string) (bool, string) {
	normalised := strings.TrimSpace(commandType)
	switch mode {
	case TrustModeManaged, "":
		return true, ""
	case TrustModeRestricted:
		if !IsDestructive(normalised) {
			return true, ""
		}
		for _, allowed := range allowedCommands {
			if strings.TrimSpace(allowed) == normalised {
				return true, ""
			}
		}
		return false, fmt.Sprintf(
			"command %q is in the destructive category and is not permitted in "+
				"trust mode %q; add it to allowedCommands in the agent config to enable it",
			normalised, mode,
		)
	default:
		// Unknown mode — treat as restricted.
		if !IsDestructive(normalised) {
			return true, ""
		}
		for _, allowed := range allowedCommands {
			if strings.TrimSpace(allowed) == normalised {
				return true, ""
			}
		}
		return false, fmt.Sprintf(
			"command %q denied: unknown trust mode %q treated as restricted",
			normalised, mode,
		)
	}
}
