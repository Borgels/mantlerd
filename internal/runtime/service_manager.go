package runtime

// ServiceManager abstracts service lifecycle operations across OSes.
type ServiceManager interface {
	Install(name string, execStart string, env map[string]string) error
	Start(name string) error
	Stop(name string) error
	Restart(name string) error
	IsActive(name string) (bool, error)
	Uninstall(name string) error
	Logs(name string, lines int) (string, error)
}
