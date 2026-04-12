package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Borgels/mantlerd/internal/discovery"
	agenttools "github.com/Borgels/mantlerd/internal/tools"
	"github.com/Borgels/mantlerd/internal/types"
	"github.com/spf13/cobra"
)

var (
	toolDiagnoseLevel string
	toolMeasureMatrix bool
	toolMeasureJSON   bool
)

var toolCmd = &cobra.Command{
	Use:   "tool",
	Short: "Manage host tools",
	Long: `Manage host-level tools used by mantlerd (diagnostics, containers, and runtime dependencies).

Examples:
  mantler tool list
  mantler tool status
  mantler tool measure-bandwidth
  mantler tool diagnose dcgm --level quick
  mantler tool install docker`,
}

var toolListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tool states for this host",
	Run:   runToolList,
}

var toolStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Alias for tool list",
	Run:   runToolList,
}

var toolMeasureBandwidthCmd = &cobra.Command{
	Use:   "measure-bandwidth",
	Short: "Measure/report GPU memory bandwidth for detected GPUs",
	Run:   runToolMeasureBandwidth,
}

var toolDoctorGPUStackCmd = &cobra.Command{
	Use:   "doctor-gpu-stack",
	Short: "Report accelerator stack versions",
	Run:   runToolDoctorGPUStack,
}

var toolDiagnoseCmd = &cobra.Command{
	Use:   "diagnose <tool>",
	Short: "Run a tool diagnostic",
	Args:  cobra.ExactArgs(1),
	Run:   runToolDiagnose,
}

var toolInstallCmd = &cobra.Command{
	Use:   "install <tool>",
	Short: "Install a tool",
	Args:  cobra.ExactArgs(1),
	Run:   runToolInstall,
}

var toolUninstallCmd = &cobra.Command{
	Use:   "uninstall <tool>",
	Short: "Uninstall a tool",
	Args:  cobra.ExactArgs(1),
	Run:   runToolUninstall,
}

func init() {
	rootCmd.AddCommand(toolCmd)
	toolCmd.AddCommand(toolListCmd)
	toolCmd.AddCommand(toolStatusCmd)
	toolCmd.AddCommand(toolMeasureBandwidthCmd)
	toolCmd.AddCommand(toolDiagnoseCmd)
	toolCmd.AddCommand(toolInstallCmd)
	toolCmd.AddCommand(toolUninstallCmd)
	toolCmd.AddCommand(toolDoctorGPUStackCmd)

	toolDiagnoseCmd.Flags().StringVar(&toolDiagnoseLevel, "level", "quick", "Diagnostic level (quick, standard, deep)")
	toolMeasureBandwidthCmd.Flags().BoolVar(&toolMeasureMatrix, "matrix", false, "Include pairwise GPU matrix when available")
	toolMeasureBandwidthCmd.Flags().BoolVar(&toolMeasureJSON, "json", false, "Emit structured JSON output")
}

func runToolList(cmd *cobra.Command, args []string) {
	manager := agenttools.NewManager()
	report := discovery.Collect()
	items := manager.InstalledTools(report.GPUVendor)
	sort.Slice(items, func(i, j int) bool {
		return items[i].Type < items[j].Type
	})

	fmt.Printf("Detected GPU vendor: %s\n", fallbackUnknown(report.GPUVendor))
	if len(items) == 0 {
		fmt.Println("No tools available.")
		return
	}
	fmt.Println()
	fmt.Println("Tools:")
	for _, item := range items {
		version := normalizeInline(item.Version)
		detail := strings.TrimSpace(item.Detail)
		fmt.Printf("  %-28s %-12s", item.Type, item.Status)
		if version != "" {
			fmt.Printf(" %s", version)
		}
		fmt.Println()
		if detail != "" {
			fmt.Printf("    %s\n", detail)
		}
	}
}

func runToolMeasureBandwidth(cmd *cobra.Command, args []string) {
	manager := agenttools.NewManager()
	report := discovery.Collect()
	if len(report.GPUs) == 0 {
		fmt.Println("No GPUs detected.")
		return
	}
	type gpuMeasurement struct {
		Name          string  `json:"name"`
		Index         int     `json:"index,omitempty"`
		BandwidthGBps float64 `json:"bandwidthGbps,omitempty"`
		Source        string  `json:"source"`
	}
	type response struct {
		GPUVendor       string                       `json:"gpuVendor"`
		GPUs            []gpuMeasurement             `json:"gpus"`
		GPUInterconnect *types.GPUInterconnectReport `json:"gpuInterconnect,omitempty"`
	}
	results := make([]gpuMeasurement, 0, len(report.GPUs))

	if !toolMeasureJSON {
		fmt.Printf("Detected GPU vendor: %s\n", fallbackUnknown(report.GPUVendor))
		fmt.Println()
	}
	seen := make(map[string]struct{}, len(report.GPUs))
	for _, gpu := range report.GPUs {
		name := normalizeInline(gpu.Name)
		if name == "" {
			continue
		}
		lowerName := strings.ToLower(name)
		if strings.Contains(lowerName, "vga compatible controller") || strings.HasPrefix(lowerName, "00:") || strings.HasPrefix(lowerName, "01:") || strings.HasPrefix(lowerName, "02:") {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		value, source := manager.MeasureGPUBandwidth(report.GPUVendor, gpu.Name)
		results = append(results, gpuMeasurement{
			Name:          name,
			Index:         gpu.Index,
			BandwidthGBps: value,
			Source:        source,
		})
		if toolMeasureJSON {
			continue
		}
		if value > 0 {
			fmt.Printf("%s: %.2f GB/s (%s)\n", name, value, source)
		} else {
			fmt.Printf("%s: unavailable (%s)\n", name, source)
		}
	}
	var gpuInterconnect *types.GPUInterconnectReport
	if toolMeasureMatrix {
		gpuInterconnect = report.GPUInterconnect
		if !toolMeasureJSON {
			if gpuInterconnect == nil || len(gpuInterconnect.BandwidthMatrix) == 0 {
				fmt.Println("\nPairwise matrix: unavailable")
			} else {
				fmt.Printf("\nPairwise matrix source: %s\n", fallbackUnknown(gpuInterconnect.MeasurementSource))
				for _, entry := range gpuInterconnect.BandwidthMatrix {
					fmt.Printf("  GPU%d -> GPU%d: %.2f GB/s\n", entry.FromIndex, entry.ToIndex, entry.BandwidthGBps)
				}
			}
		}
	}
	if toolMeasureJSON {
		payload := response{
			GPUVendor:       report.GPUVendor,
			GPUs:            results,
			GPUInterconnect: gpuInterconnect,
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(payload)
	}
}

func runToolDoctorGPUStack(cmd *cobra.Command, args []string) {
	report := discovery.Collect()
	stack := report.AcceleratorStack
	if stack == nil {
		fmt.Println("No accelerator stack data available.")
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(stack)
}

func runToolDiagnose(cmd *cobra.Command, args []string) {
	manager := agenttools.NewManager()
	toolType, err := parseToolType(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	result, err := manager.RunDiagnostic(toolType, toolDiagnoseLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "diagnostic failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Tool: %s\n", toolType)
	fmt.Printf("Source: %s\n", fallbackUnknown(result.Source))
	if result.MemoryBandwidthGBps > 0 {
		fmt.Printf("Memory bandwidth: %.2f GB/s\n", result.MemoryBandwidthGBps)
	}
	if strings.TrimSpace(result.Detail) != "" {
		fmt.Printf("Detail: %s\n", result.Detail)
	}
}

func runToolInstall(cmd *cobra.Command, args []string) {
	manager := agenttools.NewManager()
	toolType, err := parseToolType(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := manager.Install(toolType); err != nil {
		if errors.Is(err, agenttools.ErrNotImplemented) {
			fmt.Fprintf(os.Stderr, "install not implemented for %s on this host: %v\n", toolType, err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Installed tool: %s\n", toolType)
}

func runToolUninstall(cmd *cobra.Command, args []string) {
	manager := agenttools.NewManager()
	toolType, err := parseToolType(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := manager.Uninstall(toolType); err != nil {
		if errors.Is(err, agenttools.ErrNotImplemented) {
			fmt.Fprintf(os.Stderr, "uninstall not implemented for %s on this host: %v\n", toolType, err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "uninstall failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Uninstalled tool: %s\n", toolType)
}

func parseToolType(input string) (types.ToolType, error) {
	normalized := strings.TrimSpace(strings.ToLower(input))
	switch normalized {
	case string(types.ToolDCGM):
		return types.ToolDCGM, nil
	case string(types.ToolNvBandwidth):
		return types.ToolNvBandwidth, nil
	case string(types.ToolRocmBandwidthTest):
		return types.ToolRocmBandwidthTest, nil
	case string(types.ToolDocker):
		return types.ToolDocker, nil
	case string(types.ToolNvidiaContainerToolkit):
		return types.ToolNvidiaContainerToolkit, nil
	default:
		return "", fmt.Errorf(
			"unknown tool %q (supported: %s, %s, %s, %s, %s)",
			input,
			types.ToolDCGM,
			types.ToolNvBandwidth,
			types.ToolRocmBandwidthTest,
			types.ToolDocker,
			types.ToolNvidiaContainerToolkit,
		)
	}
}

func fallbackUnknown(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

func normalizeInline(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	parts := strings.Fields(value)
	return strings.Join(parts, " ")
}
