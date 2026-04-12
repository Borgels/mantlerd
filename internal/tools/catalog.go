package tools

import "github.com/Borgels/mantlerd/internal/types"

type ToolSpec struct {
	Name             types.ToolType
	DisplayName      string
	Category         string
	SupportedVendors []string
	RequiresGPU      bool
	CreateDriver     func() Driver
}

var toolCatalog = []ToolSpec{
	{
		Name:             types.ToolDCGM,
		DisplayName:      "NVIDIA DCGM",
		Category:         "gpu_diagnostics",
		SupportedVendors: []string{"nvidia"},
		RequiresGPU:      true,
		CreateDriver:     func() Driver { return newDCGMDriver() },
	},
	{
		Name:             types.ToolNvBandwidth,
		DisplayName:      "NVIDIA NVBandwidth",
		Category:         "gpu_diagnostics",
		SupportedVendors: []string{"nvidia"},
		RequiresGPU:      true,
		CreateDriver:     func() Driver { return newNvBandwidthDriver() },
	},
	{
		Name:             types.ToolRocmBandwidthTest,
		DisplayName:      "ROCm bandwidth test",
		Category:         "gpu_diagnostics",
		SupportedVendors: []string{"amd"},
		RequiresGPU:      true,
		CreateDriver:     func() Driver { return newRocmBandwidthDriver() },
	},
	{
		Name:             types.ToolDocker,
		DisplayName:      "Docker",
		Category:         "container",
		SupportedVendors: []string{"all"},
		CreateDriver:     func() Driver { return newDockerDriver() },
	},
	{
		Name:             types.ToolNvidiaContainerToolkit,
		DisplayName:      "NVIDIA Container Toolkit",
		Category:         "container",
		SupportedVendors: []string{"nvidia"},
		RequiresGPU:      true,
		CreateDriver:     func() Driver { return newNvidiaContainerToolkitDriver() },
	},
}
