package tools

import "fmt"

type dockerDriver struct{}

func newDockerDriver() Driver { return &dockerDriver{} }

func (d *dockerDriver) Name() string { return "docker" }

func (d *dockerDriver) Install() error {
	return fmt.Errorf("%w: docker install automation not configured on this host", ErrNotImplemented)
}

func (d *dockerDriver) Uninstall() error {
	return fmt.Errorf("%w: docker uninstall automation not configured on this host", ErrNotImplemented)
}

func (d *dockerDriver) IsInstalled() bool { return commandExists("docker") }

func (d *dockerDriver) IsReady() bool { return d.IsInstalled() }

func (d *dockerDriver) Version() string {
	return commandVersion("docker", "--version")
}

func (d *dockerDriver) RunDiagnostic(level string) (DiagnosticResult, error) {
	if !d.IsInstalled() {
		return DiagnosticResult{}, fmt.Errorf("docker is not installed")
	}
	return DiagnosticResult{
		Detail: "docker binary detected",
		Source: "docker",
	}, nil
}

func (d *dockerDriver) Configure(config map[string]any) error { return nil }
