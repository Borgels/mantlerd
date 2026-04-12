package trainer

import (
	"context"
	"strings"
)

type pythonPackageDriver struct {
	trainerType string
	displayName string
	runner      *containerRunner
}

func newPythonPackageDriver(trainerType string, packageName string, displayName string) Driver {
	_ = packageName
	return &pythonPackageDriver{
		trainerType: strings.TrimSpace(trainerType),
		displayName: strings.TrimSpace(displayName),
		runner:      newContainerRunner(),
	}
}

func (d *pythonPackageDriver) Install(ctx context.Context) (string, error) {
	if err := d.runner.ValidateRuntime(ctx, d.trainerType); err != nil {
		return "", err
	}
	spec, err := resolveContainerSpec(d.trainerType)
	if err != nil {
		return "", err
	}
	return "container:" + spec.Image, nil
}

func (d *pythonPackageDriver) Uninstall(ctx context.Context) error {
	return nil
}

func (d *pythonPackageDriver) StartTraining(ctx context.Context, req TrainingRequest, emitProgress func(TrainingProgress)) (TrainingResult, error) {
	req.TrainerType = firstNonEmpty(req.TrainerType, d.trainerType)
	if strings.TrimSpace(req.Method) == "" {
		req.Method = "lora"
	}
	return d.runner.RunTraining(ctx, d.trainerType, req, emitProgress)
}

func (d *pythonPackageDriver) StopTraining(ctx context.Context, commandID string, saveCheckpoint bool) error {
	return d.runner.StopTraining(ctx, commandID, saveCheckpoint)
}

func (d *pythonPackageDriver) ExportCheckpoint(ctx context.Context, trainingCommandID string, formats []string) (ExportResult, error) {
	if len(formats) == 0 {
		formats = []string{"safetensors"}
	}
	return d.runner.Export(ctx, d.trainerType, trainingCommandID, formats)
}
