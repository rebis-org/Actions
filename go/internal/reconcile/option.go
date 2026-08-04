package reconcile

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	PlatformGitHub   Platform = "github"
	PlatformCNB      Platform = "cnb"
	PlatformCodeberg Platform = "codeberg"

	defaultCNBURL        = "https://cnb.cool"
	defaultCodebergURL   = "https://codeberg.org"
	defaultConcurrency   = 2
	defaultGitName       = "reconcile"
	defaultGitEmail      = "reconcile@local"
	defaultHTTPTimeout   = time.Minute
	defaultResponseSize  = int64(64 << 10)
	defaultCNBUploadTTL  = 3600
	defaultStateFile     = "state.json"
	defaultGitHubPages   = 100
	defaultCNBPages      = 100
	defaultCodebergPages = 50
	maximumConcurrency   = 10
)

type Platform string

type Credentials struct {
	Organization string
	Repository   string
	Git          string
	Release      string
}

type TargetConfig struct {
	Platform   Platform
	URL        string
	Root       string
	GitName    string
	GitEmail   string
	Credential Credentials
}

type RepositorySelector struct {
	Include []string
	Exclude []string
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

func targetSpecifications() [2]targetSpec {
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
			pageSize:    defaultCodebergPages,
			gitUsername: func(target TargetConfig) string { return target.Credential.Git },
			connect: func(services services, config TargetConfig) (provider, error) {
				return newCodebergClient(services, config)
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
		positiveInteger("CNB_UPLOAD_TTL"),
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
	}

	for _, spec := range targetSpecifications() {
		if target := targetFromEnv(spec); target.Root != "" {
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
	if config.Concurrency == 0 {
		config.Concurrency = defaultConcurrency
	}

	if config.HTTPTimeout == 0 {
		config.HTTPTimeout = defaultHTTPTimeout
	}

	if config.ResponseSize == 0 {
		config.ResponseSize = defaultResponseSize
	}

	if config.CNBUploadTTL == 0 {
		config.CNBUploadTTL = defaultCNBUploadTTL
	}

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

	if config.CNBUploadTTL <= 0 {
		return configurationError("CNB_UPLOAD_TTL must be a positive integer")
	}

	if len(config.Targets) == 0 {
		return configurationError("configure at least one mirror target")
	}

	return nil
}

func normalizeTargets(targets []TargetConfig) ([]TargetConfig, error) {
	normalized := append([]TargetConfig(nil), targets...)
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

		if target.URL == "" {
			target.URL = spec.defaultURL
		}

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

	parsed, err := url.ParseRequestURI(target.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" {
		return configurationError(spec.environment + "_URL must be an absolute HTTP(S) URL")
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return configurationError(spec.environment + "_URL must use HTTP or HTTPS")
	}

	return nil
}

func targetSpecification(platform Platform) (targetSpec, bool) {
	for _, spec := range targetSpecifications() {
		if spec.platform == platform {
			return spec, true
		}
	}

	return targetSpec{}, false
}

func targetFromEnv(spec targetSpec) TargetConfig {
	name := spec.environment

	return TargetConfig{
		Platform: spec.platform,
		URL:      envDefault(name+"_URL", spec.defaultURL),
		Root:     strings.Trim(os.Getenv(name+"_ROOT_ORGANIZATION"), " /"),
		GitName:  os.Getenv(name + "_GIT_NAME"),
		GitEmail: os.Getenv(name + "_GIT_EMAIL"),
		Credential: Credentials{
			Organization: os.Getenv(name + "_ORG_TOKEN"),
			Repository:   os.Getenv(name + "_REPO_TOKEN"),
			Git:          os.Getenv(name + "_GIT_TOKEN"),
			Release:      os.Getenv(name + "_RELEASE_TOKEN"),
		},
	}
}

//nolint:ireturn
func envValue[T any](key string, fallback T, parse func(string) (T, error)) (T, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return fallback, nil
	}

	return parse(value)
}

func boundedInteger(name string, minimum, maximum int) func(string) (int, error) {
	return func(value string) (int, error) {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < minimum || parsed > maximum {
			return 0, configurationError(fmt.Sprintf(
				"%s must be an integer between %d and %d",
				name,
				minimum,
				maximum,
			))
		}

		return parsed, nil
	}
}

func positiveInteger(name string) func(string) (int, error) {
	return func(value string) (int, error) {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return 0, configurationError(name + " must be a positive integer")
		}

		return parsed, nil
	}
}

func positiveInteger64(name string) func(string) (int64, error) {
	return func(value string) (int64, error) {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 1 {
			return 0, configurationError(name + " must be a positive integer")
		}

		return parsed, nil
	}
}

func positiveDuration(name string) func(string) (time.Duration, error) {
	return func(value string) (time.Duration, error) {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return 0, configurationError(name + " must be a positive duration (e.g. 60s)")
		}

		return parsed, nil
	}
}

func boolean(name string) func(string) (bool, error) {
	return func(value string) (bool, error) {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return false, configurationError(name + " must be true or false")
		}

		return parsed, nil
	}
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

func environmentStateFile() string {
	if value, ok := os.LookupEnv("STATE_FILE"); ok {
		return value
	}

	return defaultStateFile
}

func repositoryList(value string) []string {
	list := make([]string, 0)

	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			list = append(list, item)
		}
	}

	return list
}

func (selector RepositorySelector) selectRepositories(repositories []Repository) []Repository {
	if len(selector.Include) == 0 && len(selector.Exclude) == 0 {
		return repositories
	}

	selected := make([]Repository, 0, len(repositories))
	for _, repository := range repositories {
		if selector.matches(repository) {
			selected = append(selected, repository)
		}
	}

	return selected
}

func (selector RepositorySelector) matches(repository Repository) bool {
	return (len(selector.Include) == 0 || matchesAny(selector.Include, repository)) &&
		!matchesAny(selector.Exclude, repository)
}

func matchesAny(patterns []string, repository Repository) bool {
	for _, pattern := range patterns {
		for _, candidate := range [...]string{repository.fullName(), repository.Name} {
			if match, _ := path.Match(pattern, candidate); match {
				return true
			}
		}
	}

	return false
}
