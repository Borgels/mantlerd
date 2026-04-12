package tools

import (
	"fmt"
	"time"
)

type rocmBandwidthDriver struct{}

func newRocmBandwidthDriver() Driver { return &rocmBandwidthDriver{} }

func (d *rocmBandwidthDriver) Name() string { return "rocm_bandwidth_test" }

func (d *rocmBandwidthDriver) Install() error {
	return fmt.Errorf("%w: rocm bandwidth tool install automation not configured on this host", ErrNotImplemented)
}

func (d *rocmBandwidthDriver) Uninstall() error {
	return fmt.Errorf("%w: rocm bandwidth tool uninstall automation not configured on this host", ErrNotImplemented)
}

func (d *rocmBandwidthDriver) IsInstalled() bool { return commandExists("rocm-bandwidth-test") }

func (d *rocmBandwidthDriver) IsReady() bool { return d.IsInstalled() }

func (d *rocmBandwidthDriver) Version() string {
	return commandVersion("rocm-bandwidth-test", "--version")
}

func (d *rocmBandwidthDriver) RunDiagnostic(level string) (DiagnosticResult, error) {
	if !d.IsInstalled() {
		return DiagnosticResult{}, fmt.Errorf("rocm-bandwidth-test is not installed")
	}
	output, err := commandOutput(2*time.Minute, "rocm-bandwidth-test")
	if err != nil {
		return DiagnosticResult{}, err
	}
	value := parseMaxBandwidthFromText(output)
	if value <= 0 {
		return DiagnosticResult{}, fmt.Errorf("rocm-bandwidth-test output did not include bandwidth")
	}
	return DiagnosticResult{
		MemoryBandwidthGBps: value,
		Detail:              "bandwidth measured via rocm-bandwidth-test",
		Source:              "measured_rocm",
	}, nil
}

func (d *rocmBandwidthDriver) Configure(config map[string]any) error { return nil }
