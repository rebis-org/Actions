package reconcile

import (
	"context"
	"errors"
)

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

type releaseOps[T any] struct {
	pageSize int
	list     func(context.Context, string, int, int) ([]T, error)
	create   func(context.Context, string, Release) (T, error)
	update   func(context.Context, string, releaseState, Release) error
	view     func(T) releaseState
	assets   assetOps
}

type assetOps struct {
	matches func(context.Context, string, string, assetState, ReleaseAsset) (bool, error)
	replace func(context.Context, string, string, *assetState, ReleaseAsset) error
	remove  func(context.Context, string, string, string) error
}

func reconcileReleaseSet[T any](
	ctx context.Context,
	repositoryPath string,
	desired []Release,
	operations releaseOps[T],
) error {
	existing, err := collect(operations.pageSize, func(page int) ([]T, error) {
		return operations.list(ctx, repositoryPath, page, operations.pageSize)
	})
	if err != nil {
		return err
	}

	byTag := make(map[string]releaseState, len(existing))
	for _, item := range existing {
		current := operations.view(item)
		byTag[current.tag] = current
	}

	failures := make([]error, 0)

	for _, wanted := range desired {
		current, err := upsertRelease(ctx, repositoryPath, wanted, byTag, operations)
		if err != nil {
			failures = append(failures, err)

			continue
		}

		err = reconcileReleaseAssets(
			ctx,
			repositoryPath,
			current.id,
			current.assets,
			wanted.Assets,
			operations.assets,
		)
		if err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

func upsertRelease[T any](
	ctx context.Context,
	repositoryPath string,
	desired Release,
	existing map[string]releaseState,
	operations releaseOps[T],
) (releaseState, error) {
	current, exists := existing[desired.TagName]
	if exists {
		return current, operations.update(ctx, repositoryPath, current, desired)
	}

	created, err := operations.create(ctx, repositoryPath, desired)
	if err != nil {
		return releaseState{}, err
	}

	return operations.view(created), nil
}

func reconcileReleaseAssets(
	ctx context.Context,
	repositoryPath, releaseID string,
	existing []assetState,
	desired []ReleaseAsset,
	operations assetOps,
) error {
	byName := make(map[string]assetState, len(existing))
	for _, asset := range existing {
		byName[asset.name] = asset
	}

	wanted := make(map[string]bool, len(desired))
	failures := make([]error, 0)

	for _, asset := range desired {
		wanted[asset.Name] = true
		current, exists := byName[asset.Name]

		err := reconcileReleaseAsset(
			ctx,
			repositoryPath,
			releaseID,
			current,
			exists,
			asset,
			operations,
		)
		if err != nil {
			failures = append(failures, err)
		}
	}

	for name, current := range byName {
		if !wanted[name] {
			err := operations.remove(ctx, repositoryPath, releaseID, current.id)
			if err != nil {
				failures = append(failures, err)
			}
		}
	}

	return errors.Join(failures...)
}

func reconcileReleaseAsset(
	ctx context.Context,
	repositoryPath, releaseID string,
	current assetState,
	exists bool,
	desired ReleaseAsset,
	operations assetOps,
) error {
	if !exists {
		return operations.replace(ctx, repositoryPath, releaseID, nil, desired)
	}

	matches, err := operations.matches(ctx, repositoryPath, releaseID, current, desired)
	if err != nil || matches {
		return err
	}

	return operations.replace(ctx, repositoryPath, releaseID, &current, desired)
}
