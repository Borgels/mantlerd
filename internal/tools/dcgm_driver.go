package tools

import (
	"fmt"
	"time"
)

type dcgmDriver struct{}

func newDCGMDriver() Driver { return &dcgmDriver{} }

func (d *dcgmDriver) Name() string { return "dcgm" }

func (d *dcgmDriver) Install() error {
	return fmt.Errorf("%w: dcgm install automation not configured on this host", ErrNotImplemented)
}

func (d *dcgmDriver) Uninstall() error {
	return fmt.Errorf("%w: dcgm uninstall automation not configured on this host", ErrNotImplemented)
}

func (d *dcgmDriver) IsInstalled() bool { return commandExists("dcgmi") }

func (d *dcgmDriver) IsReady() bool { return d.IsInstalled() }

func (d *dcgmDriver) Version() string {
	return commandVersion("dcgmi", "--version")
}

func (d *dcgmDriver) RunDiagnostic(level string) (DiagnosticResult, error) {
	if !d.IsInstalled() {
		return DiagnosticResult{}, fmt.Errorf("dcgm is not installed")
	}
	output, err := commandOutput(2*time.Minute, "dcgmi", "diag", "-r", "memory_bandwidth", "-j")
	if err != nil {
		return DiagnosticResult{}, err
	}
	value := parseBandwidthFromJSON(output)
	if value <= 0 {
		value = parseMaxBandwidthFromText(output)
	}
	if value <= 0 {
		return DiagnosticResult{}, fmt.Errorf("dcgm output did not include bandwidth")
	}
	return DiagnosticResult{
		MemoryBandwidthGBps: value,
		Detail:              "bandwidth measured via dcgmi diag",
		Source:              "measured_dcgm",
	}, nil
}

func (d *dcgmDriver) Configure(config map[string]any) error { return nil }
