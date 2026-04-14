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
	RuntimeQuantCPP RuntimeType = "quantcpp"
	RuntimeMLX      RuntimeType = "mlx"
)

type TrainerType string

const (
	TrainerUnsloth      TrainerType = "unsloth"
	TrainerAxolotl      TrainerType = "axolotl"
	TrainerTRL          TrainerType = "trl"
	TrainerLlamaFactory TrainerType = "llamafactory"
)

type TrainerStatus string

const (
	TrainerNotInstalled TrainerStatus = "not_installed"
	TrainerInstalling   TrainerStatus = "installing"
	TrainerReady        TrainerStatus = "ready"
	TrainerTraining     TrainerStatus = "training"
	TrainerFailed       TrainerStatus = "failed"
)

type InstalledTrainer struct {
	ID      string        `json:"id"`
	Type    TrainerType   `json:"type"`
	Name    string        `json:"name"`
	Status  TrainerStatus `json:"status"`
	Version string        `json:"version,omitempty"`
	Detail  string        `json:"detail,omitempty"`
}

type ToolType string

const (
	ToolDCGM                   ToolType = "dcgm"
	ToolNvBandwidth            ToolType = "nvbandwidth"
	ToolRocmBandwidthTest      ToolType = "rocm_bandwidth_test"
	ToolDocker                 ToolType = "docker"
	ToolNvidiaContainerToolkit ToolType = "nvidia_container_toolkit"
)

type ToolStatus string

const (
	ToolNotInstalled ToolStatus = "not_installed"
	ToolInstalling   ToolStatus = "installing"
	ToolReady        ToolStatus = "ready"
	ToolFailed       ToolStatus = "failed"
	ToolUnsupported  ToolStatus = "unsupported"
)

type InstalledTool struct {
	ID              string     `json:"id"`
	Type            ToolType   `json:"type"`
	Name            string     `json:"name,omitempty"`
	Status          ToolStatus `json:"status"`
	Version         string     `json:"version,omitempty"`
	Detail          string     `json:"detail,omitempty"`
	DiagnosticError string     `json:"diagnosticError,omitempty"`
}

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
	ModelID            string             `json:"modelId"`
	Runtime            RuntimeType        `json:"runtime,omitempty"`
	Digest             string             `json:"digest,omitempty"`
	UpdateAvailable    *bool              `json:"updateAvailable,omitempty"`
	Status             ModelInstallStatus `json:"status"`
	StatusTimestamp    string             `json:"statusTimestamp,omitempty"`
	FailReason         string             `json:"failReason,omitempty"`
	Capabilities       *ModelCapabilities `json:"capabilities,omitempty"`
	QuantizationFormat string             `json:"quantizationFormat,omitempty"`
	ArchitectureFamily string             `json:"architectureFamily,omitempty"`
	ModelSizeBytes     int64              `json:"modelSizeBytes,omitempty"`
	ParameterCount     string             `json:"parameterCount,omitempty"`
	IsMoe              bool               `json:"isMoe,omitempty"`
	NumExperts         int                `json:"numExperts,omitempty"`
	ActiveExperts      int                `json:"activeExperts,omitempty"`
	ContextLength      int                `json:"contextLength,omitempty"`
}

type DeployedMantle struct {
	MantleFingerprint string  `json:"mantleFingerprint"`
	BaseFingerprint   string  `json:"baseFingerprint,omitempty"`
	Slug              string  `json:"slug,omitempty"`
	EndpointPath      string  `json:"endpointPath,omitempty"`
	EndpointHealth    string  `json:"endpointHealth,omitempty"`
	Status            string  `json:"status"`
	CPUPercent        float64 `json:"cpuPercent,omitempty"`
	MemoryMB          int     `json:"memoryMb,omitempty"`
	PipelineStage     string  `json:"pipelineStage,omitempty"`
}

type AgentHealth string

const (
	AgentHealthy  AgentHealth = "healthy"
	AgentDegraded AgentHealth = "degraded"
	AgentBusy     AgentHealth = "busy"
)

type GPUInfo struct {
	Name                string  `json:"name"`
	Index               int     `json:"index,omitempty"`
	UUID                string  `json:"uuid,omitempty"`
	PCIBusID            string  `json:"pciBusId,omitempty"`
	MemoryTotalMB       int     `json:"memoryTotalMb,omitempty"`
	MemoryUsedMB        int     `json:"memoryUsedMb,omitempty"`
	MemoryFreeMB        int     `json:"memoryFreeMb,omitempty"`
	Architecture        string  `json:"architecture,omitempty"`
	ComputeCapability   string  `json:"computeCapability,omitempty"`
	UnifiedMemory       *bool   `json:"unifiedMemory,omitempty"`
	MemoryBandwidthGBps float64 `json:"memoryBandwidthGBps,omitempty"`
	BandwidthSource     string  `json:"bandwidthSource,omitempty"`
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

type GPUInterconnectEdge struct {
	FromIndex int    `json:"fromIndex"`
	ToIndex   int    `json:"toIndex"`
	LinkType  string `json:"linkType"`
	Path      string `json:"path,omitempty"`
	LinkCount int    `json:"linkCount,omitempty"`
}

type GPUBandwidthMatrixEntry struct {
	FromIndex     int     `json:"fromIndex"`
	ToIndex       int     `json:"toIndex"`
	BandwidthGBps float64 `json:"bandwidthGbps"`
	Direction     string  `json:"direction,omitempty"`
}

type GPUInterconnectReport struct {
	Edges             []GPUInterconnectEdge     `json:"edges,omitempty"`
	BandwidthMatrix   []GPUBandwidthMatrixEntry `json:"bandwidthMatrix,omitempty"`
	MeasurementSource string                    `json:"measurementSource,omitempty"`
	MeasuredAt        string                    `json:"measuredAt,omitempty"`
	Detail            string                    `json:"detail,omitempty"`
}

type AcceleratorStackReport struct {
	NvidiaDriverVersion string `json:"nvidiaDriverVersion,omitempty"`
	CudaToolkitVersion  string `json:"cudaToolkitVersion,omitempty"`
	CudaRuntimeVersion  string `json:"cudaRuntimeVersion,omitempty"`
	CudnnVersion        string `json:"cudnnVersion,omitempty"`
	NcclVersion         string `json:"ncclVersion,omitempty"`
	RocmVersion         string `json:"rocmVersion,omitempty"`
	HipRuntimeVersion   string `json:"hipRuntimeVersion,omitempty"`
	MiopenVersion       string `json:"miopenVersion,omitempty"`
	RcclVersion         string `json:"rcclVersion,omitempty"`
	ContainerRuntime    string `json:"containerRuntime,omitempty"`
	NvidiaToolkit       string `json:"nvidiaToolkit,omitempty"`
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

type MachineOrigin struct {
	Kind              string              `json:"kind"`
	ComputeProviderID string              `json:"computeProviderId,omitempty"`
	InstanceID        string              `json:"instanceId,omitempty"`
	HourlyRate        float64             `json:"hourlyRate,omitempty"`
	Region            string              `json:"region,omitempty"`
	GPUType           string              `json:"gpuType,omitempty"`
	GPUCount          int                 `json:"gpuCount,omitempty"`
	ProvisionedAt     string              `json:"provisionedAt,omitempty"`
	AutoShutdown      *AutoShutdownConfig `json:"autoShutdown,omitempty"`
}

type MachineConnectivity struct {
	Kind           string `json:"kind"`
	Address        string `json:"address,omitempty"`
	Port           int    `json:"port,omitempty"`
	TLSEnabled     bool   `json:"tlsEnabled"`
	RelayConnected bool   `json:"relayConnected"`
	RelayLatencyMs int64  `json:"relayLatencyMs,omitempty"`
	TailscaleIP    string `json:"tailscaleIp,omitempty"`
	FunnelEnabled  bool   `json:"funnelEnabled"`
	FunnelHostname string `json:"funnelHostname,omitempty"`
	TunnelHostname string `json:"tunnelHostname,omitempty"`
}

type AutoShutdownConfig struct {
	IdleMinutes int     `json:"idleMinutes,omitempty"`
	MaxHours    float64 `json:"maxHours,omitempty"`
}

type CheckinRequest struct {
	MachineID              string                         `json:"machineId"`
	Hostname               string                         `json:"hostname,omitempty"`
	Addresses              []string                       `json:"addresses,omitempty"`
	Connectivity           *MachineConnectivity           `json:"connectivity,omitempty"`
	OS                     string                         `json:"os,omitempty"`
	CPUArch                string                         `json:"cpuArch,omitempty"`
	GPUVendor              string                         `json:"gpuVendor,omitempty"`
	HardwareSummary        string                         `json:"hardwareSummary,omitempty"`
	RAMTotalMB             int                            `json:"ramTotalMb,omitempty"`
	Origin                 *MachineOrigin                 `json:"origin,omitempty"`
	GPUs                   []GPUInfo                      `json:"gpus,omitempty"`
	Interconnect           *InterconnectReport            `json:"interconnect,omitempty"`
	GPUInterconnect        *GPUInterconnectReport         `json:"gpuInterconnect,omitempty"`
	AcceleratorStack       *AcceleratorStackReport        `json:"acceleratorStack,omitempty"`
	AgentVersion           string                         `json:"agentVersion,omitempty"`
	AgentHealth            AgentHealth                    `json:"agentHealth,omitempty"`
	RuntimeStatus          RuntimeStatus                  `json:"runtimeStatus,omitempty"`
	RuntimeStatuses        map[RuntimeType]RuntimeStatus  `json:"runtimeStatuses,omitempty"`
	RuntimeType            RuntimeType                    `json:"runtimeType,omitempty"`
	InstalledRuntimeTypes  []RuntimeType                  `json:"installedRuntimeTypes,omitempty"`
	RuntimeVersion         string                         `json:"runtimeVersion,omitempty"`
	RuntimeVersions        map[RuntimeType]string         `json:"runtimeVersions,omitempty"`
	RuntimeConfigs         map[RuntimeType]map[string]any `json:"runtimeConfigs,omitempty"`
	StageEncryptionKey     string                         `json:"stageEncryptionKey,omitempty"`
	StageSigningKey        string                         `json:"stageSigningKey,omitempty"`
	StageKeyFingerprint    string                         `json:"stageKeyFingerprint,omitempty"`
	InstalledTrainers      []InstalledTrainer             `json:"installedTrainers,omitempty"`
	InstalledTools         []InstalledTool                `json:"installedTools,omitempty"`
	InstalledModels        []InstalledModel               `json:"installedModels,omitempty"`
	DeployedMantles        []DeployedMantle               `json:"deployedMantles,omitempty"`
	InstalledHarnesses     []InstalledHarness             `json:"installedHarnesses,omitempty"`
	InstalledOrchestrators []InstalledOrchestrator        `json:"installedOrchestrators,omitempty"`
	OutcomeEvents          []OutcomeEvent                 `json:"outcomeEvents,omitempty"`
	Uptime                 int64                          `json:"uptime,omitempty"`
	LoadAvg                []float64                      `json:"loadAvg,omitempty"`
}

type OutcomeEvent struct {
	PlanID                string                 `json:"planId,omitempty"`
	TaskID                string                 `json:"taskId,omitempty"`
	MantleFingerprint     string                 `json:"mantleFingerprint,omitempty"`
	BaseFingerprint       string                 `json:"baseFingerprint,omitempty"`
	EventType             string                 `json:"eventType"`
	EvidenceKind          string                 `json:"evidenceKind,omitempty"`
	Workload              string                 `json:"workload,omitempty"`
	GradedByServer        bool                   `json:"gradedByServer,omitempty"`
	EvalPromptID          string                 `json:"evalPromptId,omitempty"`
	EvalOutput            string                 `json:"evalOutput,omitempty"`
	EvalSessionToken      string                 `json:"evalSessionToken,omitempty"`
	SessionIssuedAtMs     int64                  `json:"sessionIssuedAtMs,omitempty"`
	BenchmarkSuiteID      string                 `json:"benchmarkSuiteId,omitempty"`
	BenchmarkSuiteVersion string                 `json:"benchmarkSuiteVersion,omitempty"`
	RuntimeImage          string                 `json:"runtimeImage,omitempty"`
	DurationMs            int64                  `json:"durationMs,omitempty"`
	TokenUsage            *OutcomeTokenUsage     `json:"tokenUsage,omitempty"`
	BenchmarkMetrics      *ModelBenchmarkMetrics `json:"benchmarkMetrics,omitempty"`
	QualityScore          *float64               `json:"qualityScore,omitempty"`
	CostUSD               *float64               `json:"costUsd,omitempty"`
	ExitCode              int                    `json:"exitCode,omitempty"`
	CrashSignature        string                 `json:"crashSignature,omitempty"`
	Detail                string                 `json:"detail,omitempty"`
	PipelineStage         string                 `json:"pipelineStage,omitempty"`
	StageIntegrity        *StageIntegrity        `json:"stageIntegrity,omitempty"`
	Timestamp             string                 `json:"timestamp"`
}

type StageIntegrity struct {
	StageID               string `json:"stageId"`
	StageKind             string `json:"stageKind"`
	ContractVersion       string `json:"contractVersion,omitempty"`
	ModelID               string `json:"modelId"`
	RuntimeID             string `json:"runtimeId"`
	InputHash             string `json:"inputHash"`
	OutputHash            string `json:"outputHash"`
	InputTokens           int    `json:"inputTokens"`
	OutputTokens          int    `json:"outputTokens"`
	DurationMs            int64  `json:"durationMs"`
	Timestamp             string `json:"timestamp"`
	MachineKeyFingerprint string `json:"machineKeyFingerprint"`
	Signature             string `json:"signature"`
}

type CompressedArtifact struct {
	Kind        string `json:"kind"`
	Reference   string `json:"reference"`
	Description string `json:"description"`
}

type CompressedToolPendingCall struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type CompressedToolState struct {
	ActiveTools  []string                  `json:"activeTools"`
	PendingCalls []CompressedToolPendingCall `json:"pendingCalls"`
}

type CompressedContext struct {
	ContractVersion   string               `json:"contractVersion"`
	Summary           string               `json:"summary"`
	PreservedFacts    []string             `json:"preservedFacts"`
	Decisions         []string             `json:"decisions"`
	ReferencedArtifacts []CompressedArtifact `json:"referencedArtifacts"`
	UnresolvedQuestions []string           `json:"unresolvedQuestions"`
	LatestUserIntent  string               `json:"latestUserIntent"`
	ToolState         *CompressedToolState `json:"toolState,omitempty"`
}

type StageEnvelope struct {
	Version             string          `json:"version"`
	RequestID           string          `json:"requestId"`
	StageID             string          `json:"stageId"`
	StageKind           string          `json:"stageKind"`
	TargetMachineID     string          `json:"targetMachineId"`
	RouteKind           string          `json:"routeKind"`
	EncryptedPayload    string          `json:"encryptedPayload"`
	Nonce               string          `json:"nonce"`
	EphemeralPublicKey  string          `json:"ephemeralPublicKey"`
	PriorStageSigningKey string         `json:"priorStageSigningKey,omitempty"`
	PriorIntegrity      *StageIntegrity `json:"priorIntegrity,omitempty"`
	Continuation        *StageContinuation `json:"continuation,omitempty"`
	Billing             *StageBilling   `json:"billing,omitempty"`
}

type StageContinuation struct {
	NextStageKind          string `json:"nextStageKind"`
	NextTargetMachineID    string `json:"nextTargetMachineId"`
	NextRouteKind          string `json:"nextRouteKind"`
	NextTargetEncryptionKey string `json:"nextTargetEncryptionKey"`
	NextTargetSigningKey    string `json:"nextTargetSigningKey"`
	NextTargetKeyFingerprint string `json:"nextTargetKeyFingerprint"`
}

type StageBilling struct {
	OrgID             string `json:"orgId"`
	APIKeyID          string `json:"apiKeyId"`
	MantleFingerprint string `json:"mantleFingerprint"`
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
	ID              string                  `json:"id,omitempty"`
	Name            string                  `json:"name,omitempty"`
	Type            string                  `json:"type"`
	Status          string                  `json:"status"`
	StatusTimestamp string                  `json:"statusTimestamp,omitempty"`
	Version         string                  `json:"version,omitempty"`
	ExecutablePath  string                  `json:"executablePath,omitempty"`
	Detail          string                  `json:"detail,omitempty"`
	Transport       *HarnessTransportConfig `json:"transport,omitempty"`
	ModelSelection  string                  `json:"modelSelection,omitempty"`
	ManagedModelID  string                  `json:"managedModelId,omitempty"`
	Capabilities    *HarnessCapabilities    `json:"capabilities,omitempty"`
}

type OrchestratorCapabilities struct {
	SupportsQualityGates     *bool `json:"supportsQualityGates,omitempty"`
	SupportsSkillInjection   *bool `json:"supportsSkillInjection,omitempty"`
	SupportsSubTasks         *bool `json:"supportsSubTasks,omitempty"`
	SupportsConcurrentAgents *bool `json:"supportsConcurrentAgents,omitempty"`
}

type InstalledOrchestrator struct {
	ID              string                    `json:"id,omitempty"`
	Name            string                    `json:"name,omitempty"`
	Type            string                    `json:"type"`
	Status          string                    `json:"status"`
	StatusTimestamp string                    `json:"statusTimestamp,omitempty"`
	Version         string                    `json:"version,omitempty"`
	Detail          string                    `json:"detail,omitempty"`
	Capabilities    *OrchestratorCapabilities `json:"capabilities,omitempty"`
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
	ComputeAlternatives []struct {
		ProviderID          string  `json:"providerId,omitempty"`
		Provider            string  `json:"provider,omitempty"`
		ComputeType         string  `json:"computeType,omitempty"`
		Region              string  `json:"region,omitempty"`
		WorkloadFit         string  `json:"workloadFit,omitempty"`
		Rationale           string  `json:"rationale,omitempty"`
		EstimatedHourlyRate float64 `json:"estimatedHourlyRate,omitempty"`
	} `json:"computeAlternatives,omitempty"`
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

type ExploreQuery struct {
	Runtime             string                    `json:"runtime,omitempty"`
	ModelID             string                    `json:"modelId,omitempty"`
	Workload            string                    `json:"workload,omitempty"`
	Priority            string                    `json:"priority,omitempty"`
	ModelType           string                    `json:"modelType,omitempty"`
	Harness             string                    `json:"harness,omitempty"`
	Orchestrator        string                    `json:"orchestrator,omitempty"`
	MaxAttempts         int                       `json:"maxAttempts,omitempty"`
	Capabilities        *ExploreCapabilities      `json:"capabilities,omitempty"`
	ModelMetadata       map[string]map[string]any `json:"modelMetadata,omitempty"`
	ExcludeFingerprints []string                  `json:"excludeFingerprints,omitempty"`
}

type ExploreCapabilities struct {
	Docker          bool `json:"docker"`
	HFToken         bool `json:"hfToken"`
	GPUDriverLoaded bool `json:"gpuDriverLoaded"`
}

type RuntimePlanFile struct {
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
	Content     string `json:"content,omitempty"`
}

type ExploreSelection struct {
	MachineID string `json:"machineId"`
	ModelID   string `json:"modelId"`
	Runtime   string `json:"runtime"`
}

type ExplorePlan struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	Confidence        string `json:"confidence"`
	BaseFingerprint   string `json:"baseFingerprint"`
	MantleFingerprint string `json:"mantleFingerprint"`
	Compatibility     struct {
		Allowed  bool     `json:"allowed"`
		Blockers []string `json:"blockers"`
		Warnings []string `json:"warnings"`
	} `json:"compatibility"`
	ResolvedLayers struct {
		MachineID    string `json:"machineId"`
		ModelID      string `json:"modelId"`
		Runtime      string `json:"runtime"`
		Backend      string `json:"backend,omitempty"`
		Harness      string `json:"harness,omitempty"`
		Orchestrator string `json:"orchestrator,omitempty"`
		Provider     string `json:"provider,omitempty"`
	} `json:"resolvedLayers"`
	RuntimePlan struct {
		Image        string            `json:"image,omitempty"`
		Env          map[string]string `json:"env,omitempty"`
		Args         []string          `json:"args,omitempty"`
		Files        []RuntimePlanFile `json:"files,omitempty"`
		InstallMode  string            `json:"installMode,omitempty"`
		HealthChecks []string          `json:"healthChecks,omitempty"`
	} `json:"runtimePlan"`
	CreatedAt  string `json:"createdAt"`
	AppliedAt  string `json:"appliedAt,omitempty"`
	VerifiedAt string `json:"verifiedAt,omitempty"`
	FailedAt   string `json:"failedAt,omitempty"`
	ScoredAt   string `json:"scoredAt,omitempty"`
}

type ExploreResponse struct {
	Selection ExploreSelection `json:"selection"`
	Plan      ExplorePlan      `json:"plan"`
	Attempts  int              `json:"attempts"`
}

type CompatCatalog struct {
	Models          []map[string]any `json:"models"`
	RuntimeRules    []map[string]any `json:"runtimeRules"`
	GPUCapabilities []map[string]any `json:"gpuCapabilities"`
	CuratedRecipes  []map[string]any `json:"curatedRecipes"`
	Integrations    []map[string]any `json:"integrations"`
}

type ScoreRawSignals struct {
	Compatibility *float64 `json:"compatibility"`
	Reliability   *float64 `json:"reliability"`
	Performance   *float64 `json:"performance"`
	Throughput    *float64 `json:"throughput"`
	TaskQuality   *float64 `json:"taskQuality"`
	Efficiency    *float64 `json:"efficiency"`
	Cost          *float64 `json:"cost"`
}

type EvidenceBreakdown struct {
	Verified     int `json:"verified"`
	Observed     int `json:"observed"`
	SelfReported int `json:"selfReported"`
}

type ScoreResponse struct {
	MantleFingerprint string            `json:"mantleFingerprint"`
	Overall           float64           `json:"overall"`
	ProfileID         string            `json:"profileId"`
	FormulaVersion    int               `json:"formulaVersion"`
	ConfidenceTier    string            `json:"confidenceTier"`
	EvidenceSignals   int               `json:"evidenceSignals"`
	EvidenceCount     int               `json:"evidenceCount"`
	EvidenceBreakdown EvidenceBreakdown `json:"evidenceBreakdown"`
	RawSignals        ScoreRawSignals   `json:"rawSignals"`
	UpdatedAt         string            `json:"updatedAt"`
}

type EvalChallengeSessionStartResponse struct {
	SessionID             string                `json:"sessionId"`
	BenchmarkSuiteID      string                `json:"benchmarkSuiteId"`
	BenchmarkSuiteVersion string                `json:"benchmarkSuiteVersion"`
	NextPromptID          string                `json:"nextPromptId,omitempty"`
	Prompts               []EvalChallengePrompt `json:"prompts,omitempty"`
}

type EvalChallengePrompt struct {
	ID            string   `json:"id"`
	Category      string   `json:"category"`
	Workload      string   `json:"workload"`
	Prompt        string   `json:"prompt"`
	SystemPrompt  string   `json:"systemPrompt,omitempty"`
	MaxTokens     int      `json:"maxTokens,omitempty"`
	ContextLength string   `json:"contextLength,omitempty"`
	SuiteID       string   `json:"suiteId,omitempty"`
	SuiteVersion  string   `json:"suiteVersion,omitempty"`
	Choices       []string `json:"choices,omitempty"`
	Subject       string   `json:"subject,omitempty"`
}

type EvalChallengeResponseResult struct {
	SessionID    string `json:"sessionId"`
	Done         bool   `json:"done"`
	NextPromptID string `json:"nextPromptId,omitempty"`
	ScoreCount   int    `json:"scoreCount,omitempty"`
}

type EvalChallengeStatus struct {
	SessionID    string `json:"sessionId"`
	Done         bool   `json:"done"`
	Cursor       int    `json:"cursor"`
	TotalPrompts int    `json:"totalPrompts"`
	ScoreCount   int    `json:"scoreCount,omitempty"`
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

type DesiredToolTarget struct {
	Tool           ToolType `json:"tool"`
	TargetVersion  string   `json:"targetVersion,omitempty"`
	PackageVariant string   `json:"packageVariant,omitempty"`
	InstallHint    string   `json:"installHint,omitempty"`
}

type DesiredConfig struct {
	Runtimes      []RuntimeType         `json:"runtimes"`
	Tools         []ToolType            `json:"tools,omitempty"`
	ToolTargets   []DesiredToolTarget   `json:"toolTargets,omitempty"`
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
	CredentialRefs []string               `json:"credentialRefs,omitempty"`
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
