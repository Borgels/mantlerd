package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/client"
	agenteval "github.com/Borgels/mantlerd/internal/eval"
	"github.com/Borgels/mantlerd/internal/runtime"
	"github.com/Borgels/mantlerd/internal/types"
	"github.com/spf13/cobra"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Setup machine runtime/model from recommendations",
	Run:   runSetup,
}

var (
	setupRecommended bool
	setupYes         bool
	setupWorkload    string
	setupEval        bool
)

func init() {
	rootCmd.AddCommand(setupCmd)
	setupCmd.Flags().BoolVar(&setupRecommended, "recommended", false, "Install top recommended stack")
	setupCmd.Flags().BoolVar(&setupYes, "yes", false, "Skip confirmation prompt")
	setupCmd.Flags().StringVar(&setupWorkload, "workload", "", "Workload (coding, chat, creative, reasoning, agents, vision)")
	setupCmd.Flags().BoolVar(&setupEval, "eval", false, "Run quick eval after successful setup")
}

func runSetup(cmd *cobra.Command, args []string) {
	if !setupRecommended {
		fmt.Println("Nothing to do. Try: mantler setup --recommended")
		return
	}

	cfg := loadConfig(cmd)
	cl, err := client.New(cfg.ServerURL, cfg.Token, cfg.Insecure)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating API client: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := cl.Recommend(ctx, types.RecommendQuery{
		MachineID: cfg.MachineID,
		Workload:  strings.TrimSpace(setupWorkload),
		Limit:     1,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading recommendations: %v\n", err)
		os.Exit(1)
	}
	if len(resp.Stacks) == 0 || resp.Stacks[0].ResolvedLayers == nil {
		fmt.Fprintln(os.Stderr, "No recommended stack available for this machine.")
		os.Exit(1)
	}
	top := resp.Stacks[0].ResolvedLayers
	runtimeName := strings.TrimSpace(top.Runtime)
	modelID := strings.TrimSpace(top.ModelID)
	harnessName := strings.TrimSpace(top.Harness)
	if runtimeName == "" || modelID == "" {
		fmt.Fprintln(os.Stderr, "Top recommendation is missing runtime/model data.")
		os.Exit(1)
	}
	fmt.Printf("Recommended stack: %s + %s", runtimeName, modelID)
	if harnessName != "" {
		fmt.Printf(" + %s", harnessName)
	}
	fmt.Println()

	if !setupYes {
		fmt.Printf("Install %s + %s? [Y/n] ", runtimeName, modelID)
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "" && answer != "y" && answer != "yes" {
			fmt.Println("Cancelled.")
			return
		}
	}

	manager := runtime.NewManager()
	if err := manager.EnsureRuntime(runtimeName); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to install runtime %s: %v\n", runtimeName, err)
		os.Exit(1)
	}
	if err := manager.EnsureModelWithRuntime(modelID, runtimeName, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to pull model %s on %s: %v\n", modelID, runtimeName, err)
		fmt.Fprintln(os.Stderr, "Runtime install succeeded. You can safely re-run `mantler setup --recommended` after fixing the model error.")
		os.Exit(1)
	}

	if harnessName != "" {
		fmt.Printf("Harness recommendation: %s (configure in Mantler UI or desired config).\n", harnessName)
	}
	if setupEval {
		workload := strings.TrimSpace(setupWorkload)
		if workload == "" {
			workload = "coding"
		}
		fmt.Printf("Running quick eval for %s (%s)...\n", modelID, workload)
		runner := agenteval.NewRunner(manager)
		summary, evalErr := runner.Run(context.Background(), modelID, workload, "quick", localEvalPrompts(workload, "quick"), nil)
		if evalErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: quick eval failed: %v\n", evalErr)
		} else {
			printEvalSummary(summary)
			if reportErr := reportEvalSummary(
				cl,
				cfg.MachineID,
				modelID,
				runtimeName,
				summary,
				workload,
				0,
				"",
			); reportErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to report eval outcomes: %v\n", reportErr)
			} else {
				fmt.Println("Quick eval reported to Mantler server.")
			}
		}
	}
	fmt.Println("Setup complete.")
}
