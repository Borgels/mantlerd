package runtime

func NewServiceManager() ServiceManager {
	return &systemdServiceManager{}
}
