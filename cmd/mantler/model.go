package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/config"
	"github.com/Borgels/mantlerd/internal/runtime"
	"github.com/Borgels/mantlerd/internal/types"
	"github.com/spf13/cobra"
)

var modelCmd = &cobra.Command{
	Use:   "model",
	Short: "Manage ML models",
	Long: `Manage ML models across runtimes.

Models are the machine learning models that runtimes execute. This command
allows you to list, pull, remove, and benchmark models across different runtimes.`,
}

var modelListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed models",
	Long: `List all installed models across all runtimes.

Shows model ID, runtime, and status for each installed model.`,
	Run: runModelList,
}

var modelPullCmd = &cobra.Command{
	Use:   "pull <model>",
	Short: "Pull/download a model",
	Long: `Pull (download) a model from a model registry.

This command downloads the specified model to the specified runtime.
If no runtime is specified, it uses the default runtime (usually ollama).

Examples:
  mantler model pull llama2:7b
  mantler model pull llama2:7b --runtime ollama
  mantler model pull mistral:7b --runtime llamacpp`,
	Args: cobra.ExactArgs(1),
	Run:  runModelPull,
}

var modelRemoveCmd = &cobra.Command{
	Use:   "remove <model>",
	Short: "Remove a model",
	Long: `Remove a model from a runtime.

This command removes the specified model from the specified runtime.
If no runtime is specified, it attempts to remove from all runtimes.

Examples:
  mantler model remove llama2:7b
  mantler model remove llama2:7b --runtime ollama`,
	Args: cobra.ExactArgs(1),
	Run:  runModelRemove,
}

var modelBenchmarkCmd = &cobra.Command{
	Use:   "benchmark <model>",
	Short: "Benchmark a model",
	Long: `Run performance benchmarks on a model.

This command runs various benchmarks to measure model performance:
- Time to First Token (TTFT)
- Output Tokens Per Second
- Total Latency
- Prompt Tokens Per Second

For workload quality evaluation (MMLU/GSM8K/GPQA and similar suites),
use "mantler eval" instead.

Benchmark profiles:
  quick    - Quick benchmark (few tokens, 1 run)
  standard - Standard benchmark (default)
  deep     - Deep benchmark (many tokens, multiple runs)

Examples:
  mantler model benchmark llama2:7b
  mantler model benchmark llama2:7b --profile deep
  mantler model benchmark llama2:7b --runtime ollama --profile quick`,
	Args: cobra.ExactArgs(1),
	Run:  runModelBenchmark,
}

var (
	modelRuntime string
	modelProfile string
)

func init() {
	rootCmd.AddCommand(modelCmd)
	modelCmd.AddCommand(modelListCmd)
	modelCmd.AddCommand(modelPullCmd)
	modelCmd.AddCommand(modelRemoveCmd)
	modelCmd.AddCommand(modelBenchmarkCmd)

	// Flags for pull command
	modelPullCmd.Flags().StringVarP(&modelRuntime, "runtime", "r", "", "Runtime to use (ollama, llamacpp, vllm, tensorrt)")

	// Flags for remove command
	modelRemoveCmd.Flags().StringVarP(&modelRuntime, "runtime", "r", "", "Runtime to remove from (default: all runtimes)")

	// Flags for benchmark command
	modelBenchmarkCmd.Flags().StringVarP(&modelRuntime, "runtime", "r", "", "Runtime to use (default: first ready runtime)")
	modelBenchmarkCmd.Flags().StringVarP(&modelProfile, "profile", "p", "standard", "Benchmark profile (quick, standard, deep)")
}

func runModelList(cmd *cobra.Command, args []string) {
	manager := runtime.NewManager()
	installedRuntimes := manager.InstalledRuntimes()

	if len(installedRuntimes) == 0 {
		fmt.Println("No runtimes installed.")
		fmt.Println("\nTo install a runtime, use: mantler runtime install <runtime>")
		return
	}

	fmt.Println("Installed Models:")
	fmt.Println()

	type installedModelsProvider interface {
		InstalledModels() []types.InstalledModel
	}

	totalModels := 0
	for _, runtimeName := range installedRuntimes {
		driver, err := manager.DriverFor(runtimeName)
		if err != nil {
			continue
		}

		var models []types.InstalledModel
		if provider, ok := driver.(installedModelsProvider); ok {
			models = provider.InstalledModels()
		} else {
			// Fall back to ListModels for simpler drivers
			modelIDs := driver.ListModels()
			for _, modelID := range modelIDs {
				models = append(models, types.InstalledModel{
					ModelID: strings.TrimSpace(modelID),
					Runtime: types.RuntimeType(runtimeName),
					Status:  types.ModelReady,
				})
			}
		}

		if len(models) == 0 {
			continue
		}

		fmt.Printf("  %s:\n", runtimeName)
		for _, model := range models {
			status := "ready"
			if model.Status != "" {
				status = string(model.Status)
			}
			fmt.Printf("    - %-30s [%s]\n", model.ModelID, status)
			totalModels++
		}
		fmt.Println()
	}

	if totalModels == 0 {
		fmt.Println("No models installed.")
		fmt.Println("\nTo pull a model, use: mantler model pull <model>")
	} else {
		fmt.Printf("Total: %d models\n", totalModels)
	}
}

func runModelPull(cmd *cobra.Command, args []string) {
	modelID := args[0]

	manager := runtime.NewManager()

	// Determine which runtime to use
	targetRuntime := modelRuntime
	if targetRuntime == "" {
		// Use first ready runtime
		readyRuntimes := manager.ReadyRuntimes()
		if len(readyRuntimes) == 0 {
			// Fall back to first installed runtime
			installedRuntimes := manager.InstalledRuntimes()
			if len(installedRuntimes) == 0 {
				fmt.Fprintln(os.Stderr, "Error: No runtimes installed")
				fmt.Fprintln(os.Stderr, "Install a runtime first: mantler runtime install <runtime>")
				os.Exit(1)
			}
			targetRuntime = installedRuntimes[0]
		} else {
			targetRuntime = readyRuntimes[0]
		}
	}

	fmt.Printf("Pulling model: %s\n", modelID)
	fmt.Printf("Runtime: %s\n", targetRuntime)
	fmt.Println("This may take a few minutes...")
	fmt.Println()

	lastStatus := ""
	reportProgress := func(progress runtime.PullProgress) {
		status := strings.TrimSpace(progress.Status)
		if status == "" || status == lastStatus {
			return
		}
		lastStatus = status
		if progress.Total > 0 {
			fmt.Printf("  → %s (%.1f%%)\n", status, progress.Percent)
			return
		}
		fmt.Printf("  → %s\n", status)
	}

	if err := manager.PrepareModelWithRuntimeProgressCtx(context.Background(), modelID, targetRuntime, nil, reportProgress); err != nil {
		fmt.Fprintf(os.Stderr, "Error pulling model: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("  → starting runtime with pulled model")
	if err := manager.StartModelWithRuntime(modelID, targetRuntime, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting model after pull: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n✓ Model %s pulled successfully to %s\n", modelID, targetRuntime)
}

func runModelRemove(cmd *cobra.Command, args []string) {
	modelID := args[0]

	manager := runtime.NewManager()

	// Determine which runtime(s) to remove from
	targetRuntime := modelRuntime
	if targetRuntime == "" {
		// Remove from all runtimes
		installedRuntimes := manager.InstalledRuntimes()
		if len(installedRuntimes) == 0 {
			fmt.Fprintln(os.Stderr, "Error: No runtimes installed")
			os.Exit(1)
		}

		removed := false
		for _, runtimeName := range installedRuntimes {
			driver, err := manager.DriverFor(runtimeName)
			if err != nil {
				continue
			}

			// Check if model exists in this runtime
			models := driver.ListModels()
			found := false
			for _, m := range models {
				if strings.TrimSpace(m) == modelID {
					found = true
					break
				}
			}

			if !found {
				continue
			}

			fmt.Printf("Removing model %s from %s...\n", modelID, runtimeName)
			if err := driver.RemoveModel(modelID); err != nil {
				fmt.Fprintf(os.Stderr, "Error removing model from %s: %v\n", runtimeName, err)
				continue
			}

			fmt.Printf("✓ Removed from %s\n", runtimeName)
			removed = true
		}

		if !removed {
			fmt.Printf("Model %s not found in any runtime\n", modelID)
		}
		return
	}

	// Remove from specific runtime
	driver, err := manager.DriverFor(targetRuntime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Check if model exists
	models := driver.ListModels()
	found := false
	for _, m := range models {
		if strings.TrimSpace(m) == modelID {
			found = true
			break
		}
	}

	if !found {
		fmt.Fprintf(os.Stderr, "Error: Model %s not found in %s\n", modelID, targetRuntime)
		os.Exit(1)
	}

	fmt.Printf("Removing model %s from %s...\n", modelID, targetRuntime)
	if err := driver.RemoveModel(modelID); err != nil {
		fmt.Fprintf(os.Stderr, "Error removing model: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Model %s removed successfully from %s\n", modelID, targetRuntime)
}

func runModelBenchmark(cmd *cobra.Command, args []string) {
	modelID := args[0]

	manager := runtime.NewManager()

	// Determine which runtime to use
	targetRuntime := modelRuntime
	if targetRuntime == "" {
		// Use first ready runtime
		readyRuntimes := manager.ReadyRuntimes()
		if len(readyRuntimes) == 0 {
			fmt.Fprintln(os.Stderr, "Error: No runtimes ready")
			fmt.Fprintln(os.Stderr, "Start a runtime first or specify one with --runtime")
			os.Exit(1)
		}
		targetRuntime = readyRuntimes[0]
	}

	// Validate profile
	profile := strings.ToLower(modelProfile)
	if profile != "quick" && profile != "standard" && profile != "deep" {
		fmt.Fprintf(os.Stderr, "Error: invalid profile: %s\n", modelProfile)
		fmt.Fprintln(os.Stderr, "Valid profiles: quick, standard, deep")
		os.Exit(1)
	}

	fmt.Printf("Benchmarking model: %s\n", modelID)
	fmt.Printf("Runtime: %s\n", targetRuntime)
	fmt.Printf("Profile: %s\n", profile)
	fmt.Println()

	samplePromptTokens, sampleOutputTokens, concurrency, runs := benchmarkProfile(profile)
	results, err := manager.BenchmarkModel(
		modelID,
		samplePromptTokens,
		sampleOutputTokens,
		concurrency,
		runs,
		nil,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running benchmark: %v\n", err)
		os.Exit(1)
	}

	// Display results
	fmt.Println("Benchmark Results:")
	fmt.Println()
	fmt.Printf("  Time to First Token:  %.2f ms\n", results.TTFTMs)
	fmt.Printf("  Output Tokens/sec:    %.2f\n", results.OutputTokensPerSec)
	fmt.Printf("  Total Latency:        %.2f ms\n", results.TotalLatencyMs)
	fmt.Printf("  Prompt Tokens/sec:    %.2f\n", results.PromptTokensPerSec)
	fmt.Printf("  P95 TTFT (small):     %.2f ms\n", results.P95TTFTMsAtSmallConcurrency)

	fmt.Println()

	baseFingerprint := ""
	if cfg, err := config.Load(config.DefaultConfigPath()); err == nil && strings.TrimSpace(cfg.MachineID) != "" {
		baseFingerprint = buildBaseFingerprint(cfg.MachineID, modelID, targetRuntime, "")
	}
	buffer := newOutcomeBuffer()
	buffer.Add(types.OutcomeEvent{
		BaseFingerprint: baseFingerprint,
		EventType:       "task_success",
		EvidenceKind:    "verified_benchmark",
		Detail:          fmt.Sprintf("model benchmark profile=%s runtime=%s", profile, targetRuntime),
		BenchmarkMetrics: &types.ModelBenchmarkMetrics{
			TTFTMs:                      results.TTFTMs,
			OutputTokensPerSec:          results.OutputTokensPerSec,
			TotalLatencyMs:              results.TotalLatencyMs,
			PromptTokensPerSec:          results.PromptTokensPerSec,
			P95TTFTMsAtSmallConcurrency: results.P95TTFTMsAtSmallConcurrency,
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})

	// Output JSON for scripting
	if false { // Disabled by default, can add flag to enable
		jsonOutput, _ := json.MarshalIndent(results, "", "  ")
		fmt.Println("JSON Output:")
		fmt.Println(string(jsonOutput))
	}
}

func benchmarkProfile(profile string) (samplePromptTokens int, sampleOutputTokens int, concurrency int, runs int) {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "quick":
		return 320, 128, 1, 3
	case "deep":
		return 1024, 512, 3, 12
	default:
		return 640, 256, 2, 8
	}
}
