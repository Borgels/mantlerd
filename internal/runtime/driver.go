package runtime

import (
	"context"

	"github.com/Borgels/mantlerd/internal/types"
)

type Driver interface {
	Name() string
	Install() error
	Uninstall() error
	IsInstalled() bool
	IsReady() bool
	Version() string
	EnsureModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error
	PrepareModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error
	StartModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error
	StopModel(modelID string) error
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

// CancellableDriver extends Driver with context-aware preparation for cancellation support.
type CancellableDriver interface {
	Driver
	PrepareModelWithFlagsCtx(ctx context.Context, modelID string, flags *types.ModelFeatureFlags) error
}

// BuildOptions specifies parameters for TensorRT engine compilation.
type BuildOptions struct {
	Quantization string // "fp4", "fp8", "int8", "none"
	TPSize       int    // Tensor parallelism
	MaxBatchSize int
	MaxSeqLen    int
}

// BuildableDriver extends Driver with TensorRT-style engine build support.
type BuildableDriver interface {
	Driver
	BuildModel(ctx context.Context, modelID string, opts BuildOptions) error
	IsModelBuilt(modelID string) bool
	BuiltEnginePath(modelID string) (string, bool)
}
