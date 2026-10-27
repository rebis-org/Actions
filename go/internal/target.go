package actions

import (
	"cmp"
	"fmt"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
)

const (
	PlatformGitHub   Platform = "github"
	PlatformCNB      Platform = "cnb"
	PlatformCodeberg Platform = "codeberg"
	PlatformForgejo  Platform = "forgejo"

	defaultCNBURL      = "https://cnb.cool"
	defaultCodebergURL = "https://codeberg.org"

	defaultCNBPageSize   = 100
	defaultGiteaPageSize = 50

	valueAll     = "all"
	bypassHeader = "X-Reconcile-Bypass" //nolint:gosec // custom header name, not a credential
)

type Platform string

func (platform Platform) label() string {
	switch platform {
	case PlatformGitHub:
		return "GitHub"
	case PlatformCNB:
		return "CNB"
	case PlatformCodeberg:
		return "Codeberg"
	case PlatformForgejo:
		return "Forgejo"
	default:
		return string(platform)
	}
}

func (platform Platform) wrap(operation string, err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%s: %s: %w", platform.label(), operation, err)
}

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
	PageSize           int
	Credential         Credentials
	RepositorySelector RepositorySelector

	gitUsername string
	mapping     pathMapping
	connect     func(services, TargetConfig) (provider, error)
}

type targetSpec struct {
	platform    Platform
	environment string
	defaultURL  string
	pageSize    int
	mapping     pathMapping
	gitUsername func(TargetConfig) string
	connect     func(services, TargetConfig) (provider, error)
}

func targetSpecifications() [3]targetSpec {
	return [...]targetSpec{
		{
			platform:    PlatformCNB,
			environment: "CNB",
			defaultURL:  defaultCNBURL,
			pageSize:    defaultCNBPageSize,
			mapping: pathMapping{
				source:       ".github/.cnb",
				target:       ".cnb",
				readmeToRoot: true,
				stripHealth:  true,
			},
			gitUsername: func(TargetConfig) string { return "cnb" },
			connect:     connectProvider(newCNBClient),
		},
		{
			platform:    PlatformCodeberg,
			environment: "CODEBERG",
			defaultURL:  defaultCodebergURL,
			pageSize:    defaultGiteaPageSize,
			mapping: pathMapping{
				source:      ".github/.forgejo",
				target:      ".forgejo",
				stripHealth: true,
			},
			gitUsername: func(target TargetConfig) string { return target.Credential.Git },
			connect:     connectProvider(newGiteaClient),
		},
		{
			platform:    PlatformForgejo,
			environment: "FORGEJO",
			pageSize:    defaultGiteaPageSize,
			mapping: pathMapping{
				source:      ".github/.forgejo",
				target:      ".forgejo",
				stripHealth: true,
			},
			gitUsername: func(target TargetConfig) string { return target.Credential.GitUsername },
			connect:     connectProvider(newGiteaClient),
		},
	}
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

func normalizeTargets(targets []TargetConfig, pageSize int) ([]TargetConfig, error) {
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
		target.PageSize = cmp.Or(pageSize, spec.pageSize)
		target.gitUsername = spec.gitUsername(*target)
		target.mapping = spec.mapping
		target.connect = spec.connect

		err := target.validate(spec)
		if err != nil {
			return nil, err
		}
	}

	return normalized, nil
}

func (target TargetConfig) validate(spec targetSpec) error {
	required := [...]string{
		target.Root,
		target.GitName,
		target.GitEmail,
		target.Credential.Organization,
		target.Credential.Repository,
		target.Credential.Git,
		target.Credential.Release,
	}
	if slices.Contains(required[:], "") {
		name := spec.environment

		return configurationError(fmt.Sprintf(
			"%s_ROOT_ORGANIZATION, %s_ORG_TOKEN, %s_REPO_TOKEN, %s_GIT_TOKEN, %s_RELEASE_TOKEN, %s_GIT_NAME and %s_GIT_EMAIL are required",
			name,
			name,
			name,
			name,
			name,
			name,
			name,
		))
	}

	if !validEndpointURL(target.URL) {
		return configurationError(spec.environment + "_URL must be an absolute HTTP(S) URL")
	}

	if target.gitUsername == "" {
		return configurationError(fmt.Sprintf(
			"%s git username cannot be resolved: configure %s_GIT_USERNAME or %s_GIT_TOKEN",
			spec.environment,
			spec.environment,
			spec.environment,
		))
	}

	return nil
}

func connectProvider[C provider](
	connect func(services, TargetConfig) (C, error),
) func(services, TargetConfig) (provider, error) {
	return func(services services, config TargetConfig) (provider, error) {
		return connect(services, config)
	}
}

type RepositorySelector struct {
	Include []string
	Exclude []string
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

func repositorySelectorFromEnv(name string, fallback RepositorySelector) RepositorySelector {
	include, hasInclude := os.LookupEnv(name + "_REPOS")

	exclude, hasExclude := os.LookupEnv(name + "_EXCLUDE_REPOS")
	if !hasInclude && !hasExclude {
		return fallback
	}

	selector := RepositorySelector{Exclude: repositoryList(exclude)}
	if strings.TrimSpace(include) != "*" {
		selector.Include = repositoryList(include)
	}

	return selector
}

func (selector RepositorySelector) matches(repository Repository) bool {
	return (len(selector.Include) == 0 || matchesAny(selector.Include, repository)) &&
		!matchesAny(selector.Exclude, repository)
}

func matchesAny(patterns []string, repository Repository) bool {
	candidates := [...]string{repository.fullName(), repository.Name}

	return slices.ContainsFunc(patterns, func(pattern string) bool {
		return slices.ContainsFunc(candidates[:], func(candidate string) bool {
			match, _ := path.Match(pattern, candidate)

			return match
		})
	})
}

type gitCredential struct {
	username string
	token    string
}

func forgeGitCredentials() map[string]gitCredential {
	credentials := make(map[string]gitCredential)

	for _, spec := range targetSpecifications() {
		target := targetFromEnv(spec, RepositorySelector{})

		username := spec.gitUsername(target)
		if target.Credential.Git == "" || username == "" {
			continue
		}

		parsed, err := url.Parse(target.URL)
		if err != nil {
			continue
		}

		host := strings.ToLower(parsed.Hostname())
		if host == "" {
			continue
		}

		credentials[host] = gitCredential{username: username, token: target.Credential.Git}
	}

	return credentials
}

type clients[T any] struct {
	organization T
	repository   T
	release      T
}

func connectClients[T any](
	credentials Credentials,
	connect func(string) (T, error),
) (clients[T], error) {
	var result clients[T]

	for _, slot := range [...]struct {
		target *T
		token  string
	}{
		{target: &result.organization, token: credentials.Organization},
		{target: &result.repository, token: credentials.Repository},
		{target: &result.release, token: credentials.Release},
	} {
		client, err := connect(slot.token)
		if err != nil {
			return clients[T]{}, err
		}

		*slot.target = client
	}

	return result, nil
}
