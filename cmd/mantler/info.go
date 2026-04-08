package main

import (
	"fmt"
	"strings"

	"github.com/Borgels/mantlerd/internal/config"
	"github.com/Borgels/mantlerd/internal/discovery"
	"github.com/Borgels/mantlerd/internal/runtime"
	"github.com/spf13/cobra"
)

var infoCmd = &cobra.Command{
	Use:   "info",
	Short: "Display system information",
	Long: `Display detailed system information collected by the agent.

This command shows:
- Machine metadata (hostname, IP addresses)
- Hardware summary (CPU, memory, GPU)
- Agent version
- Installed runtimes and their status
- Installed models (if any)

Use this command to inspect the machine's capabilities and current state.`,
	Run: runInfo,
}

func init() {
	rootCmd.AddCommand(infoCmd)
}

func runInfo(cmd *cobra.Command, args []string) {
	// Collect system information
	report := discovery.Collect()

	fmt.Println("System Information")
	fmt.Println("==================")
	fmt.Println()

	// Machine metadata
	fmt.Println("Machine Details:")
	fmt.Printf("  Hostname:    %s\n", report.Hostname)
	if len(report.Addresses) > 0 {
		fmt.Printf("  Addresses:   %s\n", strings.Join(report.Addresses, ", "))
	}
	fmt.Println()

	// Hardware summary
	fmt.Println("Hardware Summary:")
	if report.HardwareSummary == "" {
		fmt.Println("  (no hardware information available)")
	} else {
		fmt.Printf("  %s\n", report.HardwareSummary)
	}
	fmt.Println()

	// Agent information
	fmt.Println("Agent:")
	fmt.Printf("  Version:     %s\n", agentVersion)
	fmt.Println()

	// Runtime information
	manager := runtime.NewManager()
	installedRuntimes := manager.InstalledRuntimes()
	readyRuntimes := manager.ReadyRuntimes()

	fmt.Println("Runtimes:")
	if len(installedRuntimes) == 0 {
		fmt.Println("  No runtimes installed")
	} else {
		readyMap := make(map[string]bool)
		for _, r := range readyRuntimes {
			readyMap[r] = true
		}

		for _, runtimeName := range installedRuntimes {
			status := "not ready"
			if readyMap[runtimeName] {
				status = "ready"
			}
			version := manager.RuntimeVersion(runtimeName)
			fmt.Printf("  %-12s %-12s %s\n", runtimeName, status, version)
		}
	}
	fmt.Println()

	// Model information
	modelCount := 0
	for _, runtimeName := range installedRuntimes {
		driver, err := manager.DriverFor(runtimeName)
		if err != nil {
			continue
		}

		models := driver.ListModels()
		modelCount += len(models)
	}

	fmt.Println("Models:")
	if modelCount == 0 {
		fmt.Println("  No models installed")
	} else {
		fmt.Printf("  %d models installed across all runtimes\n", modelCount)
		fmt.Println("  Use 'mantler model list' for details")
	}
	fmt.Println()

	// Orchestrator information
	orchestratorStatuses := []localOrchestratorStatus{
		inspectOrchestratorStatus("builtin"),
		inspectOrchestratorStatus("crewai"),
		inspectOrchestratorStatus("langgraph"),
		inspectOrchestratorStatus("autogen"),
	}
	readyOrchestrators := 0
	fmt.Println("Orchestrators:")
	for _, orchestrator := range orchestratorStatuses {
		if orchestrator.Status == "ready" {
			readyOrchestrators++
		}
		version := orchestrator.Version
		if version == "" {
			version = "-"
		}
		fmt.Printf("  %-12s %-13s %s\n", orchestrator.Type, orchestrator.Status, version)
	}
	fmt.Println()

	// Quick status summary
	fmt.Println("Quick Status:")
	if len(readyRuntimes) > 0 {
		fmt.Printf("  ✓ %d/%d runtimes ready\n", len(readyRuntimes), len(installedRuntimes))
	} else if len(installedRuntimes) > 0 {
		fmt.Printf("  ⚠ %d runtimes installed but none ready\n", len(installedRuntimes))
	} else {
		fmt.Println("  ⚠ No runtimes installed")
	}

	if modelCount > 0 {
		fmt.Printf("  ✓ %d models available\n", modelCount)
	}
	fmt.Printf("  ✓ %d/%d orchestrators ready\n", readyOrchestrators, len(orchestratorStatuses))

	// Config file location
	configPath := cfgFile
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}
	fmt.Printf("  Config: %s\n", configPath)
}
