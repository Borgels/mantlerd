package trainer

import (
	"context"
	"strings"
)

type unslothDriver struct {
	runner *containerRunner
}

func newUnslothDriver() Driver {
	return &unslothDriver{runner: newContainerRunner()}
}

func (d *unslothDriver) Install(ctx context.Context) (string, error) {
	if err := d.runner.ValidateRuntime(ctx, "unsloth"); err != nil {
		return "", err
	}
	return "container:unsloth/unsloth:latest", nil
}

func (d *unslothDriver) Uninstall(ctx context.Context) error {
	return nil
}

func (d *unslothDriver) StartTraining(ctx context.Context, req TrainingRequest, emitProgress func(TrainingProgress)) (TrainingResult, error) {
	req.TrainerType = firstNonEmpty(req.TrainerType, "unsloth")
	if strings.TrimSpace(req.Method) == "" {
		req.Method = "qlora"
	}
	return d.runner.RunTraining(ctx, "unsloth", req, emitProgress)
}

func (d *unslothDriver) StopTraining(ctx context.Context, commandID string, saveCheckpoint bool) error {
	return d.runner.StopTraining(ctx, commandID, saveCheckpoint)
}

func (d *unslothDriver) ExportCheckpoint(ctx context.Context, trainingCommandID string, formats []string) (ExportResult, error) {
	if len(formats) == 0 {
		formats = []string{"safetensors"}
	}
	return d.runner.Export(ctx, "unsloth", trainingCommandID, formats)
}
