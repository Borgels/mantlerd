package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
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
	exploreRuntime   string
	exploreModel     string
	exploreWorkload  string
	explorePriority  string
	exploreModelType string
	exploreYes       bool
	exploreEval      bool
	exploreJSON      bool
)

func init() {
	rootCmd.AddCommand(exploreCmd)
	exploreCmd.Flags().StringVar(&exploreRuntime, "runtime", "", "Runtime constraint (llamacpp, ollama, vllm, tensorrt)")
	exploreCmd.Flags().StringVar(&exploreModel, "model", "", "Model ID constraint")
	exploreCmd.Flags().StringVar(&exploreWorkload, "workload", "coding", "Workload (coding, chat, creative, reasoning, agents, vision)")
	exploreCmd.Flags().StringVar(&explorePriority, "priority", "balanced", "Preference priority (light, balanced, powerful)")
	exploreCmd.Flags().StringVar(&exploreModelType, "model-type", "any", "Model type preference (any, dense, quantized)")
	exploreCmd.Flags().BoolVar(&exploreYes, "yes", false, "Skip confirmation prompt")
	exploreCmd.Flags().BoolVar(&exploreEval, "eval", false, "Run full quick eval prompts (default runs smoke eval)")
	exploreCmd.Flags().BoolVar(&exploreJSON, "json", false, "Print machine-readable JSON output")
}

type exploreOutput struct {
	Recommendation *types.ExploreResponse `json:"recommendation,omitempty"`
	EvalSummary    *types.EvalRunSummary  `json:"evalSummary,omitempty"`
	Score          *types.ScoreResponse   `json:"score,omitempty"`
}

const compatCatalogSyncStatePath = "/var/lib/mantler/compat-catalog-sync.json"
const compatCatalogCachePath = "/var/lib/mantler/compat-catalog.json"

type compatCatalogSyncState struct {
	LastSyncedAt    string `json:"lastSyncedAt"`
	LastCatalogHash string `json:"lastCatalogHash"`
}

func loadCompatCatalogCache() types.CompatCatalog {
	raw, err := os.ReadFile(compatCatalogCachePath)
	if err != nil {
		return types.CompatCatalog{}
	}
	var catalog types.CompatCatalog
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return types.CompatCatalog{}
	}
	return catalog
}

func saveCompatCatalogCache(catalog types.CompatCatalog) error {
	if err := os.MkdirAll(filepath.Dir(compatCatalogCachePath), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(compatCatalogCachePath, append(raw, '\n'), 0o600)
}

func mergeCompatCatalog(base types.CompatCatalog, delta types.CompatCatalog) types.CompatCatalog {
	base.Models = mergeCompatCatalogEntries(base.Models, delta.Models, "id")
	base.RuntimeRules = mergeCompatCatalogEntries(base.RuntimeRules, delta.RuntimeRules, "id")
	base.GPUCapabilities = mergeCompatCatalogEntries(base.GPUCapabilities, delta.GPUCapabilities, "namePattern")
	base.CuratedRecipes = mergeCompatCatalogEntries(base.CuratedRecipes, delta.CuratedRecipes, "id")
	base.Integrations = mergeCompatCatalogEntries(base.Integrations, delta.Integrations, "key")
	return base
}

func mergeCompatCatalogEntries(base []map[string]any, delta []map[string]any, key string) []map[string]any {
	if len(delta) == 0 {
		return base
	}
	merged := make([]map[string]any, 0, len(base)+len(delta))
	indexByKey := make(map[string]int, len(base)+len(delta))

	appendEntry := func(entry map[string]any) {
		if entry == nil {
			return
		}
		entryKey, _ := entry[key].(string)
		if strings.TrimSpace(entryKey) == "" {
			merged = append(merged, entry)
			return
		}
		if idx, ok := indexByKey[entryKey]; ok {
			merged[idx] = entry
			return
		}
		indexByKey[entryKey] = len(merged)
		merged = append(merged, entry)
	}

	for _, entry := range base {
		appendEntry(entry)
	}
	for _, entry := range delta {
		appendEntry(entry)
	}
	return merged
}

func compactOllamaModelMetadata(raw map[string]any) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	compact := map[string]any{}
	if details, ok := raw["details"]; ok {
		compact["details"] = details
	}
	if capabilities, ok := raw["capabilities"]; ok {
		compact["capabilities"] = capabilities
	}
	if modelInfo, ok := raw["model_info"]; ok {
		compact["model_info"] = modelInfo
	}
	if parameters, ok := raw["parameters"]; ok {
		compact["parameters"] = parameters
	}
	if modelfile, ok := raw["modelfile"]; ok {
		compact["modelfile"] = modelfile
	}
	return compact
}

func collectExploreModelMetadata() map[string]map[string]any {
	manager := runtime.NewManager()
	driver, err := manager.DriverFor("ollama")
	if err != nil || !driver.IsInstalled() {
		return nil
	}
	metadataDriver, ok := driver.(runtime.ModelMetadataDriver)
	if !ok {
		return nil
	}
	modelIDs := driver.ListModels()
	if len(modelIDs) == 0 {
		return nil
	}
	sort.Strings(modelIDs)
	if len(modelIDs) > 64 {
		modelIDs = modelIDs[:64]
	}
	metadataByModel := make(map[string]map[string]any, len(modelIDs))
	for _, modelID := range modelIDs {
		trimmed := strings.TrimSpace(modelID)
		if trimmed == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		raw, showErr := metadataDriver.ShowModel(ctx, trimmed)
		cancel()
		if showErr != nil {
			continue
		}
		compact := compactOllamaModelMetadata(raw)
		if len(compact) == 0 {
			continue
		}
		metadataByModel[trimmed] = compact
	}
	if len(metadataByModel) == 0 {
		return nil
	}
	return metadataByModel
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
	priority := strings.ToLower(strings.TrimSpace(explorePriority))
	if priority == "" {
		priority = "balanced"
	}
	switch priority {
	case "light", "balanced", "powerful":
	default:
		return fmt.Errorf("invalid priority; use light, balanced, or powerful")
	}
	modelType := strings.ToLower(strings.TrimSpace(exploreModelType))
	if modelType == "" {
		modelType = "any"
	}
	switch modelType {
	case "any", "dense", "quantized":
	default:
		return fmt.Errorf("invalid model-type; use any, dense, or quantized")
	}

	cl, err := client.New(cfg.ServerURL, cfg.Token, cfg.Insecure)
	if err != nil {
		return fmt.Errorf("error creating API client: %w", err)
	}
	modelMetadata := collectExploreModelMetadata()
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
			Priority:            priority,
			ModelType:           modelType,
			MaxAttempts:         5,
			Capabilities:        &capabilities,
			ModelMetadata:       modelMetadata,
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

		challengeSessionID := ""
		prompts := localEvalPrompts(workload, "quick")
		challengeCtx, challengeCancel := context.WithTimeout(context.Background(), 20*time.Second)
		challengeStart, challengeErr := cl.StartEvalSession(challengeCtx, workload, "quick", "mantler-standard")
		challengeCancel()
		if challengeErr == nil && challengeStart != nil {
			challengeSessionID = strings.TrimSpace(challengeStart.SessionID)
			if len(challengeStart.Prompts) > 0 {
				prompts = make([]agenteval.Prompt, 0, len(challengeStart.Prompts))
				for _, prompt := range challengeStart.Prompts {
					prompts = append(prompts, agenteval.Prompt{
						ID:            prompt.ID,
						Category:      prompt.Category,
						Workload:      prompt.Workload,
						Prompt:        prompt.Prompt,
						SystemPrompt:  prompt.SystemPrompt,
						MaxTokens:     prompt.MaxTokens,
						ContextLength: prompt.ContextLength,
						SuiteID:       prompt.SuiteID,
						SuiteVersion:  prompt.SuiteVersion,
						Choices:       prompt.Choices,
						Subject:       prompt.Subject,
					})
				}
			}
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
		if challengeSessionID != "" {
			for _, sample := range summary.Samples {
				respondCtx, respondCancel := context.WithTimeout(context.Background(), 15*time.Second)
				_, respondErr := cl.RespondToChallenge(
					respondCtx,
					challengeSessionID,
					sample.PromptID,
					sample.Output,
					sample.LatencyMs,
					sample.TTFTMs,
					sample.TokensPerSec,
					sample.OutputTokens,
				)
				respondCancel()
				if respondErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to upload challenge response for prompt %s: %v\n", sample.PromptID, respondErr)
				}
			}
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
			evidenceCount := score.EvidenceCount
			if evidenceCount <= 0 {
				evidenceCount = score.EvidenceSignals
			}
			profileID := strings.TrimSpace(score.ProfileID)
			if profileID == "" {
				profileID = "balanced"
			}
			fmt.Printf(
				"Score: %s (%s preset, based on %d runs, formula v%d)\n",
				formatStackScore(score.Overall),
				profileID,
				evidenceCount,
				max(1, score.FormulaVersion),
			)
		}
		if syncErr := syncCompatCatalogDelta(cl); syncErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to sync compat catalog delta: %v\n", syncErr)
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

func loadCompatCatalogSyncState() compatCatalogSyncState {
	raw, err := os.ReadFile(compatCatalogSyncStatePath)
	if err != nil {
		return compatCatalogSyncState{}
	}
	var state compatCatalogSyncState
	if err := json.Unmarshal(raw, &state); err != nil {
		return compatCatalogSyncState{}
	}
	return state
}

func saveCompatCatalogSyncState(state compatCatalogSyncState) error {
	if err := os.MkdirAll(filepath.Dir(compatCatalogSyncStatePath), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(compatCatalogSyncStatePath, raw, 0o600)
}

func syncCompatCatalogDelta(cl *client.Client) error {
	state := loadCompatCatalogSyncState()
	cachedCatalog := loadCompatCatalogCache()
	var since *time.Time
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(state.LastSyncedAt)); err == nil {
		since = &parsed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	catalog, err := cl.GetCompatCatalog(
		ctx,
		[]string{"models", "runtime_rules", "gpu_capabilities", "curated_recipes", "integrations"},
		since,
	)
	if err != nil {
		return err
	}
	mergedCatalog := mergeCompatCatalog(cachedCatalog, *catalog)
	if err := saveCompatCatalogCache(mergedCatalog); err != nil {
		return err
	}
	rawCatalog, err := json.Marshal(mergedCatalog)
	if err != nil {
		return err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(rawCatalog))
	return saveCompatCatalogSyncState(compatCatalogSyncState{
		LastSyncedAt:    time.Now().UTC().Format(time.RFC3339),
		LastCatalogHash: hash,
	})
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
		avgQuality := totalQuality / float64(len(summary.Samples))
		normalizedQuality := avgQuality / 80.0
		if normalizedQuality > 0 {
			overall = math.Floor(1000.0 * math.Pow(normalizedQuality, 1.5))
		}
	}
	fingerprint := strings.TrimSpace(exploreResp.Plan.BaseFingerprint)
	if fingerprint == "" {
		fingerprint = strings.TrimSpace(exploreResp.Plan.MantleFingerprint)
	}
	return &types.ScoreResponse{
		MantleFingerprint: fingerprint,
		Overall:           overall,
		ProfileID:         "balanced",
		FormulaVersion:    1,
		ConfidenceTier:    "provisional",
		EvidenceSignals:   len(summary.Samples),
		EvidenceCount:     len(summary.Samples),
		UpdatedAt:         time.Now().UTC().Format(time.RFC3339),
	}
}

func formatStackScore(value float64) string {
	if value <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%s", formatThousands(int64(value+0.5)))
}

func formatThousands(value int64) string {
	raw := strconv.FormatInt(value, 10)
	if len(raw) <= 3 {
		return raw
	}
	var b strings.Builder
	prefix := len(raw) % 3
	if prefix == 0 {
		prefix = 3
	}
	b.WriteString(raw[:prefix])
	for i := prefix; i < len(raw); i += 3 {
		b.WriteString(",")
		b.WriteString(raw[i : i+3])
	}
	return b.String()
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
		sanitizedDestination, err := sanitizeRuntimePlanPath(destination)
		if err != nil {
			return nil, err
		}
		content := []byte(file.Content)
		if len(content) == 0 && source != "" {
			sourceFile, err := os.Open(source)
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
		original, readErr := os.ReadFile(sanitizedDestination)
		if readErr != nil {
			if !errors.Is(readErr, os.ErrNotExist) {
				return nil, readErr
			}
			destExists = false
			original = nil
		}
		if err := os.MkdirAll(filepath.Dir(sanitizedDestination), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(sanitizedDestination, content, 0o600); err != nil {
			return nil, err
		}
		restoreFns = append(restoreFns, func() {
			if destExists {
				_ = os.WriteFile(sanitizedDestination, original, 0o600)
				return
			}
			_ = os.Remove(sanitizedDestination)
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

func sanitizeRuntimePlanPath(destination string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(destination))
	if cleaned == "." || cleaned == "" {
		return "", fmt.Errorf("runtime plan destination is empty")
	}
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("runtime plan destination must be absolute: %s", destination)
	}
	allowedRoots := []string{
		"/etc/mantler",
		"/var/lib/mantler",
		"/opt/mantler",
	}
	for _, root := range allowedRoots {
		if cleaned == root || strings.HasPrefix(cleaned, root+"/") {
			return cleaned, nil
		}
	}
	return "", fmt.Errorf("runtime plan destination %q is outside allowed roots", destination)
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
			if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
				target = strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(target, "/")
			}
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
