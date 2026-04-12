package discovery

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

var topoLinkPattern = regexp.MustCompile(`^(NVL\d+|NV\d+|PIX|PXB|PHB|SYS|NODE|SOC)$`)

func CollectGPUInterconnect(gpuVendor string, gpus []GPUInfo) *types.GPUInterconnectReport {
	if strings.TrimSpace(strings.ToLower(gpuVendor)) != "nvidia" || len(gpus) < 2 {
		return nil
	}

	report := &types.GPUInterconnectReport{
		MeasuredAt: time.Now().UTC().Format(time.RFC3339),
	}

	edges, topoDetail := collectNvidiaTopologyEdges(len(gpus))
	if len(edges) > 0 {
		report.Edges = edges
		report.Detail = topoDetail
	}

	if matrix, source := collectNvidiaBandwidthMatrix(); len(matrix) > 0 {
		report.BandwidthMatrix = matrix
		report.MeasurementSource = source
	} else if len(report.Edges) > 0 {
		report.MeasurementSource = "nvidia_smi_topology_only"
	}

	if len(report.Edges) == 0 && len(report.BandwidthMatrix) == 0 {
		return nil
	}
	return report
}

func collectNvidiaTopologyEdges(gpuCount int) ([]types.GPUInterconnectEdge, string) {
	output, err := exec.Command("nvidia-smi", "topo", "-m").Output()
	if err != nil {
		return nil, ""
	}
	text := string(output)
	lines := strings.Split(text, "\n")
	rowByGPU := map[int][]string{}
	headers := []int{}
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "Legend") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.EqualFold(fields[0], "GPU0") {
			headers = parseGPUHeader(fields)
			continue
		}
		if idx, ok := parseGPUToken(fields[0]); ok {
			rowByGPU[idx] = fields[1:]
		}
	}
	if len(headers) == 0 || len(rowByGPU) == 0 {
		return nil, strings.TrimSpace(text)
	}

	edges := make([]types.GPUInterconnectEdge, 0, gpuCount*gpuCount)
	for fromIdx, row := range rowByGPU {
		for col, token := range row {
			if col >= len(headers) {
				continue
			}
			toIdx := headers[col]
			if fromIdx == toIdx {
				continue
			}
			linkType, linkCount, ok := classifyTopoToken(token)
			if !ok {
				continue
			}
			edges = append(edges, types.GPUInterconnectEdge{
				FromIndex: fromIdx,
				ToIndex:   toIdx,
				LinkType:  linkType,
				Path:      strings.TrimSpace(token),
				LinkCount: linkCount,
			})
		}
	}
	return dedupeTopologyEdges(edges), strings.TrimSpace(text)
}

func parseGPUHeader(fields []string) []int {
	indices := make([]int, 0, len(fields))
	for _, field := range fields {
		if idx, ok := parseGPUToken(field); ok {
			indices = append(indices, idx)
		}
	}
	return indices
}

func parseGPUToken(token string) (int, bool) {
	normalized := strings.TrimSpace(strings.ToUpper(token))
	if !strings.HasPrefix(normalized, "GPU") {
		return 0, false
	}
	raw := strings.TrimPrefix(normalized, "GPU")
	idx, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return idx, true
}

func classifyTopoToken(raw string) (string, int, bool) {
	token := strings.ToUpper(strings.TrimSpace(raw))
	if token == "" || token == "X" || token == "N/A" {
		return "", 0, false
	}
	if !topoLinkPattern.MatchString(token) {
		return "", 0, false
	}
	switch {
	case strings.HasPrefix(token, "NVL"), strings.HasPrefix(token, "NV"):
		count := 1
		suffix := strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(token, "NVL"), "NV"), " ")
		if parsed, err := strconv.Atoi(suffix); err == nil && parsed > 0 {
			count = parsed
		}
		return "nvlink", count, true
	case token == "PIX" || token == "PXB" || token == "PHB" || token == "SYS" || token == "NODE" || token == "SOC":
		return "pcie", 1, true
	default:
		return "", 0, false
	}
}

func dedupeTopologyEdges(edges []types.GPUInterconnectEdge) []types.GPUInterconnectEdge {
	if len(edges) < 2 {
		return edges
	}
	result := make([]types.GPUInterconnectEdge, 0, len(edges))
	seen := make(map[string]struct{}, len(edges))
	for _, edge := range edges {
		key := fmt.Sprintf("%d:%d:%s:%d", edge.FromIndex, edge.ToIndex, edge.LinkType, edge.LinkCount)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, edge)
	}
	return result
}

func collectNvidiaBandwidthMatrix() ([]types.GPUBandwidthMatrixEntry, string) {
	binary := resolveNvBandwidthBinary()
	if binary == "" {
		return nil, ""
	}
	output, err := exec.Command(binary, "-t", "device_to_device_memcpy_read_ce").Output()
	if err != nil {
		return nil, ""
	}
	matrix := parseNvBandwidthTextMatrix(string(output))
	if len(matrix) == 0 {
		return nil, ""
	}
	return matrix, "measured_nvbandwidth_matrix"
}

func parseNvBandwidthTextMatrix(output string) []types.GPUBandwidthMatrixEntry {
	scanner := bufio.NewScanner(strings.NewReader(output))
	headers := []int{}
	entries := make([]types.GPUBandwidthMatrixEntry, 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(fields[0]), "GPU") && len(headers) == 0 {
			headers = parseGPUHeader(fields)
			continue
		}
		fromIdx, ok := parseGPUToken(fields[0])
		if !ok || len(headers) == 0 {
			continue
		}
		for i := 1; i < len(fields); i++ {
			col := i - 1
			if col >= len(headers) {
				continue
			}
			toIdx := headers[col]
			if fromIdx == toIdx {
				continue
			}
			value, err := strconv.ParseFloat(strings.TrimRight(fields[i], ","), 64)
			if err != nil || value <= 0 {
				continue
			}
			entries = append(entries, types.GPUBandwidthMatrixEntry{
				FromIndex:     fromIdx,
				ToIndex:       toIdx,
				BandwidthGBps: value,
				Direction:     "read",
			})
		}
	}
	return entries
}

func resolveNvBandwidthBinary() string {
	if path, err := exec.LookPath("nvbandwidth"); err == nil {
		return path
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	candidates := []string{
		homeDir + "/.local/bin/nvbandwidth",
		"/usr/local/bin/nvbandwidth",
	}
	for _, candidate := range candidates {
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}
