package commands

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Borgels/mantlerd/internal/config"
	agenteval "github.com/Borgels/mantlerd/internal/eval"
	agentruntime "github.com/Borgels/mantlerd/internal/runtime"
	agenttrainer "github.com/Borgels/mantlerd/internal/trainer"
	"github.com/Borgels/mantlerd/internal/types"
)

// ErrCommandCancelled is returned when a command is cancelled via CancelCommand.
var ErrCommandCancelled = errors.New("command cancelled")

type Executor struct {
	runtimeManager *agentruntime.Manager
	trainerManager *agenttrainer.Manager
	cfg            config.Config
	progress       func(payload types.AckRequest)
	outcome        func(event types.OutcomeEvent)

	// Active command cancellation support
	activeCancelMu sync.Mutex
	activeCancel   map[string]context.CancelFunc

	// Active orchestrator manifests by command ID.
	activeManifestMu sync.Mutex
	activeManifests  map[string]*types.ResourceManifest
}

type ExecutionResult struct {
	Details       string
	ResultPayload interface{}
}

const (
	CommandLaneHeavy = "heavy"
	CommandLaneLight = "light"
)

// CommandLane classifies a command type into execution lanes.
// Unknown command types default to heavy for safety.
func CommandLane(commandType string) string {
	switch commandType {
	case "cancel_command",
		"health_check",
		"training_status",
		"model_eval",
		"sync_harnesses",
		"harness_login",
		"run_harness_exec",
		"run_orchestrator_exec":
		return CommandLaneLight
	default:
		return CommandLaneHeavy
	}
}

const gooseInstallScriptURL = "https://github.com/block/goose/raw/main/download_cli.sh"

func NewExecutor(
	runtimeManager *agentruntime.Manager,
	trainerManager *agenttrainer.Manager,
	cfg config.Config,
	progress func(payload types.AckRequest),
	outcome func(event types.OutcomeEvent),
) *Executor {
	return &Executor{
		runtimeManager:  runtimeManager,
		trainerManager:  trainerManager,
		cfg:             cfg,
		progress:        progress,
		outcome:         outcome,
		activeCancel:    make(map[string]context.CancelFunc),
		activeManifests: make(map[string]*types.ResourceManifest),
	}
}

func (e *Executor) emitOutcome(event types.OutcomeEvent) {
	if e.outcome == nil {
		return
	}
	if strings.TrimSpace(event.EventType) == "" {
		return
	}
	if strings.TrimSpace(event.Timestamp) == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	e.outcome(event)
}

func (e *Executor) registerManifest(commandID string, manifest *types.ResourceManifest) {
	if manifest == nil || strings.TrimSpace(commandID) == "" {
		return
	}
	e.activeManifestMu.Lock()
	e.activeManifests[commandID] = manifest
	e.activeManifestMu.Unlock()
}

func (e *Executor) unregisterManifest(commandID string) {
	if strings.TrimSpace(commandID) == "" {
		return
	}
	e.activeManifestMu.Lock()
	delete(e.activeManifests, commandID)
	e.activeManifestMu.Unlock()
}

func (e *Executor) ActiveManifestModelIDs(localMachineID string) []string {
	result := make([]string, 0)
	seen := make(map[string]struct{})
	e.activeManifestMu.Lock()
	defer e.activeManifestMu.Unlock()
	for _, manifest := range e.activeManifests {
		if manifest == nil {
			continue
		}
		for _, model := range manifest.Models {
			if model.Source != "machine" {
				continue
			}
			if strings.TrimSpace(model.MachineID) != strings.TrimSpace(localMachineID) {
				continue
			}
			modelID := strings.TrimSpace(model.ModelID)
			if modelID == "" {
				continue
			}
			if _, ok := seen[modelID]; ok {
				continue
			}
			seen[modelID] = struct{}{}
			result = append(result, modelID)
		}
	}
	return result
}

// CancelCommand attempts to cancel an in-flight command by its ID.
// Returns true if the command was found and cancellation was signalled.
func (e *Executor) CancelCommand(commandID string) bool {
	e.activeCancelMu.Lock()
	defer e.activeCancelMu.Unlock()
	if cancel, ok := e.activeCancel[commandID]; ok {
		cancel()
		return true
	}
	return false
}

func (e *Executor) registerCancel(commandID string, cancel context.CancelFunc) {
	e.activeCancelMu.Lock()
	e.activeCancel[commandID] = cancel
	e.activeCancelMu.Unlock()
}

func (e *Executor) unregisterCancel(commandID string) {
	e.activeCancelMu.Lock()
	delete(e.activeCancel, commandID)
	e.activeCancelMu.Unlock()
}

// Execute runs a command without cancellation support (legacy).
func (e *Executor) Execute(command types.AgentCommand) (ExecutionResult, error) {
	return e.ExecuteWithContext(context.Background(), command)
}

// ExecuteWithContext runs a command with cancellation support.
func (e *Executor) ExecuteWithContext(ctx context.Context, command types.AgentCommand) (ExecutionResult, error) {
	// Create a cancellable context for this command
	cmdCtx, cancel := context.WithCancel(ctx)
	e.registerCancel(command.ID, cancel)
	defer e.unregisterCancel(command.ID)

	switch command.Type {
	case "cancel_command":
		targetID := optionalStringParam(command.Params, "targetCommandId")
		if targetID == "" {
			return ExecutionResult{}, fmt.Errorf("missing targetCommandId param")
		}
		if e.CancelCommand(targetID) {
			return ExecutionResult{Details: "cancellation signalled"}, nil
		}
		return ExecutionResult{Details: "command not found or already completed"}, nil
	case "install_runtime":
		rawRuntime, ok := command.Params["runtime"]
		if !ok {
			return ExecutionResult{}, fmt.Errorf("missing runtime param")
		}
		runtimeName, ok := rawRuntime.(string)
		if !ok || runtimeName == "" {
			return ExecutionResult{}, fmt.Errorf("invalid runtime param")
		}
		if strings.EqualFold(strings.TrimSpace(runtimeName), "vllm") {
			return ExecutionResult{}, withVLLMRuntimeEnv(command.Params, func() error {
				return e.runtimeManager.InstallRuntime(runtimeName)
			})
		}
		if strings.EqualFold(strings.TrimSpace(runtimeName), "llamacpp") {
			return ExecutionResult{}, withLlamaCppConfig(command.Params, func() error {
				return e.runtimeManager.InstallRuntime(runtimeName)
			})
		}
		if strings.EqualFold(strings.TrimSpace(runtimeName), "quantcpp") {
			return ExecutionResult{}, withQuantCppConfig(command.Params, func() error {
				return e.runtimeManager.InstallRuntime(runtimeName)
			})
		}
		return ExecutionResult{}, e.runtimeManager.InstallRuntime(runtimeName)
	case "install_trainer":
		if e.trainerManager == nil {
			return ExecutionResult{}, fmt.Errorf("trainer manager unavailable")
		}
		trainerType := optionalStringParam(command.Params, "trainerType")
		if trainerType == "" {
			trainerType = optionalStringParam(command.Params, "type")
		}
		if trainerType == "" {
			trainerType = "unsloth"
		}
		trainerType = strings.ToLower(strings.TrimSpace(trainerType))
		version, err := e.trainerManager.Install(cmdCtx, trainerType)
		if err != nil {
			return ExecutionResult{}, err
		}
		return ExecutionResult{
			Details: fmt.Sprintf("%s installed", trainerType),
			ResultPayload: map[string]any{
				"trainerType": trainerType,
				"version":     version,
			},
		}, nil
	case "uninstall_trainer":
		if e.trainerManager == nil {
			return ExecutionResult{}, fmt.Errorf("trainer manager unavailable")
		}
		trainerType := optionalStringParam(command.Params, "trainerType")
		if trainerType == "" {
			trainerType = optionalStringParam(command.Params, "type")
		}
		if trainerType == "" {
			return ExecutionResult{}, fmt.Errorf("missing trainerType param")
		}
		if err := e.trainerManager.Uninstall(cmdCtx, trainerType); err != nil {
			return ExecutionResult{}, err
		}
		return ExecutionResult{
			Details: fmt.Sprintf("%s uninstalled", trainerType),
		}, nil
	case "pull_model":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return ExecutionResult{}, err
		}
		flags := modelFeatureFlagsParam(command.Params)
		runtimeName := optionalStringParam(command.Params, "runtime")
		if err := applyRuntimeProfileOverrides(command.Params, runtimeName); err != nil {
			return ExecutionResult{}, err
		}
		if err := e.runtimeManager.PrepareModelWithRuntimeCtx(cmdCtx, modelID, runtimeName, flags); err != nil {
			if errors.Is(err, context.Canceled) {
				return ExecutionResult{}, ErrCommandCancelled
			}
			return ExecutionResult{}, err
		}
		return ExecutionResult{}, nil
	case "start_model":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return ExecutionResult{}, err
		}
		flags := modelFeatureFlagsParam(command.Params)
		runtimeName := optionalStringParam(command.Params, "runtime")
		if err := applyRuntimeProfileOverrides(command.Params, runtimeName); err != nil {
			return ExecutionResult{}, err
		}
		compatibilityPlanID := optionalStringParam(command.Params, "compatibilityPlanId")
		mantleFingerprint := optionalStringParam(command.Params, "mantleFingerprint")
		e.runtimeManager.SetActiveContext(compatibilityPlanID, mantleFingerprint)
		defer e.runtimeManager.ClearActiveContext()
		return ExecutionResult{}, e.runtimeManager.StartModelWithRuntime(modelID, runtimeName, flags)
	case "stop_model":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return ExecutionResult{}, err
		}
		runtimeName := optionalStringParam(command.Params, "runtime")
		compatibilityPlanID := optionalStringParam(command.Params, "compatibilityPlanId")
		mantleFingerprint := optionalStringParam(command.Params, "mantleFingerprint")
		baseFingerprint := optionalStringParam(command.Params, "baseFingerprint")
		startedAt := time.Now()
		stopErr := e.runtimeManager.StopModelWithRuntime(modelID, runtimeName)
		eventType := "stop_success"
		detail := "model stop succeeded"
		if stopErr != nil {
			eventType = "stop_failure"
			detail = stopErr.Error()
		}
		e.emitOutcome(types.OutcomeEvent{
			PlanID:            compatibilityPlanID,
			MantleFingerprint: mantleFingerprint,
			BaseFingerprint:   baseFingerprint,
			EventType:         eventType,
			DurationMs:        time.Since(startedAt).Milliseconds(),
			Detail:            detail,
			Timestamp:         time.Now().UTC().Format(time.RFC3339),
		})
		return ExecutionResult{}, stopErr
	case "remove_model":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return ExecutionResult{}, err
		}
		runtimeName := optionalStringParam(command.Params, "runtime")
		return ExecutionResult{}, e.runtimeManager.RemoveModelWithRuntime(modelID, runtimeName)
	case "build_model":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return ExecutionResult{}, err
		}
		opts := parseBuildOptions(command.Params)
		if err := e.runtimeManager.BuildModel(cmdCtx, modelID, opts); err != nil {
			if errors.Is(err, context.Canceled) {
				return ExecutionResult{}, ErrCommandCancelled
			}
			return ExecutionResult{}, err
		}
		return ExecutionResult{Details: "engine built successfully"}, nil
	case "health_check":
		return ExecutionResult{}, nil
	case "model_eval":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return ExecutionResult{}, err
		}
		workload := optionalStringParam(command.Params, "workload")
		if workload == "" {
			workload = "coding"
		}
		profile := optionalStringParam(command.Params, "profile")
		if profile == "" {
			profile = "standard"
		}
		prompts, err := evalPromptsParam(command.Params)
		if err != nil {
			return ExecutionResult{}, err
		}
		runner := agenteval.NewRunner(e.runtimeManager)
		summary, err := runner.Run(cmdCtx, modelID, workload, profile, prompts, func(progress agenteval.Progress) {
			if e.progress == nil {
				return
			}
			raw, marshalErr := json.Marshal(map[string]any{
				"progress": map[string]any{
					"scope":         "model_eval",
					"category":      progress.Category,
					"currentPrompt": progress.CurrentPrompt,
					"completed":     progress.Completed,
					"total":         progress.Total,
				},
			})
			if marshalErr != nil {
				return
			}
			e.progress(types.AckRequest{
				CommandID: command.ID,
				Status:    "in_progress",
				Details:   string(raw),
			})
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return ExecutionResult{}, ErrCommandCancelled
			}
			return ExecutionResult{}, err
		}
		if token := optionalStringParam(command.Params, "evalSessionToken"); token != "" {
			summary.EvalSessionToken = token
		}
		details, err := json.Marshal(summary)
		if err != nil {
			return ExecutionResult{}, fmt.Errorf("encode eval summary: %w", err)
		}
		return ExecutionResult{
			Details:       string(details),
			ResultPayload: summary,
		}, nil
	case "start_training":
		if e.trainerManager == nil {
			return ExecutionResult{}, fmt.Errorf("trainer manager unavailable")
		}
		trainerType := optionalStringParam(command.Params, "trainerType")
		if trainerType == "" {
			trainerType = "unsloth"
		}
		trainerType = strings.ToLower(strings.TrimSpace(trainerType))
		request := agenttrainer.TrainingRequest{
			CommandID:       command.ID,
			TrainerID:       optionalStringParam(command.Params, "trainerId"),
			TrainerType:     trainerType,
			Method:          optionalStringParam(command.Params, "method"),
			BaseModel:       optionalStringParam(command.Params, "baseModel"),
			Dataset:         optionalStringParam(command.Params, "dataset"),
			Hyperparameters: optionalMapParam(command.Params, "hyperparameters"),
			ExportFormats:   optionalStringArrayParam(command.Params, "exportFormats"),
			TargetRuntime:   optionalStringParam(command.Params, "targetRuntime"),
		}
		if request.TrainerID == "" {
			request.TrainerID = "trainer-" + trainerType
		}
		result, err := e.trainerManager.StartTraining(cmdCtx, request, func(progress agenttrainer.TrainingProgress) {
			if e.progress == nil {
				return
			}
			progressPayload := map[string]any{"scope": "training"}
			rawProgress, marshalErr := json.Marshal(progress)
			if marshalErr != nil {
				return
			}
			if unmarshalErr := json.Unmarshal(rawProgress, &progressPayload); unmarshalErr != nil {
				return
			}
			raw, marshalErr := json.Marshal(progressPayload)
			if marshalErr != nil {
				return
			}
			e.progress(types.AckRequest{
				CommandID: command.ID,
				Status:    "in_progress",
				Details:   string(raw),
			})
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return ExecutionResult{}, ErrCommandCancelled
			}
			return ExecutionResult{}, err
		}
		return ExecutionResult{
			Details:       result.Detail,
			ResultPayload: result,
		}, nil
	case "stop_training":
		if e.trainerManager == nil {
			return ExecutionResult{}, fmt.Errorf("trainer manager unavailable")
		}
		targetCommandID := optionalStringParam(command.Params, "targetCommandId")
		if targetCommandID == "" {
			targetCommandID = optionalStringParam(command.Params, "commandId")
		}
		if targetCommandID == "" {
			return ExecutionResult{}, fmt.Errorf("missing targetCommandId param")
		}
		saveCheckpoint := optionalBoolParam(command.Params, "saveCheckpoint", true)
		if err := e.trainerManager.StopTraining(cmdCtx, targetCommandID, saveCheckpoint); err != nil {
			return ExecutionResult{}, err
		}
		return ExecutionResult{
			Details: fmt.Sprintf("training stopped for %s", targetCommandID),
		}, nil
	case "training_status":
		if e.trainerManager == nil {
			return ExecutionResult{}, fmt.Errorf("trainer manager unavailable")
		}
		targetCommandID := optionalStringParam(command.Params, "targetCommandId")
		if targetCommandID == "" {
			targetCommandID = optionalStringParam(command.Params, "commandId")
		}
		if targetCommandID == "" {
			return ExecutionResult{}, fmt.Errorf("missing targetCommandId param")
		}
		progress, ok := e.trainerManager.GetJobStatus(targetCommandID)
		if !ok {
			return ExecutionResult{}, fmt.Errorf("training job not found: %s", targetCommandID)
		}
		return ExecutionResult{
			Details:       "training status",
			ResultPayload: progress,
		}, nil
	case "export_checkpoint":
		if e.trainerManager == nil {
			return ExecutionResult{}, fmt.Errorf("trainer manager unavailable")
		}
		targetCommandID := optionalStringParam(command.Params, "trainingCommandId")
		if targetCommandID == "" {
			targetCommandID = optionalStringParam(command.Params, "commandId")
		}
		if targetCommandID == "" {
			return ExecutionResult{}, fmt.Errorf("missing trainingCommandId param")
		}
		formats := optionalStringArrayParam(command.Params, "formats")
		trainerType := optionalStringParam(command.Params, "trainerType")
		result, err := e.trainerManager.ExportCheckpoint(cmdCtx, targetCommandID, trainerType, formats)
		if err != nil {
			return ExecutionResult{}, err
		}
		return ExecutionResult{
			Details:       fmt.Sprintf("exported checkpoint for %s", targetCommandID),
			ResultPayload: result,
		}, nil
	case "uninstall_runtime":
		rawRuntime, ok := command.Params["runtime"]
		if !ok {
			return ExecutionResult{}, fmt.Errorf("missing runtime param")
		}
		runtimeName, ok := rawRuntime.(string)
		if !ok || runtimeName == "" {
			return ExecutionResult{}, fmt.Errorf("invalid runtime param")
		}
		return ExecutionResult{}, e.runtimeManager.UninstallRuntime(runtimeName)
	case "restart_runtime":
		runtimeName := optionalStringParam(command.Params, "runtime")
		if runtimeName == "" {
			return ExecutionResult{}, e.runtimeManager.RestartRuntime()
		}
		if strings.EqualFold(strings.TrimSpace(runtimeName), "vllm") {
			return ExecutionResult{}, withVLLMRuntimeEnv(command.Params, func() error {
				return e.runtimeManager.RestartRuntimeNamed(runtimeName)
			})
		}
		if strings.EqualFold(strings.TrimSpace(runtimeName), "llamacpp") {
			return ExecutionResult{}, withLlamaCppConfig(command.Params, func() error {
				return e.runtimeManager.RestartRuntimeNamed(runtimeName)
			})
		}
		if strings.EqualFold(strings.TrimSpace(runtimeName), "quantcpp") {
			return ExecutionResult{}, withQuantCppConfig(command.Params, func() error {
				return e.runtimeManager.RestartRuntimeNamed(runtimeName)
			})
		}
		return ExecutionResult{}, e.runtimeManager.RestartRuntimeNamed(runtimeName)
	case "update_agent":
		version := "latest"
		if rawVersion, ok := command.Params["version"]; ok {
			if parsedVersion, ok := rawVersion.(string); ok && strings.TrimSpace(parsedVersion) != "" {
				version = strings.TrimSpace(parsedVersion)
			}
		}
		details, err := e.startAgentUpdate(version)
		if err != nil {
			return ExecutionResult{Details: details}, err
		}
		return ExecutionResult{Details: details}, nil
	case "self_shutdown":
		delaySeconds := intParam(command.Params, "delaySeconds", 10)
		halt := optionalBoolParam(command.Params, "haltMachine", true)
		if err := scheduleSelfShutdown(delaySeconds, halt); err != nil {
			return ExecutionResult{}, err
		}
		return ExecutionResult{
			Details: fmt.Sprintf("self shutdown scheduled in %d seconds (haltMachine=%t)", delaySeconds, halt),
		}, nil
	case "run_harness_exec":
		return e.runHarnessExec(command)
	case "harness_login":
		return e.runHarnessLogin(command)
	case "run_orchestrator_exec":
		return e.runOrchestratorExec(command)
	case "sync_harnesses":
		desiredHarnesses, err := desiredHarnessesParam(command.Params)
		if err != nil {
			return ExecutionResult{}, err
		}
		if e.progress != nil {
			e.progress(types.AckRequest{
				CommandID: command.ID,
				Status:    "in_progress",
				Details:   fmt.Sprintf("Checking %d harness(es)...", len(desiredHarnesses)),
			})
		}
		e.prepareHarnessesForSync(desiredHarnesses, command.ID)
		installedHarnesses := probeInstalledHarnesses(desiredHarnesses)
		readyCount := 0
		offlineCount := 0
		failedCount := 0
		for _, harness := range installedHarnesses {
			switch harness.Status {
			case "ready":
				readyCount++
			case "offline":
				offlineCount++
			case "failed":
				failedCount++
			}
		}
		return ExecutionResult{
			Details: fmt.Sprintf(
				"Harness check complete (%d ready, %d offline, %d failed).",
				readyCount,
				offlineCount,
				failedCount,
			),
			ResultPayload: map[string]any{
				"installedHarnesses": installedHarnesses,
			},
		}, nil
	default:
		return ExecutionResult{}, fmt.Errorf("unsupported command type: %s", command.Type)
	}
}

func stringParam(params map[string]interface{}, key string) (string, error) {
	raw, ok := params[key]
	if !ok {
		return "", fmt.Errorf("missing %s param", key)
	}
	value, ok := raw.(string)
	if !ok || value == "" {
		return "", fmt.Errorf("invalid %s param", key)
	}
	return value, nil
}

func modelFeatureFlagsParam(params map[string]interface{}) *types.ModelFeatureFlags {
	raw, ok := params["featureFlags"]
	if !ok {
		return nil
	}
	obj, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	flags := &types.ModelFeatureFlags{
		Streaming: true,
		Thinking:  false,
	}
	if streaming, ok := obj["streaming"].(bool); ok {
		flags.Streaming = streaming
	}
	if thinking, ok := obj["thinking"].(bool); ok {
		flags.Thinking = thinking
	}
	return flags
}

func intParam(params map[string]interface{}, key string, fallback int) int {
	raw, ok := params[key]
	if !ok {
		return fallback
	}
	switch value := raw.(type) {
	case float64:
		if value <= 0 {
			return fallback
		}
		return int(value)
	case int:
		if value <= 0 {
			return fallback
		}
		return value
	default:
		return fallback
	}
}

func optionalStringParam(params map[string]interface{}, key string) string {
	raw, ok := params[key]
	if !ok {
		return ""
	}
	value, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func optionalStringArrayParam(params map[string]interface{}, key string) []string {
	raw, ok := params[key]
	if !ok {
		return nil
	}
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		value, isString := item.(string)
		if !isString {
			continue
		}
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		result = append(result, trimmed)
	}
	return result
}

func optionalMapParam(params map[string]interface{}, key string) map[string]interface{} {
	raw, ok := params[key]
	if !ok {
		return nil
	}
	value, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	return value
}

func optionalBoolParam(params map[string]interface{}, key string, fallback bool) bool {
	raw, ok := params[key]
	if !ok {
		return fallback
	}
	value, ok := raw.(bool)
	if !ok {
		return fallback
	}
	return value
}

func evalPromptsParam(params map[string]interface{}) ([]agenteval.Prompt, error) {
	raw, ok := params["prompts"]
	if !ok {
		return nil, fmt.Errorf("missing prompts param")
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal prompts param: %w", err)
	}
	var prompts []agenteval.Prompt
	if err := json.Unmarshal(payload, &prompts); err != nil {
		return nil, fmt.Errorf("decode prompts param: %w", err)
	}
	if len(prompts) == 0 {
		return nil, fmt.Errorf("prompts param must not be empty")
	}
	return prompts, nil
}

func parseBuildOptions(params map[string]interface{}) agentruntime.BuildOptions {
	opts := agentruntime.BuildOptions{
		Quantization: optionalStringParam(params, "quantization"),
		TPSize:       intParam(params, "tpSize", 1),
		MaxBatchSize: intParam(params, "maxBatchSize", 4),
		MaxSeqLen:    intParam(params, "maxSeqLen", 8192),
	}
	// Also check nested buildOptions object
	if raw, ok := params["buildOptions"].(map[string]interface{}); ok {
		if q := optionalStringParam(raw, "quantization"); q != "" {
			opts.Quantization = q
		}
		if v := intParam(raw, "tpSize", 0); v > 0 {
			opts.TPSize = v
		}
		if v := intParam(raw, "maxBatchSize", 0); v > 0 {
			opts.MaxBatchSize = v
		}
		if v := intParam(raw, "maxSeqLen", 0); v > 0 {
			opts.MaxSeqLen = v
		}
	}
	return opts
}

type requiredProfileFile struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// runtimeMountPaths maps container mount points to host paths.
// Container paths like /app/* are written to host equivalents.
var runtimeMountPaths = map[string]map[string]string{
	"vllm": {
		"/app": "/opt/mantler/vllm-app",
	},
	"tensorrt": {
		"/app": "/opt/mantler/tensorrt-app",
	},
}

var (
	vllmRuntimeEnvPath        = "/etc/mantler/vllm.env"
	tensorrtRuntimeEnvPath    = "/etc/mantler/tensorrt.env"
	llamaCppRuntimeConfigPath = "/etc/mantler/llamacpp.json"
	quantCppRuntimeConfigPath = "/etc/mantler/quantcpp.json"
	runtimeProfileStateRoot   = "/var/lib/mantler/runtime-profiles"
)

// resolveHostPath converts a container destination path to a host path.
// If the destination starts with a known mount prefix, it's rewritten.
// Otherwise, the destination is used as-is (assumed to be a host path).
func resolveHostPath(runtimeName string, containerPath string) string {
	mounts, ok := runtimeMountPaths[strings.ToLower(runtimeName)]
	if !ok {
		return containerPath
	}
	for containerPrefix, hostPrefix := range mounts {
		if strings.HasPrefix(containerPath, containerPrefix+"/") {
			return hostPrefix + containerPath[len(containerPrefix):]
		}
		if containerPath == containerPrefix {
			return hostPrefix
		}
	}
	return containerPath
}

type runtimeProfileParams struct {
	ID                   string                `json:"id"`
	Label                string                `json:"label"`
	Runtime              string                `json:"runtime"`
	ContainerImage       string                `json:"containerImage"`
	EnvironmentVariables map[string]string     `json:"environmentVariables"`
	ExtraArgs            []string              `json:"extraArgs"`
	RequiredFiles        []requiredProfileFile `json:"requiredFiles"`
}

func decodeRuntimeProfile(params map[string]interface{}, runtimeHint string) (*runtimeProfileParams, string, error) {
	raw, ok := params["runtimeProfile"]
	if !ok || raw == nil {
		return nil, "", nil
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return nil, "", fmt.Errorf("encode runtime profile: %w", err)
	}
	var profile runtimeProfileParams
	if err := json.Unmarshal(payload, &profile); err != nil {
		return nil, "", fmt.Errorf("decode runtime profile: %w", err)
	}
	runtimeName := strings.ToLower(strings.TrimSpace(runtimeHint))
	if runtimeName == "" {
		runtimeName = strings.ToLower(strings.TrimSpace(profile.Runtime))
	}
	if runtimeName == "" {
		return nil, "", nil
	}
	return &profile, runtimeName, nil
}

func runtimeProfileStatePath(runtimeName string) string {
	normalized := strings.ToLower(strings.TrimSpace(runtimeName))
	if normalized == "" {
		normalized = "unknown"
	}
	return filepath.Join(runtimeProfileStateRoot, normalized+".json")
}

func verifyRuntimeProfileApplied(runtimeName string, profile runtimeProfileParams) error {
	var envPath string
	switch strings.ToLower(strings.TrimSpace(runtimeName)) {
	case "vllm":
		envPath = vllmRuntimeEnvPath
	case "tensorrt":
		envPath = tensorrtRuntimeEnvPath
	default:
		return nil
	}

	values := readEnvFile(envPath)
	if image := strings.TrimSpace(profile.ContainerImage); image != "" {
		expected := strings.TrimSpace(values[strings.ToUpper(strings.TrimSpace(runtimeName))+"_CONTAINER_IMAGE"])
		if expected != image {
			return fmt.Errorf("runtime profile image mismatch: want %s, got %s", image, expected)
		}
	}
	for key, value := range profile.EnvironmentVariables {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if strings.TrimSpace(values[strings.TrimSpace(key)]) != strings.TrimSpace(value) {
			return fmt.Errorf("runtime profile env mismatch for %s", key)
		}
	}
	for _, file := range profile.RequiredFiles {
		dst := strings.TrimSpace(file.Destination)
		if dst == "" {
			continue
		}
		hostPath := resolveHostPath(runtimeName, dst)
		if _, err := os.Stat(hostPath); err != nil {
			return fmt.Errorf("required profile file missing at %s: %w", hostPath, err)
		}
	}

	statePath := runtimeProfileStatePath(runtimeName)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return fmt.Errorf("create runtime profile state directory: %w", err)
	}
	statePayload := map[string]any{
		"id":         strings.TrimSpace(profile.ID),
		"label":      strings.TrimSpace(profile.Label),
		"runtime":    strings.ToLower(strings.TrimSpace(runtimeName)),
		"verifiedAt": time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(statePayload)
	if err != nil {
		return fmt.Errorf("marshal runtime profile state: %w", err)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		return fmt.Errorf("write runtime profile state: %w", err)
	}
	return nil
}

func applyRuntimeProfileOverrides(params map[string]interface{}, runtimeHint string) error {
	profile, runtimeName, err := decodeRuntimeProfile(params, runtimeHint)
	if err != nil {
		return err
	}
	if profile == nil || runtimeName == "" {
		return nil
	}
	for _, file := range profile.RequiredFiles {
		src := strings.TrimSpace(file.Source)
		dst := strings.TrimSpace(file.Destination)
		if src == "" || dst == "" {
			continue
		}
		// Resolve container path to host path based on runtime mount mapping
		hostPath := resolveHostPath(runtimeName, dst)
		if err := downloadProfileFile(src, hostPath); err != nil {
			return err
		}
	}

	switch runtimeName {
	case "vllm":
		values := readEnvFile(vllmRuntimeEnvPath)
		if strings.TrimSpace(profile.ContainerImage) != "" {
			values["VLLM_CONTAINER_IMAGE"] = strings.TrimSpace(profile.ContainerImage)
		}
		if len(profile.ExtraArgs) > 0 {
			values["VLLM_EXTRA_ARGS"] = strings.TrimSpace(strings.Join(profile.ExtraArgs, " "))
		}
		for key, value := range profile.EnvironmentVariables {
			if strings.TrimSpace(key) == "" {
				continue
			}
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
		if err := writeEnvFile(vllmRuntimeEnvPath, values); err != nil {
			return err
		}
		return verifyRuntimeProfileApplied(runtimeName, *profile)
	case "tensorrt":
		values := readEnvFile(tensorrtRuntimeEnvPath)
		if strings.TrimSpace(profile.ContainerImage) != "" {
			values["TENSORRT_CONTAINER_IMAGE"] = strings.TrimSpace(profile.ContainerImage)
		}
		if len(profile.ExtraArgs) > 0 {
			values["TENSORRT_EXTRA_ARGS"] = strings.TrimSpace(strings.Join(profile.ExtraArgs, " "))
		}
		for key, value := range profile.EnvironmentVariables {
			if strings.TrimSpace(key) == "" {
				continue
			}
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
		if err := writeEnvFile(tensorrtRuntimeEnvPath, values); err != nil {
			return err
		}
		return verifyRuntimeProfileApplied(runtimeName, *profile)
	default:
		return nil
	}
}

func downloadProfileFile(source string, destination string) error {
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(source)
	if err != nil {
		return fmt.Errorf("download required profile file %s: %w", source, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("download required profile file %s failed with status %d", source, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read required profile file %s: %w", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create required profile file directory: %w", err)
	}
	if err := os.WriteFile(destination, body, 0o644); err != nil {
		return fmt.Errorf("write required profile file: %w", err)
	}
	return nil
}

func readEnvFile(path string) map[string]string {
	values := map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if len(val) >= 2 {
			if strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"") {
				if unquoted, err := strconv.Unquote(val); err == nil {
					val = unquoted
				} else {
					val = strings.Trim(val, "\"")
				}
			} else if strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'") {
				val = val[1 : len(val)-1]
			}
		}
		if key != "" {
			values[key] = val
		}
	}
	return values
}

func writeEnvFile(path string, values map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s=%q", key, values[key]))
	}
	payload := strings.Join(lines, "\n")
	if payload != "" {
		payload += "\n"
	}
	return os.WriteFile(path, []byte(payload), 0o600)
}

func withVLLMRuntimeEnv(params map[string]interface{}, fn func() error) error {
	mode := strings.ToLower(optionalStringParam(params, "runtimeMode"))
	image := optionalStringParam(params, "containerImage")

	var restoreMode *string
	var restoreImage *string

	if mode != "" {
		if mode != "container" && mode != "native" {
			return fmt.Errorf("invalid runtimeMode param for vllm: %s", mode)
		}
		if current, ok := os.LookupEnv("VLLM_RUNTIME_MODE"); ok {
			tmp := current
			restoreMode = &tmp
		}
		_ = os.Setenv("VLLM_RUNTIME_MODE", mode)
	}
	if image != "" {
		if current, ok := os.LookupEnv("VLLM_CONTAINER_IMAGE"); ok {
			tmp := current
			restoreImage = &tmp
		}
		_ = os.Setenv("VLLM_CONTAINER_IMAGE", image)
	}
	if mode != "" || image != "" {
		if err := persistVLLMEnvOverrides(mode, image); err != nil {
			return err
		}
	}
	defer func() {
		if mode != "" {
			if restoreMode != nil {
				_ = os.Setenv("VLLM_RUNTIME_MODE", *restoreMode)
			} else {
				_ = os.Unsetenv("VLLM_RUNTIME_MODE")
			}
		}
		if image != "" {
			if restoreImage != nil {
				_ = os.Setenv("VLLM_CONTAINER_IMAGE", *restoreImage)
			} else {
				_ = os.Unsetenv("VLLM_CONTAINER_IMAGE")
			}
		}
	}()

	return fn()
}

type llamaCppRuntimeConfig struct {
	Model       string `json:"model,omitempty"`
	Port        int    `json:"port,omitempty"`
	Backend     string `json:"backend,omitempty"`
	NGPULayers  int    `json:"nGpuLayers,omitempty"`
	ContextSize int    `json:"contextSize,omitempty"`
}

type quantCppRuntimeConfig struct {
	Model          string `json:"model,omitempty"`
	Port           int    `json:"port,omitempty"`
	KeyQuantType   string `json:"keyQuantType,omitempty"`
	ValueQuantType string `json:"valueQuantType,omitempty"`
	Threads        int    `json:"threads,omitempty"`
	ContextSize    int    `json:"contextSize,omitempty"`
}

func parseOptionalIntParam(params map[string]interface{}, key string) (int, bool) {
	raw, ok := params[key]
	if !ok {
		return 0, false
	}
	switch value := raw.(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	default:
		return 0, false
	}
}

func isSafeQuantToken(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	for _, ch := range trimmed {
		if (ch >= 'a' && ch <= 'z') ||
			(ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '_' || ch == '-' || ch == '.' {
			continue
		}
		return false
	}
	return true
}

func withLlamaCppConfig(params map[string]interface{}, fn func() error) error {
	backend := strings.ToLower(optionalStringParam(params, "backend"))
	nGpuLayers, hasNGpuLayers := parseOptionalIntParam(params, "nGpuLayers")
	contextSize, hasContextSize := parseOptionalIntParam(params, "contextSize")

	if backend == "" && !hasNGpuLayers && !hasContextSize {
		return fn()
	}
	if backend != "" && backend != "cpu" && backend != "cuda" && backend != "vulkan" && backend != "metal" && backend != "rocm" {
		return fmt.Errorf("invalid backend param for llamacpp: %s", backend)
	}
	if hasNGpuLayers && nGpuLayers < -1 {
		return fmt.Errorf("invalid nGpuLayers param for llamacpp: %d", nGpuLayers)
	}
	if hasContextSize && contextSize <= 0 {
		return fmt.Errorf("invalid contextSize param for llamacpp: %d", contextSize)
	}

	cfg := llamaCppRuntimeConfig{
		Port:        1234,
		Backend:     "cpu",
		NGPULayers:  -1,
		ContextSize: 8192,
	}
	if raw, err := os.ReadFile(llamaCppRuntimeConfigPath); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	if backend != "" {
		cfg.Backend = backend
	}
	if hasNGpuLayers {
		cfg.NGPULayers = nGpuLayers
	}
	if hasContextSize {
		cfg.ContextSize = contextSize
	}
	if cfg.Port <= 0 {
		cfg.Port = 1234
	}
	if cfg.ContextSize <= 0 {
		cfg.ContextSize = 8192
	}
	if cfg.NGPULayers == 0 {
		cfg.NGPULayers = -1
	}
	if err := os.MkdirAll(filepath.Dir(llamaCppRuntimeConfigPath), 0o755); err != nil {
		return fmt.Errorf("create llamacpp config directory: %w", err)
	}
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode llamacpp config: %w", err)
	}
	if err := os.WriteFile(llamaCppRuntimeConfigPath, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write llamacpp config: %w", err)
	}
	return fn()
}

func withQuantCppConfig(params map[string]interface{}, fn func() error) error {
	keyQuantType := strings.TrimSpace(optionalStringParam(params, "keyQuantType"))
	valueQuantType := strings.TrimSpace(optionalStringParam(params, "valueQuantType"))
	threads, hasThreads := parseOptionalIntParam(params, "threads")
	contextSize, hasContextSize := parseOptionalIntParam(params, "contextSize")

	if keyQuantType == "" && valueQuantType == "" && !hasThreads && !hasContextSize {
		return fn()
	}
	if hasThreads && threads <= 0 {
		return fmt.Errorf("invalid threads param for quantcpp: %d", threads)
	}
	if hasContextSize && contextSize <= 0 {
		return fmt.Errorf("invalid contextSize param for quantcpp: %d", contextSize)
	}
	if keyQuantType != "" && !isSafeQuantToken(keyQuantType) {
		return fmt.Errorf("invalid keyQuantType param for quantcpp: %s", keyQuantType)
	}
	if valueQuantType != "" && !isSafeQuantToken(valueQuantType) {
		return fmt.Errorf("invalid valueQuantType param for quantcpp: %s", valueQuantType)
	}

	cfg := quantCppRuntimeConfig{
		Port:           8080,
		KeyQuantType:   "uniform_4b",
		ValueQuantType: "q4",
		Threads:        max(1, stdruntime.NumCPU()),
		ContextSize:    8192,
	}
	if raw, err := os.ReadFile(quantCppRuntimeConfigPath); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	if keyQuantType != "" {
		cfg.KeyQuantType = keyQuantType
	}
	if valueQuantType != "" {
		cfg.ValueQuantType = valueQuantType
	}
	if hasThreads {
		cfg.Threads = threads
	}
	if hasContextSize {
		cfg.ContextSize = contextSize
	}
	if cfg.Port <= 0 {
		cfg.Port = 8080
	}
	if strings.TrimSpace(cfg.KeyQuantType) == "" {
		cfg.KeyQuantType = "uniform_4b"
	}
	if strings.TrimSpace(cfg.ValueQuantType) == "" {
		cfg.ValueQuantType = "q4"
	}
	if cfg.Threads <= 0 {
		cfg.Threads = max(1, stdruntime.NumCPU())
	}
	if cfg.ContextSize <= 0 {
		cfg.ContextSize = 8192
	}
	if err := os.MkdirAll(filepath.Dir(quantCppRuntimeConfigPath), 0o755); err != nil {
		return fmt.Errorf("create quantcpp config directory: %w", err)
	}
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode quantcpp config: %w", err)
	}
	if err := os.WriteFile(quantCppRuntimeConfigPath, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write quantcpp config: %w", err)
	}
	return fn()
}

func persistVLLMEnvOverrides(mode string, image string) error {
	const vllmEnvPath = "/etc/mantler/vllm.env"
	if mode == "" && image == "" {
		return nil
	}
	values := map[string]string{}
	order := make([]string, 0, 8)
	addKey := func(key, value string) {
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	if raw, err := os.ReadFile(vllmEnvPath); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			if key == "" {
				continue
			}
			if len(val) >= 2 {
				if strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"") {
					if unquoted, unquoteErr := strconv.Unquote(val); unquoteErr == nil {
						val = unquoted
					} else {
						val = strings.Trim(val, "\"")
					}
				} else if strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'") {
					val = val[1 : len(val)-1]
				}
			}
			addKey(key, val)
		}
	}
	if mode != "" {
		addKey("VLLM_RUNTIME_MODE", mode)
	}
	if image != "" {
		addKey("VLLM_CONTAINER_IMAGE", image)
	}
	if err := os.MkdirAll(filepath.Dir(vllmEnvPath), 0o755); err != nil {
		return fmt.Errorf("create vllm env directory: %w", err)
	}
	lines := make([]string, 0, len(order))
	for _, key := range order {
		lines = append(lines, fmt.Sprintf("%s=%q", key, values[key]))
	}
	payload := strings.Join(lines, "\n")
	if payload != "" {
		payload += "\n"
	}
	if err := os.WriteFile(vllmEnvPath, []byte(payload), 0o600); err != nil {
		return fmt.Errorf("persist vllm runtime settings: %w", err)
	}
	return nil
}

func (e *Executor) startAgentUpdate(version string) (string, error) {
	installer := "https://raw.githubusercontent.com/Borgels/mantlerd/master/scripts/install.sh"
	commandParts := []string{
		"curl", "-fsSL", shellQuote(installer), "|", "sh", "-s", "--",
		"--token", shellQuote(e.cfg.Token),
		"--machine", shellQuote(e.cfg.MachineID),
		"--server", shellQuote(e.cfg.ServerURL),
		"--version", shellQuote(version),
	}
	if e.cfg.Insecure {
		commandParts = append(commandParts, "--insecure")
	}

	commandLine := strings.Join(commandParts, " ")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", commandLine)
	cmd.Env = append(os.Environ(), "MANTLERD_SELF_UPDATE=true", "CLAWCONTROL_AGENT_SELF_UPDATE=true")
	output, err := cmd.CombinedOutput()
	details := strings.TrimSpace(string(output))
	if details == "" {
		details = fmt.Sprintf("Agent update completed (target version: %s)", version)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return details, fmt.Errorf("agent update timed out after 5m")
		}
		if isSignalTermination(err) {
			return "Agent service restart triggered during update. Update likely applied; waiting for service to check in again.", nil
		}
		return details, fmt.Errorf("agent update failed: %w", err)
	}
	return details, nil
}

func isSignalTermination(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return false
	}
	switch status.Signal() {
	case syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGKILL:
		return true
	default:
		return false
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func scheduleSelfShutdown(delaySeconds int, haltMachine bool) error {
	if delaySeconds < 1 {
		delaySeconds = 1
	}
	command := fmt.Sprintf("sleep %d; ", delaySeconds)
	if stdruntime.GOOS == "darwin" {
		if haltMachine {
			command += "(sudo -n shutdown -h now || shutdown -h now || sudo -n halt || halt)"
		} else {
			command += "(pkill -f \"mantler start\" || true)"
		}
	} else {
		if haltMachine {
			command += "(sudo -n systemctl poweroff || sudo -n shutdown -h now || sudo -n halt || sudo -n poweroff || systemctl poweroff || shutdown -h now || halt || poweroff)"
		} else {
			command += "(sudo -n systemctl stop mantlerd || systemctl stop mantlerd || pkill -f \"mantler start\" || true)"
		}
	}
	cmd := exec.Command("sh", "-c", command)
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "mantlerd: self_shutdown scheduled pid=%d delay=%ds haltMachine=%t\n", cmd.Process.Pid, delaySeconds, haltMachine)
	go func() {
		if err := cmd.Wait(); err != nil {
			fmt.Fprintf(os.Stderr, "mantlerd: self_shutdown process pid=%d exited with error: %v\n", cmd.Process.Pid, err)
			return
		}
		fmt.Fprintf(os.Stderr, "mantlerd: self_shutdown process pid=%d completed\n", cmd.Process.Pid)
	}()
	return nil
}

func desiredHarnessesParam(params map[string]interface{}) ([]types.DesiredHarness, error) {
	raw, ok := params["harnesses"]
	if !ok {
		return nil, fmt.Errorf("missing harnesses param")
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal harnesses param: %w", err)
	}
	var harnesses []types.DesiredHarness
	if err := json.Unmarshal(payload, &harnesses); err != nil {
		return nil, fmt.Errorf("decode harnesses param: %w", err)
	}
	return harnesses, nil
}

func probeInstalledHarnesses(desired []types.DesiredHarness) []types.InstalledHarness {
	result := make([]types.InstalledHarness, 0, len(desired))
	for _, harness := range desired {
		if strings.TrimSpace(harness.Type) == "" {
			continue
		}
		item := types.InstalledHarness{
			ID:             harness.ID,
			Name:           harness.Name,
			Type:           harness.Type,
			Status:         "configuring",
			ModelSelection: harness.ModelSelection,
			ManagedModelID: harness.ManagedModelID,
			Capabilities:   harness.Capabilities,
		}

		switch harness.Type {
		case "codex_cli":
			commandName := strings.TrimSpace(harness.Transport.Command)
			if commandName == "" {
				commandName = "codex"
			}
			args := append([]string{}, harness.Transport.Args...)
			if len(args) == 0 {
				args = []string{"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox"}
			}
			item.Transport = &types.HarnessTransportConfig{Kind: "cli", Command: commandName, Args: args}

			path, err := resolveCommandPath(commandName)
			if err != nil {
				item.Status = "offline"
				item.Detail = fmt.Sprintf("%s was not found in PATH", commandName)
				result = append(result, item)
				continue
			}
			item.Transport.Command = path

			item.ExecutablePath = path
			item.Version = probeHarnessVersion(path)
			authConfigured := codexAuthConfigured(len(harness.CredentialRefs) > 0)
			if authConfigured {
				item.Status = "ready"
				if item.Version != "" {
					item.Detail = "Detected " + item.Version
				} else {
					item.Detail = "Detected executable at " + path
				}
			} else {
				item.Status = "offline"
				if item.Version != "" {
					item.Detail = fmt.Sprintf(
						"Detected %s, but authentication is not configured (set an API credential or run ChatGPT login).",
						item.Version,
					)
				} else {
					item.Detail = "Codex CLI is installed, but authentication is not configured (set an API credential or run ChatGPT login)."
				}
			}
			result = append(result, item)
		case "goose":
			baseURL := strings.TrimSpace(harness.Transport.BaseURL)
			if baseURL == "" {
				baseURL = "https://127.0.0.1:3000"
			}
			commandName := strings.TrimSpace(harness.Transport.Command)
			if commandName == "" {
				commandName = "goosed"
			}
			args := append([]string{}, harness.Transport.Args...)
			if len(args) == 0 {
				args = []string{"agent"}
			}
			item.Transport = &types.HarnessTransportConfig{
				Kind:    "daemon",
				BaseURL: baseURL,
				Command: commandName,
				Args:    args,
			}

			if gooseDaemonReachable(baseURL) {
				item.Status = "ready"
				item.Detail = "Goose daemon is reachable at " + baseURL
				result = append(result, item)
				continue
			}

			path, err := resolveCommandPath(commandName)
			if err != nil {
				item.Status = "offline"
				item.Detail = fmt.Sprintf("Goose daemon not reachable at %s and %s was not found in PATH", baseURL, commandName)
				result = append(result, item)
				continue
			}
			item.Transport.Command = path

			item.Status = "ready"
			item.ExecutablePath = path
			item.Version = probeHarnessVersion(path)
			if item.Version != "" {
				item.Detail = fmt.Sprintf("Detected %s. Daemon is not currently reachable, but %s can be started on demand.", item.Version, commandName)
			} else {
				item.Detail = fmt.Sprintf("Detected executable at %s. Daemon will be started on demand if needed.", path)
			}
			result = append(result, item)
		case "custom_openai":
			baseURL := strings.TrimSpace(harness.Transport.BaseURL)
			endpointPath := strings.TrimSpace(harness.Transport.EndpointPath)
			if endpointPath == "" {
				endpointPath = "/v1/chat/completions"
			}
			item.Transport = &types.HarnessTransportConfig{
				Kind:         "openai_http",
				BaseURL:      baseURL,
				EndpointPath: endpointPath,
			}
			if baseURL == "" {
				item.Status = "failed"
				item.Detail = "Missing base URL for OpenAI-compatible endpoint."
			} else {
				item.Status = "ready"
				item.Detail = fmt.Sprintf("Configured endpoint %s%s", strings.TrimRight(baseURL, "/"), endpointPath)
			}
			result = append(result, item)
		default:
			commandName := strings.TrimSpace(harness.Transport.Command)
			if commandName == "" {
				commandName = defaultCLIHarnessCommand(harness.Type)
			}
			item.Transport = &types.HarnessTransportConfig{
				Kind:    "cli",
				Command: commandName,
				Args:    append([]string{}, harness.Transport.Args...),
			}
			if commandName == "" {
				item.Status = "failed"
				item.Detail = fmt.Sprintf("No default command is defined for harness type %s.", harness.Type)
				result = append(result, item)
				continue
			}
			path, err := resolveCommandPath(commandName)
			if err != nil {
				item.Status = "offline"
				item.Detail = fmt.Sprintf("%s was not found in PATH", commandName)
				result = append(result, item)
				continue
			}
			item.Transport.Command = path

			item.ExecutablePath = path
			item.Version = probeHarnessVersion(path)
			if supportsAgentHarnessExecution(harness.Type) {
				item.Status = "ready"
				if item.Version != "" {
					item.Detail = "Detected " + item.Version
				} else {
					item.Detail = "Detected executable at " + path
				}
			} else {
				item.Status = "failed"
				if item.Version != "" {
					item.Detail = fmt.Sprintf(
						"Detected %s, but this agent build cannot execute %s harness jobs yet.",
						item.Version,
						harness.Type,
					)
				} else {
					item.Detail = fmt.Sprintf(
						"Detected executable at %s, but this agent build cannot execute %s harness jobs yet.",
						path,
						harness.Type,
					)
				}
			}
			result = append(result, item)
		}
	}
	return result
}

func defaultCLIHarnessCommand(harnessType string) string {
	switch harnessType {
	case "opencode":
		return "opencode"
	case "claude_code":
		return "claude"
	case "openharness":
		return "openharness"
	case "aider":
		return "aider"
	case "open_interpreter":
		return "interpreter"
	default:
		return ""
	}
}

func supportsAgentHarnessExecution(harnessType string) bool {
	return harnessType == "codex_cli" || harnessType == "goose"
}

func (e *Executor) prepareHarnessesForSync(desired []types.DesiredHarness, commandID string) {
	for _, harness := range desired {
		if harness.Type != "goose" {
			continue
		}
		if err := e.prepareGooseHarness(harness); err != nil {
			if e.progress != nil {
				e.progress(types.AckRequest{
					CommandID: commandID,
					Status:    "in_progress",
					Details:   fmt.Sprintf("Goose setup check: %v", err),
				})
			}
			continue
		}
		if e.progress != nil {
			e.progress(types.AckRequest{
				CommandID: commandID,
				Status:    "in_progress",
				Details:   "Goose setup verified.",
			})
		}
	}
}

func (e *Executor) prepareGooseHarness(harness types.DesiredHarness) error {
	baseURL := strings.TrimSpace(harness.Transport.BaseURL)
	if baseURL == "" {
		baseURL = "https://127.0.0.1:3000"
	}
	if gooseDaemonReachable(baseURL) {
		return nil
	}
	commandName := strings.TrimSpace(harness.Transport.Command)
	if commandName == "" {
		commandName = "goosed"
	}
	if _, err := resolveCommandPath(commandName); err == nil {
		return nil
	}
	if err := installGooseCLI(); err != nil {
		return fmt.Errorf("unable to bootstrap Goose CLI: %w", err)
	}
	if _, err := resolveCommandPath(commandName); err != nil {
		return fmt.Errorf("Goose CLI bootstrap completed but %s is still unavailable", commandName)
	}
	return nil
}

func installGooseCLI() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := fmt.Sprintf("set -euo pipefail; tmp_script=$(mktemp); trap 'rm -f \"$tmp_script\"' EXIT; curl -fsSL %s -o \"$tmp_script\"; chmod +x \"$tmp_script\"; CONFIGURE=false \"$tmp_script\"", shellQuote(gooseInstallScriptURL))
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, trimmed)
	}
	return nil
}

func resolveCommandPath(commandName string) (string, error) {
	commandName = strings.TrimSpace(commandName)
	if commandName == "" {
		return "", fmt.Errorf("empty command")
	}
	if path, err := exec.LookPath(commandName); err == nil {
		return path, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", commandName),
		filepath.Join("/usr/local/bin", commandName),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&0111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%s not found", commandName)
}

func gooseDaemonReachable(baseURL string) bool {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return false
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get(baseURL + "/status")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func probeHarnessVersion(commandName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, commandName, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func codexAuthConfigured(hasCredentialRefs bool) bool {
	if hasCredentialRefs {
		return true
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	authPath := filepath.Join(homeDir, ".codex", "auth.json")
	content, err := os.ReadFile(authPath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(content)) != ""
}
