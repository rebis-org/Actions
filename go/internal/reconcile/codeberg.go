package reconcile

func newCodebergClient(services services, config TargetConfig) (*giteaClient, error) {
	return newGiteaClient(services, config)
}
