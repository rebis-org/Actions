package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/go-github/v89/github"
)

var (
	errGitHubRepositoryIncomplete = errors.New("GitHub returned an incomplete repository record")
	errReleaseAssetUnavailable    = errors.New("release asset content is unavailable")
)

type Repository struct {
	Owner            string
	Name             string
	CloneURL         string
	Description      string
	Homepage         string
	Topics           []string
	DefaultBranch    string
	License          string
	Private          bool
	Archived         bool
	HasIssues        bool
	HasWiki          bool
	HasProjects      bool
	HasDownloads     bool
	HasDiscussions   bool
	AllowSquashMerge bool
	AllowMergeCommit bool
	AllowRebaseMerge bool
}

func (repository Repository) fullName() string {
	return repository.Owner + "/" + repository.Name
}

type Organization struct {
	Name        string
	FullName    string
	Description string
	Website     string
	Email       string
	Location    string
	AvatarURL   string
}

type Release struct {
	TagName         string
	Name            string
	Body            string
	TargetCommitish string
	Draft           bool
	Prerelease      bool
	Latest          bool
	Assets          []ReleaseAsset
}

type ReleaseAsset struct {
	Name        string
	Digest      string
	ContentType string
	Size        int64
	content     *cachedAsset
}

type Label struct {
	Name        string
	Color       string
	Description string
}

type Milestone struct {
	Title       string
	Description string
	State       string
	Deadline    *time.Time
}

func (asset ReleaseAsset) open(ctx context.Context) (io.ReadCloser, error) {
	if asset.content == nil {
		return nil, errReleaseAssetUnavailable
	}

	return asset.content.open(ctx)
}

type githubSource struct {
	client   *github.Client
	http     transport
	pageSize int
}

func newGitHubSource(services services, token string) (*githubSource, error) {
	client, err := github.NewClient(
		github.WithHTTPClient(services.http.client),
		github.WithAuthToken(token),
	)
	if err != nil {
		return nil, fmt.Errorf("configure GitHub client: %w", err)
	}

	return &githubSource{
		client:   client,
		http:     services.http,
		pageSize: services.pages(PlatformGitHub),
	}, nil
}

func (source *githubSource) repositories(
	ctx context.Context,
	excludeForks bool,
) ([]Repository, error) {
	pages, err := githubPages(source.pageSize, func(options *github.ListOptions) (
		[]*github.Repository,
		*github.Response,
		error,
	) {
		return source.client.Repositories.ListByAuthenticatedUser(
			ctx,
			&github.RepositoryListByAuthenticatedUserOptions{ListOptions: *options},
		)
	})
	if err != nil {
		return nil, fmt.Errorf("list GitHub repositories: %w", err)
	}

	repositories := make([]Repository, 0, len(pages))
	for _, page := range pages {
		if excludeForks && page.GetFork() {
			continue
		}

		repository, err := repositoryFromGitHub(page)
		if err != nil {
			return nil, err
		}

		repositories = append(repositories, repository)
	}

	return repositories, nil
}

func repositoryFromGitHub(source *github.Repository) (Repository, error) {
	repository := Repository{
		Owner:            source.GetOwner().GetLogin(),
		Name:             source.GetName(),
		CloneURL:         source.GetCloneURL(),
		Description:      source.GetDescription(),
		Homepage:         source.GetHomepage(),
		Topics:           canonical(source.Topics),
		DefaultBranch:    source.GetDefaultBranch(),
		License:          source.GetLicense().GetSPDXID(),
		Private:          source.GetPrivate(),
		Archived:         source.GetArchived(),
		HasIssues:        source.GetHasIssues(),
		HasWiki:          source.GetHasWiki(),
		HasProjects:      source.GetHasProjects(),
		HasDownloads:     source.GetHasDownloads(),
		HasDiscussions:   source.GetHasDiscussions(),
		AllowSquashMerge: source.GetAllowSquashMerge(),
		AllowMergeCommit: source.GetAllowMergeCommit(),
		AllowRebaseMerge: source.GetAllowRebaseMerge(),
	}
	if repository.Owner == "" || repository.Name == "" || repository.CloneURL == "" {
		return Repository{}, errGitHubRepositoryIncomplete
	}

	return repository, nil
}

func (source *githubSource) organization(ctx context.Context, name string) (Organization, error) {
	organization, _, err := source.client.Organizations.Get(ctx, name)
	if err != nil {
		return Organization{}, fmt.Errorf("get GitHub organization %q: %w", name, err)
	}

	return Organization{
		Name:        organization.GetLogin(),
		FullName:    organization.GetName(),
		Description: organization.GetDescription(),
		Website:     normalizedSite(organization.GetBlog()),
		Email:       organization.GetEmail(),
		Location:    organization.GetLocation(),
		AvatarURL:   organization.GetAvatarURL(),
	}, nil
}

func (source *githubSource) labels(
	ctx context.Context,
	repository Repository,
) ([]Label, error) {
	items, err := githubPages(source.pageSize, func(options *github.ListOptions) (
		[]*github.Label,
		*github.Response,
		error,
	) {
		return source.client.Issues.ListLabels(
			ctx,
			repository.Owner,
			repository.Name,
			options,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("list labels for %s: %w", repository.fullName(), err)
	}

	labels := make([]Label, len(items))
	for index, item := range items {
		labels[index] = Label{
			Name:        item.GetName(),
			Color:       item.GetColor(),
			Description: item.GetDescription(),
		}
	}

	return labels, nil
}

func (source *githubSource) milestones(
	ctx context.Context,
	repository Repository,
) ([]Milestone, error) {
	items, err := githubPages(source.pageSize, func(options *github.ListOptions) (
		[]*github.Milestone,
		*github.Response,
		error,
	) {
		return source.client.Issues.ListMilestones(
			ctx,
			repository.Owner,
			repository.Name,
			&github.MilestoneListOptions{State: valueAll, ListOptions: *options},
		)
	})
	if err != nil {
		return nil, fmt.Errorf("list milestones for %s: %w", repository.fullName(), err)
	}

	milestones := make([]Milestone, len(items))
	for index, item := range items {
		var deadline *time.Time
		if due := item.GetDueOn().Time; !due.IsZero() {
			deadline = &due
		}

		milestones[index] = Milestone{
			Title:       item.GetTitle(),
			Description: item.GetDescription(),
			State:       item.GetState(),
			Deadline:    deadline,
		}
	}

	return milestones, nil
}

func (source *githubSource) releases(
	ctx context.Context,
	repository Repository,
) ([]Release, error) {
	pages, err := githubPages(source.pageSize, func(options *github.ListOptions) (
		[]*github.RepositoryRelease,
		*github.Response,
		error,
	) {
		return source.client.Repositories.ListReleases(
			ctx,
			repository.Owner,
			repository.Name,
			options,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("list releases for %s: %w", repository.fullName(), err)
	}

	releases := make([]Release, len(pages))
	for index, page := range pages {
		releases[index] = source.release(repository, page)
	}

	if len(releases) == 0 {
		return releases, nil
	}

	latest, _, latestErr := source.client.Repositories.GetLatestRelease(
		ctx,
		repository.Owner,
		repository.Name,
	)
	if latestErr == nil {
		for index := range releases {
			if releases[index].TagName == latest.GetTagName() {
				releases[index].Latest = true

				break
			}
		}
	}

	return releases, nil
}

func (source *githubSource) release(
	repository Repository,
	item *github.RepositoryRelease,
) Release {
	release := Release{
		TagName:         item.GetTagName(),
		Name:            item.GetName(),
		Body:            item.GetBody(),
		TargetCommitish: item.GetTargetCommitish(),
		Draft:           item.GetDraft(),
		Prerelease:      item.GetPrerelease(),
		Assets:          make([]ReleaseAsset, len(item.Assets)),
	}
	for index, item := range item.Assets {
		id := item.GetID()
		release.Assets[index] = ReleaseAsset{
			Name:        item.GetName(),
			Digest:      item.GetDigest(),
			ContentType: item.GetContentType(),
			Size:        int64(item.GetSize()),
			content: &cachedAsset{fetch: func(ctx context.Context) (io.ReadCloser, error) {
				reader, _, err := source.client.Repositories.DownloadReleaseAsset(
					ctx,
					repository.Owner,
					repository.Name,
					id,
					source.http.client,
				)
				if err != nil {
					return nil, fmt.Errorf("download release asset %s: %w", item.GetName(), err)
				}

				return reader, nil
			}},
		}
	}

	return release
}

func closeReleaseAssets(releases []Release) {
	for _, release := range releases {
		for _, asset := range release.Assets {
			if asset.content != nil {
				asset.content.close()
			}
		}
	}
}

func githubPages[T any](
	pageSize int,
	fetch func(*github.ListOptions) ([]T, *github.Response, error),
) ([]T, error) {
	options := &github.ListOptions{PerPage: pageSize}
	all := make([]T, 0)

	for {
		page, response, err := fetch(options)
		if err != nil {
			return nil, err
		}

		all = append(all, page...)
		if response.NextPage == 0 {
			return all, nil
		}

		options.Page = response.NextPage
	}
}
