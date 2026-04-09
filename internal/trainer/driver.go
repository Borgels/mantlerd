package trainer

import "context"

type TrainingRequest struct {
	CommandID       string
	TrainerID       string
	TrainerType     string
	Method          string
	BaseModel       string
	Dataset         string
	Hyperparameters map[string]interface{}
	ExportFormats   []string
	TargetRuntime   string
}

type TrainingProgress struct {
	CurrentStep     int     `json:"currentStep,omitempty"`
	TotalSteps      int     `json:"totalSteps,omitempty"`
	CurrentEpoch    float64 `json:"currentEpoch,omitempty"`
	TotalEpochs     float64 `json:"totalEpochs,omitempty"`
	Loss            float64 `json:"loss,omitempty"`
	LearningRate    float64 `json:"learningRate,omitempty"`
	GradientNorm    float64 `json:"gradientNorm,omitempty"`
	TokensProcessed int     `json:"tokensProcessed,omitempty"`
	ElapsedSeconds  int     `json:"elapsedSeconds,omitempty"`
	EtaSeconds      int     `json:"etaSeconds,omitempty"`
	GPUUtilization  float64 `json:"gpuUtilization,omitempty"`
	GPUTemperature  float64 `json:"gpuTemperature,omitempty"`
	VRAMUsedMB      int     `json:"vramUsedMb,omitempty"`
	VRAMTotalMB     int     `json:"vramTotalMb,omitempty"`
}

type TrainingResult struct {
	Detail string `json:"detail,omitempty"`
}

type ExportArtifact struct {
	Format     string `json:"format"`
	OutputPath string `json:"outputPath"`
	ModelID    string `json:"modelId"`
	Runtime    string `json:"runtime,omitempty"`
}

type ExportResult struct {
	TrainingCommandID string           `json:"trainingCommandId"`
	Exports           []ExportArtifact `json:"exports"`
}

type Driver interface {
	Install(ctx context.Context) (version string, err error)
	Uninstall(ctx context.Context) error
	StartTraining(ctx context.Context, req TrainingRequest, emitProgress func(TrainingProgress)) (TrainingResult, error)
	StopTraining(ctx context.Context, commandID string, saveCheckpoint bool) error
	ExportCheckpoint(ctx context.Context, trainingCommandID string, formats []string) (ExportResult, error)
}
