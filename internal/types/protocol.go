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
	RuntimeLMStudio RuntimeType = "lmstudio"
	RuntimeTensorRT RuntimeType = "tensorrt"
)

type ModelInstallStatus string

const (
	ModelInstalling ModelInstallStatus = "installing"
	ModelReady      ModelInstallStatus = "ready"
	ModelFailed     ModelInstallStatus = "failed"
)

type InstalledModel struct {
	ModelID      string             `json:"modelId"`
	Runtime      RuntimeType        `json:"runtime,omitempty"`
	Status       ModelInstallStatus `json:"status"`
	Capabilities *ModelCapabilities `json:"capabilities,omitempty"`
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

type CheckinRequest struct {
<<<<<<< HEAD
	MachineID             string                        `json:"machineId"`
	Hostname              string                        `json:"hostname,omitempty"`
	Addresses             []string                      `json:"addresses,omitempty"`
	HardwareSummary       string                        `json:"hardwareSummary,omitempty"`
	AgentVersion          string                        `json:"agentVersion,omitempty"`
	RuntimeStatus         RuntimeStatus                 `json:"runtimeStatus,omitempty"`
	RuntimeStatuses       map[RuntimeType]RuntimeStatus `json:"runtimeStatuses,omitempty"`
	RuntimeType           RuntimeType                   `json:"runtimeType,omitempty"`
	InstalledRuntimeTypes []RuntimeType                 `json:"installedRuntimeTypes,omitempty"`
	RuntimeVersion        string                        `json:"runtimeVersion,omitempty"`
	RuntimeVersions       map[RuntimeType]string        `json:"runtimeVersions,omitempty"`
	InstalledModels       []InstalledModel              `json:"installedModels,omitempty"`
	Uptime                int64                         `json:"uptime,omitempty"`
	LoadAvg               []float64                     `json:"loadAvg,omitempty"`
=======
	MachineID             string                 `json:"machineId"`
	Hostname              string                 `json:"hostname,omitempty"`
	Addresses             []string               `json:"addresses,omitempty"`
	HardwareSummary       string                 `json:"hardwareSummary,omitempty"`
	AgentVersion          string                 `json:"agentVersion,omitempty"`
	RuntimeStatus         RuntimeStatus          `json:"runtimeStatus,omitempty"`
	RuntimeType           RuntimeType            `json:"runtimeType,omitempty"`
	InstalledRuntimeTypes []RuntimeType          `json:"installedRuntimeTypes,omitempty"`
	RuntimeVersion        string                 `json:"runtimeVersion,omitempty"`
	RuntimeVersions       map[RuntimeType]string `json:"runtimeVersions,omitempty"`
	InstalledModels       []InstalledModel       `json:"installedModels,omitempty"`
	InstalledHarnesses    []InstalledHarness     `json:"installedHarnesses,omitempty"`
	Uptime                int64                  `json:"uptime,omitempty"`
	LoadAvg               []float64              `json:"loadAvg,omitempty"`
>>>>>>> 1e0794c (feat: add harness sync lifecycle and bump to v0.2.8)
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

type AgentCommand struct {
	ID     string                 `json:"id"`
	Type   string                 `json:"type"`
	Params map[string]interface{} `json:"params"`
}

type CheckinResponse struct {
	Ack           bool           `json:"ack"`
	ServerTime    string         `json:"serverTime"`
	DesiredConfig DesiredConfig  `json:"desiredConfig"`
	Commands      []AgentCommand `json:"commands"`
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
	Runtimes     []RuntimeType    `json:"runtimes"`
	Models       []string         `json:"models"`
	ModelTargets []ModelTarget    `json:"modelTargets,omitempty"`
	Harnesses    []DesiredHarness `json:"harnesses,omitempty"`
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
