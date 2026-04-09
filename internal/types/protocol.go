package types

type RuntimeStatus string

const (
	RuntimeNotInstalled RuntimeStatus = "not_installed"
	RuntimeInstalling   RuntimeStatus = "installing"
	RuntimeReady        RuntimeStatus = "ready"
	RuntimeFailed       RuntimeStatus = "failed"
)

type RuntimeType string

const (
	RuntimeVLLM     RuntimeType = "vllm"
	RuntimeOllama   RuntimeType = "ollama"
	RuntimeLlamaCpp RuntimeType = "llamacpp"
	RuntimeTensorRT RuntimeType = "tensorrt"
)

type ModelInstallStatus string

const (
	ModelDownloading ModelInstallStatus = "downloading"
	ModelDownloaded  ModelInstallStatus = "downloaded"
	ModelBuilding    ModelInstallStatus = "building" // TensorRT engine compilation
	ModelBuilt       ModelInstallStatus = "built"    // Engine ready, not serving
	ModelInstalling  ModelInstallStatus = "installing"
	ModelStarting    ModelInstallStatus = "starting"
	ModelReady       ModelInstallStatus = "ready"
	ModelStopping    ModelInstallStatus = "stopping"
	ModelFailed      ModelInstallStatus = "failed"
)

type InstalledModel struct {
	ModelID         string             `json:"modelId"`
	Runtime         RuntimeType        `json:"runtime,omitempty"`
	Digest          string             `json:"digest,omitempty"`
	UpdateAvailable *bool              `json:"updateAvailable,omitempty"`
	Status          ModelInstallStatus `json:"status"`
	FailReason      string             `json:"failReason,omitempty"`
	Capabilities    *ModelCapabilities `json:"capabilities,omitempty"`
}

type GPUInfo struct {
	Name              string `json:"name"`
	MemoryTotalMB     int    `json:"memoryTotalMb,omitempty"`
	MemoryUsedMB      int    `json:"memoryUsedMb,omitempty"`
	MemoryFreeMB      int    `json:"memoryFreeMb,omitempty"`
	Architecture      string `json:"architecture,omitempty"`
	ComputeCapability string `json:"computeCapability,omitempty"`
	UnifiedMemory     *bool  `json:"unifiedMemory,omitempty"`
}

type NetworkLink struct {
	Interface    string `json:"interface"`
	Type         string `json:"type"`
	State        string `json:"state"`
	Speed        string `json:"speed,omitempty"`
	LocalAddress string `json:"localAddress,omitempty"`
	PeerAddress  string `json:"peerAddress,omitempty"`
	Subnet       string `json:"subnet,omitempty"`
	MTU          int    `json:"mtu,omitempty"`
}

type RdmaDevice struct {
	Name   string `json:"name"`
	Type   string `json:"type,omitempty"`
	State  string `json:"state"`
	Netdev string `json:"netdev,omitempty"`
}

type InterconnectReport struct {
	Links         []NetworkLink `json:"links"`
	RdmaDevices   []RdmaDevice  `json:"rdmaDevices,omitempty"`
	PeerHostnames []string      `json:"peerHostnames,omitempty"`
	FabricID      string        `json:"fabricId,omitempty"`
}

type ModelCapabilities struct {
	SupportsStreaming  *bool    `json:"supportsStreaming,omitempty"`
	SupportsThinking   *bool    `json:"supportsThinking,omitempty"`
	SupportsTools      *bool    `json:"supportsTools,omitempty"`
	SupportsJSONOutput *bool    `json:"supportsJsonOutput,omitempty"`
	Modalities         []string `json:"modalities,omitempty"`
}

type ModelBenchmarkMetrics struct {
	TTFTMs                      float64 `json:"ttftMs"`
	OutputTokensPerSec          float64 `json:"outputTokensPerSec"`
	TotalLatencyMs              float64 `json:"totalLatencyMs"`
	PromptTokensPerSec          float64 `json:"promptTokensPerSec"`
	P95TTFTMsAtSmallConcurrency float64 `json:"p95TtftMsAtSmallConcurrency"`
}

type EvalSampleDetail struct {
	PromptID     string  `json:"promptId"`
	Category     string  `json:"category"`
	Passed       bool    `json:"passed"`
	QualityScore float64 `json:"qualityScore"`
	LatencyMs    float64 `json:"latencyMs"`
	TTFTMs       float64 `json:"ttftMs,omitempty"`
	TokensPerSec float64 `json:"tokensPerSec,omitempty"`
	OutputTokens int     `json:"outputTokens,omitempty"`
	Output       string  `json:"output,omitempty"`
	Notes        string  `json:"notes,omitempty"`
}

type EvalRunSummary struct {
	Workload         string             `json:"workload"`
	Profile          string             `json:"profile"`
	Samples          []EvalSampleDetail `json:"samples"`
	EvalSessionToken string             `json:"evalSessionToken,omitempty"`
	ResourceUsage    *struct {
		VRAMMB int `json:"vramMb,omitempty"`
		RAMMB  int `json:"ramMb,omitempty"`
	} `json:"resourceUsage,omitempty"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt"`
}

type CheckinRequest struct {
	MachineID              string                         `json:"machineId"`
	Hostname               string                         `json:"hostname,omitempty"`
	Addresses              []string                       `json:"addresses,omitempty"`
	OS                     string                         `json:"os,omitempty"`
	CPUArch                string                         `json:"cpuArch,omitempty"`
	GPUVendor              string                         `json:"gpuVendor,omitempty"`
	HardwareSummary        string                         `json:"hardwareSummary,omitempty"`
	RAMTotalMB             int                            `json:"ramTotalMb,omitempty"`
	GPUs                   []GPUInfo                      `json:"gpus,omitempty"`
	Interconnect           *InterconnectReport            `json:"interconnect,omitempty"`
	AgentVersion           string                         `json:"agentVersion,omitempty"`
	RuntimeStatus          RuntimeStatus                  `json:"runtimeStatus,omitempty"`
	RuntimeStatuses        map[RuntimeType]RuntimeStatus  `json:"runtimeStatuses,omitempty"`
	RuntimeType            RuntimeType                    `json:"runtimeType,omitempty"`
	InstalledRuntimeTypes  []RuntimeType                  `json:"installedRuntimeTypes,omitempty"`
	RuntimeVersion         string                         `json:"runtimeVersion,omitempty"`
	RuntimeVersions        map[RuntimeType]string         `json:"runtimeVersions,omitempty"`
	RuntimeConfigs         map[RuntimeType]map[string]any `json:"runtimeConfigs,omitempty"`
	InstalledModels        []InstalledModel               `json:"installedModels,omitempty"`
	InstalledHarnesses     []InstalledHarness             `json:"installedHarnesses,omitempty"`
	InstalledOrchestrators []InstalledOrchestrator        `json:"installedOrchestrators,omitempty"`
	OutcomeEvents          []OutcomeEvent                 `json:"outcomeEvents,omitempty"`
	Uptime                 int64                          `json:"uptime,omitempty"`
	LoadAvg                []float64                      `json:"loadAvg,omitempty"`
}

type OutcomeEvent struct {
	PlanID            string             `json:"planId,omitempty"`
	TaskID            string             `json:"taskId,omitempty"`
	MantleFingerprint string             `json:"mantleFingerprint,omitempty"`
	BaseFingerprint   string             `json:"baseFingerprint,omitempty"`
	EventType         string             `json:"eventType"`
	EvidenceKind      string             `json:"evidenceKind,omitempty"`
	Workload          string             `json:"workload,omitempty"`
	GradedByServer    bool               `json:"gradedByServer,omitempty"`
	EvalPromptID      string             `json:"evalPromptId,omitempty"`
	EvalOutput        string             `json:"evalOutput,omitempty"`
	EvalSessionToken  string             `json:"evalSessionToken,omitempty"`
	SessionIssuedAtMs int64              `json:"sessionIssuedAtMs,omitempty"`
	RuntimeImage      string             `json:"runtimeImage,omitempty"`
	DurationMs        int64              `json:"durationMs,omitempty"`
	TokenUsage        *OutcomeTokenUsage `json:"tokenUsage,omitempty"`
	QualityScore      *float64           `json:"qualityScore,omitempty"`
	CostUSD           *float64           `json:"costUsd,omitempty"`
	ExitCode          int                `json:"exitCode,omitempty"`
	CrashSignature    string             `json:"crashSignature,omitempty"`
	Detail            string             `json:"detail,omitempty"`
	Timestamp         string             `json:"timestamp"`
}

type OutcomeTokenUsage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
}

type HarnessCapabilities struct {
	SupportsStreaming           *bool `json:"supportsStreaming,omitempty"`
	SupportsTools               *bool `json:"supportsTools,omitempty"`
	SupportsJSONOutput          *bool `json:"supportsJsonOutput,omitempty"`
	SupportsTemperatureOverride *bool `json:"supportsTemperatureOverride,omitempty"`
	SupportsContextOverride     *bool `json:"supportsContextOverride,omitempty"`
	SupportsMetrics             *bool `json:"supportsMetrics,omitempty"`
	SupportsSessions            *bool `json:"supportsSessions,omitempty"`
	SupportsInterrupt           *bool `json:"supportsInterrupt,omitempty"`
}

type HarnessTransportConfig struct {
	Kind         string   `json:"kind"`
	BaseURL      string   `json:"baseUrl,omitempty"`
	EndpointPath string   `json:"endpointPath,omitempty"`
	Command      string   `json:"command,omitempty"`
	Args         []string `json:"args,omitempty"`
}

type InstalledHarness struct {
	ID             string                  `json:"id,omitempty"`
	Name           string                  `json:"name,omitempty"`
	Type           string                  `json:"type"`
	Status         string                  `json:"status"`
	Version        string                  `json:"version,omitempty"`
	ExecutablePath string                  `json:"executablePath,omitempty"`
	Detail         string                  `json:"detail,omitempty"`
	Transport      *HarnessTransportConfig `json:"transport,omitempty"`
	ModelSelection string                  `json:"modelSelection,omitempty"`
	ManagedModelID string                  `json:"managedModelId,omitempty"`
	Capabilities   *HarnessCapabilities    `json:"capabilities,omitempty"`
}

type OrchestratorCapabilities struct {
	SupportsQualityGates     *bool `json:"supportsQualityGates,omitempty"`
	SupportsSkillInjection   *bool `json:"supportsSkillInjection,omitempty"`
	SupportsSubTasks         *bool `json:"supportsSubTasks,omitempty"`
	SupportsConcurrentAgents *bool `json:"supportsConcurrentAgents,omitempty"`
}

type InstalledOrchestrator struct {
	ID           string                    `json:"id,omitempty"`
	Name         string                    `json:"name,omitempty"`
	Type         string                    `json:"type"`
	Status       string                    `json:"status"`
	Version      string                    `json:"version,omitempty"`
	Detail       string                    `json:"detail,omitempty"`
	Capabilities *OrchestratorCapabilities `json:"capabilities,omitempty"`
}

type AgentCommand struct {
	ID     string                 `json:"id"`
	Type   string                 `json:"type"`
	Params map[string]interface{} `json:"params"`
}

type CheckinResponse struct {
	Ack             bool               `json:"ack"`
	ServerTime      string             `json:"serverTime"`
	DesiredConfig   DesiredConfig      `json:"desiredConfig"`
	Recommendations *RecommendResponse `json:"recommendations,omitempty"`
	Commands        []AgentCommand     `json:"commands"`
}

type RecommendContext struct {
	Machine *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"machine,omitempty"`
	Runtime      string `json:"runtime,omitempty"`
	Model        string `json:"model,omitempty"`
	Backend      string `json:"backend,omitempty"`
	Harness      string `json:"harness,omitempty"`
	Orchestrator string `json:"orchestrator,omitempty"`
	Workload     string `json:"workload,omitempty"`
}

type RecommendLayerEntry struct {
	IntegrationKey string  `json:"integrationKey,omitempty"`
	Name           string  `json:"name,omitempty"`
	Description    string  `json:"description,omitempty"`
	Score          float64 `json:"score,omitempty"`
	VerifiedRuns   int     `json:"verifiedRuns,omitempty"`
	Rationale      string  `json:"rationale,omitempty"`
}

type RecommendModelEntry struct {
	ModelID      string  `json:"modelId"`
	Runtime      string  `json:"runtime,omitempty"`
	Name         string  `json:"name,omitempty"`
	Score        float64 `json:"score,omitempty"`
	VerifiedRuns int     `json:"verifiedRuns,omitempty"`
	Rationale    string  `json:"rationale,omitempty"`
}

type RecommendStackEntry struct {
	MantleFingerprint string  `json:"mantleFingerprint"`
	Score             float64 `json:"score,omitempty"`
	VerifiedRuns      int     `json:"verifiedRuns,omitempty"`
	ResolvedLayers    *struct {
		Runtime      string `json:"runtime,omitempty"`
		ModelID      string `json:"modelId,omitempty"`
		Harness      string `json:"harness,omitempty"`
		Orchestrator string `json:"orchestrator,omitempty"`
	} `json:"resolvedLayers,omitempty"`
}

type RecommendResponse struct {
	Context           RecommendContext      `json:"context"`
	Runtimes          []RecommendLayerEntry `json:"runtimes,omitempty"`
	Models            []RecommendModelEntry `json:"models,omitempty"`
	Harnesses         []RecommendLayerEntry `json:"harnesses,omitempty"`
	Orchestrators     []RecommendLayerEntry `json:"orchestrators,omitempty"`
	Stacks            []RecommendStackEntry `json:"stacks,omitempty"`
	CloudAlternatives []struct {
		Provider    string `json:"provider,omitempty"`
		Model       string `json:"model,omitempty"`
		WorkloadFit string `json:"workloadFit,omitempty"`
		Rationale   string `json:"rationale,omitempty"`
		CostTier    string `json:"costTier,omitempty"`
	} `json:"cloudAlternatives,omitempty"`
}

type RecommendQuery struct {
	MachineID     string `json:"machineId,omitempty"`
	HardwareClass string `json:"hardwareClass,omitempty"`
	Runtime       string `json:"runtime,omitempty"`
	ModelID       string `json:"modelId,omitempty"`
	Backend       string `json:"backend,omitempty"`
	Harness       string `json:"harness,omitempty"`
	Orchestrator  string `json:"orchestrator,omitempty"`
	Role          string `json:"role,omitempty"`
	Workload      string `json:"workload,omitempty"`
	Limit         int    `json:"limit,omitempty"`
}

type ModelFeatureFlags struct {
	Streaming bool `json:"streaming"`
	Thinking  bool `json:"thinking"`
}

type ModelTarget struct {
	ModelID      string            `json:"modelId"`
	Runtime      RuntimeType       `json:"runtime,omitempty"`
	FeatureFlags ModelFeatureFlags `json:"featureFlags"`
}

type DesiredConfig struct {
	Runtimes      []RuntimeType         `json:"runtimes"`
	Models        []string              `json:"models"`
	ModelTargets  []ModelTarget         `json:"modelTargets,omitempty"`
	Harnesses     []DesiredHarness      `json:"harnesses,omitempty"`
	Orchestrators []DesiredOrchestrator `json:"orchestrators,omitempty"`
}

type DesiredHarness struct {
	ID             string                 `json:"id,omitempty"`
	Name           string                 `json:"name,omitempty"`
	Type           string                 `json:"type"`
	Status         string                 `json:"status,omitempty"`
	Lifecycle      string                 `json:"lifecycle,omitempty"`
	Transport      HarnessTransportConfig `json:"transport"`
	ModelSelection string                 `json:"modelSelection,omitempty"`
	ManagedModelID string                 `json:"managedModelId,omitempty"`
	Description    string                 `json:"description,omitempty"`
	Capabilities   *HarnessCapabilities   `json:"capabilities,omitempty"`
}

type DesiredOrchestrator struct {
	ID           string                    `json:"id,omitempty"`
	Name         string                    `json:"name,omitempty"`
	Type         string                    `json:"type"`
	Status       string                    `json:"status,omitempty"`
	Version      string                    `json:"version,omitempty"`
	Detail       string                    `json:"detail,omitempty"`
	Command      string                    `json:"command,omitempty"`
	WorkingDir   string                    `json:"workingDir,omitempty"`
	Capabilities *OrchestratorCapabilities `json:"capabilities,omitempty"`
}

type CommandStreamUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type CommandStreamEvent struct {
	ID        string              `json:"id"`
	Timestamp string              `json:"timestamp"`
	Type      string              `json:"type"`
	Content   string              `json:"content,omitempty"`
	Actions   []string            `json:"actions,omitempty"`
	Usage     *CommandStreamUsage `json:"usage,omitempty"`
	Detail    string              `json:"detail,omitempty"`
}

type AckRequest struct {
	CommandID     string               `json:"commandId"`
	Status        string               `json:"status"`
	Details       string               `json:"details,omitempty"`
	StreamEvents  []CommandStreamEvent `json:"streamEvents,omitempty"`
	ResultPayload interface{}          `json:"resultPayload,omitempty"`
}
