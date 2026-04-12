package trainer

import (
	"fmt"
	"strconv"
	"strings"
)

type containerSpec struct {
	TrainerType string
	Image       string
	TrainCmd    func(req TrainingRequest) string
	ExportCmd   func(formats []string) string
}

var trainerContainerSpecs = map[string]containerSpec{
	"unsloth": {
		TrainerType: "unsloth",
		Image:       "unsloth/unsloth:latest",
		TrainCmd: func(req TrainingRequest) string {
			return firstNonEmpty(
				stringHyperparameter(req.Hyperparameters, "command"),
				fmt.Sprintf(
					`python -m unsloth.cli.train --model "%s" --dataset "%s" --method "%s" --output_dir /output --epochs %s --learning_rate %s --batch_size %s`,
					req.BaseModel,
					req.Dataset,
					normalizedMethod(req.Method),
					floatOrDefault(req.Hyperparameters, "epochs", "3"),
					floatOrDefault(req.Hyperparameters, "learning_rate", "0.0002"),
					floatOrDefault(req.Hyperparameters, "batch_size", "4"),
				),
			)
		},
		ExportCmd: defaultExportCommand,
	},
	"axolotl": {
		TrainerType: "axolotl",
		Image:       "winglian/axolotl:latest",
		TrainCmd: func(req TrainingRequest) string {
			return firstNonEmpty(
				stringHyperparameter(req.Hyperparameters, "command"),
				fmt.Sprintf(
					`axolotl train --base_model "%s" --dataset "%s" --output_dir /output --adapter "%s" --num_epochs %s --micro_batch_size %s`,
					req.BaseModel,
					req.Dataset,
					normalizedMethod(req.Method),
					floatOrDefault(req.Hyperparameters, "epochs", "3"),
					floatOrDefault(req.Hyperparameters, "batch_size", "4"),
				),
			)
		},
		ExportCmd: defaultExportCommand,
	},
	"trl": {
		TrainerType: "trl",
		Image:       "huggingface/trl:latest",
		TrainCmd: func(req TrainingRequest) string {
			subcommand := "sft"
			if strings.EqualFold(strings.TrimSpace(req.Method), "dpo") {
				subcommand = "dpo"
			}
			return firstNonEmpty(
				stringHyperparameter(req.Hyperparameters, "command"),
				fmt.Sprintf(
					`trl %s --model_name_or_path "%s" --dataset_name "%s" --output_dir /output --num_train_epochs %s --learning_rate %s --per_device_train_batch_size %s`,
					subcommand,
					req.BaseModel,
					req.Dataset,
					floatOrDefault(req.Hyperparameters, "epochs", "3"),
					floatOrDefault(req.Hyperparameters, "learning_rate", "0.0002"),
					floatOrDefault(req.Hyperparameters, "batch_size", "2"),
				),
			)
		},
		ExportCmd: defaultExportCommand,
	},
	"llamafactory": {
		TrainerType: "llamafactory",
		Image:       "hiyouga/llamafactory:latest",
		TrainCmd: func(req TrainingRequest) string {
			return firstNonEmpty(
				stringHyperparameter(req.Hyperparameters, "command"),
				fmt.Sprintf(
					`llamafactory-cli train --model_name_or_path "%s" --dataset "%s" --finetuning_type "%s" --output_dir /output --num_train_epochs %s --learning_rate %s --per_device_train_batch_size %s`,
					req.BaseModel,
					req.Dataset,
					normalizedMethod(req.Method),
					floatOrDefault(req.Hyperparameters, "epochs", "3"),
					floatOrDefault(req.Hyperparameters, "learning_rate", "0.0002"),
					floatOrDefault(req.Hyperparameters, "batch_size", "4"),
				),
			)
		},
		ExportCmd: defaultExportCommand,
	},
}

func resolveContainerSpec(trainerType string) (containerSpec, error) {
	normalized := strings.TrimSpace(strings.ToLower(trainerType))
	spec, ok := trainerContainerSpecs[normalized]
	if !ok {
		return containerSpec{}, fmt.Errorf("unsupported trainer type: %s", trainerType)
	}
	return spec, nil
}

func defaultExportCommand(formats []string) string {
	if len(formats) == 0 {
		return `echo "No explicit export command requested; using generated training artifacts"`
	}
	quoted := make([]string, 0, len(formats))
	for _, format := range formats {
		format = strings.TrimSpace(strings.ToLower(format))
		if format == "" {
			continue
		}
		quoted = append(quoted, format)
	}
	if len(quoted) == 0 {
		return `echo "No valid export formats requested"`
	}
	return fmt.Sprintf(`echo "Preparing exports for formats: %s"`, strings.Join(quoted, ","))
}

func normalizedMethod(method string) string {
	normalized := strings.TrimSpace(strings.ToLower(method))
	switch normalized {
	case "qlora", "lora", "full", "dpo":
		return normalized
	default:
		return "lora"
	}
}

func stringHyperparameter(h map[string]interface{}, key string) string {
	if h == nil {
		return ""
	}
	value, ok := h[key]
	if !ok {
		return ""
	}
	asString, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(asString)
}

func floatOrDefault(h map[string]interface{}, key string, fallback string) string {
	if h == nil {
		return fallback
	}
	value, ok := h[key]
	if !ok {
		return fallback
	}
	switch typed := value.(type) {
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case string:
		cleaned := strings.TrimSpace(typed)
		if cleaned != "" {
			return cleaned
		}
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
