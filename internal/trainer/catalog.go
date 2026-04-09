package trainer

import "sort"

type TrainerSpec struct {
	Name         string
	DisplayName  string
	Description  string
	CreateDriver func() Driver
}

var trainerCatalog = []TrainerSpec{
	{
		Name:         "unsloth",
		DisplayName:  "Unsloth",
		Description:  "Fast local fine-tuning with LoRA and QLoRA workflows",
		CreateDriver: newUnslothDriver,
	},
	{
		Name:         "axolotl",
		DisplayName:  "Axolotl",
		Description:  "Config-driven fine-tuning toolkit",
		CreateDriver: func() Driver { return newStubDriver("axolotl") },
	},
	{
		Name:         "trl",
		DisplayName:  "TRL",
		Description:  "Transformer reinforcement learning and SFT trainer",
		CreateDriver: func() Driver { return newStubDriver("trl") },
	},
	{
		Name:         "llamafactory",
		DisplayName:  "LLaMA-Factory",
		Description:  "Unified training toolkit for adapter and full finetune jobs",
		CreateDriver: func() Driver { return newStubDriver("llamafactory") },
	},
}

func TrainerCatalog() []TrainerSpec {
	result := make([]TrainerSpec, len(trainerCatalog))
	copy(result, trainerCatalog)
	return result
}

func SupportedTrainerNames() []string {
	names := make([]string, 0, len(trainerCatalog))
	for _, spec := range trainerCatalog {
		names = append(names, spec.Name)
	}
	sort.Strings(names)
	return names
}

func NewDriverRegistry() map[string]Driver {
	drivers := make(map[string]Driver, len(trainerCatalog))
	for _, spec := range trainerCatalog {
		drivers[spec.Name] = spec.CreateDriver()
	}
	return drivers
}
