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
	"github.com/Borgels/mantlerd/internal/config"
	agenteval "github.com/Borgels/mantlerd/internal/eval"
	"github.com/Borgels/mantlerd/internal/runtime"
	"github.com/Borgels/mantlerd/internal/types"
	"github.com/spf13/cobra"
)

var evalCmd = &cobra.Command{
	Use:   "eval <model>",
	Short: "Run workload-aware model evaluation",
	Long: `Run quality-focused benchmark suites against a model.

This command executes benchmark prompts, summarizes pass/quality metrics,
and optionally reports outcomes to the Mantler server.

Use "mantler model benchmark" when you want throughput/latency measurements
(TTFT, tokens/sec, latency) instead of workload quality evaluation.

Examples:
  mantler eval llama3.2
  mantler eval llama3.2 --suite mmlu --profile standard
  mantler eval llama3.2 --json --stealth
  mantler eval --list-suites`,
	Args: func(cmd *cobra.Command, args []string) error {
		listSuites, _ := cmd.Flags().GetBool("list-suites")
		if listSuites {
			if len(args) > 0 {
				return fmt.Errorf("unknown arguments for --list-suites: %s", strings.Join(args, " "))
			}
			return nil
		}
		return cobra.ExactArgs(1)(cmd, args)
	},
	Run: runEval,
}

var (
	evalWorkload       string
	evalProfile        string
	evalSuite          string
	evalRuntime        string
	evalJSON           bool
	evalStealth        bool
	evalRate           bool
	evalListSuites     bool
	evalNonInteractive bool
)

var allowedEvalWorkloads = map[string]struct{}{
	"coding": {}, "chat": {}, "creative": {}, "reasoning": {}, "agents": {}, "vision": {},
}

var allowedEvalSuites = map[string]struct{}{
	"mantler-standard": {},
	"mmlu":             {},
	"gsm8k":            {},
	"gpqa":             {},
}

var evalSuiteOrder = []string{"mantler-standard", "mmlu", "gsm8k", "gpqa"}

type evalReportingContext struct {
	client    *client.Client
	machineID string
}

func init() {
	rootCmd.AddCommand(evalCmd)
	evalCmd.Flags().StringVar(&evalWorkload, "workload", "coding", "Workload (coding, chat, creative, reasoning, agents, vision)")
	evalCmd.Flags().StringVar(&evalProfile, "profile", "quick", "Eval profile (quick, standard, deep)")
	evalCmd.Flags().StringVar(&evalSuite, "suite", "mantler-standard", "Benchmark suite (mantler-standard, mmlu, gsm8k, gpqa)")
	evalCmd.Flags().StringVar(&evalRuntime, "runtime", "", "Runtime hint (llamacpp, ollama, vllm, tensorrt)")
	evalCmd.Flags().BoolVar(&evalJSON, "json", false, "Print raw JSON summary")
	evalCmd.Flags().BoolVar(&evalStealth, "stealth", false, "Run local-only eval (skip prompt fetch and telemetry upload)")
	evalCmd.Flags().BoolVar(&evalRate, "rate", false, "Prompt for an optional 1-5 user rating after the run")
	evalCmd.Flags().BoolVar(&evalListSuites, "list-suites", false, "List supported benchmark suites and exit")
	evalCmd.Flags().BoolVarP(&evalNonInteractive, "non-interactive", "y", false, "Skip interactive prompts (useful in CI)")
}

func runEval(cmd *cobra.Command, args []string) {
	if evalListSuites {
		fmt.Println("Supported benchmark suites:")
		for _, suite := range evalSuiteOrder {
			fmt.Printf("  - %s\n", suite)
		}
		return
	}

	modelID := strings.TrimSpace(args[0])
	if modelID == "" {
		fmt.Fprintln(os.Stderr, "Model ID is required.")
		os.Exit(1)
	}
	workload := strings.TrimSpace(evalWorkload)
	if workload == "" {
		workload = "coding"
	}
	if _, ok := allowedEvalWorkloads[workload]; !ok {
		fmt.Fprintln(os.Stderr, "Invalid workload. Use coding, chat, creative, reasoning, agents, or vision.")
		os.Exit(1)
	}
	profile := strings.TrimSpace(evalProfile)
	if profile == "" {
		profile = "quick"
	}
	if profile != "quick" && profile != "standard" && profile != "deep" {
		fmt.Fprintln(os.Stderr, "Invalid profile. Use quick, standard, or deep.")
		os.Exit(1)
	}
	suiteID := strings.TrimSpace(evalSuite)
	if suiteID == "" {
		suiteID = "mantler-standard"
	}
	if _, ok := allowedEvalSuites[suiteID]; !ok {
		fmt.Fprintf(os.Stderr, "Invalid suite: %s\n", suiteID)
		fmt.Fprintf(os.Stderr, "Valid suites: %s\n", strings.Join(evalSuiteOrder, ", "))
		os.Exit(1)
	}

	var reportingCtx *evalReportingContext
	if !evalStealth {
		var reportingErr error
		reportingCtx, reportingErr = loadEvalReportingContext(cmd)
		if reportingErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: eval telemetry reporting disabled: %v\n", reportingErr)
		}
	} else {
		fmt.Fprintln(os.Stderr, "Stealth mode enabled: running local eval only (no prompt fetch, no telemetry upload).")
	}
	prompts := localEvalPrompts(workload, profile)
	evalSessionToken := ""
	runtimeHint := strings.TrimSpace(evalRuntime)
	if reportingCtx != nil {
		promptCtx, promptCancel := context.WithTimeout(context.Background(), 15*time.Second)
		serverPrompts, sessionToken, promptErr := reportingCtx.client.GetEvalPrompts(promptCtx, workload, profile, suiteID)
		promptCancel()
		if promptErr == nil && len(serverPrompts) > 0 {
			prompts = serverPrompts
			evalSessionToken = strings.TrimSpace(sessionToken)
		}
	}
	manager := runtime.NewManager()
	if runtimeHint != "" {
		runtimeName := runtimeHint
		_ = manager.EnsureRuntime(runtimeName)
	}
	runner := agenteval.NewRunner(manager)
	effectiveRuntime := resolveEvalRuntime(manager, modelID, runtimeHint)

	fmt.Printf("Evaluating %s for %s (%s, suite %s, %d prompts)\n", modelID, workload, profile, suiteID, len(prompts))
	summary, err := runner.Run(context.Background(), modelID, workload, profile, prompts, nil)
	if evalSessionToken != "" {
		summary.EvalSessionToken = evalSessionToken
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Eval failed: %v\n", err)
		os.Exit(1)
	}

	rating := 0
	if !evalJSON {
		printEvalSummary(summary)
		if evalRate && !evalNonInteractive {
			reader := bufio.NewReader(os.Stdin)
			fmt.Print("Rate this model (1-5, or skip): ")
			answer, _ := reader.ReadString('\n')
			answer = strings.TrimSpace(answer)
			switch answer {
			case "1", "2", "3", "4", "5":
				rating = int(answer[0] - '0')
			}
		}
	}

	if evalJSON {
		raw, marshalErr := json.MarshalIndent(summary, "", "  ")
		if marshalErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to encode JSON output: %v\n", marshalErr)
			os.Exit(1)
		}
		fmt.Println(string(raw))
	}

	if reportingCtx != nil {
		suiteVersion := ""
		if len(prompts) > 0 {
			suiteVersion = strings.TrimSpace(prompts[0].SuiteVersion)
		}
		err = reportEvalSummary(
			reportingCtx.client,
			reportingCtx.machineID,
			modelID,
			effectiveRuntime,
			summary,
			workload,
			rating,
			evalSessionToken,
			suiteID,
			suiteVersion,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to report eval outcomes to server: %v\n", err)
			return
		}
		if !evalJSON {
			fmt.Println("Results reported to Mantler server.")
		}
	}
}

func loadEvalReportingContext(cmd *cobra.Command) (*evalReportingContext, error) {
	configPath := cfgFile
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}
	fileCfg := config.Config{}
	if loadedCfg, err := config.Load(configPath); err == nil {
		fileCfg = loadedCfg
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("load config: %w", err)
	}

	flagsCfg := config.Config{}
	if cmd.Flags().Changed("server") {
		flagsCfg.ServerURL = serverURL
	}
	if cmd.Flags().Changed("token") {
		flagsCfg.Token = token
	}
	if cmd.Flags().Changed("machine") {
		flagsCfg.MachineID = machineID
	}
	if cmd.Flags().Changed("insecure") {
		flagsCfg.Insecure = insecure
	}
	cfg := config.Merge(fileCfg, flagsCfg)
	if cfg.ServerURL == "" || cfg.Token == "" || cfg.MachineID == "" {
		return nil, fmt.Errorf("missing config fields (server/token/machine)")
	}
	cl, err := client.New(cfg.ServerURL, cfg.Token, cfg.Insecure)
	if err != nil {
		return nil, fmt.Errorf("create api client: %w", err)
	}
	return &evalReportingContext{
		client:    cl,
		machineID: cfg.MachineID,
	}, nil
}

func reportEvalSummary(
	cl *client.Client,
	machineID string,
	modelID string,
	runtimeName string,
	summary types.EvalRunSummary,
	workload string,
	rating int,
	evalSessionToken string,
	benchmarkSuiteID string,
	benchmarkSuiteVersion string,
) error {
	baseFingerprint := buildBaseFingerprint(machineID, modelID, runtimeName, "")
	events := make([]types.OutcomeEvent, 0, len(summary.Samples)+1)
	for _, sample := range summary.Samples {
		eventType := "task_failure"
		if sample.Passed {
			eventType = "task_success"
		}
		score := sample.QualityScore
		events = append(events, types.OutcomeEvent{
			BaseFingerprint:       baseFingerprint,
			EventType:             eventType,
			EvidenceKind:          "benchmark",
			Workload:              workload,
			BenchmarkSuiteID:      strings.TrimSpace(benchmarkSuiteID),
			BenchmarkSuiteVersion: strings.TrimSpace(benchmarkSuiteVersion),
			EvalPromptID:          sample.PromptID,
			EvalOutput:            sample.Output,
			EvalSessionToken:      strings.TrimSpace(evalSessionToken),
			DurationMs:            int64(sample.LatencyMs),
			TokenUsage: &types.OutcomeTokenUsage{
				PromptTokens:     0,
				CompletionTokens: sample.OutputTokens,
				TotalTokens:      sample.OutputTokens,
			},
			QualityScore: &score,
			Detail:       sample.Notes,
			Timestamp:    time.Now().UTC().Format(time.RFC3339),
		})
	}
	if rating > 0 {
		score := float64(rating * 20)
		events = append(events, types.OutcomeEvent{
			BaseFingerprint:       baseFingerprint,
			EventType:             "task_success",
			EvidenceKind:          "benchmark",
			Workload:              workload,
			BenchmarkSuiteID:      strings.TrimSpace(benchmarkSuiteID),
			BenchmarkSuiteVersion: strings.TrimSpace(benchmarkSuiteVersion),
			EvalSessionToken:      strings.TrimSpace(evalSessionToken),
			QualityScore:          &score,
			Detail:                fmt.Sprintf("User preference rating: %d/5", rating),
			Timestamp:             time.Now().UTC().Format(time.RFC3339),
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := cl.Checkin(ctx, types.CheckinRequest{
		MachineID:     machineID,
		OutcomeEvents: events,
	})
	return err
}

func resolveEvalRuntime(manager *runtime.Manager, modelID string, runtimeHint string) string {
	hint := strings.TrimSpace(runtimeHint)
	if hint != "" {
		return hint
	}
	targetModel := strings.TrimSpace(modelID)
	if targetModel == "" {
		return "ollama"
	}
	for _, runtimeName := range manager.InstalledRuntimes() {
		driver, err := manager.DriverFor(runtimeName)
		if err != nil {
			continue
		}
		for _, installed := range driver.ListModels() {
			if strings.TrimSpace(installed) == targetModel {
				return runtimeName
			}
		}
	}
	return "ollama"
}

func stableHash(input string) string {
	hash := uint32(5381)
	for _, ch := range input {
		hash = ((hash << 5) + hash) ^ uint32(ch)
	}
	return fmt.Sprintf("%08x", hash)
}

func hashSeed(seed string) string {
	runes := []rune(seed)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	reversed := string(runes)
	return stableHash(seed) + stableHash(reversed)
}

func buildBaseFingerprint(machineID string, modelID string, runtimeName string, backend string) string {
	seed := strings.TrimSpace(machineID) + "|" +
		strings.TrimSpace(modelID) + "|" +
		strings.TrimSpace(runtimeName) + "|" +
		strings.TrimSpace(backend)
	return hashSeed(seed)
}

func localEvalPrompts(workload string, profile string) []agenteval.Prompt {
	base := []agenteval.Prompt{
		{ID: workload + ":quality-1", Category: "quality", Workload: workload, Prompt: "Evaluate response quality for the selected workload."},
		{ID: workload + ":speed-1", Category: "speed", Workload: workload, Prompt: "Measure throughput and latency."},
		{ID: workload + ":stability-1", Category: "stability", Workload: workload, Prompt: "Check instruction consistency across runs."},
		{ID: workload + ":tool-1", Category: "tool_use", Workload: workload, Prompt: "Assess tool-call correctness and order."},
		{ID: workload + ":context-1", Category: "context", Workload: workload, Prompt: "Evaluate short vs long context behavior.", ContextLength: "long"},
		{ID: workload + ":eff-1", Category: "efficiency", Workload: workload, Prompt: "Estimate resource efficiency under workload."},
	}
	switch profile {
	case "deep":
		return append(base, append(base, base...)...)
	case "standard":
		return append(base, base...)
	default:
		return base
	}
}

func printEvalSummary(summary types.EvalRunSummary) {
	var totalScore float64
	passed := 0
	for _, sample := range summary.Samples {
		totalScore += sample.QualityScore
		if sample.Passed {
			passed++
		}
	}
	avg := 0.0
	if len(summary.Samples) > 0 {
		avg = totalScore / float64(len(summary.Samples))
	}
	fmt.Printf("Samples: %d/%d passed, avg quality %.1f\n", passed, len(summary.Samples), avg)
	if summary.ResourceUsage != nil {
		fmt.Printf("Resource usage: RAM %d MB", summary.ResourceUsage.RAMMB)
		if summary.ResourceUsage.VRAMMB > 0 {
			fmt.Printf(", VRAM %d MB", summary.ResourceUsage.VRAMMB)
		}
		fmt.Println()
	}
}
