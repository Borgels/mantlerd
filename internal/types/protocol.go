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
	RuntimeVLLM   RuntimeType = "vllm"
	RuntimeOllama RuntimeType = "ollama"
)

type ModelInstallStatus string

const (
	ModelInstalling ModelInstallStatus = "installing"
	ModelReady      ModelInstallStatus = "ready"
	ModelFailed     ModelInstallStatus = "failed"
)

type InstalledModel struct {
	ModelID string             `json:"modelId"`
	Status  ModelInstallStatus `json:"status"`
}

type CheckinRequest struct {
	MachineID             string           `json:"machineId"`
	Hostname              string           `json:"hostname,omitempty"`
	Addresses             []string         `json:"addresses,omitempty"`
	HardwareSummary       string           `json:"hardwareSummary,omitempty"`
	AgentVersion          string           `json:"agentVersion,omitempty"`
	RuntimeStatus         RuntimeStatus    `json:"runtimeStatus,omitempty"`
	RuntimeType           RuntimeType      `json:"runtimeType,omitempty"`
	InstalledRuntimeTypes []RuntimeType    `json:"installedRuntimeTypes,omitempty"`
	RuntimeVersion        string           `json:"runtimeVersion,omitempty"`
	InstalledModels       []InstalledModel `json:"installedModels,omitempty"`
	Uptime                int64            `json:"uptime,omitempty"`
	LoadAvg               []float64        `json:"loadAvg,omitempty"`
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
