package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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

var runtimePlanAllowedRoots = []string{
	"/etc/mantler",
	"/var/lib/mantler",
}

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
	capabilities := probeExploreCapabilities()
	runtimeConstraint := strings.TrimSpace(exploreRuntime)
	excludedFingerprints := map[string]struct{}{}
	forceOllama := false
	for recommendationAttempt := 0; recommendationAttempt < 3; recommendationAttempt++ {
		queryRuntime := runtimeConstraint
		if forceOllama {
			queryRuntime = "ollama"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		exploreResp, exploreErr := cl.Explore(ctx, types.ExploreQuery{
			Runtime:             queryRuntime,
			ModelID:             strings.TrimSpace(exploreModel),
			Workload:            workload,
			MaxAttempts:         5,
			Capabilities:        &capabilities,
			ExcludeFingerprints: mapKeys(excludedFingerprints),
		})
		cancel()
		if exploreErr != nil {
			if shouldForceOllamaFromExploreError(exploreErr) && !forceOllama {
				forceOllama = true
				runtimeConstraint = "ollama"
				if !exploreJSON {
					fmt.Fprintf(os.Stderr, "Warning: explore candidate search failed (%v), forcing ollama fallback\n", exploreErr)
				}
				continue
			}
			return fmt.Errorf("explore request failed: %w", exploreErr)
		}

		runtimeName := strings.TrimSpace(exploreResp.Selection.Runtime)
		modelID := strings.TrimSpace(exploreResp.Selection.ModelID)
		if runtimeName == "" || modelID == "" {
			return fmt.Errorf("explore recommendation is missing runtime/model data")
		}
		if isInvalidRuntimeModelCombination(runtimeName, modelID) {
			recordFailedFingerprint(exploreResp, excludedFingerprints)
			if recommendationAttempt == 0 && !forceOllama {
				if !exploreJSON {
					fmt.Fprintf(os.Stderr, "Warning: recommended stack %s + %s is not locally pullable, requesting next candidate\n", runtimeName, modelID)
				}
				continue
			}
			if !forceOllama {
				forceOllama = true
				runtimeConstraint = "ollama"
				if !exploreJSON {
					fmt.Fprintf(os.Stderr, "Warning: recommended stack %s + %s is not locally pullable, forcing ollama fallback\n", runtimeName, modelID)
				}
				continue
			}
			return fmt.Errorf("explore recommendation selected an invalid runtime/model combination: %s + %s", runtimeName, modelID)
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
			if shouldFallbackToOllama(runtimeName, err) {
				recordFailedFingerprint(exploreResp, excludedFingerprints)
				if recommendationAttempt == 0 && !forceOllama {
					if !exploreJSON {
						fmt.Fprintf(os.Stderr, "Warning: runtime %s failed preflight (%v), requesting next explore candidate\n", runtimeName, err)
					}
					continue
				}
				if !forceOllama {
					forceOllama = true
					runtimeConstraint = "ollama"
					if !exploreJSON {
						fmt.Fprintf(os.Stderr, "Warning: runtime %s failed preflight (%v), forcing ollama fallback\n", runtimeName, err)
					}
					continue
				}
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
			if shouldFallbackToOllama(runtimeName, err) {
				recordFailedFingerprint(exploreResp, excludedFingerprints)
				if recommendationAttempt == 0 && !forceOllama {
					if !exploreJSON {
						fmt.Fprintf(os.Stderr, "Warning: prepare failed on %s (%v), requesting next explore candidate\n", runtimeName, err)
					}
					continue
				}
				if !forceOllama {
					forceOllama = true
					runtimeConstraint = "ollama"
					if !exploreJSON {
						fmt.Fprintf(os.Stderr, "Warning: prepare failed on %s (%v), forcing ollama fallback\n", runtimeName, err)
					}
					continue
				}
			}
			return fmt.Errorf("failed to prepare model %s on %s: %w", modelID, runtimeName, err)
		}
		if strings.EqualFold(strings.TrimSpace(exploreResp.Plan.RuntimePlan.InstallMode), "download_only") {
			restoreRuntimePlan()
			if !exploreJSON {
				fmt.Println("Runtime plan installMode=download_only; model prepared and execution skipped.")
			}
			if exploreJSON {
				output := exploreOutput{
					Recommendation: exploreResp,
				}
				encoded, marshalErr := json.MarshalIndent(output, "", "  ")
				if marshalErr != nil {
					return fmt.Errorf("marshal json output: %w", marshalErr)
				}
				fmt.Println(string(encoded))
			}
			return nil
		}
		if err := manager.StartModelWithRuntime(modelID, runtimeName, nil); err != nil {
			restoreRuntimePlan()
			if shouldFallbackToOllama(runtimeName, err) {
				recordFailedFingerprint(exploreResp, excludedFingerprints)
				if recommendationAttempt == 0 && !forceOllama {
					if !exploreJSON {
						fmt.Fprintf(os.Stderr, "Warning: start failed on %s (%v), requesting next explore candidate\n", runtimeName, err)
					}
					continue
				}
				if !forceOllama {
					forceOllama = true
					runtimeConstraint = "ollama"
					if !exploreJSON {
						fmt.Fprintf(os.Stderr, "Warning: start failed on %s (%v), forcing ollama fallback\n", runtimeName, err)
					}
					continue
				}
			}
			return fmt.Errorf("failed to start model %s on %s: %w", modelID, runtimeName, err)
		}
		if err := waitForExploreHealthChecks(runtimeName, exploreResp.Plan.RuntimePlan.HealthChecks, 30*time.Second); err != nil {
			restoreRuntimePlan()
			if shouldFallbackToOllama(runtimeName, err) {
				recordFailedFingerprint(exploreResp, excludedFingerprints)
				if recommendationAttempt == 0 && !forceOllama {
					if !exploreJSON {
						fmt.Fprintf(os.Stderr, "Warning: health checks failed on %s (%v), requesting next explore candidate\n", runtimeName, err)
					}
					continue
				}
				if !forceOllama {
					forceOllama = true
					runtimeConstraint = "ollama"
					if !exploreJSON {
						fmt.Fprintf(os.Stderr, "Warning: health checks failed on %s (%v), forcing ollama fallback\n", runtimeName, err)
					}
					continue
				}
			}
			return fmt.Errorf("health checks failed for %s on %s: %w", modelID, runtimeName, err)
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
		evalSessionToken := ""
		promptCtx, promptCancel := context.WithTimeout(context.Background(), 15*time.Second)
		serverPrompts, sessionToken, promptErr := cl.GetEvalPrompts(promptCtx, workload, "quick", "mantler-standard")
		promptCancel()
		if promptErr == nil && len(serverPrompts) > 0 {
			prompts = serverPrompts
			evalSessionToken = strings.TrimSpace(sessionToken)
		} else if !exploreJSON {
			fmt.Fprintf(os.Stderr, "Warning: failed to fetch server eval prompts/session; using local prompts (%v)\n", promptErr)
		}
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
		if evalSessionToken != "" {
			summary.EvalSessionToken = evalSessionToken
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
	overrideImage := strings.TrimSpace(plan.RuntimePlan.Image)
	filteredArgs := filterRuntimePlanArgs(runtimeName, plan.RuntimePlan.Args)
	overrideArgs := strings.TrimSpace(strings.Join(filteredArgs, " "))
	filteredEnv := filterRuntimePlanEnv(runtimeName, plan.RuntimePlan.Env)
	var restoreFns []func()

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
	case "ollama":
		envPath = "/etc/mantler/ollama.env"
		argsKey = "OLLAMA_EXTRA_ARGS"
	}

	if envPath != "" {
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
		if overrideImage != "" && imageKey != "" {
			values[imageKey] = overrideImage
		}
		if overrideArgs != "" && argsKey != "" {
			values[argsKey] = overrideArgs
		}
		for key, value := range filteredEnv {
			trimmedKey := strings.TrimSpace(key)
			if trimmedKey == "" {
				continue
			}
			values[trimmedKey] = strings.TrimSpace(value)
		}
		if len(values) > 0 {
			if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
				return func() {}, err
			}
			if err := os.WriteFile(envPath, []byte(renderEnvContent(values)), 0o600); err != nil {
				return func() {}, err
			}
			restoreFns = append(restoreFns, func() {
				if existed {
					_ = os.WriteFile(envPath, originalRaw, 0o600)
					return
				}
				_ = os.Remove(envPath)
			})
		}
	}

	if fileRestore, err := applyRuntimePlanFiles(plan.RuntimePlan.Files); err != nil {
		return func() {}, err
	} else if fileRestore != nil {
		restoreFns = append(restoreFns, fileRestore)
	}

	if len(restoreFns) == 0 {
		return func() {}, nil
	}
	needsOllamaRestart := runtimeName == "ollama" &&
		(overrideImage != "" || overrideArgs != "" || len(filteredEnv) > 0 || len(plan.RuntimePlan.Files) > 0)
	if needsOllamaRestart {
		if err := restartOllamaService(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to restart ollama after applying runtime plan: %v\n", err)
		}
	}
	return func() {
		for i := len(restoreFns) - 1; i >= 0; i-- {
			restoreFns[i]()
		}
		if needsOllamaRestart {
			if err := restartOllamaService(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to restart ollama after restoring runtime plan: %v\n", err)
			}
		}
	}, nil
}

func restartOllamaService() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "restart", "ollama")
	if out, err := cmd.CombinedOutput(); err != nil {
		output := strings.TrimSpace(string(out))
		if output == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, output)
	}
	return nil
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

func applyRuntimePlanFiles(files []types.RuntimePlanFile) (func(), error) {
	restoreFns := make([]func(), 0)
	for _, file := range files {
		source := strings.TrimSpace(file.Source)
		destination := strings.TrimSpace(file.Destination)
		if destination == "" {
			continue
		}
		safeDestination, err := sanitizeRuntimePlanPath(destination)
		if err != nil {
			return nil, err
		}
		content := []byte(file.Content)
		if len(content) == 0 && source != "" {
			safeSource, err := sanitizeRuntimePlanPath(source)
			if err != nil {
				return nil, err
			}
			sourceFile, err := os.Open(safeSource)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, err
			}
			content, err = io.ReadAll(sourceFile)
			_ = sourceFile.Close()
			if err != nil {
				return nil, err
			}
		}
		if len(content) == 0 {
			continue
		}

		destExists := true
		original, readErr := os.ReadFile(safeDestination)
		if readErr != nil {
			if !errors.Is(readErr, os.ErrNotExist) {
				return nil, readErr
			}
			destExists = false
			original = nil
		}
		if err := os.MkdirAll(filepath.Dir(safeDestination), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(safeDestination, content, 0o600); err != nil {
			return nil, err
		}
		restoreFns = append(restoreFns, func() {
			if destExists {
				_ = os.WriteFile(safeDestination, original, 0o600)
				return
			}
			_ = os.Remove(safeDestination)
		})
	}
	if len(restoreFns) == 0 {
		return nil, nil
	}
	return func() {
		for i := len(restoreFns) - 1; i >= 0; i-- {
			restoreFns[i]()
		}
	}, nil
}

func sanitizeRuntimePlanPath(path string) (string, error) {
	cleanPath := filepath.Clean(strings.TrimSpace(path))
	if cleanPath == "." || cleanPath == "" {
		return "", fmt.Errorf("runtime plan file path is empty")
	}
	if !filepath.IsAbs(cleanPath) {
		return "", fmt.Errorf("runtime plan path %q must be absolute", path)
	}
	for _, root := range runtimePlanAllowedRoots {
		cleanRoot := filepath.Clean(root)
		if cleanPath == cleanRoot || strings.HasPrefix(cleanPath, cleanRoot+string(os.PathSeparator)) {
			return cleanPath, nil
		}
	}
	return "", fmt.Errorf("runtime plan path %q must be under %s", path, strings.Join(runtimePlanAllowedRoots, ", "))
}

func waitForExploreHealthChecks(runtimeName string, checks []string, timeout time.Duration) error {
	trimmedChecks := make([]string, 0, len(checks))
	for _, entry := range checks {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		trimmedChecks = append(trimmedChecks, trimmed)
	}
	if len(trimmedChecks) == 0 {
		return nil
	}
	baseURL := runtimeHealthBaseURL(runtimeName)
	client := &http.Client{Timeout: 4 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		allHealthy := true
		for _, check := range trimmedChecks {
			target := check
			if strings.EqualFold(strings.TrimSpace(runtimeName), "ollama") && strings.TrimSpace(target) == "/health" {
				target = "/api/tags"
			}
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
				parsed, parseErr := url.Parse(target)
				if parseErr != nil {
					lastErr = parseErr
					allHealthy = false
					continue
				}
				target = parsed.Path
				if target == "" {
					target = "/"
				}
			}
			target = strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(target, "/")
			req, err := http.NewRequest(http.MethodGet, target, nil)
			if err != nil {
				lastErr = err
				allHealthy = false
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				lastErr = err
				allHealthy = false
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 400 {
				lastErr = fmt.Errorf("health check %s returned status %d", target, resp.StatusCode)
				allHealthy = false
			}
		}
		if allHealthy {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out waiting for health checks")
	}
	return lastErr
}

func isInvalidRuntimeModelCombination(runtimeName string, modelID string) bool {
	runtimeName = strings.ToLower(strings.TrimSpace(runtimeName))
	modelID = strings.TrimSpace(modelID)
	if runtimeName == "ollama" {
		return false
	}
	return strings.Contains(modelID, "/") && strings.Contains(modelID, ":")
}

func shouldForceOllamaFromExploreError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "cuda backend requires an available gpu") ||
		strings.Contains(message, "no compatible recommendation candidates") ||
		strings.Contains(message, "unable to resolve a fully compatible explore candidate")
}

func runtimeHealthBaseURL(runtimeName string) string {
	switch strings.ToLower(strings.TrimSpace(runtimeName)) {
	case "ollama":
		base := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
		if base == "" {
			return "http://127.0.0.1:11434"
		}
		if strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://") {
			return strings.TrimRight(base, "/")
		}
		return "http://" + strings.TrimRight(base, "/")
	default:
		return "http://127.0.0.1:8000"
	}
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

func probeExploreCapabilities() types.ExploreCapabilities {
	caps := types.ExploreCapabilities{
		Docker:          false,
		HFToken:         false,
		GPUDriverLoaded: false,
	}
	dockerCtx, dockerCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dockerCancel()
	if err := exec.CommandContext(dockerCtx, "docker", "info").Run(); err == nil {
		caps.Docker = true
	}
	hfToken := strings.TrimSpace(os.Getenv("HF_TOKEN"))
	hfHubToken := strings.TrimSpace(os.Getenv("HUGGING_FACE_HUB_TOKEN"))
	caps.HFToken = hfToken != "" || hfHubToken != ""
	nvidiaCtx, nvidiaCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer nvidiaCancel()
	if err := exec.CommandContext(nvidiaCtx, "nvidia-smi", "-L").Run(); err == nil {
		caps.GPUDriverLoaded = true
	}
	return caps
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		keys = append(keys, trimmed)
	}
	sort.Strings(keys)
	return keys
}

func recordFailedFingerprint(resp *types.ExploreResponse, target map[string]struct{}) {
	if resp == nil {
		return
	}
	fp := strings.TrimSpace(resp.Plan.BaseFingerprint)
	if fp == "" {
		fp = strings.TrimSpace(resp.Plan.MantleFingerprint)
	}
	if fp == "" {
		return
	}
	target[fp] = struct{}{}
}

func filterRuntimePlanArgs(runtimeName string, args []string) []string {
	allowedPrefixes := map[string][]string{
		"vllm":     {"--model", "--tensor-parallel-size", "--max-model-len", "--gpu-memory-utilization", "--dtype"},
		"tensorrt": {"--model", "--max_batch_size", "--max_input_len", "--max_output_len", "--max_beam_width"},
		"ollama":   {"--num-gpu", "--num-ctx", "--temperature", "--top-p"},
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
		"vllm":     {"VLLM_", "HF_", "HUGGINGFACE_", "NVIDIA_", "CUDA_"},
		"tensorrt": {"TENSORRT_", "HF_", "HUGGINGFACE_", "NVIDIA_", "CUDA_"},
		"ollama":   {"OLLAMA_", "HF_", "HUGGINGFACE_"},
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
