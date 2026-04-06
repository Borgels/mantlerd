package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var orchestratorCmd = &cobra.Command{
	Use:   "orchestrator",
	Short: "Manage orchestrators",
	Long: `Manage orchestrators (built-in, CrewAI, LangGraph, AutoGen).

Orchestrators run multi-agent workflows and task execution logic.
This command allows you to list, inspect, install, and remove orchestrators.`,
}

var orchestratorListCmd = &cobra.Command{
	Use:   "list",
	Short: "List orchestrators",
	Run:   runOrchestratorList,
}

var orchestratorStatusCmd = &cobra.Command{
	Use:   "status [orchestrator]",
	Short: "Show orchestrator status",
	Args:  cobra.MaximumNArgs(1),
	Run:   runOrchestratorStatus,
}

var orchestratorInstallCmd = &cobra.Command{
	Use:   "install <orchestrator>",
	Short: "Install an orchestrator",
	Long: `Install an external orchestrator.

Supported orchestrators:
  crewai
  langgraph
  autogen (ag2)

The built-in orchestrator is always available and does not need installation.`,
	Args: cobra.ExactArgs(1),
	Run:  runOrchestratorInstall,
}

var orchestratorRemoveCmd = &cobra.Command{
	Use:   "remove <orchestrator>",
	Short: "Remove an orchestrator",
	Long: `Remove an external orchestrator (best-effort uninstall).

Supported orchestrators:
  crewai
  langgraph
  autogen (ag2)

The built-in orchestrator cannot be removed.`,
	Args: cobra.ExactArgs(1),
	Run:  runOrchestratorRemove,
}

func init() {
	rootCmd.AddCommand(orchestratorCmd)
	orchestratorCmd.AddCommand(orchestratorListCmd)
	orchestratorCmd.AddCommand(orchestratorStatusCmd)
	orchestratorCmd.AddCommand(orchestratorInstallCmd)
	orchestratorCmd.AddCommand(orchestratorRemoveCmd)
}

type localOrchestratorStatus struct {
	Type    string
	Name    string
	Status  string
	Version string
	Detail  string
}

func runOrchestratorList(cmd *cobra.Command, args []string) {
	statuses := []localOrchestratorStatus{
		inspectOrchestratorStatus("builtin"),
		inspectOrchestratorStatus("crewai"),
		inspectOrchestratorStatus("langgraph"),
		inspectOrchestratorStatus("autogen"),
	}

	fmt.Println("Orchestrators:")
	fmt.Println()
	for _, status := range statuses {
		version := status.Version
		if version == "" {
			version = "-"
		}
		fmt.Printf("  %-12s %-11s %s\n", status.Type, status.Status, version)
	}
}

func runOrchestratorStatus(cmd *cobra.Command, args []string) {
	if len(args) == 0 {
		runOrchestratorList(cmd, args)
		return
	}

	orchestratorType, err := normalizeExternalOrchestrator(args[0], true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	status := inspectOrchestratorStatus(orchestratorType)
	fmt.Printf("Orchestrator: %s\n", status.Name)
	fmt.Printf("  Type: %s\n", status.Type)
	fmt.Printf("  Status: %s\n", status.Status)
	if status.Version != "" {
		fmt.Printf("  Version: %s\n", status.Version)
	}
	if status.Detail != "" {
		fmt.Printf("  Detail: %s\n", status.Detail)
	}
}

func runOrchestratorInstall(cmd *cobra.Command, args []string) {
	orchestratorType, err := normalizeExternalOrchestrator(args[0], false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	command := defaultOrchestratorCommand(orchestratorType)
	fmt.Printf("Installing orchestrator: %s\n", orchestratorType)
	path, detail, err := ensureOrchestratorExecutable(orchestratorType, command)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error installing orchestrator: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Orchestrator %s is ready\n", orchestratorType)
	if path != "" {
		fmt.Printf("  Path: %s\n", path)
	}
	if detail != "" {
		fmt.Printf("  %s\n", detail)
	}
}

func runOrchestratorRemove(cmd *cobra.Command, args []string) {
	orchestratorType, err := normalizeExternalOrchestrator(args[0], false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("Removing orchestrator: %s\n", orchestratorType)
	if err := uninstallOrchestrator(orchestratorType); err != nil {
		fmt.Fprintf(os.Stderr, "Error removing orchestrator: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Orchestrator %s removed (best effort)\n", orchestratorType)
}

func normalizeExternalOrchestrator(value string, allowBuiltin bool) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "builtin":
		if allowBuiltin {
			return normalized, nil
		}
		return "", fmt.Errorf("builtin orchestrator cannot be installed/removed")
	case "crewai", "langgraph", "autogen", "ag2":
		if normalized == "ag2" {
			return "autogen", nil
		}
		return normalized, nil
	default:
		if allowBuiltin {
			return "", fmt.Errorf("unknown orchestrator: %s (supported: builtin, crewai, langgraph, autogen/ag2)", value)
		}
		return "", fmt.Errorf("unknown orchestrator: %s (supported: crewai, langgraph, autogen/ag2)", value)
	}
}

func inspectOrchestratorStatus(orchestratorType string) localOrchestratorStatus {
	name := strings.ToUpper(orchestratorType[:1]) + orchestratorType[1:]
	if orchestratorType == "builtin" {
		return localOrchestratorStatus{
			Type:   "builtin",
			Name:   "Built-in",
			Status: "ready",
			Detail: "Built-in orchestrator is managed by ClawControl.",
		}
	}

	var (
		path string
		err  error
	)
	for _, candidate := range orchestratorCommandCandidates(orchestratorType, defaultOrchestratorCommand(orchestratorType)) {
		path, err = resolveExecutableWithUserPath(candidate)
		if err == nil {
			break
		}
	}
	if err != nil {
		return localOrchestratorStatus{
			Type:   orchestratorType,
			Name:   name,
			Status: "not installed",
			Detail: err.Error(),
		}
	}

	version := probeHarnessVersion(path)
	detail := executableDetail(path, version)
	return localOrchestratorStatus{
		Type:    orchestratorType,
		Name:    name,
		Status:  "ready",
		Version: version,
		Detail:  detail,
	}
}

func uninstallOrchestrator(orchestratorType string) error {
	pkg := ""
	switch orchestratorType {
	case "crewai":
		pkg = "crewai"
	case "langgraph":
		pkg = "langgraph-cli"
	case "autogen":
		pkg = "ag2"
	default:
		return fmt.Errorf("unsupported orchestrator type: %s", orchestratorType)
	}

	// Best effort uninstall via available package managers.
	pkgs := []string{pkg}
	if orchestratorType == "autogen" {
		pkgs = append(pkgs, "pyautogen")
	}
	for _, uninstallPkg := range pkgs {
		if _, err := exec.LookPath("pipx"); err == nil {
			_, _ = exec.Command("pipx", "uninstall", uninstallPkg).CombinedOutput()
		}
		if _, err := exec.LookPath("uv"); err == nil {
			_, _ = exec.Command("uv", "tool", "uninstall", uninstallPkg).CombinedOutput()
		}
		if _, err := exec.LookPath("python3"); err == nil {
			_, _ = exec.Command("python3", "-m", "pip", "uninstall", "-y", uninstallPkg).CombinedOutput()
		}
	}

	home, err := os.UserHomeDir()
	if err == nil {
		for _, commandName := range orchestratorCommandCandidates(orchestratorType, defaultOrchestratorCommand(orchestratorType)) {
			shim := filepath.Join(home, ".local", "bin", commandName)
			_ = os.Remove(shim)
		}
		venvDir := filepath.Join(home, ".local", "share", "clawcontrol", "orchestrators", orchestratorType)
		_ = os.RemoveAll(venvDir)
	}

	return nil
}
