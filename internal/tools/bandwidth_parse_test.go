package tools

import "testing"

func TestParseBandwidthFromJSONAvoidsVersionFields(t *testing.T) {
	input := `{
		"driver_version": 580.142,
		"cuda_runtime": 13000,
		"results": {
			"device_to_device_bandwidth_gbps": 258.6
		}
	}`
	value := parseBandwidthFromJSON(input)
	if value != 258.6 {
		t.Fatalf("expected bandwidth value 258.6, got %v", value)
	}
}

func TestParseBandwidthFromJSONReturnsZeroWithoutBandwidthContext(t *testing.T) {
	input := `{
		"driver_version": 580.142,
		"cuda_runtime": 13000,
		"pci_id": 1
	}`
	value := parseBandwidthFromJSON(input)
	if value != 0 {
		t.Fatalf("expected zero when no bandwidth fields exist, got %v", value)
	}
}

func TestParseMaxBandwidthFromTextPrefersMeasuredValues(t *testing.T) {
	input := `nvbandwidth Version: v0.9
CUDA Runtime Version: 13000
CUDA Driver Version: 13000
Driver Version: 580.142

Running host_to_device_memcpy_ce.
memcpy CE CPU(row) -> GPU(column) bandwidth (GB/s)
           0
 0     59.16

SUM host_to_device_memcpy_ce 59.16`
	value := parseMaxBandwidthFromText(input)
	if value != 59.16 {
		t.Fatalf("expected parsed bandwidth 59.16, got %v", value)
	}
}
