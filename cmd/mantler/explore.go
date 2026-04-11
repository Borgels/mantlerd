package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	runtimeConstraint := strings.TrimSpace(exploreRuntime)
	for recommendationAttempt := 0; recommendationAttempt < 2; recommendationAttempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		exploreResp, exploreErr := cl.Explore(ctx, types.ExploreQuery{
			Runtime:     runtimeConstraint,
			ModelID:     strings.TrimSpace(exploreModel),
			Workload:    workload,
			MaxAttempts: 5,
		})
		cancel()
		if exploreErr != nil {
			return fmt.Errorf("explore request failed: %w", exploreErr)
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
			if image := strings.TrimSpace(exploreResp.Plan.RuntimePlan.Image); image != "" {
				fmt.Printf("Runtime plan image: %s\n", image)
			}
			if len(exploreResp.Plan.RuntimePlan.Args) > 0 {
				fmt.Printf("Runtime plan args: %s\n", strings.Join(exploreResp.Plan.RuntimePlan.Args, " "))
			}
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
			if recommendationAttempt == 0 && shouldFallbackToOllama(runtimeName, err) {
				if !exploreJSON {
					fmt.Fprintf(os.Stderr, "Warning: runtime %s failed preflight (%v), retrying explore with ollama fallback\n", runtimeName, err)
				}
				runtimeConstraint = "ollama"
				continue
			}
			return fmt.Errorf("failed to ensure runtime %s: %w", runtimeName, err)
		}
		restoreRuntimePlan := func() {}
		if restore, applyErr := applyExploreRuntimePlan(runtimeName, exploreResp.Plan); applyErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to apply runtime plan overrides: %v\n", applyErr)
		} else {
			restoreRuntimePlan = restore
		}
		if err := manager.PrepareModelWithRuntime(modelID, runtimeName, nil); err != nil {
			restoreRuntimePlan()
			if recommendationAttempt == 0 && shouldFallbackToOllama(runtimeName, err) {
				if !exploreJSON {
					fmt.Fprintf(os.Stderr, "Warning: prepare failed on %s (%v), retrying explore with ollama fallback\n", runtimeName, err)
				}
				runtimeConstraint = "ollama"
				continue
			}
			return fmt.Errorf("failed to prepare model %s on %s: %w", modelID, runtimeName, err)
		}
		if err := manager.StartModelWithRuntime(modelID, runtimeName, nil); err != nil {
			restoreRuntimePlan()
			if recommendationAttempt == 0 && shouldFallbackToOllama(runtimeName, err) {
				if !exploreJSON {
					fmt.Fprintf(os.Stderr, "Warning: start failed on %s (%v), retrying explore with ollama fallback\n", runtimeName, err)
				}
				runtimeConstraint = "ollama"
				continue
			}
			return fmt.Errorf("failed to start model %s on %s: %w", modelID, runtimeName, err)
		}
		defer restoreRuntimePlan()
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
			exploreResp.Plan.ID,
			exploreResp.Plan.MantleFingerprint,
			exploreResp.Plan.BaseFingerprint,
			buildPromptTokenHints(prompts),
		); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to report outcomes: %v\n", err)
		}

		var score *types.ScoreResponse
		targetFingerprints := []string{
			strings.TrimSpace(exploreResp.Plan.MantleFingerprint),
			strings.TrimSpace(exploreResp.Plan.BaseFingerprint),
		}
		scoreErr := error(nil)
		for _, targetFingerprint := range targetFingerprints {
			if targetFingerprint == "" {
				continue
			}
			score, scoreErr = waitForScore(cl, targetFingerprint, 4*time.Minute)
			if scoreErr == nil {
				break
			}
		}
		if scoreErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to fetch score: %v\n", scoreErr)
			score = buildLocalFallbackScore(exploreResp, summary)
		}
		if score != nil && !exploreJSON {
			fmt.Printf(
				"Score: overall=%.2f confidence=%s evidence=%d\n",
				score.Overall,
				score.ConfidenceTier,
				score.EvidenceSignals,
			)
		}

		if exploreJSON {
			redactedSummary := redactEvalSummary(summary)
			out := exploreOutput{
				Recommendation: exploreResp,
				EvalSummary:    &redactedSummary,
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
	return fmt.Errorf("explore retries exhausted")
}

func waitForScore(cl *client.Client, fingerprint string, timeout time.Duration) (*types.ScoreResponse, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		scoreCtx, scoreCancel := context.WithTimeout(context.Background(), 45*time.Second)
		score, err := cl.GetScore(scoreCtx, fingerprint)
		scoreCancel()
		if err == nil {
			return score, nil
		}
		lastErr = err
		if !isRetryableScoreError(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if lastErr == nil {
		lastErr = errors.New("score not found")
	}
	return nil, lastErr
}

func isRetryableScoreError(err error) bool {
	if err == nil {
		return false
	}
	lowerErr := strings.ToLower(err.Error())
	return strings.Contains(lowerErr, "score not found") ||
		strings.Contains(lowerErr, "context deadline exceeded") ||
		strings.Contains(lowerErr, "(500)") ||
		strings.Contains(lowerErr, "(502)") ||
		strings.Contains(lowerErr, "(503)") ||
		strings.Contains(lowerErr, "(504)") ||
		strings.Contains(lowerErr, "internal server error") ||
		strings.Contains(lowerErr, "bad gateway") ||
		strings.Contains(lowerErr, "service unavailable") ||
		strings.Contains(lowerErr, "gateway timeout") ||
		strings.Contains(lowerErr, "connection refused") ||
		strings.Contains(lowerErr, "i/o timeout")
}

func buildLocalFallbackScore(exploreResp *types.ExploreResponse, summary types.EvalRunSummary) *types.ScoreResponse {
	if exploreResp == nil {
		return nil
	}
	totalQuality := 0.0
	for _, sample := range summary.Samples {
		totalQuality += sample.QualityScore
	}
	overall := 0.0
	if len(summary.Samples) > 0 {
		overall = totalQuality / float64(len(summary.Samples))
	}
	fingerprint := strings.TrimSpace(exploreResp.Plan.BaseFingerprint)
	if fingerprint == "" {
		fingerprint = strings.TrimSpace(exploreResp.Plan.MantleFingerprint)
	}
	return &types.ScoreResponse{
		MantleFingerprint: fingerprint,
		Overall:           overall,
		ProfileID:         "balanced",
		ConfidenceTier:    "provisional",
		EvidenceSignals:   len(summary.Samples),
		UpdatedAt:         time.Now().UTC().Format(time.RFC3339),
	}
}

func applyExploreRuntimePlan(runtimeName string, plan types.ExplorePlan) (func(), error) {
	runtimeName = strings.ToLower(strings.TrimSpace(runtimeName))
	if runtimeName != "vllm" && runtimeName != "tensorrt" {
		return func() {}, nil
	}

	overrideImage := strings.TrimSpace(plan.RuntimePlan.Image)
	filteredArgs := filterRuntimePlanArgs(runtimeName, plan.RuntimePlan.Args)
	overrideArgs := strings.TrimSpace(strings.Join(filteredArgs, " "))
	filteredEnv := filterRuntimePlanEnv(runtimeName, plan.RuntimePlan.Env)
	if overrideImage == "" && overrideArgs == "" && len(filteredEnv) == 0 {
		return func() {}, nil
	}

	envPath := ""
	imageKey := ""
	argsKey := ""
	switch runtimeName {
	case "vllm":
		envPath = "/etc/mantler/vllm.env"
		imageKey = "VLLM_CONTAINER_IMAGE"
		argsKey = "VLLM_EXTRA_ARGS"
	case "tensorrt":
		envPath = "/etc/mantler/tensorrt.env"
		imageKey = "TENSORRT_CONTAINER_IMAGE"
		argsKey = "TENSORRT_EXTRA_ARGS"
	}

	existed := true
	originalRaw, readErr := os.ReadFile(envPath)
	if readErr != nil {
		if !errors.Is(readErr, os.ErrNotExist) {
			return func() {}, readErr
		}
		existed = false
		originalRaw = []byte{}
	}

	values := parseEnvContent(string(originalRaw))
	if overrideImage != "" {
		values[imageKey] = overrideImage
	}
	if overrideArgs != "" {
		values[argsKey] = overrideArgs
	}
	for key, value := range filteredEnv {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		values[trimmedKey] = strings.TrimSpace(value)
	}

	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		return func() {}, err
	}
	if err := os.WriteFile(envPath, []byte(renderEnvContent(values)), 0o600); err != nil {
		return func() {}, err
	}

	restore := func() {
		if existed {
			_ = os.WriteFile(envPath, originalRaw, 0o600)
			return
		}
		_ = os.Remove(envPath)
	}
	return restore, nil
}

func parseEnvContent(raw string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		if key == "" {
			continue
		}
		value := strings.TrimSpace(parts[1])
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		values[key] = value
	}
	return values
}

func renderEnvContent(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	// keep output deterministic for easier debugging/reviews
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s=%q", key, values[key]))
	}
	return strings.Join(lines, "\n") + "\n"
}

func shouldFallbackToOllama(runtimeName string, err error) bool {
	if strings.EqualFold(strings.TrimSpace(runtimeName), "ollama") {
		return false
	}
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "invalid runtime") ||
		strings.Contains(message, "unknown runtime") ||
		strings.Contains(message, "invalid model id") {
		return false
	}
	return true
}

func filterRuntimePlanArgs(runtimeName string, args []string) []string {
	allowedPrefixes := map[string][]string{
		"vllm": {"--model", "--tensor-parallel-size", "--max-model-len", "--gpu-memory-utilization", "--dtype"},
		"tensorrt": {"--model", "--max_batch_size", "--max_input_len", "--max_output_len", "--max_beam_width"},
	}
	prefixes := allowedPrefixes[runtimeName]
	if len(prefixes) == 0 {
		return nil
	}
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		trimmed := strings.TrimSpace(arg)
		if trimmed == "" {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(trimmed, prefix) {
				filtered = append(filtered, trimmed)
				break
			}
		}
	}
	return filtered
}

func filterRuntimePlanEnv(runtimeName string, env map[string]string) map[string]string {
	if len(env) == 0 {
		return map[string]string{}
	}
	allowedPrefixes := map[string][]string{
		"vllm": {"VLLM_", "HF_", "HUGGINGFACE_", "NVIDIA_", "CUDA_"},
		"tensorrt": {"TENSORRT_", "HF_", "HUGGINGFACE_", "NVIDIA_", "CUDA_"},
	}
	prefixes := allowedPrefixes[runtimeName]
	if len(prefixes) == 0 {
		return map[string]string{}
	}
	filtered := make(map[string]string, len(env))
	for key, value := range env {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		upperKey := strings.ToUpper(trimmedKey)
		for _, prefix := range prefixes {
			if strings.HasPrefix(upperKey, prefix) {
				filtered[trimmedKey] = strings.TrimSpace(value)
				break
			}
		}
	}
	return filtered
}
