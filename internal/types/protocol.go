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
	SupportsStreaming *bool    `json:"supportsStreaming,omitempty"`
	SupportsThinking  *bool    `json:"supportsThinking,omitempty"`
	Modalities        []string `json:"modalities,omitempty"`
}

type ModelBenchmarkMetrics struct {
	TTFTMs                      float64 `json:"ttftMs"`
	OutputTokensPerSec          float64 `json:"outputTokensPerSec"`
	TotalLatencyMs              float64 `json:"totalLatencyMs"`
	PromptTokensPerSec          float64 `json:"promptTokensPerSec"`
	P95TTFTMsAtSmallConcurrency float64 `json:"p95TtftMsAtSmallConcurrency"`
}

type CheckinRequest struct {
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
	Runtimes     []RuntimeType `json:"runtimes"`
	Models       []string      `json:"models"`
	ModelTargets []ModelTarget `json:"modelTargets,omitempty"`
}

type AckRequest struct {
	CommandID string `json:"commandId"`
	Status    string `json:"status"`
	Details   string `json:"details,omitempty"`
}
