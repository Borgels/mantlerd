package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	agenttrainer "github.com/Borgels/mantlerd/internal/trainer"
	"github.com/spf13/cobra"
)

var trainerCmd = &cobra.Command{
	Use:   "trainer",
	Short: "Manage local trainer jobs",
}

var trainerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed trainers",
	Run:   runTrainerList,
}

var trainerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show training status from local job store",
	Run:   runTrainerStatus,
}

var trainerTrainCmd = &cobra.Command{
	Use:   "train",
	Short: "Start a local training job",
	Run:   runTrainerTrain,
}

var trainerStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop an in-progress training job",
	Run:   runTrainerStop,
}

var trainerExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export artifacts from a training job",
	Run:   runTrainerExport,
}

var (
	trainerStatusCommandID string
	trainerTrainType       string
	trainerTrainModel      string
	trainerTrainDataset    string
	trainerTrainMethod     string
	trainerTrainExport     string
	trainerTrainRuntime    string
	trainerTrainHyper      string
	trainerTrainCommandID  string
	trainerStopCommandID   string
	trainerStopSave        bool
	trainerExportCommandID string
	trainerExportFormats   string
	trainerExportRuntime   string
)

func init() {
	rootCmd.AddCommand(trainerCmd)
	trainerCmd.AddCommand(trainerListCmd)
	trainerCmd.AddCommand(trainerStatusCmd)
	trainerCmd.AddCommand(trainerTrainCmd)
	trainerCmd.AddCommand(trainerStopCmd)
	trainerCmd.AddCommand(trainerExportCmd)

	trainerStatusCmd.Flags().StringVar(&trainerStatusCommandID, "command-id", "", "Training command ID")

	trainerTrainCmd.Flags().StringVar(&trainerTrainType, "type", "unsloth", "Trainer type")
	trainerTrainCmd.Flags().StringVar(&trainerTrainModel, "model", "", "Base model identifier")
	trainerTrainCmd.Flags().StringVar(&trainerTrainDataset, "dataset", "", "Dataset path or URL")
	trainerTrainCmd.Flags().StringVar(&trainerTrainMethod, "method", "qlora", "Training method")
	trainerTrainCmd.Flags().StringVar(&trainerTrainExport, "export", "safetensors", "Comma-separated export formats")
	trainerTrainCmd.Flags().StringVar(&trainerTrainRuntime, "target-runtime", "", "Optional runtime target")
	trainerTrainCmd.Flags().StringVar(&trainerTrainHyper, "hyperparameters", "", "Comma-separated key=value pairs")
	trainerTrainCmd.Flags().StringVar(&trainerTrainCommandID, "command-id", "", "Optional command ID")

	trainerStopCmd.Flags().StringVar(&trainerStopCommandID, "command-id", "", "Training command ID")
	trainerStopCmd.Flags().BoolVar(&trainerStopSave, "save-checkpoint", true, "Graceful stop to preserve checkpoint")

	trainerExportCmd.Flags().StringVar(&trainerExportCommandID, "command-id", "", "Training command ID")
	trainerExportCmd.Flags().StringVar(&trainerExportFormats, "format", "safetensors", "Comma-separated export formats")
	trainerExportCmd.Flags().StringVar(&trainerExportRuntime, "target-runtime", "", "Optional runtime hint")
}

func runTrainerList(cmd *cobra.Command, args []string) {
	manager := agenttrainer.NewManager()
	trainers := manager.InstalledTrainers()
	if len(trainers) == 0 {
		fmt.Println("No trainers installed.")
		return
	}
	fmt.Println("Trainers:")
	for _, trainer := range trainers {
		version := strings.TrimSpace(trainer.Version)
		if version == "" {
			version = "-"
		}
		fmt.Printf("  %-12s %-12s %s\n", trainer.Type, trainer.Status, version)
	}
}

func runTrainerStatus(cmd *cobra.Command, args []string) {
	manager := agenttrainer.NewManager()
	jobs := manager.Jobs(strings.TrimSpace(trainerStatusCommandID))
	if len(jobs) == 0 {
		if strings.TrimSpace(trainerStatusCommandID) != "" {
			fmt.Fprintf(os.Stderr, "No job found for %s\n", trainerStatusCommandID)
			os.Exit(1)
		}
		fmt.Println("No training jobs recorded.")
		return
	}
	payload, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to encode status: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(payload))
}

func runTrainerTrain(cmd *cobra.Command, args []string) {
	if strings.TrimSpace(trainerTrainModel) == "" || strings.TrimSpace(trainerTrainDataset) == "" {
		fmt.Fprintln(os.Stderr, "--model and --dataset are required")
		os.Exit(1)
	}
	manager := agenttrainer.NewManager()
	commandID := strings.TrimSpace(trainerTrainCommandID)
	if commandID == "" {
		commandID = fmt.Sprintf("local-train-%d", time.Now().Unix())
	}
	request := agenttrainer.TrainingRequest{
		CommandID:       commandID,
		TrainerID:       "trainer-" + strings.ToLower(strings.TrimSpace(trainerTrainType)),
		TrainerType:     strings.ToLower(strings.TrimSpace(trainerTrainType)),
		Method:          strings.ToLower(strings.TrimSpace(trainerTrainMethod)),
		BaseModel:       strings.TrimSpace(trainerTrainModel),
		Dataset:         strings.TrimSpace(trainerTrainDataset),
		Hyperparameters: parseKeyValueMap(trainerTrainHyper),
		ExportFormats:   parseCSV(trainerTrainExport),
		TargetRuntime:   strings.TrimSpace(trainerTrainRuntime),
	}

	fmt.Printf("Starting training job %s\n", commandID)
	result, err := manager.StartTraining(context.Background(), request, func(progress agenttrainer.TrainingProgress) {
		if progress.CurrentStep > 0 && progress.TotalSteps > 0 {
			fmt.Printf("  step %d/%d %s\n", progress.CurrentStep, progress.TotalSteps, strings.TrimSpace(progress.Detail))
			return
		}
		if strings.TrimSpace(progress.Detail) != "" {
			fmt.Printf("  %s\n", strings.TrimSpace(progress.Detail))
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "training failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ %s\n", result.Detail)
}

func runTrainerStop(cmd *cobra.Command, args []string) {
	commandID := strings.TrimSpace(trainerStopCommandID)
	if commandID == "" {
		fmt.Fprintln(os.Stderr, "--command-id is required")
		os.Exit(1)
	}
	manager := agenttrainer.NewManager()
	if err := manager.StopTraining(context.Background(), commandID, trainerStopSave); err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop training: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ stopped %s\n", commandID)
}

func runTrainerExport(cmd *cobra.Command, args []string) {
	commandID := strings.TrimSpace(trainerExportCommandID)
	if commandID == "" {
		fmt.Fprintln(os.Stderr, "--command-id is required")
		os.Exit(1)
	}
	manager := agenttrainer.NewManager()
	jobs := manager.Jobs(commandID)
	trainerType := ""
	if len(jobs) > 0 {
		trainerType = jobs[0].TrainerType
	}
	result, err := manager.ExportCheckpoint(
		context.Background(),
		commandID,
		trainerType,
		parseCSV(trainerExportFormats),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export failed: %v\n", err)
		os.Exit(1)
	}
	if strings.TrimSpace(trainerExportRuntime) != "" {
		fmt.Printf("Target runtime: %s\n", trainerExportRuntime)
	}
	fmt.Printf("Exported %d artifact(s)\n", len(result.Exports))
	for _, artifact := range result.Exports {
		fmt.Printf("  - %s %s (%d bytes)\n", artifact.Format, artifact.OutputPath, artifact.SizeBytes)
	}
}

func parseCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		cleaned := strings.TrimSpace(part)
		if cleaned == "" {
			continue
		}
		result = append(result, cleaned)
	}
	return result
}

func parseKeyValueMap(value string) map[string]interface{} {
	result := map[string]interface{}{}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, raw, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		raw = strings.TrimSpace(raw)
		if key == "" || raw == "" {
			continue
		}
		if parsed, err := strconv.ParseFloat(raw, 64); err == nil {
			result[key] = parsed
		} else {
			result[key] = raw
		}
	}
	return result
}
