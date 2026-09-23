package reconcile

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	PlatformGitHub   Platform = "github"
	PlatformCNB      Platform = "cnb"
	PlatformCodeberg Platform = "codeberg"
	PlatformForgejo  Platform = "forgejo"

	defaultCNBURL       = "https://cnb.cool"
	defaultCodebergURL  = "https://codeberg.org"
	defaultConcurrency  = 2
	defaultGitName      = "reconcile"
	defaultGitEmail     = "reconcile@local"
	defaultHTTPTimeout  = time.Minute
	defaultResponseSize = int64(64 << 10)
	defaultStateFile    = "state.json"
	defaultGitHubPages  = 100
	defaultCNBPages     = 100
	defaultGiteaPages   = 50
	maximumConcurrency  = 10

	maximumCNBAssetTTL  = 180
	defaultCNBUploadTTL = maximumCNBAssetTTL

	bypassHeader = "X-Reconcile-Bypass"
)

type Platform string

type Credentials struct {
	Organization string
	Repository   string
	Git          string
	GitUsername  string
	Release      string
}

type Bypass struct {
	Host  string
	Token string
}

func (bypass Bypass) matches(endpoint string) bool {
	if bypass.Host == "" || bypass.Token == "" || endpoint == "" {
		return false
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}

	return strings.EqualFold(parsed.Hostname(), bypass.Host)
}

type TargetConfig struct {
	Platform           Platform
	URL                string
	Root               string
	GitName            string
	GitEmail           string
	Credential         Credentials
	RepositorySelector RepositorySelector
}

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

type targetSpec struct {
	platform    Platform
	environment string
	defaultURL  string
	pageSize    int
	gitUsername func(TargetConfig) string
	connect     func(services, TargetConfig) (provider, error)
}

type configurationError string

func (err configurationError) Error() string {
	return string(err)
}

func targetSpecifications() [3]targetSpec {
	return [...]targetSpec{
		{
			platform:    PlatformCNB,
			environment: "CNB",
			defaultURL:  defaultCNBURL,
			pageSize:    defaultCNBPages,
			gitUsername: func(TargetConfig) string { return "cnb" },
			connect: func(services services, config TargetConfig) (provider, error) {
				return newCNBClient(services, config)
			},
		},
		{
			platform:    PlatformCodeberg,
			environment: "CODEBERG",
			defaultURL:  defaultCodebergURL,
			pageSize:    defaultGiteaPages,
			gitUsername: func(target TargetConfig) string { return target.Credential.Git },
			connect: func(services services, config TargetConfig) (provider, error) {
				return newCodebergClient(services, config)
			},
		},
		{
			platform:    PlatformForgejo,
			environment: "FORGEJO",
			pageSize:    defaultGiteaPages,
			gitUsername: func(target TargetConfig) string { return target.Credential.GitUsername },
			connect: func(services services, config TargetConfig) (provider, error) {
				return newForgejoClient(services, config)
			},
		},
	}
}

func ConfigFromEnv() (Config, error) {
	concurrency, concurrencyErr := envValue(
		"RECONCILE_CONCURRENCY",
		defaultConcurrency,
		boundedInteger("RECONCILE_CONCURRENCY", 1, maximumConcurrency),
	)
	pageSize, pageErr := envValue(
		"RECONCILE_PAGE_SIZE",
		0,
		positiveInteger("RECONCILE_PAGE_SIZE"),
	)
	timeout, timeoutErr := envValue(
		"RECONCILE_HTTP_TIMEOUT",
		defaultHTTPTimeout,
		positiveDuration("RECONCILE_HTTP_TIMEOUT"),
	)
	responseSize, responseErr := envValue(
		"RECONCILE_RESPONSE_SIZE",
		defaultResponseSize,
		positiveInteger64("RECONCILE_RESPONSE_SIZE"),
	)
	uploadTTL, uploadErr := envValue(
		"CNB_UPLOAD_TTL",
		defaultCNBUploadTTL,
		boundedInteger("CNB_UPLOAD_TTL", 1, maximumCNBAssetTTL),
	)
	excludeForks, excludeErr := envValue(
		"RECONCILE_EXCLUDE_FORKS",
		false,
		boolean("RECONCILE_EXCLUDE_FORKS"),
	)

	force, forceErr := envValue(
		"RECONCILE_FORCE_RECONCILE",
		false,
		boolean("RECONCILE_FORCE_RECONCILE"),
	)

	err := errors.Join(
		concurrencyErr,
		pageErr,
		timeoutErr,
		responseErr,
		uploadErr,
		excludeErr,
		forceErr,
	)
	if err != nil {
		return Config{}, err
	}

	config := Config{
		GitHubToken:        os.Getenv("RECONCILE_GITHUB_TOKEN"),
		GitHubOrganization: strings.Trim(os.Getenv("RECONCILE_GITHUB_ORGANIZATION"), " /\n"),
		GitMessage:         os.Getenv("RECONCILE_GIT_COMMIT_MESSAGE"),
		RepositorySelector: RepositorySelector{
			Include: repositoryList(os.Getenv("RECONCILE_REPOS")),
			Exclude: repositoryList(os.Getenv("RECONCILE_EXCLUDE_REPOS")),
		},
		StateFile:    environmentStateFile(),
		PageSize:     pageSize,
		Concurrency:  concurrency,
		HTTPTimeout:  timeout,
		ResponseSize: responseSize,
		CNBUploadTTL: uploadTTL,
		ExcludeForks: excludeForks,
		Force:        force,
		Bypass: Bypass{
			Host:  os.Getenv("RECONCILE_BYPASS_HOST"),
			Token: os.Getenv("RECONCILE_BYPASS_TOKEN"),
		},
	}

	for _, spec := range targetSpecifications() {
		if target := targetFromEnv(spec, config.RepositorySelector); target.Root != "" {
			config.Targets = append(config.Targets, target)
		}
	}

	return config.normalize()
}

func (config Config) normalize() (Config, error) {
	config = config.withDefaults()

	err := config.validateRuntime()
	if err != nil {
		return Config{}, err
	}

	targets, err := normalizeTargets(config.Targets)
	if err != nil {
		return Config{}, err
	}

	config.Targets = targets

	return config, nil
}

func (config Config) withDefaults() Config {
	config.Concurrency = cmp.Or(config.Concurrency, defaultConcurrency)
	config.HTTPTimeout = cmp.Or(config.HTTPTimeout, defaultHTTPTimeout)
	config.ResponseSize = cmp.Or(config.ResponseSize, defaultResponseSize)
	config.CNBUploadTTL = cmp.Or(config.CNBUploadTTL, defaultCNBUploadTTL)

	if config.Now == nil {
		config.Now = time.Now
	}

	return config
}

func (config Config) validateRuntime() error {
	if config.GitHubToken == "" {
		return configurationError("RECONCILE_GITHUB_TOKEN is required")
	}

	if config.Concurrency < 1 || config.Concurrency > maximumConcurrency {
		return configurationError("RECONCILE_CONCURRENCY must be an integer between 1 and 10")
	}

	if config.PageSize < 0 {
		return configurationError("RECONCILE_PAGE_SIZE must be a positive integer")
	}

	if config.HTTPTimeout <= 0 {
		return configurationError("RECONCILE_HTTP_TIMEOUT must be a positive duration (e.g. 60s)")
	}

	if config.ResponseSize <= 0 {
		return configurationError("RECONCILE_RESPONSE_SIZE must be a positive integer")
	}

	if config.CNBUploadTTL < 1 || config.CNBUploadTTL > maximumCNBAssetTTL {
		return configurationError(fmt.Sprintf(
			"CNB_UPLOAD_TTL must be an integer between 1 and %d (asset retention days)",
			maximumCNBAssetTTL,
		))
	}

	if (config.Bypass.Host == "") != (config.Bypass.Token == "") {
		return configurationError("RECONCILE_BYPASS_HOST and RECONCILE_BYPASS_TOKEN must be configured together")
	}

	if len(config.Targets) == 0 {
		return configurationError("configure at least one mirror target")
	}

	return nil
}

func normalizeTargets(targets []TargetConfig) ([]TargetConfig, error) {
	normalized := slices.Clone(targets)
	seen := make(map[Platform]bool, len(normalized))

	for index := range normalized {
		target := &normalized[index]

		spec, ok := targetSpecification(target.Platform)
		if !ok {
			return nil, configurationError(fmt.Sprintf(
				"unsupported mirror target %q",
				target.Platform,
			))
		}

		if seen[target.Platform] {
			return nil, configurationError(fmt.Sprintf(
				"mirror target %q is configured more than once",
				target.Platform,
			))
		}

		seen[target.Platform] = true

		target.URL = cmp.Or(target.URL, spec.defaultURL)

		err := target.validate(spec)
		if err != nil {
			return nil, err
		}
	}

	return normalized, nil
}

func (target TargetConfig) validate(spec targetSpec) error {
	for _, value := range [...]string{
		target.Root,
		target.GitName,
		target.GitEmail,
		target.Credential.Organization,
		target.Credential.Repository,
		target.Credential.Git,
		target.Credential.Release,
	} {
		if value != "" {
			continue
		}

		return configurationError(fmt.Sprintf(
			"%s_ROOT_ORGANIZATION with %s_ORG_TOKEN, %s_REPO_TOKEN, %s_GIT_TOKEN, %s_RELEASE_TOKEN, %s_GIT_NAME and %s_GIT_EMAIL are required",
			spec.environment,
			spec.environment,
			spec.environment,
			spec.environment,
			spec.environment,
			spec.environment,
			spec.environment,
		))
	}

	err := target.validateURL(spec.environment)
	if err != nil {
		return err
	}

	if spec.gitUsername(target) == "" {
		return configurationError(fmt.Sprintf(
			"%s git username cannot be resolved: configure %s_GIT_USERNAME or %s_GIT_TOKEN",
			spec.environment,
			spec.environment,
			spec.environment,
		))
	}

	return nil
}

func (target TargetConfig) validateURL(environment string) error {
	parsed, err := url.ParseRequestURI(target.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" {
		return configurationError(environment + "_URL must be an absolute HTTP(S) URL")
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return configurationError(environment + "_URL must use HTTP or HTTPS")
	}

	return nil
}

func targetSpecification(platform Platform) (targetSpec, bool) {
	specs := targetSpecifications()

	index := slices.IndexFunc(specs[:], func(spec targetSpec) bool {
		return spec.platform == platform
	})
	if index < 0 {
		return targetSpec{}, false
	}

	return specs[index], true
}

func targetFromEnv(spec targetSpec, fallback RepositorySelector) TargetConfig {
	name := spec.environment

	return TargetConfig{
		Platform:           spec.platform,
		URL:                envDefault(name+"_URL", spec.defaultURL),
		Root:               strings.Trim(os.Getenv(name+"_ROOT_ORGANIZATION"), " /"),
		GitName:            os.Getenv(name + "_GIT_NAME"),
		GitEmail:           os.Getenv(name + "_GIT_EMAIL"),
		RepositorySelector: repositorySelectorFromEnv(name, fallback),
		Credential: Credentials{
			Organization: os.Getenv(name + "_ORG_TOKEN"),
			Repository:   os.Getenv(name + "_REPO_TOKEN"),
			Git:          os.Getenv(name + "_GIT_TOKEN"),
			GitUsername:  os.Getenv(name + "_GIT_USERNAME"),
			Release:      os.Getenv(name + "_RELEASE_TOKEN"),
		},
	}
}
