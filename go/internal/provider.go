package actions

import (
	"context"
	"log/slog"
	"path"
	"strings"
)

type provider interface {
	reconcileRoot(ctx context.Context, organization *Organization, avatar *Avatar) error
	prepareRepository(ctx context.Context, repository Repository) (string, error)
	reconcileLabels(ctx context.Context, repository Repository, labels []Label) error
	reconcileReleases(ctx context.Context, repository Repository, releases []Release) error
	reconcileArchive(ctx context.Context, repository Repository) error
	snapshot(ctx context.Context) (*platformState, error)
}

type milestoneProvider interface {
	reconcileMilestones(ctx context.Context, repository Repository, milestones []Milestone) error
}

type targetBase struct {
	platform Platform
	webURL   string
	root     string
	pageSize int
	services services
}

func newTargetBase(services services, config TargetConfig) targetBase {
	return targetBase{
		platform: config.Platform,
		webURL:   strings.TrimRight(config.URL, "/"),
		root:     config.Root,
		pageSize: config.PageSize,
		services: services,
	}
}

func (target targetBase) repositoryPath(repository Repository) string {
	return path.Join(target.root, repository.Name)
}

func (target targetBase) cloneURL(repository Repository) string {
	return target.webURL + "/" + target.repositoryPath(repository) + ".git"
}

func (target targetBase) reconcileIdentity(
	ctx context.Context,
	organization *Organization,
	avatar *Avatar,
	update func(context.Context, Organization) error,
	upload func(context.Context, Avatar) error,
) error {
	if organization != nil {
		err := update(ctx, *organization)
		if err != nil {
			return err
		}
	}

	if avatar != nil {
		err := target.services.http.retryAvatar(ctx, *avatar, upload)
		if err != nil {
			slog.Warn("cannot reconcile avatar", "target", target.platform, "error", err)
		}
	}

	return nil
}

type reconcileTarget struct {
	platform Platform
	provider provider
	git      Git
	mapping  pathMapping
	selector RepositorySelector
}

func (target reconcileTarget) prepare(
	ctx context.Context,
	repository Repository,
) (preparedTarget, error) {
	url, err := target.provider.prepareRepository(ctx, repository)
	if err != nil {
		return preparedTarget{}, err
	}

	return preparedTarget{
		provider: target.provider,
		remote: Remote{
			Name:    string(target.platform),
			URL:     url,
			Git:     target.git,
			mapping: target.mapping,
		},
	}, nil
}

type preparedTarget struct {
	provider provider
	remote   Remote
}
