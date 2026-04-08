package runtime

import "sort"

type Family string

const (
	FamilyOllama   Family = "ollama"
	FamilyVLLM     Family = "vllm"
	FamilyLlamaCPP Family = "llama.cpp"
	FamilyTensorRT Family = "tensorrt"
	FamilyQuantCPP Family = "quant.cpp"
)

type RuntimeSpec struct {
	Name            string
	DisplayName     string
	Family          Family
	Description     string
	BackendVariants []string
	CreateDriver    func() Driver
}

var runtimeCatalog = []RuntimeSpec{
	{
		Name:            "ollama",
		DisplayName:     "Ollama",
		Family:          FamilyOllama,
		Description:     "Simple local runtime with built-in model management",
		BackendVariants: []string{"cpu", "cuda"},
		CreateDriver:    newOllamaDriver,
	},
	{
		Name:            "vllm",
		DisplayName:     "vLLM",
		Family:          FamilyVLLM,
		Description:     "High-throughput runtime for single-model serving",
		BackendVariants: []string{"cuda"},
		CreateDriver:    newVLLMDriver,
	},
	{
		Name:            "llamacpp",
		DisplayName:     "llama.cpp",
		Family:          FamilyLlamaCPP,
		Description:     "llama.cpp runtime managed by mantlerd",
		BackendVariants: []string{"cpu", "cuda", "vulkan", "metal", "rocm"},
		CreateDriver:    newLlamaCppDriver,
	},
	{
		Name:            "tensorrt",
		DisplayName:     "TensorRT-LLM",
		Family:          FamilyTensorRT,
		Description:     "NVIDIA TensorRT-LLM optimized serving runtime",
		BackendVariants: []string{"cuda"},
		CreateDriver:    newTensorRTDriver,
	},
	{
		Name:            "quantcpp",
		DisplayName:     "quant.cpp",
		Family:          FamilyQuantCPP,
		Description:     "Minimal quant.cpp runtime with KV cache compression",
		BackendVariants: []string{"cpu"},
		CreateDriver:    newQuantCppDriver,
	},
}

var supportedRuntimeNameSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(runtimeCatalog))
	for _, spec := range runtimeCatalog {
		set[spec.Name] = struct{}{}
	}
	return set
}()

func RuntimeCatalog() []RuntimeSpec {
	result := make([]RuntimeSpec, len(runtimeCatalog))
	copy(result, runtimeCatalog)
	return result
}

func SupportedRuntimeNames() []string {
	names := make([]string, 0, len(runtimeCatalog))
	for _, spec := range runtimeCatalog {
		names = append(names, spec.Name)
	}
	sort.Strings(names)
	return names
}

func IsSupportedRuntimeName(runtimeName string) bool {
	_, ok := supportedRuntimeNameSet[runtimeName]
	return ok
}

func NewDriverRegistry() map[string]Driver {
	drivers := make(map[string]Driver, len(runtimeCatalog))
	for _, spec := range runtimeCatalog {
		drivers[spec.Name] = spec.CreateDriver()
	}
	return drivers
}
