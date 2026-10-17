package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const remoteFailureSlots = 2

type gitIdentity struct {
	name  string
	email string
}

type Git struct {
	gitIdentity

	message    func() string
	credential string
	bypass     Bypass
	tool       gitTool
	force      bool
}

type Remote struct {
	Name string
	URL  string
	Git  Git

	mapping pathMapping
}

type pathMapping struct {
	source       string
	target       string
	readmeToRoot bool
	stripHealth  bool
}

type branchSnapshot struct {
	commit bool
	push   bool
}

func newGit(
	identity gitIdentity,
	message string,
	credential gitCredential,
	bypass Bypass,
	force bool,
	now func() time.Time,
) Git {
	source := func() string { return message }
	if message == "" {
		source = func() string {
			return "feat: reconcile [" + now().UTC().Format(time.RFC3339) + "]"
		}
	}

	encoded := basicCredential(credential.username, credential.token)

	return Git{
		gitIdentity: identity,
		message:     source,
		credential:  encoded,
		bypass:      bypass,
		tool:        gitTool{Redact: redactReplacer(credential.token, encoded, bypass.Token)},
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

	if len(branches) == 0 {
		return nil
	}

	failures := make([]error, 0, len(remotes))

	for _, remote := range remotes {
		err := repo.reconcileRemote(ctx, branches, remote)
		if err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
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
	return git.tool.run(ctx, directory, git.environment(endpoint), arguments...)
}

func (git Git) environment(endpoint string) []string {
	entries := make([]entry, 0, maxEnvironmentEntries)

	if endpoint != "" {
		entries = append(entries, authorization(endpoint, "Basic "+git.credential))
	}

	if git.bypass.matches(endpoint) {
		entries = append(entries, entry{
			Key:   "http." + endpoint + ".extraHeader",
			Value: bypassHeader + ": " + git.bypass.Token,
		})
	}

	return gitEnvironment(entries...)
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

func (repo gitRepository) reconcileRemote(
	ctx context.Context,
	branches []string,
	remote Remote,
) error {
	err := repo.run(ctx, "remote", "add", remote.Name, remote.URL)
	if err != nil {
		return fmt.Errorf("add remote %s for %s: %w", remote.Name, repo.repository.fullName(), err)
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
		return fmt.Errorf("fetch %s for %s: %w", remote.Name, repo.repository.fullName(), err)
	}

	failures := make([]error, 0, len(branches)+remoteFailureSlots)

	err = remote.Git.run(
		ctx,
		repo.directory,
		remote.URL,
		"push",
		"--force",
		remote.Name,
		"refs/tags/*:refs/tags/*",
	)
	if err != nil {
		failures = append(failures, fmt.Errorf(
			"push tags to %s for %s: %w",
			remote.Name,
			repo.repository.fullName(),
			err,
		))
	}

	for _, branch := range branches {
		err := repo.reconcileBranch(ctx, remote, branch)
		if err != nil {
			failures = append(failures, err)
		}
	}

	if len(branches) > 0 {
		err = remote.Git.run(ctx, repo.directory, remote.URL, "lfs", "push", "--all", remote.Name)
		if err != nil {
			failures = append(failures, fmt.Errorf(
				"push LFS to %s for %s: %w",
				remote.Name,
				repo.repository.fullName(),
				err,
			))
		}
	}

	return errors.Join(failures...)
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

	snapshot, err := repo.filter(ctx, remote, branch)
	if err != nil {
		return err
	}

	publishErr := repo.publish(ctx, remote, branch, snapshot)
	resetErr := repo.run(ctx, "reset", "--hard", "origin/"+branch)

	return errors.Join(publishErr, resetErr)
}

func (repo gitRepository) filter(
	ctx context.Context,
	remote Remote,
	branch string,
) (branchSnapshot, error) {
	remoteBranch := remote.Name + "/" + branch

	err := repo.removePath(ctx, ".github")
	if err != nil {
		return branchSnapshot{}, err
	}

	if remote.mapping.stripHealth {
		err = repo.stripDocuments(ctx)
		if err != nil {
			return branchSnapshot{}, err
		}
	}

	if remote.mapping.source != "" {
		err = repo.applyMapping(ctx, remoteBranch, branch, remote.mapping)
		if err != nil {
			return branchSnapshot{}, err
		}
	}

	if repo.source.force {
		commit, err := repo.stagedChanges(ctx)

		return branchSnapshot{commit: commit, push: true}, err
	}

	return repo.filteredSnapshot(ctx, remoteBranch)
}

func (repo gitRepository) filteredSnapshot(
	ctx context.Context,
	remoteBranch string,
) (branchSnapshot, error) {
	exists, err := repo.referenceExists(ctx, remoteBranch)
	if err != nil {
		return branchSnapshot{}, err
	}

	commit, err := repo.stagedChanges(ctx)
	if err != nil {
		return branchSnapshot{}, err
	}

	push := !exists
	if exists {
		identical, err := repo.matchesReference(ctx, remoteBranch)
		if err != nil {
			return branchSnapshot{}, err
		}

		push = !identical
	}

	return branchSnapshot{commit: commit, push: push}, nil
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

	err = repo.run(ctx, "checkout", "origin/"+sourceBranch, "--", mapping.source)
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

	hasPath, err := repo.treeHas(ctx, reference, path)
	if err != nil || !hasPath {
		return err
	}

	return repo.run(ctx, "checkout", reference, "--", path)
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

func (repo gitRepository) stripDocuments(ctx context.Context) error {
	patterns := transform([]string{
		"code_of_conduct",
		"contributing",
		"security",
		"support",
		"governance",
	}, func(name string) string {
		return ":(icase)" + name + ".*"
	})

	arguments := slices.Concat([]string{"rm", "--ignore-unmatch"}, patterns)

	return repo.run(ctx, arguments...)
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
			"push %s to %s for %s: %w",
			branch,
			remote.Name,
			repo.repository.fullName(),
			err,
		)
	}

	return nil
}

func (repo gitRepository) referenceExists(ctx context.Context, reference string) (bool, error) {
	return repo.hasOutput(
		ctx,
		"for-each-ref",
		"--format=%(objectname)",
		"refs/remotes/"+reference,
	)
}

func (repo gitRepository) treeHas(ctx context.Context, reference, path string) (bool, error) {
	return repo.hasOutput(ctx, "ls-tree", "--name-only", reference, path)
}

func (repo gitRepository) stagedChanges(ctx context.Context) (bool, error) {
	return repo.hasOutput(ctx, "diff", "--cached", "--name-only")
}

func (repo gitRepository) matchesReference(ctx context.Context, reference string) (bool, error) {
	changed, err := repo.hasOutput(ctx, "diff", "--cached", "--name-only", reference)

	return !changed, err
}

func (repo gitRepository) hasOutput(ctx context.Context, arguments ...string) (bool, error) {
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
