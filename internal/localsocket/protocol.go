package localsocket

// PullRequest is sent by the CLI to request a model pull.
type PullRequest struct {
	ModelID string            `json:"modelId"`
	Runtime string            `json:"runtime"`
	Flags   map[string]string `json:"flags,omitempty"`
}

// StartRequest is sent by the CLI to start a model.
type StartRequest struct {
	ModelID string            `json:"modelId"`
	Runtime string            `json:"runtime"`
	Flags   map[string]string `json:"flags,omitempty"`
}

// StopRequest is sent by the CLI to stop a model or a runtime.
type StopRequest struct {
	ModelID string `json:"modelId,omitempty"`
	Runtime string `json:"runtime,omitempty"`
}

// RuntimeRequest is sent for install/restart of a runtime.
type RuntimeRequest struct {
	Name string `json:"name"`
}

// ProgressEvent is a single line streamed back during a pull (newline-delimited JSON).
type ProgressEvent struct {
	Status  string  `json:"status,omitempty"`
	Percent float64 `json:"percent,omitempty"`
	Total   int64   `json:"total,omitempty"`
	Done    bool    `json:"done,omitempty"`
	Error   string  `json:"error,omitempty"`
}

// Response is the envelope returned for synchronous operations.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
