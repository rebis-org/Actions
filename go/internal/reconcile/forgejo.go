package reconcile

func newForgejoClient(services services, config TargetConfig) (*giteaClient, error) {
	return newGiteaClient(services, config)
}
