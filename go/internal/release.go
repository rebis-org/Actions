package actions

import "context"

type releaseState struct {
	id     string
	tag    string
	name   string
	body   string
	target string
	draft  bool
	pre    bool
	latest bool
	assets []assetState
}

type assetState struct {
	id   string
	name string
	size int64
	hash string
}

type releaseOps struct {
	pageSize int
	limit    int
	slots    chan struct{}
	list     func(context.Context, string, int, int) ([]releaseState, error)
	create   func(context.Context, string, Release) (releaseState, error)
	update   func(context.Context, string, releaseState, Release) error
	assets   assetOps
}

type assetOps struct {
	matches func(context.Context, string, string, assetState, ReleaseAsset) (bool, error)
	replace func(context.Context, string, string, *assetState, ReleaseAsset) error
	remove  func(context.Context, string, string, string) error
}

func reconcileReleaseSet(
	ctx context.Context,
	repositoryPath string,
	desired []Release,
	operations releaseOps,
) error {
	existing, err := collect(operations.pageSize, func(page int) ([]releaseState, error) {
		return operations.list(ctx, repositoryPath, page, operations.pageSize)
	})
	if err != nil {
		return err
	}

	operations.slots = make(chan struct{}, max(1, operations.limit))

	return converge(ctx, desired, existing, convergence[Release, releaseState]{
		desiredKey:  func(release Release) string { return release.TagName },
		existingKey: func(release releaseState) string { return release.tag },
		create: func(release Release) (releaseState, error) {
			return operations.create(ctx, repositoryPath, release)
		},
		update: func(release Release, current releaseState) error {
			return operations.update(ctx, repositoryPath, current, release)
		},
		then: func(release Release, current releaseState) error {
			return reconcileReleaseAssets(
				ctx,
				repositoryPath,
				current.id,
				current.assets,
				release.Assets,
				operations,
			)
		},
		slots: operations.slots,
		limit: operations.limit,
	})
}

func reconcileReleaseAssets(
	ctx context.Context,
	repositoryPath, releaseID string,
	existing []assetState,
	desired []ReleaseAsset,
	operations releaseOps,
) error {
	return converge(ctx, desired, existing, convergence[ReleaseAsset, assetState]{
		desiredKey:  func(asset ReleaseAsset) string { return asset.Name },
		existingKey: func(asset assetState) string { return asset.name },
		equal: func(desired ReleaseAsset, current assetState) (bool, error) {
			return operations.assets.matches(ctx, repositoryPath, releaseID, current, desired)
		},
		create: func(desired ReleaseAsset) (assetState, error) {
			err := operations.assets.replace(ctx, repositoryPath, releaseID, nil, desired)

			return assetState{}, err
		},
		update: func(desired ReleaseAsset, current assetState) error {
			return operations.assets.replace(ctx, repositoryPath, releaseID, &current, desired)
		},
		remove: func(current assetState) error {
			return operations.assets.remove(ctx, repositoryPath, releaseID, current.id)
		},
		slots:         operations.slots,
		removeOnError: true,
		limit:         operations.limit,
	})
}
