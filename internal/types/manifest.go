package types

type ManifestModelCapabilities struct {
	Tools      bool `json:"tools"`
	Thinking   bool `json:"thinking"`
	Streaming  bool `json:"streaming"`
	JSONOutput bool `json:"jsonOutput"`
}

type ManifestModel struct {
	ID             string                    `json:"id"`
	ModelID        string                    `json:"modelId"`
	Endpoint       string                    `json:"endpoint"`
	Runtime        string                    `json:"runtime,omitempty"`
	MachineID      string                    `json:"machineId"`
	MachineName    string                    `json:"machineName"`
	Source         string                    `json:"source"`
	ProviderID     string                    `json:"providerId,omitempty"`
	APIKey         string                    `json:"apiKey,omitempty"`
	Capabilities   ManifestModelCapabilities `json:"capabilities"`
	ContextWindow  int                       `json:"contextWindow,omitempty"`
	ParameterCount string                    `json:"parameterCount,omitempty"`
}

type ManifestHarness struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Type                  string `json:"type"`
	MachineID             string `json:"machineId"`
	MachineName           string `json:"machineName"`
	Status                string `json:"status"`
	SupportsLocalEndpoint bool   `json:"supportsLocalEndpoint"`
}

type ManifestSkill struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	PromptFragment  string   `json:"promptFragment,omitempty"`
	Tools           []string `json:"tools,omitempty"`
	ApplicableRoles []string `json:"applicableRoles,omitempty"`
	Source          string   `json:"source"`
}

type ResolvedRole struct {
	RoleID           string          `json:"roleId"`
	RoleName         string          `json:"roleName"`
	AssignedModelID  string          `json:"assignedModelId,omitempty"`
	AssignedEndpoint string          `json:"assignedEndpoint,omitempty"`
	Skills           []ManifestSkill `json:"skills,omitempty"`
}

type ResourceManifest struct {
	GeneratedAt string            `json:"generatedAt"`
	BlueprintID string            `json:"blueprintId,omitempty"`
	Models      []ManifestModel   `json:"models"`
	Harnesses   []ManifestHarness `json:"harnesses"`
	Skills      []ManifestSkill   `json:"skills"`
	Roles       []ResolvedRole    `json:"roles"`
}
