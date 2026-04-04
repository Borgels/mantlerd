package runtime

import "github.com/Borgels/clawcontrol-agent/internal/types"

type Driver interface {
	Name() string
	Install() error
	Uninstall() error
	IsInstalled() bool
	IsReady() bool
	Version() string
	EnsureModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error
	ListModels() []string
	HasModel(modelID string) bool
	RemoveModel(modelID string) error
	BenchmarkModel(
		modelID string,
		samplePromptTokens int,
		sampleOutputTokens int,
		concurrency int,
		runs int,
		onProgress func(BenchmarkProgress),
	) (BenchmarkResult, error)
	RestartRuntime() error
}
