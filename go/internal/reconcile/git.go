package reconcile

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var (
	errTargetConfigure = errors.New("configure target")
	errTargetFetch     = errors.New("fetch target")
	errTargetPush      = errors.New("push target")
	errTargetPushLFS   = errors.New("push LFS to target")
)

type gitIdentity struct {
	name  string
	email string
}

type Git struct {
	gitIdentity

	message    func() string
	credential string
	redact     *strings.Replacer
	force      bool
}

type Remote struct {
	Name string
	URL  string
	Git  Git
}

type branchSnapshot struct {
	commit bool
	push   bool
}

func newGit(
	name, email, message, username, password string,
	force bool,
	now func() time.Time,
) Git {
	credential := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))

	messageSource := func() string { return message }
	if message == "" {
		messageSource = func() string {
			return fmt.Sprintf("feat: reconcile [%s]", now().UTC().Format(time.RFC3339))
		}
	}

	replacements := make([]string, 0)

	for _, secret := range [...]string{password, credential} {
		if secret != "" {
			replacements = append(replacements, secret, "***")
		}
	}

	return Git{
		gitIdentity: gitIdentity{name: name, email: email},
		message:     messageSource,
		credential:  credential,
		redact:      strings.NewReplacer(replacements...),
		force:       force,
	}
}

func (git Git) reconcile(ctx context.Context, repository Repository, remotes []Remote) error {
	workspace, err := os.MkdirTemp("", "reconcile-*")
	if err != nil {
		return fmt.Errorf("create temporary repository: %w", err)
	}
	defer func() { _ = os.RemoveAll(workspace) }()

	repo := gitRepository{
		directory:  filepath.Join(workspace, "repository"),
		repository: repository,
		source:     git,
	}

	err = repo.prepare(ctx)
	if err != nil {
		return err
	}

	branches, err := repo.branches(ctx)
	if err != nil {
		return fmt.Errorf("list branches for %s: %w", repository.fullName(), err)
	}

	for _, remote := range remotes {
		err := repo.reconcileTarget(ctx, branches, remote)
		if err != nil {
			return err
		}
	}

	return nil
}

type gitRepository struct {
	directory  string
	repository Repository
	source     Git
}

func (repo gitRepository) prepare(ctx context.Context) error {
	err := repo.source.run(
		ctx,
		"",
		repo.repository.CloneURL,
		"clone",
		"--no-checkout",
		repo.repository.CloneURL,
		repo.directory,
	)
	if err != nil {
		return fmt.Errorf("clone %s: %w", repo.repository.fullName(), err)
	}

	err = repo.configureIdentity(ctx, repo.source.gitIdentity)
	if err != nil {
		return err
	}

	err = repo.source.run(
		ctx,
		repo.directory,
		repo.repository.CloneURL,
		"lfs",
		"fetch",
		"--all",
		"origin",
	)
	if err != nil {
		return fmt.Errorf("fetch LFS for %s: %w", repo.repository.fullName(), err)
	}

	return nil
}

func (repo gitRepository) branches(ctx context.Context) ([]string, error) {
	output, err := repo.output(
		ctx,
		"for-each-ref",
		"--format=%(refname:strip=3)",
		"refs/remotes/origin/",
	)
	if err != nil {
		return nil, err
	}

	branches := make([]string, 0)

	for branch := range strings.FieldsSeq(output) {
		if branch != "HEAD" {
			branches = append(branches, branch)
		}
	}

	slices.Sort(branches)

	return branches, nil
}

func (repo gitRepository) reconcileTarget(
	ctx context.Context,
	branches []string,
	remote Remote,
) error {
	err := repo.run(ctx, "remote", "add", remote.Name, remote.URL)
	if err != nil {
		return fmt.Errorf(
			"%w %s for %s: %w",
			errTargetConfigure,
			remote.Name,
			repo.repository.fullName(),
			err,
		)
	}

	err = remote.Git.run(
		ctx,
		repo.directory,
		remote.URL,
		"fetch",
		"--prune",
		remote.Name,
		"+refs/heads/*:refs/remotes/"+remote.Name+"/*",
	)
	if err != nil {
		return fmt.Errorf(
			"%w %s for %s: %w",
			errTargetFetch,
			remote.Name,
			repo.repository.fullName(),
			err,
		)
	}

	for _, branch := range branches {
		err := repo.reconcileBranch(ctx, remote, branch)
		if err != nil {
			return err
		}
	}

	err = remote.Git.run(
		ctx,
		repo.directory,
		remote.URL,
		"lfs",
		"push",
		"--all",
		remote.Name,
	)
	if err != nil {
		return fmt.Errorf(
			"%w %s for %s: %w",
			errTargetPushLFS,
			remote.Name,
			repo.repository.fullName(),
			err,
		)
	}

	return nil
}

func (repo gitRepository) reconcileBranch(
	ctx context.Context,
	remote Remote,
	branch string,
) error {
	err := repo.run(
		ctx,
		"checkout",
		"--force",
		"--detach",
		"origin/"+branch,
	)
	if err != nil {
		return fmt.Errorf("checkout %s for %s: %w", branch, repo.repository.fullName(), err)
	}

	snapshot, err := repo.filter(ctx, remote.Name, branch)
	if err != nil {
		return err
	}

	publishErr := repo.publish(ctx, remote, branch, snapshot)
	resetErr := repo.run(ctx, "reset", "--hard", "origin/"+branch)

	return errors.Join(publishErr, resetErr)
}

type pathMapping struct {
	source       string
	target       string
	readmeToRoot bool
	stripHealth  bool
}

func platformMapping(remoteName string) pathMapping {
	switch Platform(remoteName) {
	case PlatformGitHub:
		return pathMapping{}
	case PlatformCNB:
		return pathMapping{
			source:       ".github/.cnb",
			target:       ".cnb",
			readmeToRoot: true,
			stripHealth:  true,
		}
	case PlatformCodeberg:
		return pathMapping{source: ".github/.forgejo", target: ".forgejo", stripHealth: true}
	default:
		return pathMapping{}
	}
}

func (repo gitRepository) filter(
	ctx context.Context,
	remoteName, branch string,
) (branchSnapshot, error) {
	mapping := platformMapping(remoteName)
	remoteBranch := remoteName + "/" + branch

	err := repo.removePath(ctx, ".github")
	if err != nil {
		return branchSnapshot{}, err
	}

	if mapping.stripHealth {
		arguments := []string{"rm", "--ignore-unmatch"}
		for _, name := range [...]string{
			"code_of_conduct",
			"contributing",
			"security",
			"support",
			"governance",
		} {
			arguments = append(arguments, ":(icase)"+name+".*")
		}

		err := repo.run(ctx, arguments...)
		if err != nil {
			return branchSnapshot{}, err
		}
	}

	if mapping.source != "" {
		err := repo.applyMapping(ctx, remoteBranch, branch, mapping)
		if err != nil {
			return branchSnapshot{}, err
		}
	}

	if repo.source.force {
		commit, err := repo.changed(ctx, "", false)

		return branchSnapshot{commit: commit, push: true}, err
	}

	exists, err := repo.referenceExists(ctx, remoteBranch)
	if err != nil {
		return branchSnapshot{}, err
	}

	changed, err := repo.changed(ctx, remoteBranch, exists)

	return branchSnapshot{commit: changed, push: !exists || changed}, err
}

func (repo gitRepository) applyMapping(
	ctx context.Context,
	remoteBranch, sourceBranch string,
	mapping pathMapping,
) error {
	hasMapped, err := repo.treeHas(ctx, "origin/"+sourceBranch, mapping.source)
	if err != nil {
		return err
	}

	err = repo.removePath(ctx, mapping.target)
	if err != nil {
		return err
	}

	if !hasMapped {
		return repo.restoreMapping(ctx, remoteBranch, mapping.target)
	}

	err = repo.run(
		ctx,
		"checkout",
		"origin/"+sourceBranch,
		"--",
		mapping.source,
	)
	if err != nil {
		return err
	}

	err = repo.promoteReadme(ctx, sourceBranch, mapping)
	if err != nil {
		return err
	}

	return repo.moveMapping(ctx, mapping)
}

func (repo gitRepository) removePath(ctx context.Context, path string) error {
	return repo.run(ctx, "rm", "-r", "--ignore-unmatch", path)
}

func (repo gitRepository) restoreMapping(ctx context.Context, reference, path string) error {
	exists, err := repo.referenceExists(ctx, reference)
	if err != nil || !exists {
		return err
	}

	return repo.restorePath(ctx, reference, path)
}

func (repo gitRepository) promoteReadme(
	ctx context.Context,
	sourceBranch string,
	mapping pathMapping,
) error {
	if !mapping.readmeToRoot {
		return nil
	}

	readme := mapping.source + "/README.md"

	exists, err := repo.treeHas(ctx, "origin/"+sourceBranch, readme)
	if err != nil || !exists {
		return err
	}

	err = repo.removePath(ctx, "README.md")
	if err != nil {
		return err
	}

	return repo.run(ctx, "mv", readme, "README.md")
}

func (repo gitRepository) moveMapping(ctx context.Context, mapping pathMapping) error {
	remaining, err := repo.output(ctx, "ls-files", mapping.source)
	if err != nil || strings.TrimSpace(remaining) == "" {
		return err
	}

	return repo.run(ctx, "mv", mapping.source, mapping.target)
}

func (repo gitRepository) publish(
	ctx context.Context,
	remote Remote,
	branch string,
	snapshot branchSnapshot,
) error {
	if !snapshot.push {
		return nil
	}

	if snapshot.commit {
		err := repo.configureIdentity(ctx, remote.Git.gitIdentity)
		if err != nil {
			return err
		}

		err = repo.run(ctx, "commit", "-m", repo.source.message())
		if err != nil {
			return err
		}
	}

	err := remote.Git.run(
		ctx,
		repo.directory,
		remote.URL,
		"push",
		"--force",
		remote.Name,
		"HEAD:refs/heads/"+branch,
	)
	if err != nil {
		return fmt.Errorf(
			"%w %s to %s for %s: %w",
			errTargetPush,
			branch,
			remote.Name,
			repo.repository.fullName(),
			err,
		)
	}

	return nil
}

func (repo gitRepository) referenceExists(
	ctx context.Context,
	reference string,
) (bool, error) {
	output, err := repo.output(
		ctx,
		"for-each-ref",
		"--format=%(objectname)",
		"refs/remotes/"+reference,
	)

	return strings.TrimSpace(output) != "", err
}

func (repo gitRepository) treeHas(
	ctx context.Context,
	reference, path string,
) (bool, error) {
	output, err := repo.output(
		ctx,
		"ls-tree",
		"--name-only",
		reference,
		path,
	)

	return strings.TrimSpace(output) != "", err
}

func (repo gitRepository) restorePath(ctx context.Context, reference, path string) error {
	hasPath, err := repo.treeHas(ctx, reference, path)
	if err != nil || !hasPath {
		return err
	}

	return repo.run(ctx, "checkout", reference, "--", path)
}

func (repo gitRepository) changed(
	ctx context.Context,
	reference string,
	exists bool,
) (bool, error) {
	arguments := []string{"diff", "--cached", "--name-only"}
	if exists {
		arguments = append(arguments, reference)
	}

	output, err := repo.output(ctx, arguments...)

	return strings.TrimSpace(output) != "", err
}

func (repo gitRepository) configureIdentity(ctx context.Context, identity gitIdentity) error {
	err := repo.run(ctx, "config", "user.name", identity.name)
	if err != nil {
		return err
	}

	return repo.run(ctx, "config", "user.email", identity.email)
}

func (repo gitRepository) run(ctx context.Context, arguments ...string) error {
	return repo.source.run(ctx, repo.directory, "", arguments...)
}

func (repo gitRepository) output(ctx context.Context, arguments ...string) (string, error) {
	return repo.source.output(ctx, repo.directory, "", arguments...)
}

func (git Git) run(
	ctx context.Context,
	directory, endpoint string,
	arguments ...string,
) error {
	_, err := git.output(ctx, directory, endpoint, arguments...)

	return err
}

func (git Git) output(
	ctx context.Context,
	directory, endpoint string,
	arguments ...string,
) (string, error) {
	if directory != "" {
		arguments = append([]string{"-C", directory}, arguments...)
	}

	command := exec.CommandContext(ctx, "git", arguments...) //nolint:gosec

	command.Env = append(os.Environ(), git.environment(endpoint)...)

	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, git.redact.Replace(string(output)))
	}

	return string(output), nil
}

func (git Git) environment(endpoint string) []string {
	count := 3

	environment := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_KEY_0=filter.lfs.smudge",
		"GIT_CONFIG_VALUE_0=git-lfs smudge --skip -- %f",
		"GIT_CONFIG_KEY_1=credential.helper",
		"GIT_CONFIG_VALUE_1=",
		"GIT_CONFIG_KEY_2=core.hooksPath",
		"GIT_CONFIG_VALUE_2=/dev/null",
	}
	if endpoint != "" {
		environment = append(
			environment,
			"GIT_CONFIG_KEY_3=http."+endpoint+".extraHeader",
			"GIT_CONFIG_VALUE_3=Authorization: Basic "+git.credential,
		)
		count++
	}

	return append(environment, fmt.Sprintf("GIT_CONFIG_COUNT=%d", count))
}
