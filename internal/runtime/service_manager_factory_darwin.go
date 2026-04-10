package runtime

func NewServiceManager() ServiceManager {
	return &launchdServiceManager{}
}
