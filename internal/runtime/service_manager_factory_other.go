//go:build !linux && !darwin

package runtime

import "fmt"

type unsupportedServiceManager struct{}

func NewServiceManager() ServiceManager {
	return &unsupportedServiceManager{}
}

func (m *unsupportedServiceManager) Install(name string, execStart string, env map[string]string) error {
	return fmt.Errorf("service manager unsupported on this platform")
}
func (m *unsupportedServiceManager) Start(name string) error   { return fmt.Errorf("service manager unsupported on this platform") }
func (m *unsupportedServiceManager) Stop(name string) error    { return fmt.Errorf("service manager unsupported on this platform") }
func (m *unsupportedServiceManager) Restart(name string) error { return fmt.Errorf("service manager unsupported on this platform") }
func (m *unsupportedServiceManager) IsActive(name string) (bool, error) {
	return false, fmt.Errorf("service manager unsupported on this platform")
}
func (m *unsupportedServiceManager) Uninstall(name string) error { return fmt.Errorf("service manager unsupported on this platform") }
func (m *unsupportedServiceManager) Logs(name string, lines int) (string, error) {
	return "", fmt.Errorf("service manager unsupported on this platform")
}
