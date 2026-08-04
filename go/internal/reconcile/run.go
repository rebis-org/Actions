package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
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

	source, err := newGitHubSource(services, config.GitHubToken)
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
			git: newGit(
				target.GitName,
				target.GitEmail,
				config.GitMessage,
				spec.gitUsername(target),
				target.Credential.Git,
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

	repositories = run.config.RepositorySelector.selectRepositories(repositories)

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
	prepared, preparationErr := parallel(
		ctx,
		len(run.targets),
		run.targets,
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

	run.metadata(ctx, repository, prepared)
	run.releases(ctx, repository, prepared)

	err := errors.Join(preparationErr, mirrorErr)
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

type namedOperations[D, E any] struct {
	desiredName  func(D) string
	existingName func(E) string
	equal        func(D, E) bool
	create       func(D) error
	update       func(D, E) error
	remove       func(E) error
}

func reconcileNamed[D, E any](desired []D, existing []E, operations namedOperations[D, E]) error {
	byName := make(map[string]E, len(existing))
	for _, item := range existing {
		byName[metadataKey(operations.existingName(item))] = item
	}

	wanted := make(map[string]bool, len(desired))
	failures := make([]error, 0)

	for _, item := range desired {
		key := metadataKey(operations.desiredName(item))
		wanted[key] = true

		current, found := byName[key]
		if !found {
			failures = append(failures, operations.create(item))
		} else if !operations.equal(item, current) {
			failures = append(failures, operations.update(item, current))
		}
	}

	err := errors.Join(failures...)
	if err != nil {
		return err
	}

	failures = failures[:0]

	for _, item := range existing {
		if !wanted[metadataKey(operations.existingName(item))] {
			failures = append(failures, operations.remove(item))
		}
	}

	return errors.Join(failures...)
}

func metadataKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func sameColor(left, right string) bool {
	return strings.EqualFold(strings.TrimPrefix(left, "#"), strings.TrimPrefix(right, "#"))
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
	existing, err := releasePages(ctx, repositoryPath, operations)
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

func releasePages[T any](
	ctx context.Context,
	repositoryPath string,
	operations releaseOps[T],
) ([]T, error) {
	all := make([]T, 0)
	err := paginate(operations.pageSize, func(page int) ([]T, error) {
		return operations.list(ctx, repositoryPath, page, operations.pageSize)
	}, func(item T) error {
		all = append(all, item)

		return nil
	})

	return all, err
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

type outcome[T any] struct {
	value T
	err   error
}

func parallel[I, O any](
	ctx context.Context,
	limit int,
	inputs []I,
	run func(I) (O, error),
) ([]O, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	results := make([]outcome[O], len(inputs))

	jobs := make(chan int, len(inputs))
	for index := range inputs {
		jobs <- index
	}

	close(jobs)

	var workers sync.WaitGroup
	for range max(1, min(limit, len(inputs))) {
		workers.Go(func() {
			for index := range jobs {
				err := ctx.Err()
				if err != nil {
					results[index].err = err

					continue
				}

				results[index].value, results[index].err = run(inputs[index])
			}
		})
	}

	workers.Wait()

	values := make([]O, 0, len(inputs))
	failures := make([]error, 0)

	for _, result := range results {
		if result.err != nil {
			failures = append(failures, result.err)
		} else {
			values = append(values, result.value)
		}
	}

	return values, errors.Join(failures...)
}
