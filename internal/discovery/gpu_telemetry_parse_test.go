package discovery

import "testing"

func TestParseCudaRuntimeVersionFromNvidiaSmiHeader(t *testing.T) {
	input := "NVIDIA-SMI 580.142                Driver Version: 580.142        CUDA Version: 13.0"
	got := parseCudaRuntimeVersion(input)
	if got != "13.0" {
		t.Fatalf("expected CUDA runtime 13.0, got %q", got)
	}
}

func TestParseNvBandwidthTextMatrix(t *testing.T) {
	input := `
GPU0 GPU1
GPU0 0 650.5
GPU1 640.2 0
`
	entries := parseNvBandwidthTextMatrix(input)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].FromIndex != 0 || entries[0].ToIndex != 1 || entries[0].BandwidthGBps != 650.5 {
		t.Fatalf("unexpected first entry: %+v", entries[0])
	}
	if entries[1].FromIndex != 1 || entries[1].ToIndex != 0 || entries[1].BandwidthGBps != 640.2 {
		t.Fatalf("unexpected second entry: %+v", entries[1])
	}
}

func TestClassifyTopoToken(t *testing.T) {
	linkType, linkCount, ok := classifyTopoToken("NV4")
	if !ok || linkType != "nvlink" || linkCount != 4 {
		t.Fatalf("expected NV4 -> nvlink/4, got ok=%v type=%q count=%d", ok, linkType, linkCount)
	}

	linkType, linkCount, ok = classifyTopoToken("PHB")
	if !ok || linkType != "pcie" || linkCount != 1 {
		t.Fatalf("expected PHB -> pcie/1, got ok=%v type=%q count=%d", ok, linkType, linkCount)
	}
}
