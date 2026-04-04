package main

import (
	"fmt"
	"os"

	"github.com/Borgels/clawcontrol-agent/internal/runtime"
	"github.com/spf13/cobra"
)

var runtimeCmd = &cobra.Command{
	Use:   "runtime",
	Short: "Manage ML runtimes",
	Long: `Manage ML runtimes (Ollama, LM Studio, vLLM, TensorRT).

Runtimes are the inference engines that execute models. This command
allows you to list, install, and manage different runtime types.`,
}

var runtimeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed runtimes",
	Long: `List all installed ML runtimes.

Shows runtime name, status, and version for each installed runtime.
A runtime is 'installed' if its binary is found on the system.
A runtime is 'ready' if it's installed and responding to requests.`,
	Run: runRuntimeList,
}

var runtimeStatusCmd = &cobra.Command{
	Use:   "status [runtime]",
	Short: "Show runtime status",
	Long: `Show detailed status for one or all runtimes.

Without arguments, shows status for all installed runtimes.
With a runtime name argument, shows detailed status for that runtime.

Examples:
  clawcontrol runtime status
  clawcontrol runtime status ollama`,
	Args: cobra.MaximumNArgs(1),
	Run:  runRuntimeStatus,
}

var runtimeInstallCmd = &cobra.Command{
	Use:   "install <runtime>",
	Short: "Install a runtime",
	Long: `Install a ML runtime.

This command downloads and installs the specified runtime if it's not
already installed. The runtime will be configured for use with ClawControl.

Supported runtimes:
  ollama    - Ollama inference engine
  lmstudio  - LM Studio
  vllm      - vLLM inference server
  tensorrt  - NVIDIA TensorRT

Examples:
  clawcontrol runtime install ollama
  clawcontrol runtime install vllm`,
	Args: cobra.ExactArgs(1),
	Run:  runRuntimeInstall,
}

var runtimeRestartCmd = &cobra.Command{
	Use:   "restart <runtime>",
	Short: "Restart a runtime",
	Long: `Restart a ML runtime.

This command restarts the specified runtime service. Useful for
applying configuration changes or recovering from errors.

Examples:
  clawcontrol runtime restart ollama`,
	Args: cobra.ExactArgs(1),
	Run:  runRuntimeRestart,
}

func init() {
	rootCmd.AddCommand(runtimeCmd)
	runtimeCmd.AddCommand(runtimeListCmd)
	runtimeCmd.AddCommand(runtimeStatusCmd)
	runtimeCmd.AddCommand(runtimeInstallCmd)
	runtimeCmd.AddCommand(runtimeRestartCmd)
}

func runRuntimeList(cmd *cobra.Command, args []string) {
	manager := runtime.NewManager()
	installed := manager.InstalledRuntimes()
	ready := manager.ReadyRuntimes()

	if len(installed) == 0 {
		fmt.Println("No runtimes installed.")
		fmt.Println("\nTo install a runtime, use: clawcontrol runtime install <runtime>")
		return
	}

	fmt.Println("Installed Runtimes:")
	fmt.Println()

	// Create a map for quick lookup of ready runtimes
	readyMap := make(map[string]bool)
	for _, r := range ready {
		readyMap[r] = true
	}

	for _, runtimeName := range installed {
		version := manager.RuntimeVersion(runtimeName)
		status := "not ready"
		if readyMap[runtimeName] {
			status = "ready"
		}

		fmt.Printf("  %-12s %-10s %s\n", runtimeName, status, version)
	}

	fmt.Println()
	fmt.Printf("Total: %d installed, %d ready\n", len(installed), len(ready))
}

func runRuntimeStatus(cmd *cobra.Command, args []string) {
	manager := runtime.NewManager()

	if len(args) == 0 {
		// Show status for all runtimes
		showAllRuntimeStatus(manager)
		return
	}

	// Show status for specific runtime
	runtimeName := args[0]
	showRuntimeStatus(manager, runtimeName)
}

func showAllRuntimeStatus(manager *runtime.Manager) {
	installed := manager.InstalledRuntimes()

	if len(installed) == 0 {
		fmt.Println("No runtimes installed.")
		return
	}

	fmt.Println("Runtime Status:")
	fmt.Println()

	for _, runtimeName := range installed {
		showRuntimeStatus(manager, runtimeName)
		fmt.Println()
	}
}

func showRuntimeStatus(manager *runtime.Manager, runtimeName string) {
	driver, err := manager.DriverFor(runtimeName)
	if err != nil {
		fmt.Printf("Runtime: %s\n", runtimeName)
		fmt.Printf("  Status: not installed\n")
		fmt.Printf("  Error: %v\n", err)
		return
	}

	isInstalled := manager.IsRuntimeInstalled(runtimeName)
	isReady := driver.IsReady()
	version := manager.RuntimeVersion(runtimeName)

	fmt.Printf("Runtime: %s\n", runtimeName)
	fmt.Printf("  Status: ")
	if isReady {
		fmt.Println("ready ✓")
	} else if isInstalled {
		fmt.Println("installed (not ready)")
	} else {
		fmt.Println("not installed")
	}

	if version != "" {
		fmt.Printf("  Version: %s\n", version)
	}

	// Show models for this runtime
	models := driver.ListModels()
	if len(models) > 0 {
		fmt.Printf("  Models: %d loaded\n", len(models))
		for _, model := range models {
			fmt.Printf("    - %s\n", model)
		}
	} else {
		fmt.Println("  Models: none loaded")
	}
}

func runRuntimeInstall(cmd *cobra.Command, args []string) {
	runtimeName := args[0]

	// Validate runtime type
	validRuntimes := map[string]bool{
		"ollama":   true,
		"lmstudio": true,
		"vllm":     true,
		"tensorrt": true,
	}

	if !validRuntimes[runtimeName] {
		fmt.Fprintf(os.Stderr, "Error: unknown runtime: %s\n", runtimeName)
		fmt.Fprintln(os.Stderr, "Supported runtimes: ollama, lmstudio, vllm, tensorrt")
		os.Exit(1)
	}

	manager := runtime.NewManager()

	// Check if already installed
	if manager.IsRuntimeInstalled(runtimeName) {
		fmt.Printf("Runtime %s is already installed.\n", runtimeName)
		fmt.Printf("To reinstall, first remove it, then install again.\n")
		return
	}

	fmt.Printf("Installing runtime: %s\n", runtimeName)
	fmt.Println("This may take a few minutes...")

	if err := manager.EnsureRuntime(runtimeName); err != nil {
		fmt.Fprintf(os.Stderr, "Error installing runtime: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Runtime %s installed successfully\n", runtimeName)

	// Show version
	version := manager.RuntimeVersion(runtimeName)
	if version != "" {
		fmt.Printf("  Version: %s\n", version)
	}
}

func runRuntimeRestart(cmd *cobra.Command, args []string) {
	runtimeName := args[0]

	manager := runtime.NewManager()

	// Check if runtime is installed
	if !manager.IsRuntimeInstalled(runtimeName) {
		fmt.Fprintf(os.Stderr, "Error: runtime %s is not installed\n", runtimeName)
		os.Exit(1)
	}

	driver, err := manager.DriverFor(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Restarting runtime: %s\n", runtimeName)

	if err := driver.RestartRuntime(); err != nil {
		fmt.Fprintf(os.Stderr, "Error restarting runtime: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Runtime %s restarted successfully\n", runtimeName)
}
