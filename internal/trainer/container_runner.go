package trainer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTrainerDataDir = "/var/lib/mantler/trainers"
)

type containerRunner struct {
	dataDir string
}

func newContainerRunner() *containerRunner {
	dataDir := strings.TrimSpace(os.Getenv("MANTLER_TRAINER_DATA_DIR"))
	if dataDir == "" {
		dataDir = defaultTrainerDataDir
	}
	return &containerRunner{dataDir: dataDir}
}

func (r *containerRunner) ValidateRuntime(ctx context.Context, trainerType string) error {
	spec, err := resolveContainerSpec(trainerType)
	if err != nil {
		return err
	}
	containerBin, err := resolveContainerBinary()
	if err != nil {
		return err
	}
	if _, err := runContainerCommand(ctx, containerBin, "pull", spec.Image); err != nil {
		return fmt.Errorf("pull trainer image %s: %w", spec.Image, err)
	}
	return nil
}

func (r *containerRunner) RunTraining(
	ctx context.Context,
	trainerType string,
	req TrainingRequest,
	emitProgress func(TrainingProgress),
) (TrainingResult, error) {
	spec, err := resolveContainerSpec(trainerType)
	if err != nil {
		return TrainingResult{}, err
	}
	containerBin, err := resolveContainerBinary()
	if err != nil {
		return TrainingResult{}, err
	}
	if req.CommandID == "" {
		return TrainingResult{}, fmt.Errorf("command id is required")
	}

	jobDir := r.jobDir(req.CommandID)
	outputDir := filepath.Join(jobDir, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return TrainingResult{}, fmt.Errorf("create output dir: %w", err)
	}
	if _, err := runContainerCommand(ctx, containerBin, "pull", spec.Image); err != nil {
		return TrainingResult{}, fmt.Errorf("pull trainer image %s: %w", spec.Image, err)
	}

	containerName := trainingContainerName(req.CommandID)
	args := []string{
		"run",
		"--rm",
		"--name", containerName,
		"--gpus", "all",
		"-v", fmt.Sprintf("%s:/output", outputDir),
		"-e", "PYTHONUNBUFFERED=1",
		spec.Image,
		"bash", "-lc", spec.TrainCmd(req),
	}
	if hostDatasetPath := normalizeHostPath(req.Dataset); hostDatasetPath != "" {
		args = append(
			[]string{
				"run",
				"--rm",
				"--name", containerName,
				"--gpus", "all",
				"-v", fmt.Sprintf("%s:/output", outputDir),
				"-v", fmt.Sprintf("%s:/dataset", hostDatasetPath),
				"-e", "DATASET=/dataset",
				"-e", "PYTHONUNBUFFERED=1",
				spec.Image,
				"bash", "-lc", spec.TrainCmd(req),
			},
		)
	}

	cmd := exec.CommandContext(ctx, containerBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return TrainingResult{}, fmt.Errorf("open training stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return TrainingResult{}, fmt.Errorf("open training stderr: %w", err)
	}

	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		return TrainingResult{}, fmt.Errorf("start training container: %w", err)
	}

	progressReader := io.MultiReader(stdout, stderr)
	done := make(chan struct{})
	go func() {
		defer close(done)
		parseContainerProgress(progressReader, startedAt, emitProgress)
	}()

	waitErr := cmd.Wait()
	<-done

	if waitErr != nil {
		return TrainingResult{}, fmt.Errorf("training container failed: %w", waitErr)
	}

	artifacts, err := collectArtifacts(outputDir, req.CommandID, req.ExportFormats, req.TargetRuntime)
	if err != nil {
		return TrainingResult{}, err
	}
	return TrainingResult{
		Detail:    fmt.Sprintf("Training completed for %s", req.BaseModel),
		Artifacts: artifacts,
	}, nil
}

func (r *containerRunner) StopTraining(ctx context.Context, commandID string, saveCheckpoint bool) error {
	containerBin, err := resolveContainerBinary()
	if err != nil {
		return err
	}
	containerName := trainingContainerName(commandID)
	if saveCheckpoint {
		if _, err := runContainerCommand(ctx, containerBin, "stop", "-t", "15", containerName); err != nil {
			return err
		}
		return nil
	}
	if _, err := runContainerCommand(ctx, containerBin, "kill", containerName); err != nil {
		return err
	}
	return nil
}

func (r *containerRunner) Export(
	ctx context.Context,
	trainerType string,
	trainingCommandID string,
	formats []string,
) (ExportResult, error) {
	spec, err := resolveContainerSpec(trainerType)
	if err != nil {
		return ExportResult{}, err
	}
	outputDir := filepath.Join(r.jobDir(trainingCommandID), "output")
	if _, statErr := os.Stat(outputDir); statErr != nil {
		return ExportResult{}, fmt.Errorf("training output not found for %s", trainingCommandID)
	}

	containerBin, err := resolveContainerBinary()
	if err != nil {
		return ExportResult{}, err
	}
	exportCmd := spec.ExportCmd(formats)
	if strings.TrimSpace(exportCmd) != "" {
		args := []string{
			"run",
			"--rm",
			"--name", exportContainerName(trainingCommandID),
			"-v", fmt.Sprintf("%s:/output", outputDir),
			spec.Image,
			"bash", "-lc", exportCmd,
		}
		if _, err := runContainerCommand(ctx, containerBin, args...); err != nil {
			return ExportResult{}, fmt.Errorf("export container failed: %w", err)
		}
	}

	artifacts, err := collectArtifacts(outputDir, trainingCommandID, formats, "")
	if err != nil {
		return ExportResult{}, err
	}
	return ExportResult{
		TrainingCommandID: trainingCommandID,
		Exports:           artifacts,
	}, nil
}

func (r *containerRunner) jobDir(commandID string) string {
	return filepath.Join(r.dataDir, "jobs", sanitizeID(commandID))
}

func resolveContainerBinary() (string, error) {
	if path, err := exec.LookPath("docker"); err == nil {
		return path, nil
	}
	if path, err := exec.LookPath("podman"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("docker or podman is required for trainer execution")
}

func runContainerCommand(ctx context.Context, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	output, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(output))
	if err != nil {
		if trimmed == "" {
			return "", err
		}
		return trimmed, fmt.Errorf("%w (%s)", err, trimmed)
	}
	return trimmed, nil
}

var progressStepRe = regexp.MustCompile(`(?i)(?:step|iter(?:ation)?)\s+(\d+)\s*/\s*(\d+)`)
var lossRe = regexp.MustCompile(`(?i)loss[:=]\s*([0-9]*\.?[0-9]+)`)

func parseContainerProgress(reader io.Reader, startedAt time.Time, emitProgress func(TrainingProgress)) {
	if emitProgress == nil {
		return
	}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		progress := TrainingProgress{
			Status:         "running",
			Detail:         line,
			ElapsedSeconds: int(time.Since(startedAt).Seconds()),
		}
		if m := progressStepRe.FindStringSubmatch(line); len(m) == 3 {
			progress.CurrentStep = parseIntOrZero(m[1])
			progress.TotalSteps = parseIntOrZero(m[2])
			if progress.TotalSteps > progress.CurrentStep {
				progress.EtaSeconds = progress.TotalSteps - progress.CurrentStep
			}
		}
		if m := lossRe.FindStringSubmatch(line); len(m) == 2 {
			progress.Loss = parseFloatOrZero(m[1])
		}
		emitProgress(progress)
	}
}

func collectArtifacts(outputDir string, commandID string, formats []string, targetRuntime string) ([]ExportArtifact, error) {
	requested := normalizeFormats(formats)
	collected := make([]ExportArtifact, 0)
	err := filepath.WalkDir(outputDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		format := detectFormat(path)
		if format == "" {
			return nil
		}
		if len(requested) > 0 && !requested[format] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		checksum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		runtime := runtimeForFormat(format, targetRuntime)
		collected = append(collected, ExportArtifact{
			Format:     format,
			OutputPath: path,
			ModelID:    fmt.Sprintf("fine-tuned/%s-%s", commandID, format),
			Runtime:    runtime,
			SizeBytes:  info.Size(),
			SHA256:     checksum,
			CreatedAt:  info.ModTime().UTC().Format(time.RFC3339),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan artifacts: %w", err)
	}

	hasSafetensors := false
	for _, artifact := range collected {
		if artifact.Format == "safetensors" && artifact.SizeBytes > 0 {
			hasSafetensors = true
			break
		}
	}
	if !hasSafetensors {
		return nil, fmt.Errorf("training completed but no valid Safetensors artifacts produced")
	}
	if requested["gguf"] && (targetRuntime == "llamacpp" || targetRuntime == "") {
		// GGUF is optional even when requested for non-llama.cpp targets.
	}
	return collected, nil
}

func normalizeFormats(formats []string) map[string]bool {
	if len(formats) == 0 {
		return map[string]bool{}
	}
	result := make(map[string]bool, len(formats))
	for _, format := range formats {
		normalized := strings.TrimSpace(strings.ToLower(format))
		if normalized == "" {
			continue
		}
		result[normalized] = true
	}
	return result
}

func runtimeForFormat(format string, targetRuntime string) string {
	if strings.TrimSpace(targetRuntime) != "" {
		return strings.TrimSpace(targetRuntime)
	}
	switch format {
	case "gguf":
		return "llamacpp"
	case "safetensors":
		return "vllm"
	default:
		return ""
	}
}

func detectFormat(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".safetensors"):
		return "safetensors"
	case strings.HasSuffix(lower, ".gguf"):
		return "gguf"
	case strings.HasSuffix(lower, ".bin"), strings.HasSuffix(lower, ".pt"), strings.HasSuffix(lower, ".adapter"):
		return "lora_only"
	default:
		return ""
	}
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func normalizeHostPath(pathValue string) string {
	trimmed := strings.TrimSpace(pathValue)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "://") {
		return ""
	}
	if !filepath.IsAbs(trimmed) {
		return ""
	}
	return trimmed
}

func sanitizeID(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "job"
	}
	var b strings.Builder
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(strings.ToLower(b.String()), "-")
}

func trainingContainerName(commandID string) string {
	return "mantler-train-" + sanitizeID(commandID)
}

func exportContainerName(commandID string) string {
	return "mantler-export-" + sanitizeID(commandID)
}

func parseIntOrZero(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return n
}

func parseFloatOrZero(value string) float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0
	}
	return n
}
