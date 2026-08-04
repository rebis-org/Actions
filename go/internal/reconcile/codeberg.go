package reconcile

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	gitea "gitea.dev/sdk"
)

type codebergClient struct {
	targetBase

	clients      clients[*gitea.Client]
	releaseToken string
}

func newCodebergClient(services services, config TargetConfig) (*codebergClient, error) {
	clients, err := connectClients(config.Credential, func(token string) (*gitea.Client, error) {
		client, err := gitea.NewClient(
			config.URL,
			gitea.SetHTTPClient(services.http.client),
			gitea.SetToken(token),
		)
		if err != nil {
			return nil, fmt.Errorf("create Codeberg client: %w", err)
		}

		return client, nil
	})
	if err != nil {
		return nil, err
	}

	return &codebergClient{
		targetBase:   newTargetBase(services, config),
		clients:      clients,
		releaseToken: config.Credential.Release,
	}, nil
}

func (*codebergClient) normalize(response *gitea.Response, err error) error {
	if err == nil {
		return nil
	}

	if response != nil && response.StatusCode == http.StatusNotFound {
		return errNotFound
	}

	return fmt.Errorf("codeberg API: %w", err)
}

func (client *codebergClient) reconcileRoot(
	ctx context.Context,
	organization *Organization,
	avatar *Avatar,
) error {
	current, err := ensureValue(client.failure, "organization "+client.root,
		func() (*gitea.Organization, error) {
			current, response, err := client.clients.organization.Organizations.GetOrg(
				ctx,
				client.root,
			)

			return current, client.normalize(response, err)
		},
		func() error {
			_, _, err := client.clients.organization.Organizations.CreateOrg(
				ctx,
				gitea.CreateOrgOption{Name: client.root},
			)
			if err == nil {
				return nil
			}

			return fmt.Errorf("create Codeberg organization: %w", err)
		},
	)
	if err != nil {
		return err
	}

	return client.reconcileIdentity(
		ctx,
		organization,
		avatar,
		func(ctx context.Context, desired Organization) error {
			return client.reconcileOrganization(ctx, desired, current)
		},
		client.uploadAvatar,
	)
}

func (client *codebergClient) reconcileOrganization(
	ctx context.Context,
	desired Organization,
	current *gitea.Organization,
) error {
	website := current.Website
	if desired.Website != "" {
		website = desired.Website
	}

	if desired.FullName == current.FullName && desired.Description == current.Description &&
		desired.Email == current.Email && desired.Location == current.Location &&
		website == current.Website {
		return nil
	}

	update := gitea.EditOrgOption{
		FullName:                  desired.FullName,
		Description:               desired.Description,
		Email:                     desired.Email,
		Location:                  desired.Location,
		Website:                   website,
		Visibility:                gitea.VisibleType(current.Visibility),
		RepoAdminChangeTeamAccess: new(current.RepoAdminChangeTeamAccess),
	}

	_, err := client.clients.organization.Organizations.EditOrg(ctx, client.root, update)

	return client.failure.wrap("update organization "+client.root, err)
}

func (client *codebergClient) prepareRepository(
	ctx context.Context,
	repository Repository,
) (string, error) {
	repositoryPath := client.repositoryPath(repository)

	current, err := ensureValue(client.failure, "repository "+repositoryPath,
		func() (*gitea.Repository, error) {
			current, response, err := client.clients.repository.Repositories.GetRepo(
				ctx,
				client.root,
				repository.Name,
			)

			return current, client.normalize(response, err)
		},
		func() error {
			_, _, err := client.clients.repository.Repositories.CreateOrgRepo(
				ctx,
				client.root,
				gitea.CreateRepoOption{
					Name:        repository.Name,
					Description: repository.Description,
					Private:     repository.Private,
					AutoInit:    false,
				},
			)
			if err == nil {
				return nil
			}

			return fmt.Errorf("create Codeberg repository: %w", err)
		},
	)
	if err != nil {
		return "", err
	}

	err = client.reconcileRepositoryFields(
		ctx,
		repositoryPath,
		repository,
		current,
	)
	if err != nil {
		return "", err
	}

	err = client.reconcileTopics(ctx, repositoryPath, repository)
	if err != nil {
		return "", err
	}

	return client.cloneURL(repository), nil
}

func (client *codebergClient) reconcileRepositoryFields(
	ctx context.Context,
	repositoryPath string,
	desired Repository,
	current *gitea.Repository,
) error {
	update := gitea.EditRepoOption{
		Description:   changedPointer(desired.Description, current.Description),
		Website:       changedPointer(desired.Homepage, current.Website),
		DefaultBranch: changedPointer(desired.DefaultBranch, current.DefaultBranch),
		Private:       changedPointer(desired.Private, current.Private),
		Archived:      changedPointer(desired.Archived, current.Archived),
		HasIssues:     changedPointer(desired.HasIssues, current.HasIssues),
		HasWiki:       changedPointer(desired.HasWiki, current.HasWiki),
		HasProjects:   changedPointer(desired.HasProjects, current.HasProjects),
		AllowSquash:   changedPointer(desired.AllowSquashMerge, current.AllowSquash),
		AllowMerge:    changedPointer(desired.AllowMergeCommit, current.AllowMerge),
		AllowRebase:   changedPointer(desired.AllowRebaseMerge, current.AllowRebase),
	}

	if !changed(update) {
		return nil
	}

	_, _, err := client.clients.repository.Repositories.EditRepo(
		ctx,
		client.root,
		desired.Name,
		update,
	)

	return client.failure.wrap("update repository "+repositoryPath, err)
}

func (client *codebergClient) reconcileTopics(
	ctx context.Context,
	repositoryPath string,
	desired Repository,
) error {
	current, _, err := client.clients.repository.Repositories.ListRepoTopics(
		ctx,
		client.root,
		desired.Name,
		gitea.ListRepoTopicsOptions{},
	)
	if err != nil {
		slog.Warn("cannot read repository topics", "target", client.platform, "error", err)

		return nil
	}

	if slices.Equal(desired.Topics, canonical(current)) {
		return nil
	}

	_, err = client.clients.repository.Repositories.SetRepoTopics(
		ctx,
		client.root,
		desired.Name,
		desired.Topics,
	)

	return client.failure.wrap("update topics "+repositoryPath, err)
}

func (client *codebergClient) reconcileLabels(
	ctx context.Context,
	repository Repository,
	desired []Label,
) error {
	pageSize := client.services.pages(client.platform)
	existing := make([]*gitea.Label, 0)

	err := paginate(pageSize, func(page int) ([]*gitea.Label, error) {
		labels, _, err := client.clients.repository.Repositories.ListRepoLabels(
			ctx,
			client.root,
			repository.Name,
			gitea.ListLabelsOptions{
				ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
			},
		)

		return labels, client.failure.wrap("list labels "+client.repositoryPath(repository), err)
	}, func(label *gitea.Label) error {
		existing = append(existing, label)

		return nil
	})
	if err != nil {
		return err
	}

	repositoryPath := client.repositoryPath(repository)

	return reconcileNamed(desired, existing, namedOperations[Label, *gitea.Label]{
		desiredName:  func(label Label) string { return label.Name },
		existingName: func(label *gitea.Label) string { return label.Name },
		equal: func(desired Label, current *gitea.Label) bool {
			return desired.Name == current.Name && sameColor(desired.Color, current.Color) &&
				desired.Description == current.Description && !current.Exclusive &&
				!current.IsArchived
		},
		create: func(label Label) error {
			_, _, err := client.clients.repository.Repositories.CreateLabel(
				ctx,
				client.root,
				repository.Name,
				gitea.CreateLabelOption{
					Name:        label.Name,
					Color:       label.Color,
					Description: label.Description,
				},
			)

			return client.failure.wrap("create label "+repositoryPath+"/"+label.Name, err)
		},
		update: func(desired Label, current *gitea.Label) error {
			exclusive, archived := false, false
			_, _, err := client.clients.repository.Repositories.EditLabel(
				ctx,
				client.root,
				repository.Name,
				current.ID,
				gitea.EditLabelOption{
					Name:        &desired.Name,
					Color:       &desired.Color,
					Description: &desired.Description,
					Exclusive:   &exclusive,
					IsArchived:  &archived,
				},
			)

			return client.failure.wrap("update label "+repositoryPath+"/"+current.Name, err)
		},
		remove: func(label *gitea.Label) error {
			_, err := client.clients.repository.Repositories.DeleteLabel(
				ctx,
				client.root,
				repository.Name,
				label.ID,
			)

			return client.failure.wrap("delete label "+repositoryPath+"/"+label.Name, err)
		},
	})
}

func (client *codebergClient) reconcileMilestones(
	ctx context.Context,
	repository Repository,
	desired []Milestone,
) error {
	pageSize := client.services.pages(client.platform)
	existing := make([]*gitea.Milestone, 0)

	err := paginate(pageSize, func(page int) ([]*gitea.Milestone, error) {
		milestones, _, err := client.clients.repository.Repositories.ListMilestones(
			ctx,
			client.root,
			repository.Name,
			gitea.ListMilestoneOption{
				ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
				State:       valueAll,
			},
		)

		return milestones, client.failure.wrap(
			"list milestones "+client.repositoryPath(repository),
			err,
		)
	}, func(milestone *gitea.Milestone) error {
		existing = append(existing, milestone)

		return nil
	})
	if err != nil {
		return err
	}

	repositoryPath := client.repositoryPath(repository)

	return reconcileNamed(desired, existing, namedOperations[Milestone, *gitea.Milestone]{
		desiredName:  func(milestone Milestone) string { return milestone.Title },
		existingName: func(milestone *gitea.Milestone) string { return milestone.Title },
		equal: func(desired Milestone, current *gitea.Milestone) bool {
			return desired.Title == current.Title &&
				desired.Description == current.Description &&
				desired.State == string(current.State) &&
				equalTime(desired.Deadline, current.Deadline)
		},
		create: func(milestone Milestone) error {
			_, _, err := client.clients.repository.Repositories.CreateMilestone(
				ctx,
				client.root,
				repository.Name,
				gitea.CreateMilestoneOption{
					Title:       milestone.Title,
					Description: milestone.Description,
					State:       gitea.StateType(milestone.State),
					Deadline:    milestone.Deadline,
				},
			)

			return client.failure.wrap("create milestone "+repositoryPath+"/"+milestone.Title, err)
		},
		update: func(desired Milestone, current *gitea.Milestone) error {
			state := gitea.StateType(desired.State)
			_, _, err := client.clients.repository.Repositories.EditMilestone(
				ctx,
				client.root,
				repository.Name,
				current.ID,
				gitea.EditMilestoneOption{
					Title:       desired.Title,
					Description: &desired.Description,
					State:       &state,
					Deadline:    desired.Deadline,
				},
			)

			return client.failure.wrap("update milestone "+repositoryPath+"/"+current.Title, err)
		},
		remove: func(milestone *gitea.Milestone) error {
			_, err := client.clients.repository.Repositories.DeleteMilestone(
				ctx,
				client.root,
				repository.Name,
				milestone.ID,
			)

			return client.failure.wrap("delete milestone "+repositoryPath+"/"+milestone.Title, err)
		},
	})
}

func equalTime(left, right *time.Time) bool {
	return left == nil && right == nil || left != nil && right != nil && left.Equal(*right)
}

func (client *codebergClient) uploadAvatar(ctx context.Context, avatar Avatar) error {
	_, err := client.clients.organization.Organizations.UpdateOrgAvatar(
		ctx,
		client.root,
		gitea.UpdateUserAvatarOption{Image: base64.StdEncoding.EncodeToString(avatar.Data)},
	)

	return client.failure.wrap("upload avatar "+client.root, err)
}

func (client *codebergClient) snapshot(ctx context.Context) (*platformState, error) {
	snapshot := newPlatformState()

	organization, _, err := client.clients.organization.Organizations.GetOrg(ctx, client.root)
	if err != nil {
		return nil, client.failure.wrap("read organization "+client.root, err)
	}

	if organization.Name != "" {
		record, err := newRecord(map[string]any{
			"username":       organization.Name,
			fieldFullName:    organization.FullName,
			fieldDescription: organization.Description,
			fieldWebsite:     organization.Website,
			fieldEmail:       organization.Email,
			"location":       organization.Location,
		})
		if err != nil {
			return nil, err
		}

		snapshot.Organization = record
	}

	pageSize := client.services.pages(client.platform)

	err = paginate(pageSize, func(page int) ([]*gitea.Repository, error) {
		repositories, _, err := client.clients.repository.Repositories.ListOrgRepos(
			ctx,
			client.root,
			gitea.ListOrgReposOptions{
				ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
			},
		)
		if err != nil {
			return nil, client.failure.wrap("list repositories "+client.root, err)
		}

		return repositories, nil
	}, func(repository *gitea.Repository) error {
		if repository.FullName != "" {
			record, err := newRecord(map[string]any{
				fieldFullName:    repository.FullName,
				fieldName:        repository.Name,
				fieldDescription: repository.Description,
				fieldWebsite:     repository.Website,
				fieldTopics:      canonical(repository.Topics),
				"default_branch": repository.DefaultBranch,
				fieldArchived:    repository.Archived,
			})
			if err != nil {
				return err
			}

			snapshot.Repositories[repository.FullName] = record
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return snapshot, nil
}

func (client *codebergClient) reconcileReleases(
	ctx context.Context,
	repository Repository,
	releases []Release,
) error {
	return reconcileReleaseSet(
		ctx,
		client.repositoryPath(repository),
		releases,
		client.releaseOperations(),
	)
}

func (client *codebergClient) releaseOperations() releaseOps[*gitea.Release] {
	return releaseOps[*gitea.Release]{
		pageSize: client.services.pages(client.platform),
		list:     client.listReleases,
		create:   client.createRelease,
		update:   client.updateRelease,
		view:     viewCodebergRelease,
		assets: assetOps{
			matches: client.releaseAssetMatches,
			replace: client.replaceReleaseAsset,
			remove:  client.removeReleaseAsset,
		},
	}
}

func (client *codebergClient) listReleases(
	ctx context.Context,
	repositoryPath string,
	page, pageSize int,
) ([]*gitea.Release, error) {
	repository, err := splitRepositoryPath(repositoryPath)
	if err != nil {
		return nil, err
	}

	releases, _, err := client.clients.release.Releases.ListReleases(
		ctx,
		repository.owner,
		repository.name,
		gitea.ListReleasesOptions{
			ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
		},
	)
	if err != nil {
		return nil, client.failure.wrap("list releases "+repositoryPath, err)
	}

	return releases, nil
}

func (client *codebergClient) createRelease(
	ctx context.Context,
	repositoryPath string,
	release Release,
) (*gitea.Release, error) {
	repository, err := splitRepositoryPath(repositoryPath)
	if err != nil {
		return nil, err
	}

	created, _, err := client.clients.release.Releases.CreateRelease(
		ctx,
		repository.owner,
		repository.name,
		gitea.CreateReleaseOption{
			TagName:      release.TagName,
			Target:       release.TargetCommitish,
			Title:        release.Name,
			Note:         release.Body,
			IsDraft:      release.Draft,
			IsPrerelease: release.Prerelease,
		},
	)
	if err != nil {
		return nil, client.failure.wrap("create release "+release.TagName, err)
	}

	return created, nil
}

func (client *codebergClient) updateRelease(
	ctx context.Context,
	repositoryPath string,
	current releaseState,
	desired Release,
) error {
	reference, err := codebergRelease(repositoryPath, current.id)
	if err != nil {
		return err
	}

	if desired.Name == current.name && desired.Body == current.body &&
		desired.Draft == current.draft && desired.Prerelease == current.pre {
		return nil
	}

	update := gitea.EditReleaseOption{
		TagName:      current.tag,
		Target:       current.target,
		Title:        desired.Name,
		Note:         desired.Body,
		IsDraft:      new(desired.Draft),
		IsPrerelease: new(desired.Prerelease),
	}

	_, _, err = client.clients.release.Releases.EditRelease(
		ctx,
		reference.owner,
		reference.name,
		reference.id,
		update,
	)

	return client.failure.wrap("update release "+desired.TagName, err)
}

func viewCodebergRelease(source *gitea.Release) releaseState {
	assets := make([]assetState, len(source.Attachments))
	for index, asset := range source.Attachments {
		assets[index] = assetState{
			id: strconv.FormatInt(asset.ID, 10), name: asset.Name, size: asset.Size,
		}
	}

	return releaseState{
		id: strconv.FormatInt(source.ID, 10), tag: source.TagName, name: source.Title,
		body: source.Note, target: source.Target, draft: source.IsDraft,
		pre: source.IsPrerelease, assets: assets,
	}
}

func (client *codebergClient) releaseAssetMatches(
	ctx context.Context,
	repositoryPath, releaseID string,
	current assetState,
	desired ReleaseAsset,
) (bool, error) {
	if current.size != desired.Size || assetHash(desired.Digest) == "" {
		return false, nil
	}

	reference, err := codebergRelease(repositoryPath, releaseID)
	if err != nil {
		return false, fmt.Errorf("read Codeberg release attachment: %w", err)
	}

	assetID, err := strconv.ParseInt(current.id, 10, 64)
	if err != nil {
		return false, fmt.Errorf("parse release asset ID %q: %w", current.id, err)
	}

	attachment, _, err := client.clients.release.Releases.GetReleaseAttachment(
		ctx,
		reference.owner,
		reference.name,
		reference.id,
		assetID,
	)
	if err != nil {
		return false, fmt.Errorf("read Codeberg release attachment: %w", err)
	}

	request, err := newRequest(ctx, http.MethodGet, attachment.DownloadURL, nil)
	if err != nil {
		return false, err
	}

	request.Header.Set("Authorization", "token "+client.releaseToken)

	content, err := client.services.http.stream(request, "download "+attachment.DownloadURL)
	if err != nil {
		return false, err
	}
	defer func() { _ = content.Close() }()

	hash, err := hashReader(content)

	return hash == assetHash(desired.Digest), err
}

func (client *codebergClient) replaceReleaseAsset(
	ctx context.Context,
	repositoryPath, releaseID string,
	current *assetState,
	desired ReleaseAsset,
) error {
	reference, err := codebergRelease(repositoryPath, releaseID)
	if err != nil {
		return err
	}

	content, err := desired.open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = content.Close() }()

	if current != nil {
		err = client.removeReleaseAsset(
			ctx,
			repositoryPath,
			releaseID,
			current.id,
		)
		if err != nil {
			return err
		}
	}

	_, _, err = client.clients.release.Releases.CreateReleaseAttachment(
		ctx,
		reference.owner,
		reference.name,
		reference.id,
		content,
		desired.Name,
	)

	return client.failure.wrap("upload release asset "+desired.Name, err)
}

func (client *codebergClient) removeReleaseAsset(
	ctx context.Context,
	repositoryPath, releaseID, assetID string,
) error {
	reference, err := codebergRelease(repositoryPath, releaseID)
	if err != nil {
		return err
	}

	id, err := strconv.ParseInt(assetID, 10, 64)
	if err != nil {
		return fmt.Errorf("parse release asset ID %q: %w", assetID, err)
	}

	_, err = client.clients.release.Releases.DeleteReleaseAttachment(
		ctx,
		reference.owner,
		reference.name,
		reference.id,
		id,
	)

	return client.failure.wrap("delete release asset "+releaseID+"/"+assetID, err)
}

type codebergRepository struct {
	owner string
	name  string
}

type repositoryPathError string

func (err repositoryPathError) Error() string {
	return "invalid repository path " + strconv.Quote(string(err))
}

type codebergReleaseRef struct {
	codebergRepository

	id int64
}

func splitRepositoryPath(value string) (codebergRepository, error) {
	owner, name, found := strings.Cut(value, "/")
	if !found || owner == "" || name == "" {
		return codebergRepository{}, repositoryPathError(value)
	}

	return codebergRepository{owner: owner, name: name}, nil
}

func codebergRelease(repositoryPath, releaseID string) (codebergReleaseRef, error) {
	repository, err := splitRepositoryPath(repositoryPath)
	if err != nil {
		return codebergReleaseRef{}, err
	}

	id, err := strconv.ParseInt(releaseID, 10, 64)
	if err != nil {
		return codebergReleaseRef{}, fmt.Errorf("parse release ID %q: %w", releaseID, err)
	}

	return codebergReleaseRef{codebergRepository: repository, id: id}, nil
}
