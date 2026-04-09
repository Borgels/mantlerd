package trainer

import (
	"context"
	"fmt"
)

type stubDriver struct {
	trainerType string
}

func newStubDriver(trainerType string) Driver {
	return &stubDriver{trainerType: trainerType}
}

func (d *stubDriver) notSupported() error {
	return fmt.Errorf("trainer %s is not yet supported", d.trainerType)
}

func (d *stubDriver) Install(ctx context.Context) (string, error) {
	return "", d.notSupported()
}

func (d *stubDriver) Uninstall(ctx context.Context) error {
	return d.notSupported()
}

func (d *stubDriver) StartTraining(ctx context.Context, req TrainingRequest, emitProgress func(TrainingProgress)) (TrainingResult, error) {
	return TrainingResult{}, d.notSupported()
}

func (d *stubDriver) StopTraining(ctx context.Context, commandID string, saveCheckpoint bool) error {
	return d.notSupported()
}

func (d *stubDriver) ExportCheckpoint(ctx context.Context, trainingCommandID string, formats []string) (ExportResult, error) {
	return ExportResult{}, d.notSupported()
}
