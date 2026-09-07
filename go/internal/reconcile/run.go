package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
)

const (
	fieldName        = "name"
	fieldDescription = "description"
	fieldTopics      = "topics"
	fieldArchived    = "archived"
	fieldEmail       = "email"
	fieldPath        = "path"
	fieldFullName    = "full_name"
	fieldWebsite     = "website"
	valueAll         = "all"
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
	failure  platformError
	webURL   string
	root     string
	services services
}

func newTargetBase(services services, config TargetConfig) targetBase {
	return targetBase{
		platform: config.Platform,
		failure:  platformError(config.Platform),
		webURL:   strings.TrimRight(config.URL, "/"),
		root:     config.Root,
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

type mirrorTarget struct {
	platform Platform
	provider provider
	git      Git
	selector RepositorySelector
}

func (target mirrorTarget) prepare(
	ctx context.Context,
	repository Repository,
) (preparedTarget, error) {
	url, err := target.provider.prepareRepository(ctx, repository)
	if err != nil {
		return preparedTarget{}, err
	}

	return preparedTarget{
		provider: target.provider,
		remote:   Remote{Name: string(target.platform), URL: url, Git: target.git},
	}, nil
}

type preparedTarget struct {
	provider provider
	remote   Remote
}

type reconciler struct {
	config   Config
	services services
	source   *githubSource
	git      Git
	targets  []mirrorTarget
}

func Run(ctx context.Context, config Config) error {
	run, err := newReconciler(config)
	if err != nil {
		return err
	}

	return run.reconcile(ctx)
}

func newReconciler(config Config) (*reconciler, error) {
	config, err := config.normalize()
	if err != nil {
		return nil, err
	}

	services := newServices(config)

	source, err := newGitHubSource(services, config.GitHubToken, config.GitHubOrganization)
	if err != nil {
		return nil, err
	}

	run := &reconciler{
		config:   config,
		services: services,
		source:   source,
		git: newGit(
			defaultGitName,
			defaultGitEmail,
			config.GitMessage,
			"x-access-token",
			config.GitHubToken,
			config.Bypass,
			config.Force,
			services.now,
		),
		targets: make([]mirrorTarget, 0, len(config.Targets)),
	}
	for _, target := range config.Targets {
		spec, _ := targetSpecification(target.Platform)

		client, err := spec.connect(services, target)
		if err != nil {
			return nil, fmt.Errorf("configure %s client: %w", spec.environment, err)
		}

		run.targets = append(run.targets, mirrorTarget{
			platform: target.Platform,
			provider: client,
			selector: target.RepositorySelector,
			git: newGit(
				target.GitName,
				target.GitEmail,
				config.GitMessage,
				spec.gitUsername(target),
				target.Credential.Git,
				config.Bypass,
				config.Force,
				services.now,
			),
		})
	}

	return run, nil
}

func (run *reconciler) reconcile(ctx context.Context) error {
	repositories, err := run.source.repositories(ctx, run.config.ExcludeForks)
	if err != nil {
		return err
	}

	organization, avatar, err := run.organization(ctx)
	if err != nil {
		return err
	}

	_, err = parallel(
		ctx,
		len(run.targets),
		run.targets,
		func(target mirrorTarget) (struct{}, error) {
			return struct{}{}, target.provider.reconcileRoot(ctx, organization, avatar)
		},
	)
	if err != nil {
		return err
	}

	repositories = run.selectedRepositories(repositories)

	slog.Info(
		"reconciling repositories",
		"repositories", len(repositories),
		"targets", len(run.targets),
	)

	_, err = parallel(
		ctx,
		run.config.Concurrency,
		repositories,
		func(repository Repository) (struct{}, error) {
			return struct{}{}, run.repository(ctx, repository)
		},
	)
	if err != nil {
		return err
	}

	recordState(ctx, run.config, organization, repositories, run.targets)

	return nil
}

func (run *reconciler) selectedRepositories(repositories []Repository) []Repository {
	selected := make([]Repository, 0, len(repositories))

	for _, repository := range repositories {
		for _, target := range run.targets {
			if target.selector.matches(repository) {
				selected = append(selected, repository)

				break
			}
		}
	}

	return selected
}

func (run *reconciler) organization(ctx context.Context) (*Organization, *Avatar, error) {
	if run.config.GitHubOrganization == "" {
		return nil, nil, nil
	}

	organization, err := run.source.organization(ctx, run.config.GitHubOrganization)
	if err != nil {
		return nil, nil, err
	}

	if organization.AvatarURL == "" {
		return &organization, nil, nil
	}

	avatar, err := run.services.http.downloadAvatar(ctx, organization.AvatarURL)
	if err != nil {
		slog.Warn("cannot download organization avatar", "error", err)

		return &organization, nil, nil
	}

	return &organization, &avatar, nil
}

func (run *reconciler) repository(ctx context.Context, repository Repository) error {
	targets := make([]mirrorTarget, 0, len(run.targets))
	for _, target := range run.targets {
		if target.selector.matches(repository) {
			targets = append(targets, target)
		}
	}

	prepared, preparationErr := parallel(
		ctx,
		len(targets),
		targets,
		func(target mirrorTarget) (preparedTarget, error) {
			return target.prepare(ctx, repository)
		},
	)

	remotes := make([]Remote, len(prepared))
	for index := range prepared {
		remotes[index] = prepared[index].remote
	}

	var mirrorErr error
	if len(remotes) > 0 {
		mirrorErr = run.git.reconcile(ctx, repository, remotes)
	}

	_, archiveErr := parallel(
		ctx,
		len(prepared),
		prepared,
		func(target preparedTarget) (struct{}, error) {
			return struct{}{}, target.provider.reconcileArchive(ctx, repository)
		},
	)

	run.metadata(ctx, repository, prepared)
	run.releases(ctx, repository, prepared)

	err := errors.Join(preparationErr, mirrorErr, archiveErr)
	if err == nil {
		slog.Info("reconciled repository", "repository", repository.fullName())
	}

	return err
}

func (run *reconciler) metadata(
	ctx context.Context,
	repository Repository,
	targets []preparedTarget,
) {
	if len(targets) == 0 {
		return
	}

	labels, err := run.source.labels(ctx, repository)
	if err != nil {
		slog.Warn("cannot read labels", "repository", repository.fullName(), "error", err)
	} else {
		_, err = parallel(
			ctx,
			len(targets),
			targets,
			func(target preparedTarget) (struct{}, error) {
				return struct{}{}, target.provider.reconcileLabels(ctx, repository, labels)
			},
		)
		if err != nil {
			slog.Warn("cannot reconcile labels", "repository", repository.fullName(), "error", err)
		}
	}

	supported := make([]milestoneProvider, 0, len(targets))
	for _, target := range targets {
		if provider, ok := target.provider.(milestoneProvider); ok {
			supported = append(supported, provider)
		}
	}

	if len(supported) == 0 {
		return
	}

	milestones, err := run.source.milestones(ctx, repository)
	if err != nil {
		slog.Warn("cannot read milestones", "repository", repository.fullName(), "error", err)

		return
	}

	_, err = parallel(ctx, len(supported), supported, func(
		provider milestoneProvider,
	) (struct{}, error) {
		return struct{}{}, provider.reconcileMilestones(ctx, repository, milestones)
	})
	if err != nil {
		slog.Warn("cannot reconcile milestones", "repository", repository.fullName(), "error", err)
	}
}

func (run *reconciler) releases(
	ctx context.Context,
	repository Repository,
	targets []preparedTarget,
) {
	releases, err := run.source.releases(ctx, repository)
	if err != nil {
		slog.Warn("cannot read releases", "repository", repository.fullName(), "error", err)

		return
	}
	defer closeReleaseAssets(releases)

	if len(releases) == 0 {
		return
	}

	_, err = parallel(ctx, len(targets), targets, func(target preparedTarget) (struct{}, error) {
		return struct{}{}, target.provider.reconcileReleases(ctx, repository, releases)
	})
	if err != nil {
		slog.Warn("cannot reconcile releases", "repository", repository.fullName(), "error", err)
	}
}
