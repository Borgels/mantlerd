package tools

import "fmt"

type nvidiaContainerToolkitDriver struct{}

func newNvidiaContainerToolkitDriver() Driver { return &nvidiaContainerToolkitDriver{} }

func (d *nvidiaContainerToolkitDriver) Name() string { return "nvidia_container_toolkit" }

func (d *nvidiaContainerToolkitDriver) Install() error {
	return fmt.Errorf("%w: nvidia container toolkit install automation not configured on this host", ErrNotImplemented)
}

func (d *nvidiaContainerToolkitDriver) Uninstall() error {
	return fmt.Errorf("%w: nvidia container toolkit uninstall automation not configured on this host", ErrNotImplemented)
}

func (d *nvidiaContainerToolkitDriver) IsInstalled() bool {
	return commandExists("nvidia-ctk")
}

func (d *nvidiaContainerToolkitDriver) IsReady() bool { return d.IsInstalled() }

func (d *nvidiaContainerToolkitDriver) Version() string {
	return commandVersion("nvidia-ctk", "--version")
}

func (d *nvidiaContainerToolkitDriver) RunDiagnostic(level string) (DiagnosticResult, error) {
	if !d.IsInstalled() {
		return DiagnosticResult{}, fmt.Errorf("nvidia container toolkit is not installed")
	}
	return DiagnosticResult{
		Detail: "nvidia-ctk binary detected",
		Source: "nvidia-ctk",
	}, nil
}

func (d *nvidiaContainerToolkitDriver) Configure(config map[string]any) error { return nil }
