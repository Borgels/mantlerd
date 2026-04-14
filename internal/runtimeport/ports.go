package runtimeport

import "strings"

func Resolve(runtimeName string) int {
	switch strings.ToLower(strings.TrimSpace(runtimeName)) {
	case "vllm", "tensorrt":
		return 8000
	case "llamacpp":
		return 1234
	case "quantcpp", "mlx":
		return 8080
	default:
		return 11434
	}
}

