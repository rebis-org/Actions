package actions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/google/go-github/v93/github"
)

const (
	environmentMirrorToken        = "MIRROR_GITHUB_TOKEN" //nolint:gosec // name of an environment variable, not a credential
	environmentMirrorOrganization = "MIRROR_GITHUB_ORGANIZATION"
	environmentMirrorRepositories = "MIRROR_REPOS"
	environmentMirrorConcurrency  = "MIRROR_CONCURRENCY"
	environmentMirrorWorkspace    = "MIRROR_WORKSPACE"

	gitHubURL        = "https://github.com"
	gitHubHost       = "github.com"
	mirrorEntryParts = 3
	mirrorDirMode    = 0o755
	mirrorHashLength = 16
)

var errMirrorRepository = errors.New("mirror repository")

type Mirror struct {
	Name   string
	Source string
	Target string
}

type MirrorConfig struct {
	Token        string
	Organization string
	Concurrency  int
	Workspace    string
	Mirrors      []Mirror
	credentials  map[string]gitCredential
}

func MirrorConfigFromEnv() (MirrorConfig, error) {
	concurrency, err := envValue(environmentMirrorConcurrency, strconv.Atoi)
	if err != nil {
		return MirrorConfig{}, err
	}

	mirrors, err := mirrorSpecs(os.Getenv(environmentMirrorRepositories))
	if err != nil {
		return MirrorConfig{}, err
	}

	config := MirrorConfig{
		Token:        strings.TrimSpace(os.Getenv(environmentMirrorToken)),
		Organization: strings.Trim(os.Getenv(environmentMirrorOrganization), " /\n"),
		Concurrency:  concurrency,
		Workspace:    strings.TrimSpace(os.Getenv(environmentMirrorWorkspace)),
		Mirrors:      mirrors,
		credentials:  forgeGitCredentials(),
	}

	err = config.normalize()
	if err != nil {
		return MirrorConfig{}, err
	}

	return config, nil
}

func RunMirror(ctx context.Context, config MirrorConfig) error {
	err := config.normalize()
	if err != nil {
		return err
	}

	slog.Info(
		"mirroring repositories",
		"repositories", len(config.Mirrors),
		"concurrency", config.Concurrency,
	)

	_ = each(
		ctx,
		config.Concurrency,
		config.Mirrors,
		func(mirror Mirror) error {
			mirrorErr := mirrorRepository(ctx, config, mirror)
			if mirrorErr != nil {
				slog.Warn(
					"cannot mirror repository, skipping",
					"name", mirror.Name,
					"source", mirror.Source,
					"target", mirror.Target,
					"error", mirrorErr,
				)
			}

			return nil
		},
	)

	return cancellation(ctx)
}

func (config *MirrorConfig) normalize() error {
	concurrency, err := boundedConcurrency(config.Concurrency, environmentMirrorConcurrency)
	if err != nil {
		return err
	}

	config.Concurrency = concurrency

	mirrors, err := config.resolve()
	if err != nil {
		return err
	}

	config.Mirrors = mirrors

	return config.validate()
}

func (config *MirrorConfig) resolve() ([]Mirror, error) {
	if len(config.Mirrors) == 0 {
		return nil, configurationError(
			environmentMirrorRepositories + " must configure at least one name=source-url entry",
		)
	}

	mirrors := slices.Clone(config.Mirrors)
	seen := make(map[string]bool, len(mirrors))

	for index := range mirrors {
		mirror := &mirrors[index]

		mirror.Target = mirrorTargetURL(*mirror, config.Organization)
		if mirror.Target == "" {
			return nil, configurationError(fmt.Sprintf(
				"%s entry %d name %q has no target: set an explicit target URL or %s",
				environmentMirrorRepositories,
				index+1,
				mirror.Name,
				environmentMirrorOrganization,
			))
		}

		err := mirror.validate(index)
		if err != nil {
			return nil, err
		}

		key := strings.ToLower(mirror.Target)
		if seen[key] {
			return nil, configurationError(fmt.Sprintf(
				"mirror target %s is configured more than once",
				mirror.Target,
			))
		}

		seen[key] = true
	}

	return mirrors, nil
}

func (config *MirrorConfig) validate() error {
	requiresToken := slices.ContainsFunc(config.Mirrors, func(mirror Mirror) bool {
		return isGitHubTarget(mirror.Target)
	})
	if requiresToken && config.Token == "" {
		return configurationError(
			environmentMirrorToken + " is required when a target is a GitHub URL",
		)
	}

	return nil
}

func (mirror Mirror) validate(index int) error {
	if !validEndpointURL(mirror.Source) {
		return configurationError(fmt.Sprintf(
			"%s entry %d source %q must be an absolute HTTP(S) URL without credentials",
			environmentMirrorRepositories,
			index+1,
			mirror.Source,
		))
	}

	if !validEndpointURL(mirror.Target) {
		return configurationError(fmt.Sprintf(
			"%s entry %d target %q must be an absolute HTTP(S) URL without credentials",
			environmentMirrorRepositories,
			index+1,
			mirror.Target,
		))
	}

	if isGitHubTarget(mirror.Target) && !validGitHubPath(mirror.Target) {
		return configurationError(fmt.Sprintf(
			"%s entry %d target %q must name a GitHub repository as owner/repo",
			environmentMirrorRepositories,
			index+1,
			mirror.Target,
		))
	}

	return nil
}

func mirrorSpecs(value string) ([]Mirror, error) {
	fields := strings.FieldsFunc(value, func(character rune) bool {
		return character == '\n' || character == ','
	})

	specifications := make([]Mirror, 0, len(fields))

	for index, field := range fields {
		parts := strings.SplitN(strings.TrimSpace(field), "=", mirrorEntryParts)

		mirror := Mirror{Name: strings.TrimSpace(parts[0])}
		if len(parts) > 1 {
			mirror.Source = strings.TrimSpace(parts[1])
		}

		if len(parts) == mirrorEntryParts {
			mirror.Target = strings.TrimSpace(parts[2])
		}

		if mirror.Name == "" || mirror.Source == "" ||
			(len(parts) == mirrorEntryParts && mirror.Target == "") {
			return nil, mirrorEntryError(index)
		}

		specifications = append(specifications, mirror)
	}

	return specifications, nil
}

func mirrorEntryError(index int) error {
	return configurationError(fmt.Sprintf(
		"%s entry %d must have the form name=source-url or name=source-url=target-url",
		environmentMirrorRepositories,
		index+1,
	))
}

func mirrorTargetURL(mirror Mirror, organization string) string {
	if mirror.Target != "" {
		return mirror.Target
	}

	name := strings.Trim(strings.TrimSuffix(mirror.Name, ".git"), "/")
	if name == "" {
		return ""
	}

	if !strings.Contains(name, "/") {
		if organization == "" {
			return ""
		}

		name = organization + "/" + name
	}

	return gitHubURL + "/" + name + ".git"
}

func (config *MirrorConfig) credentialFor(target string) (gitCredential, bool) {
	parsed, err := url.Parse(target)
	if err != nil {
		return gitCredential{}, false
	}

	host := strings.ToLower(parsed.Hostname())
	if host == gitHubHost {
		credential := gitCredential{username: gitHubUsername, token: config.Token}

		return credential, credential.token != ""
	}

	credential, ok := config.credentials[host]

	return credential, ok
}

func mirrorRedactor(config MirrorConfig) *strings.Replacer {
	secrets := make([]string, 0, secretPairLength*(len(config.credentials)+1))

	if config.Token != "" {
		secrets = append(secrets, config.Token, basicCredential(gitHubUsername, config.Token))
	}

	for _, credential := range config.credentials {
		if credential.token == "" {
			continue
		}

		secrets = append(
			secrets,
			credential.token,
			basicCredential(credential.username, credential.token),
		)
	}

	return redactReplacer(secrets...)
}

func isGitHubTarget(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}

	return strings.EqualFold(parsed.Hostname(), gitHubHost)
}

func validGitHubPath(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}

	_, _, ok := gitHubRepository(parsed)

	return ok
}

func gitHubRepository(parsed *url.URL) (string, string, bool) {
	owner, name, found := strings.Cut(
		strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git"),
		"/",
	)
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}

	return owner, name, true
}

func mirrorRepository(ctx context.Context, config MirrorConfig, mirror Mirror) error {
	err := syncMirror(ctx, config, mirror)
	if err != nil {
		return fmt.Errorf("%w %s: %w", errMirrorRepository, mirror.Target, err)
	}

	slog.Info(
		"mirrored repository",
		"name", mirror.Name,
		"source", mirror.Source,
		"target", mirror.Target,
	)

	return nil
}

func syncMirror(ctx context.Context, config MirrorConfig, mirror Mirror) error {
	err := ensureGitHubRepository(ctx, config.Token, mirror)
	if err != nil {
		return err
	}

	workspace, cleanup, err := mirrorWorkspace(config)
	if err != nil {
		return err
	}
	defer cleanup()

	directory := filepath.Join(workspace, mirrorDirectory(mirror))
	tool := gitTool{Redact: mirrorRedactor(config)}

	err = refreshMirror(ctx, tool, directory, mirror.Source)
	if err != nil {
		return err
	}

	return pushMirror(ctx, config, tool, directory, mirror)
}

func mirrorWorkspace(config MirrorConfig) (string, func(), error) {
	if config.Workspace != "" {
		err := os.MkdirAll(config.Workspace, mirrorDirMode)
		if err != nil {
			return "", nil, fmt.Errorf("create mirror workspace %s: %w", config.Workspace, err)
		}

		return config.Workspace, func() {}, nil
	}

	workspace, err := os.MkdirTemp("", "mirror-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary workspace: %w", err)
	}

	return workspace, func() { _ = os.RemoveAll(workspace) }, nil
}

func refreshMirror(ctx context.Context, tool gitTool, directory, source string) error {
	_, err := os.Stat(directory)
	switch {
	case err == nil:
		_, err = tool.run(ctx, directory, gitEnvironment(), "remote", "update", "--prune")
		if err == nil {
			return nil
		}

		slog.Warn("cached mirror is broken, recloning", "source", source, "error", err)

		err = os.RemoveAll(directory)
		if err != nil {
			return fmt.Errorf("remove broken mirror cache: %w", err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect mirror cache: %w", err)
	}

	_, err = tool.run(ctx, "", gitEnvironment(), "clone", "--mirror", source, directory)
	if err != nil {
		return fmt.Errorf("clone %s: %w", source, err)
	}

	return nil
}

func pushMirror(
	ctx context.Context,
	config MirrorConfig,
	tool gitTool,
	directory string,
	mirror Mirror,
) error {
	credential, authorized := config.credentialFor(mirror.Target)
	if !authorized {
		slog.Warn(
			"mirror target has no configured credential, pushing anonymously",
			"target", mirror.Target,
		)
	}

	environment := gitEnvironment()
	if authorized {
		environment = gitEnvironment(authorization(
			mirror.Target,
			"Basic "+basicCredential(credential.username, credential.token),
		))
	}

	_, err := tool.run(
		ctx,
		directory,
		environment,
		"push",
		mirror.Target,
		"+refs/heads/*:refs/heads/*",
		"+refs/tags/*:refs/tags/*",
	)

	return err
}

func mirrorDirectory(mirror Mirror) string {
	hash := sha256.Sum256([]byte(mirror.Source + "\n" + mirror.Target))

	return hex.EncodeToString(hash[:])[:mirrorHashLength]
}

func ensureGitHubRepository(ctx context.Context, token string, mirror Mirror) error {
	if !isGitHubTarget(mirror.Target) {
		return nil
	}

	parsed, err := url.Parse(mirror.Target)
	if err != nil {
		return fmt.Errorf("parse target URL: %w", err)
	}

	owner, name, ok := gitHubRepository(parsed)
	if !ok {
		return nil
	}

	client, err := newGitHubClient(defaultTransport(), token)
	if err != nil {
		return err
	}

	_, _, err = client.Repositories.Get(ctx, owner, name)
	if err == nil {
		return nil
	}

	if !isNotFoundError(err) {
		return fmt.Errorf("check target repository: %w", err)
	}

	private, autoInit := false, false

	_, _, err = client.Repositories.Create(ctx, owner, github.CreateRepositoryRequest{
		Name:     name,
		Private:  &private,
		AutoInit: &autoInit,
	})
	if err != nil {
		return fmt.Errorf("create target repository: %w", err)
	}

	slog.Info("created target repository", "repository", owner+"/"+name)

	return nil
}

func isNotFoundError(err error) bool {
	response, ok := errors.AsType[*github.ErrorResponse](err)

	return ok && response.Response.StatusCode == http.StatusNotFound
}
