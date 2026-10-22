package actions

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultGitName      = "reconcile"
	defaultGitEmail     = "reconcile@local"
	defaultHTTPTimeout  = time.Minute
	defaultResponseSize = int64(64 << 10)

	maximumCNBAssetTTL  = 180
	defaultCNBUploadTTL = maximumCNBAssetTTL
)

const (
	environmentReconcileGitHubToken        = "RECONCILE_GITHUB_TOKEN"
	environmentReconcileGitHubOrganization = "RECONCILE_GITHUB_ORGANIZATION"
	environmentReconcileGitCommitMessage   = "RECONCILE_GIT_COMMIT_MESSAGE"
	environmentReconcileConcurrency        = "RECONCILE_CONCURRENCY"
	environmentReconcilePageSize           = "RECONCILE_PAGE_SIZE"
	environmentReconcileHTTPTimeout        = "RECONCILE_HTTP_TIMEOUT"
	environmentReconcileResponseSize       = "RECONCILE_RESPONSE_SIZE"
	environmentReconcileExcludeForks       = "RECONCILE_EXCLUDE_FORKS"
	environmentReconcileForce              = "RECONCILE_FORCE_RECONCILE"
	environmentReconcileRepositories       = "RECONCILE_REPOS"
	environmentReconcileExcludeRepos       = "RECONCILE_EXCLUDE_REPOS"
	environmentReconcileBypassHost         = "RECONCILE_BYPASS_HOST"
	environmentReconcileBypassToken        = "RECONCILE_BYPASS_TOKEN"
	environmentCNBUploadTTL                = "CNB_UPLOAD_TTL"
)

type Config struct {
	GitHubToken        string
	GitHubOrganization string
	GitMessage         string
	Targets            []TargetConfig
	RepositorySelector RepositorySelector
	StateFile          string
	PageSize           int
	Concurrency        int
	HTTPTimeout        time.Duration
	ResponseSize       int64
	CNBUploadTTL       int
	ExcludeForks       bool
	Force              bool
	Bypass             Bypass
	HTTPClient         *http.Client
	Now                func() time.Time
}

func ConfigFromEnv() (Config, error) {
	concurrency, concurrencyErr := envValue(environmentReconcileConcurrency, strconv.Atoi)
	pageSize, pageSizeErr := envValue(environmentReconcilePageSize, strconv.Atoi)
	timeout, timeoutErr := envValue(environmentReconcileHTTPTimeout, time.ParseDuration)
	responseSize, responseSizeErr := envValue(environmentReconcileResponseSize, parseInt64)
	uploadTTL, uploadTTLErr := envValue(environmentCNBUploadTTL, strconv.Atoi)
	excludeForks, excludeForksErr := envValue(environmentReconcileExcludeForks, strconv.ParseBool)
	force, forceErr := envValue(environmentReconcileForce, strconv.ParseBool)

	err := errors.Join(
		concurrencyErr,
		pageSizeErr,
		timeoutErr,
		responseSizeErr,
		uploadTTLErr,
		excludeForksErr,
		forceErr,
	)
	if err != nil {
		return Config{}, err
	}

	config := Config{
		GitHubToken:        os.Getenv(environmentReconcileGitHubToken),
		GitHubOrganization: strings.Trim(os.Getenv(environmentReconcileGitHubOrganization), " /\n"),
		GitMessage:         os.Getenv(environmentReconcileGitCommitMessage),
		RepositorySelector: RepositorySelector{
			Include: repositoryList(os.Getenv(environmentReconcileRepositories)),
			Exclude: repositoryList(os.Getenv(environmentReconcileExcludeRepos)),
		},
		StateFile:    stateFileFromEnv(),
		PageSize:     pageSize,
		Concurrency:  concurrency,
		HTTPTimeout:  timeout,
		ResponseSize: responseSize,
		CNBUploadTTL: uploadTTL,
		ExcludeForks: excludeForks,
		Force:        force,
		Bypass: Bypass{
			Host:  os.Getenv(environmentReconcileBypassHost),
			Token: os.Getenv(environmentReconcileBypassToken),
		},
	}

	for _, spec := range targetSpecifications() {
		if target := targetFromEnv(spec, config.RepositorySelector); target.Root != "" {
			config.Targets = append(config.Targets, target)
		}
	}

	err = config.normalize()
	if err != nil {
		return Config{}, err
	}

	return config, nil
}

func (config *Config) normalize() error {
	concurrency, err := boundedConcurrency(config.Concurrency, environmentReconcileConcurrency)
	if err != nil {
		return err
	}

	config.Concurrency = concurrency
	config.HTTPTimeout = cmp.Or(config.HTTPTimeout, defaultHTTPTimeout)
	config.ResponseSize = cmp.Or(config.ResponseSize, defaultResponseSize)
	config.CNBUploadTTL = cmp.Or(config.CNBUploadTTL, defaultCNBUploadTTL)

	if config.Now == nil {
		config.Now = time.Now
	}

	err = config.validate()
	if err != nil {
		return err
	}

	targets, err := normalizeTargets(config.Targets, config.PageSize)
	if err != nil {
		return err
	}

	config.Targets = targets

	return nil
}

func (config *Config) validate() error {
	switch {
	case config.GitHubToken == "":
		return configurationError(environmentReconcileGitHubToken + " is required")
	case len(config.Targets) == 0:
		return configurationError("configure at least one mirror target")
	}

	return config.validateLimits()
}

func (config *Config) validateLimits() error {
	switch {
	case config.PageSize < 0:
		return configurationError(environmentReconcilePageSize + " must be a positive integer")
	case config.HTTPTimeout <= 0:
		return configurationError(
			environmentReconcileHTTPTimeout + " must be a positive duration (e.g. 60s)",
		)
	case config.ResponseSize <= 0:
		return configurationError(environmentReconcileResponseSize + " must be a positive integer")
	case config.CNBUploadTTL < 1 || config.CNBUploadTTL > maximumCNBAssetTTL:
		return configurationError(fmt.Sprintf(
			"%s must be an integer between 1 and %d (asset retention days)",
			environmentCNBUploadTTL,
			maximumCNBAssetTTL,
		))
	case (config.Bypass.Host == "") != (config.Bypass.Token == ""):
		return configurationError(
			environmentReconcileBypassHost + " and " + environmentReconcileBypassToken +
				" must be configured together",
		)
	}

	return nil
}

type reconciler struct {
	config   Config
	services services
	source   *githubSource
	git      Git
	targets  []reconcileTarget
}

func Run(ctx context.Context, config Config) error {
	run, err := newReconciler(config)
	if err != nil {
		return err
	}

	return run.reconcile(ctx)
}

func newReconciler(config Config) (*reconciler, error) {
	err := config.normalize()
	if err != nil {
		return nil, err
	}

	services := newServices(config)

	source, err := newGitHubSource(
		services.http,
		cmp.Or(config.PageSize, defaultGitHubPages),
		config.GitHubToken,
		config.GitHubOrganization,
	)
	if err != nil {
		return nil, err
	}

	run := &reconciler{
		config:   config,
		services: services,
		source:   source,
		git: newGit(
			gitIdentity{name: defaultGitName, email: defaultGitEmail},
			config.GitMessage,
			gitCredential{username: gitHubUsername, token: config.GitHubToken},
			config.Bypass,
			config.Force,
			config.Now,
		),
	}

	for _, target := range config.Targets {
		client, err := target.connect(services, target)
		if err != nil {
			return nil, err
		}

		run.targets = append(run.targets, reconcileTarget{
			platform: target.Platform,
			provider: client,
			selector: target.RepositorySelector,
			mapping:  target.mapping,
			git: newGit(
				gitIdentity{name: target.GitName, email: target.GitEmail},
				config.GitMessage,
				gitCredential{username: target.gitUsername, token: target.Credential.Git},
				config.Bypass,
				config.Force,
				config.Now,
			),
		})
	}

	return run, nil
}

func (run *reconciler) reconcile(ctx context.Context) error {
	repositories, err := run.source.repositories(ctx, run.config.ExcludeForks)
	if err != nil {
		slog.Error("cannot list repositories, skipping run", "error", err)

		return cancellation(ctx)
	}

	organization, avatar := run.organization(ctx)

	err = each(ctx, len(run.targets), run.targets, func(target reconcileTarget) error {
		return target.provider.reconcileRoot(ctx, organization, avatar)
	})
	if err != nil {
		slog.Warn("cannot reconcile target roots, continuing", "error", err)
	}

	repositories = run.selectedRepositories(repositories)

	slog.Info(
		"reconciling repositories",
		"repositories", len(repositories),
		"targets", len(run.targets),
	)

	err = each(ctx, run.config.Concurrency, repositories, func(repository Repository) error {
		return run.repository(ctx, repository)
	})
	if err != nil {
		slog.Warn("some repositories failed, continuing", "error", err)
	}

	recordState(ctx, run.config, organization, repositories, run.targets)

	return cancellation(ctx)
}

func (run *reconciler) selectedRepositories(repositories []Repository) []Repository {
	selected := make([]Repository, 0, len(repositories))

	for _, repository := range repositories {
		if len(run.targetsFor(repository)) > 0 {
			selected = append(selected, repository)
		}
	}

	return selected
}

func (run *reconciler) targetsFor(repository Repository) []reconcileTarget {
	targets := make([]reconcileTarget, 0, len(run.targets))

	for _, target := range run.targets {
		if target.selector.matches(repository) {
			targets = append(targets, target)
		}
	}

	return targets
}

func (run *reconciler) organization(ctx context.Context) (*Organization, *Avatar) {
	if run.config.GitHubOrganization == "" {
		return nil, nil
	}

	organization, err := run.source.organization(ctx, run.config.GitHubOrganization)
	if err != nil {
		slog.Warn("cannot read organization, continuing without it", "error", err)

		return nil, nil
	}

	if organization.AvatarURL == "" {
		return &organization, nil
	}

	avatar, err := run.services.http.downloadAvatar(ctx, organization.AvatarURL)
	if err != nil {
		slog.Warn("cannot download organization avatar", "error", err)

		return &organization, nil
	}

	return &organization, &avatar
}

func (run *reconciler) repository(ctx context.Context, repository Repository) error {
	targets := run.targetsFor(repository)

	prepared, preparationErr := parallel(
		ctx,
		len(targets),
		targets,
		func(target reconcileTarget) (preparedTarget, error) {
			return target.prepare(ctx, repository)
		},
	)

	remotes := transform(prepared, func(target preparedTarget) Remote {
		return target.remote
	})

	stages := []func() error{
		func() error { return run.publish(ctx, repository, remotes) },
		func() error { return run.labels(ctx, repository, prepared) },
		func() error { return run.milestones(ctx, repository, prepared) },
		func() error { return run.releases(ctx, repository, prepared) },
	}

	writeErr := errors.Join(
		preparationErr,
		each(ctx, len(stages), stages, func(execute func() error) error {
			return execute()
		}),
	)
	if writeErr == nil {
		writeErr = each(ctx, len(prepared), prepared, func(target preparedTarget) error {
			return target.provider.reconcileArchive(ctx, repository)
		})
	}

	err := writeErr
	if err == nil {
		slog.Info("reconciled repository", "repository", repository.fullName())

		return nil
	}

	slog.Warn(
		"cannot reconcile repository, skipping",
		"repository", repository.fullName(),
		"error", err,
	)

	return err
}

func (run *reconciler) publish(ctx context.Context, repository Repository, remotes []Remote) error {
	if len(remotes) == 0 {
		return nil
	}

	return run.git.reconcile(ctx, repository, remotes)
}

func (run *reconciler) labels(
	ctx context.Context,
	repository Repository,
	targets []preparedTarget,
) error {
	if len(targets) == 0 {
		return nil
	}

	labels, err := run.source.labels(ctx, repository)
	if err != nil {
		return fmt.Errorf("read labels %s: %w", repository.fullName(), err)
	}

	return each(ctx, len(targets), targets, func(target preparedTarget) error {
		return target.provider.reconcileLabels(ctx, repository, labels)
	})
}

func (run *reconciler) milestones(
	ctx context.Context,
	repository Repository,
	targets []preparedTarget,
) error {
	supported := make([]milestoneProvider, 0, len(targets))

	for _, target := range targets {
		if provider, ok := target.provider.(milestoneProvider); ok {
			supported = append(supported, provider)
		}
	}

	if len(supported) == 0 {
		return nil
	}

	milestones, err := run.source.milestones(ctx, repository)
	if err != nil {
		return fmt.Errorf("read milestones %s: %w", repository.fullName(), err)
	}

	return each(ctx, len(supported), supported, func(provider milestoneProvider) error {
		return provider.reconcileMilestones(ctx, repository, milestones)
	})
}

func (run *reconciler) releases(
	ctx context.Context,
	repository Repository,
	targets []preparedTarget,
) error {
	if len(targets) == 0 {
		return nil
	}

	releases, err := run.source.releases(ctx, repository)
	if err != nil {
		return fmt.Errorf("read releases %s: %w", repository.fullName(), err)
	}
	defer closeReleaseAssets(releases)

	if len(releases) == 0 {
		return nil
	}

	return each(ctx, len(targets), targets, func(target preparedTarget) error {
		return target.provider.reconcileReleases(ctx, repository, releases)
	})
}
