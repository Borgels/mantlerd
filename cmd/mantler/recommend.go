package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/client"
	"github.com/Borgels/mantlerd/internal/types"
	"github.com/spf13/cobra"
)

var recommendCmd = &cobra.Command{
	Use:   "recommend",
	Short: "Show recommended stack and layers",
	Run:   runRecommend,
}

var (
	recommendRuntime      string
	recommendModel        string
	recommendHarness      string
	recommendOrchestrator string
	recommendBackend      string
	recommendRole         string
	recommendWorkload     string
	recommendLimit        int
	recommendJSON         bool
)

func init() {
	rootCmd.AddCommand(recommendCmd)
	recommendCmd.Flags().StringVar(&recommendRuntime, "runtime", "", "Constraint runtime (e.g. llamacpp)")
	recommendCmd.Flags().StringVar(&recommendModel, "model", "", "Constraint model ID")
	recommendCmd.Flags().StringVar(&recommendHarness, "harness", "", "Constraint harness")
	recommendCmd.Flags().StringVar(&recommendOrchestrator, "orchestrator", "", "Constraint orchestrator")
	recommendCmd.Flags().StringVar(&recommendBackend, "backend", "", "Constraint backend (cuda, metal, cpu...)")
	recommendCmd.Flags().StringVar(&recommendRole, "role", "", "Role-aware recommendation boost")
	recommendCmd.Flags().StringVar(&recommendWorkload, "workload", "", "Workload (coding, chat, creative, reasoning, agents, vision)")
	recommendCmd.Flags().IntVar(&recommendLimit, "limit", 5, "Max entries per section")
	recommendCmd.Flags().BoolVar(&recommendJSON, "json", false, "Print raw JSON response")
}

func runRecommend(cmd *cobra.Command, args []string) {
	cfg := loadConfig(cmd)
	cl, err := client.New(cfg.ServerURL, cfg.Token, cfg.Insecure)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating API client: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := cl.Recommend(ctx, types.RecommendQuery{
		MachineID:    cfg.MachineID,
		Runtime:      strings.TrimSpace(recommendRuntime),
		ModelID:      strings.TrimSpace(recommendModel),
		Harness:      strings.TrimSpace(recommendHarness),
		Orchestrator: strings.TrimSpace(recommendOrchestrator),
		Backend:      strings.TrimSpace(recommendBackend),
		Role:         strings.TrimSpace(recommendRole),
		Workload:     strings.TrimSpace(recommendWorkload),
		Limit:        recommendLimit,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading recommendations: %v\n", err)
		os.Exit(1)
	}
	if recommendJSON {
		payload, marshalErr := json.MarshalIndent(resp, "", "  ")
		if marshalErr != nil {
			fmt.Fprintf(os.Stderr, "Error formatting JSON output: %v\n", marshalErr)
			os.Exit(1)
		}
		fmt.Println(string(payload))
		return
	}

	fmt.Printf("Recommendations for machine %s\n\n", cfg.MachineID)
	if strings.TrimSpace(recommendWorkload) != "" {
		fmt.Printf("Workload: %s\n\n", strings.TrimSpace(recommendWorkload))
	}
	printLayerSection("Runtimes", resp.Runtimes)
	printModelSection("Models", resp.Models)
	printLayerSection("Harnesses", resp.Harnesses)
	printLayerSection("Orchestrators", resp.Orchestrators)
	if len(resp.CloudAlternatives) > 0 {
		fmt.Println("Cloud options:")
		for _, option := range resp.CloudAlternatives {
			fmt.Printf("  * %-10s %-28s %-6s %s\n", option.Provider, option.Model, option.CostTier, strings.TrimSpace(option.WorkloadFit))
		}
		fmt.Println()
	}
	if len(resp.Stacks) > 0 {
		fmt.Println("Top stacks:")
		for i, stack := range resp.Stacks {
			runtimeName := "-"
			modelName := "-"
			harnessName := "-"
			if stack.ResolvedLayers != nil {
				runtimeName = stack.ResolvedLayers.Runtime
				modelName = stack.ResolvedLayers.ModelID
				harnessName = stack.ResolvedLayers.Harness
			}
			fmt.Printf("  %d. %s + %s + %s  Score %.0f  %d runs\n", i+1, runtimeName, modelName, harnessName, stack.Score, stack.VerifiedRuns)
		}
		fmt.Println()
	}
	if len(resp.Runtimes) == 0 && len(resp.Models) == 0 && len(resp.Harnesses) == 0 && len(resp.Orchestrators) == 0 && len(resp.Stacks) == 0 {
		fmt.Println("No recommendations available for the current constraints.")
		return
	}
	fmt.Println("Install recommended: mantler setup --recommended")
}

func printLayerSection(title string, entries []types.RecommendLayerEntry) {
	if len(entries) == 0 {
		return
	}
	fmt.Printf("%s:\n", title)
	for _, entry := range entries {
		fmt.Printf("  * %-14s Score %.0f  %d verified runs  %s\n", entry.Name, entry.Score, entry.VerifiedRuns, strings.TrimSpace(entry.Rationale))
	}
	fmt.Println()
}

func printModelSection(title string, entries []types.RecommendModelEntry) {
	if len(entries) == 0 {
		return
	}
	fmt.Printf("%s:\n", title)
	for _, entry := range entries {
		fmt.Printf("  * %-20s Score %.0f  %d runs  %s\n", entry.ModelID, entry.Score, entry.VerifiedRuns, strings.TrimSpace(entry.Rationale))
	}
	fmt.Println()
}
