package trainer

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type pythonPackageDriver struct {
	trainerType string
	packageName string
	displayName string
}

func newPythonPackageDriver(trainerType string, packageName string, displayName string) Driver {
	return &pythonPackageDriver{
		trainerType: strings.TrimSpace(trainerType),
		packageName: strings.TrimSpace(packageName),
		displayName: strings.TrimSpace(displayName),
	}
}

func (d *pythonPackageDriver) venvDirExpr() string {
	return fmt.Sprintf("${MANTLER_TRAINER_VENV_DIR:-$HOME/.local/share/mantler/trainers/%s-venv}", d.trainerType)
}

func (d *pythonPackageDriver) Install(ctx context.Context) (string, error) {
	python, err := exec.LookPath("python3")
	if err != nil {
		return "", fmt.Errorf("python3 not found in PATH")
	}
	pipInstallCmd := fmt.Sprintf(
		`set -euo pipefail; VENV_DIR=%s; %s -m venv "$VENV_DIR"; "$VENV_DIR/bin/python" -m pip install --upgrade pip setuptools wheel; "$VENV_DIR/bin/python" -m pip install --upgrade %s`,
		d.venvDirExpr(),
		shellQuote(python),
		shellQuote(d.packageName),
	)
	cmd := exec.CommandContext(ctx, "bash", "-lc", pipInstallCmd)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return "", fmt.Errorf("install %s failed: %w (%s)", d.trainerType, runErr, strings.TrimSpace(string(output)))
	}

	versionCmd := exec.CommandContext(
		ctx,
		"bash",
		"-lc",
		fmt.Sprintf(
			`VENV_DIR=%s; "$VENV_DIR/bin/python" -c "import importlib.metadata as m, sys; sys.stdout.write(m.version(%q))"`,
			d.venvDirExpr(),
			d.packageName,
		),
	)
	versionOut, versionErr := versionCmd.CombinedOutput()
	if versionErr != nil {
		return "", fmt.Errorf(
			"install %s completed but version probe failed: %w (%s)",
			d.trainerType,
			versionErr,
			strings.TrimSpace(string(versionOut)),
		)
	}
	version := strings.TrimSpace(string(versionOut))
	if version == "" {
		return "", fmt.Errorf("install %s completed but version probe returned empty output", d.trainerType)
	}
	return version, nil
}

func (d *pythonPackageDriver) Uninstall(ctx context.Context) error {
	cmd := exec.CommandContext(
		ctx,
		"bash",
		"-lc",
		fmt.Sprintf(`set -euo pipefail; VENV_DIR=%s; rm -rf "$VENV_DIR"`, d.venvDirExpr()),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("uninstall %s failed: %w (%s)", d.trainerType, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d *pythonPackageDriver) StartTraining(ctx context.Context, req TrainingRequest, emitProgress func(TrainingProgress)) (TrainingResult, error) {
	if !mockTrainingEnabled() {
		return TrainingResult{}, fmt.Errorf(
			"real %s training is not yet implemented; set MANTLER_TRAINER_MOCK=true for mock mode",
			d.trainerType,
		)
	}
	const totalSteps = 10
	startedAt := time.Now()
	for step := 1; step <= totalSteps; step++ {
		select {
		case <-ctx.Done():
			return TrainingResult{}, ctx.Err()
		case <-time.After(1 * time.Second):
		}
		elapsed := int(time.Since(startedAt).Seconds())
		if emitProgress != nil {
			emitProgress(TrainingProgress{
				CurrentStep:     step,
				TotalSteps:      totalSteps,
				CurrentEpoch:    float64(step) / 2.0,
				TotalEpochs:     5,
				Loss:            1.2 - (float64(step) * 0.09),
				LearningRate:    0.0002,
				GradientNorm:    0.8 + (float64(step) * 0.03),
				TokensProcessed: step * 4096,
				ElapsedSeconds:  elapsed,
				EtaSeconds:      max(0, totalSteps-step),
				GPUUtilization:  float64(65 + (step % 20)),
				GPUTemperature:  62 + float64(step/2),
				VRAMUsedMB:      18000 + step*120,
				VRAMTotalMB:     24576,
			})
		}
	}
	return TrainingResult{
		Detail: fmt.Sprintf("%s training complete for %s on %s", d.displayName, req.BaseModel, req.Dataset),
	}, nil
}

func (d *pythonPackageDriver) StopTraining(ctx context.Context, commandID string, saveCheckpoint bool) error {
	return nil
}

func (d *pythonPackageDriver) ExportCheckpoint(ctx context.Context, trainingCommandID string, formats []string) (ExportResult, error) {
	if !mockTrainingEnabled() {
		return ExportResult{}, fmt.Errorf(
			"real %s export is not yet implemented; set MANTLER_TRAINER_MOCK=true for mock mode",
			d.trainerType,
		)
	}
	if len(formats) == 0 {
		formats = []string{"gguf"}
	}
	exports := make([]ExportArtifact, 0, len(formats))
	for _, format := range formats {
		normalized := strings.TrimSpace(strings.ToLower(format))
		if normalized == "" {
			continue
		}
		modelID := fmt.Sprintf("fine-tuned/%s-%s", trainingCommandID, normalized)
		runtime := ""
		switch normalized {
		case "gguf":
			runtime = "llamacpp"
		case "safetensors":
			runtime = "vllm"
		}
		exports = append(exports, ExportArtifact{
			Format:     normalized,
			OutputPath: filepath.Join("/var/lib/mantler/trainers/exports", trainingCommandID+"."+normalized),
			ModelID:    modelID,
			Runtime:    runtime,
		})
	}
	if len(exports) == 0 {
		return ExportResult{}, fmt.Errorf("no valid export formats provided")
	}
	return ExportResult{
		TrainingCommandID: trainingCommandID,
		Exports:           exports,
	}, nil
}
