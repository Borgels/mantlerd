package main

import (
	"bufio"
	"context"
	"encoding/json"
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

var exploreCmd = &cobra.Command{
	Use:   "explore",
	Short: "Find and execute a compatible mantle stack",
	Run: func(cmd *cobra.Command, args []string) {
		if err := runExplore(cmd, args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	},
}

var (
	exploreRuntime  string
	exploreModel    string
	exploreWorkload string
	exploreYes      bool
	exploreEval     bool
	exploreJSON     bool
)

func init() {
	rootCmd.AddCommand(exploreCmd)
	exploreCmd.Flags().StringVar(&exploreRuntime, "runtime", "", "Runtime constraint (llamacpp, ollama, vllm, tensorrt)")
	exploreCmd.Flags().StringVar(&exploreModel, "model", "", "Model ID constraint")
	exploreCmd.Flags().StringVar(&exploreWorkload, "workload", "coding", "Workload (coding, chat, creative, reasoning, agents, vision)")
	exploreCmd.Flags().BoolVar(&exploreYes, "yes", false, "Skip confirmation prompt")
	exploreCmd.Flags().BoolVar(&exploreEval, "eval", false, "Run full quick eval prompts (default runs smoke eval)")
	exploreCmd.Flags().BoolVar(&exploreJSON, "json", false, "Print machine-readable JSON output")
}

type exploreOutput struct {
	Recommendation *types.ExploreResponse `json:"recommendation,omitempty"`
	EvalSummary    *types.EvalRunSummary  `json:"evalSummary,omitempty"`
	Score          *types.ScoreResponse   `json:"score,omitempty"`
}

func runExplore(cmd *cobra.Command, args []string) error {
	cfg := loadConfig(cmd)
	workload := strings.TrimSpace(exploreWorkload)
	if workload == "" {
		workload = "coding"
	}
	if _, ok := allowedEvalWorkloads[workload]; !ok {
		return fmt.Errorf("invalid workload; use coding, chat, creative, reasoning, agents, or vision")
	}

	cl, err := client.New(cfg.ServerURL, cfg.Token, cfg.Insecure)
	if err != nil {
		return fmt.Errorf("error creating API client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	exploreResp, err := cl.Explore(ctx, types.ExploreQuery{
		Runtime:     strings.TrimSpace(exploreRuntime),
		ModelID:     strings.TrimSpace(exploreModel),
		Workload:    workload,
		MaxAttempts: 5,
	})
	if err != nil {
		return fmt.Errorf("explore request failed: %w", err)
	}

	runtimeName := strings.TrimSpace(exploreResp.Selection.Runtime)
	modelID := strings.TrimSpace(exploreResp.Selection.ModelID)
	if runtimeName == "" || modelID == "" {
		return fmt.Errorf("explore recommendation is missing runtime/model data")
	}
	if !exploreJSON {
		fmt.Printf("Recommended stack: %s + %s\n", runtimeName, modelID)
		fmt.Printf("Compatibility: allowed=%t confidence=%s blockers=%d warnings=%d\n",
			exploreResp.Plan.Compatibility.Allowed,
			strings.TrimSpace(exploreResp.Plan.Confidence),
			len(exploreResp.Plan.Compatibility.Blockers),
			len(exploreResp.Plan.Compatibility.Warnings),
		)
	}

	if !exploreYes && !exploreJSON {
		fmt.Printf("Proceed with %s + %s? [Y/n] ", runtimeName, modelID)
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "" && answer != "y" && answer != "yes" {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	manager := runtime.NewManager()
	if err := manager.EnsureRuntime(runtimeName); err != nil {
		return fmt.Errorf("failed to ensure runtime %s: %w", runtimeName, err)
	}
	if err := manager.PrepareModelWithRuntime(modelID, runtimeName, nil); err != nil {
		return fmt.Errorf("failed to prepare model %s on %s: %w", modelID, runtimeName, err)
	}
	if err := manager.StartModelWithRuntime(modelID, runtimeName, nil); err != nil {
		return fmt.Errorf("failed to start model %s on %s: %w", modelID, runtimeName, err)
	}
	defer func() {
		if stopErr := manager.StopModelWithRuntime(modelID, runtimeName); stopErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to stop model %s on %s: %v\n", modelID, runtimeName, stopErr)
		} else if !exploreJSON {
			fmt.Println("Model offloaded successfully.")
		}
	}()

	prompts := localEvalPrompts(workload, "quick")
	if !exploreEval && len(prompts) > 1 {
		prompts = prompts[:1]
	}
	runner := agenteval.NewRunner(manager)
	evalCtx, evalCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer evalCancel()
	summary, err := runner.Run(evalCtx, modelID, workload, "quick", prompts, nil)
	if err != nil {
		return fmt.Errorf("eval run failed: %w", err)
	}
	if !exploreJSON {
		printEvalSummary(summary)
	}

	suiteVersion := ""
	if len(prompts) > 0 {
		suiteVersion = strings.TrimSpace(prompts[0].SuiteVersion)
	}
	if err := reportEvalSummary(
		cl,
		cfg.MachineID,
		modelID,
		runtimeName,
		summary,
		workload,
		0,
		summary.EvalSessionToken,
		"mantler-standard",
		suiteVersion,
	); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to report outcomes: %v\n", err)
	}

	var score *types.ScoreResponse
	targetFingerprint := strings.TrimSpace(exploreResp.Plan.MantleFingerprint)
	if targetFingerprint != "" {
		scoreCtx, scoreCancel := context.WithTimeout(context.Background(), 15*time.Second)
		score, err = cl.GetScore(scoreCtx, targetFingerprint)
		scoreCancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to fetch score for %s: %v\n", targetFingerprint, err)
		} else if !exploreJSON {
			fmt.Printf(
				"Score: overall=%.2f confidence=%s evidence=%d\n",
				score.Overall,
				score.ConfidenceTier,
				score.EvidenceSignals,
			)
		}
	}

	if exploreJSON {
		out := exploreOutput{
			Recommendation: exploreResp,
			EvalSummary:    &summary,
			Score:          score,
		}
		raw, marshalErr := json.MarshalIndent(out, "", "  ")
		if marshalErr != nil {
			return fmt.Errorf("failed to encode JSON output: %w", marshalErr)
		}
		fmt.Println(string(raw))
		return nil
	}

	fmt.Println("Explore flow complete.")
	return nil
}
