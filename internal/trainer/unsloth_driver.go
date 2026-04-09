package trainer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type unslothDriver struct{}

func newUnslothDriver() Driver {
	return &unslothDriver{}
}

func (d *unslothDriver) Install(ctx context.Context) (string, error) {
	python, err := exec.LookPath("python3")
	if err != nil {
		return "", fmt.Errorf("python3 not found in PATH")
	}
	pipInstallCmd := fmt.Sprintf(
		`set -euo pipefail; VENV_DIR="${MANTLER_TRAINER_VENV_DIR:-$HOME/.local/share/mantler/trainers/unsloth-venv}"; %s -m venv "$VENV_DIR"; "$VENV_DIR/bin/python" -m pip install --upgrade pip setuptools wheel; "$VENV_DIR/bin/python" -m pip install --upgrade unsloth`,
		shellQuote(python),
	)
	cmd := exec.CommandContext(ctx, "bash", "-lc", pipInstallCmd)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return "", fmt.Errorf("install unsloth failed: %w (%s)", runErr, strings.TrimSpace(string(output)))
	}

	versionCmd := exec.CommandContext(
		ctx,
		"bash",
		"-lc",
		`VENV_DIR="${MANTLER_TRAINER_VENV_DIR:-$HOME/.local/share/mantler/trainers/unsloth-venv}"; "$VENV_DIR/bin/python" -c "import unsloth,sys; sys.stdout.write(getattr(unsloth, '__version__', 'unknown'))"`,
	)
	versionOut, versionErr := versionCmd.CombinedOutput()
	if versionErr != nil {
		return "", fmt.Errorf("install unsloth completed but version probe failed: %w (%s)", versionErr, strings.TrimSpace(string(versionOut)))
	}
	version := strings.TrimSpace(string(versionOut))
	if version == "" {
		return "", fmt.Errorf("install unsloth completed but version probe returned empty output")
	}
	return version, nil
}

func (d *unslothDriver) Uninstall(ctx context.Context) error {
	cmd := exec.CommandContext(
		ctx,
		"bash",
		"-lc",
		`set -euo pipefail; VENV_DIR="${MANTLER_TRAINER_VENV_DIR:-$HOME/.local/share/mantler/trainers/unsloth-venv}"; rm -rf "$VENV_DIR"`,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("uninstall unsloth failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d *unslothDriver) StartTraining(ctx context.Context, req TrainingRequest, emitProgress func(TrainingProgress)) (TrainingResult, error) {
	if !mockTrainingEnabled() {
		return TrainingResult{}, fmt.Errorf("real unsloth training is not yet implemented; set MANTLER_TRAINER_MOCK=true for mock mode")
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
	return TrainingResult{Detail: fmt.Sprintf("Training complete for %s on %s", req.BaseModel, req.Dataset)}, nil
}

func (d *unslothDriver) StopTraining(ctx context.Context, commandID string, saveCheckpoint bool) error {
	return nil
}

func (d *unslothDriver) ExportCheckpoint(ctx context.Context, trainingCommandID string, formats []string) (ExportResult, error) {
	if !mockTrainingEnabled() {
		return ExportResult{}, fmt.Errorf("real unsloth export is not yet implemented; set MANTLER_TRAINER_MOCK=true for mock mode")
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

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func mockTrainingEnabled() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv("MANTLER_TRAINER_MOCK")))
	return value == "1" || value == "true" || value == "yes"
}
