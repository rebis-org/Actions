package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	cnb "cnb.cool/cnb/sdk/go-cnb/cnb"
	"cnb.cool/cnb/sdk/go-cnb/cnb/types/api"
	"cnb.cool/cnb/sdk/go-cnb/cnb/types/constant"
	"cnb.cool/cnb/sdk/go-cnb/cnb/types/dto"
)

type cnbClient struct {
	targetBase

	clients clients[*cnb.Client]
}

var errUploadURLMissing = errors.New("upload URL is missing")

func newCNBClient(services services, config TargetConfig) (*cnbClient, error) {
	apiURL, err := url.Parse(config.URL)
	if err != nil {
		return nil, fmt.Errorf("parse CNB URL: %w", err)
	}

	if !strings.HasPrefix(apiURL.Host, "api.") {
		apiURL.Host = "api." + apiURL.Host
	}

	apiURL.Path = strings.TrimRight(apiURL.Path, "/")

	clients, err := connectClients(config.Credential, func(token string) (*cnb.Client, error) {
		client, err := cnb.NewClient(services.http.client).
			WithAuthToken(token).
			WithURLs(apiURL.String())
		if err != nil {
			return nil, fmt.Errorf("create CNB client: %w", err)
		}

		return client, nil
	})
	if err != nil {
		return nil, err
	}

	return &cnbClient{targetBase: newTargetBase(services, config), clients: clients}, nil
}

func (client *cnbClient) normalize(err error) error {
	var response *cnb.ErrorResponse
	if errors.As(err, &response) && response.Response != nil &&
		response.Response.StatusCode == http.StatusNotFound {
		return errNotFound
	}

	return err
}

func (client *cnbClient) reconcileRoot(
	ctx context.Context,
	organization *Organization,
	avatar *Avatar,
) error {
	group, err := ensureValue(client.failure, "group "+client.root,
		func() (*dto.OrganizationAccess, error) {
			group, _, err := client.clients.organization.Organizations.GetGroup(ctx, client.root)

			return group, client.normalize(err)
		},
		func() error {
			_, err := client.clients.organization.Organizations.CreateOrganization(
				ctx,
				&cnb.CreateOrganizationRequest{Path: client.root},
			)
			if err == nil {
				return nil
			}

			return fmt.Errorf("create CNB group: %w", err)
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
			return client.reconcileOrganization(ctx, desired, group)
		},
		client.uploadAvatar,
	)
}

func (client *cnbClient) reconcileOrganization(
	ctx context.Context,
	desired Organization,
	current *dto.OrganizationAccess,
) error {
	update := make(patch)
	set(update, fieldDescription, desired.Description, current.Description)
	set(update, fieldEmail, desired.Email, current.Email)

	if desired.Website != "" {
		set(update, "site", desired.Website, current.Site)
	}

	if len(update) == 0 {
		return nil
	}

	return client.failure.wrap(
		"update organization "+client.root,
		client.request(ctx, client.clients.organization, http.MethodPut, "/"+client.root, update),
	)
}

func (client *cnbClient) prepareRepository(
	ctx context.Context,
	repository Repository,
) (string, error) {
	repositoryPath := client.repositoryPath(repository)

	current, err := ensureValue(client.failure, "repository "+repositoryPath,
		func() (*dto.Repos4User, error) {
			current, _, err := client.clients.repository.Repositories.GetByID(ctx, repositoryPath)

			return current, client.normalize(err)
		},
		func() error {
			visibility := dto.CreateRepoReqVisibilityPublic
			if repository.Private {
				visibility = dto.CreateRepoReqVisibilityPrivate
			}

			_, err := client.clients.repository.Repositories.CreateRepo(
				ctx,
				client.root,
				&cnb.CreateRepoRequest{
					Name:        repository.Name,
					Description: repository.Description,
					Visibility:  visibility,
				},
			)
			if err == nil {
				return nil
			}

			return fmt.Errorf("create CNB repository: %w", err)
		},
	)
	if err != nil {
		return "", err
	}

	err = client.reconcileRepositoryMetadata(
		ctx,
		repositoryPath,
		repository,
		current,
	)
	if err != nil {
		return "", err
	}

	err = client.reconcileArchive(
		ctx,
		repositoryPath,
		repository.Archived,
		current,
	)
	if err != nil {
		return "", err
	}

	err = client.reconcilePullRequests(ctx, repositoryPath, repository)
	if err != nil {
		return "", err
	}

	return client.cloneURL(repository), nil
}

func (client *cnbClient) reconcileRepositoryMetadata(
	ctx context.Context,
	repositoryPath string,
	desired Repository,
	current *dto.Repos4User,
) error {
	update := make(patch)
	set(update, fieldDescription, desired.Description, current.Description)
	set(update, "site", desired.Homepage, current.Site)
	set(update, "license", desired.License, current.License)

	if topics := splitTopics(current.Topics); !slices.Equal(desired.Topics, topics) {
		update[fieldTopics] = desired.Topics
	}

	if len(update) == 0 {
		return nil
	}

	return client.failure.wrap(
		"update repository "+repositoryPath,
		client.request(
			ctx,
			client.clients.repository,
			http.MethodPatch,
			"/"+repositoryPath,
			update,
		),
	)
}

func (client *cnbClient) reconcileArchive(
	ctx context.Context,
	repositoryPath string,
	archived bool,
	current *dto.Repos4User,
) error {
	if (current.Status == constant.RepoStatusArchived) == archived {
		return nil
	}

	var err error
	if archived {
		_, err = client.clients.repository.Repositories.ArchiveRepo(ctx, repositoryPath)
	} else {
		_, err = client.clients.repository.Repositories.UnArchiveRepo(ctx, repositoryPath)
	}

	return client.failure.wrap("set archive state "+repositoryPath, err)
}

func (client *cnbClient) reconcilePullRequests(
	ctx context.Context,
	repositoryPath string,
	desired Repository,
) error {
	current, _, err := client.clients.repository.Gitsettings.GetPullRequestSettings(
		ctx,
		repositoryPath,
	)
	if err != nil {
		return client.failure.wrap("read pull-request settings "+repositoryPath, err)
	}

	update := patch{
		"allow_merge_commit_merge":    current.AllowMergeCommitMerge,
		"allow_rebase_merge":          current.AllowRebaseMerge,
		"allow_squash_merge":          current.AllowSquashMerge,
		"master_auto_as_reviewer":     current.MasterAutoAsReviewer,
		"merge_commit_message_style":  defaultValue(current.MergeCommitMessageStyle, "default"),
		"squash_commit_message_style": defaultValue(current.SquashCommitMessageStyle, "default"),
	}
	changed := false

	if desired.AllowMergeCommit != current.AllowMergeCommitMerge {
		update["allow_merge_commit_merge"] = desired.AllowMergeCommit
		changed = true
	}

	if desired.AllowRebaseMerge != current.AllowRebaseMerge {
		update["allow_rebase_merge"] = desired.AllowRebaseMerge
		changed = true
	}

	if desired.AllowSquashMerge != current.AllowSquashMerge {
		update["allow_squash_merge"] = desired.AllowSquashMerge
		changed = true
	}

	if !changed {
		return nil
	}

	return client.failure.wrap(
		"update pull-request settings "+repositoryPath,
		client.request(
			ctx,
			client.clients.repository,
			http.MethodPut,
			"/"+repositoryPath+"/-/settings/pull-request",
			update,
		),
	)
}

func (client *cnbClient) reconcileLabels(
	ctx context.Context,
	repository Repository,
	desired []Label,
) error {
	repositoryPath := client.repositoryPath(repository)
	pageSize := client.services.pages(client.platform)
	existing := make([]*api.Label, 0)

	err := paginate(pageSize, func(page int) ([]*api.Label, error) {
		labels, _, err := client.clients.repository.Repolabels.ListLabels(
			ctx,
			repositoryPath,
			&cnb.ListLabelsOptions{Page: page, PageSize: pageSize},
		)

		return labels, client.failure.wrap("list labels "+repositoryPath, err)
	}, func(label *api.Label) error {
		existing = append(existing, label)

		return nil
	})
	if err != nil {
		return err
	}

	return reconcileNamed(desired, existing, namedOperations[Label, *api.Label]{
		desiredName:  func(label Label) string { return label.Name },
		existingName: func(label *api.Label) string { return label.Name },
		equal: func(desired Label, current *api.Label) bool {
			return desired.Name == current.Name && sameColor(desired.Color, current.Color) &&
				desired.Description == current.Description
		},
		create: func(label Label) error {
			_, _, err := client.clients.repository.Repolabels.PostLabel(
				ctx,
				repositoryPath,
				&cnb.PostLabelRequest{
					Name:        label.Name,
					Color:       label.Color,
					Description: label.Description,
				},
			)

			return client.failure.wrap("create label "+repositoryPath+"/"+label.Name, err)
		},
		update: func(desired Label, current *api.Label) error {
			return client.failure.wrap(
				"update label "+repositoryPath+"/"+current.Name,
				client.request(
					ctx,
					client.clients.repository,
					http.MethodPatch,
					"/"+repositoryPath+"/-/labels/"+url.PathEscape(current.Name),
					patch{
						"new_name":       desired.Name,
						"color":          desired.Color,
						fieldDescription: desired.Description,
					},
				),
			)
		},
		remove: func(label *api.Label) error {
			return client.failure.wrap(
				"delete label "+repositoryPath+"/"+label.Name,
				client.request(
					ctx,
					client.clients.repository,
					http.MethodDelete,
					"/"+repositoryPath+"/-/labels/"+url.PathEscape(label.Name),
					nil,
				),
			)
		},
	})
}

func (client *cnbClient) uploadAvatar(ctx context.Context, avatar Avatar) error {
	extension := avatarExtension(avatar.ContentType)

	upload, _, err := client.clients.organization.Organizations.UploadLogos(
		ctx,
		client.root,
		&cnb.UploadLogosRequest{
			Name: "logo." + extension,
			Size: len(avatar.Data),
			Ext:  map[string]string{"format": extension},
		},
	)
	if err != nil {
		return client.failure.wrap("upload avatar "+client.root, err)
	}

	if upload.UploadUrl == "" {
		return client.failure.wrap(
			"upload avatar "+client.root,
			errUploadURLMissing,
		)
	}

	return client.services.http.uploadForm(
		ctx,
		upload.UploadUrl,
		upload.Form,
		"logo."+extension,
		avatar.Data,
	)
}

func (client *cnbClient) snapshot(ctx context.Context) (*platformState, error) {
	snapshot := newPlatformState()

	organization, _, err := client.clients.organization.Organizations.GetGroup(ctx, client.root)
	if err != nil {
		return nil, client.failure.wrap("read organization "+client.root, err)
	}

	if organization.Path != "" {
		record, err := newRecord(map[string]any{
			fieldPath:        organization.Path,
			fieldName:        organization.Name,
			fieldDescription: organization.Description,
			"site":           organization.Site,
			fieldEmail:       organization.Email,
		})
		if err != nil {
			return nil, err
		}

		snapshot.Organization = record
	}

	pageSize := client.services.pages(client.platform)

	err = paginate(pageSize, func(page int) ([]*dto.Repos4User, error) {
		repositories, _, err := client.clients.repository.Repositories.GetGroupSubRepos(
			ctx,
			client.root,
			&cnb.GetGroupSubReposOptions{Page: page, PageSize: pageSize, Descendant: valueAll},
		)
		if err != nil {
			return nil, client.failure.wrap("list repositories "+client.root, err)
		}

		return repositories, nil
	}, func(repository *dto.Repos4User) error {
		if repository.Path != "" {
			record, err := newRecord(map[string]any{
				fieldPath:        repository.Path,
				fieldName:        repository.Name,
				fieldDescription: repository.Description,
				"site":           repository.Site,
				fieldTopics:      splitTopics(repository.Topics),
				"license":        repository.License,
				fieldArchived:    repository.Status == constant.RepoStatusArchived,
			})
			if err != nil {
				return err
			}

			snapshot.Repositories[repository.Path] = record
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return snapshot, nil
}

func (client *cnbClient) reconcileReleases(
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

func (client *cnbClient) releaseOperations() releaseOps[*api.Release] {
	return releaseOps[*api.Release]{
		pageSize: client.services.pages(client.platform),
		list:     client.listReleases,
		create:   client.createRelease,
		update:   client.updateRelease,
		view:     viewCNBRelease,
		assets: assetOps{
			matches: client.releaseAssetMatches,
			replace: client.replaceReleaseAsset,
			remove:  client.removeReleaseAsset,
		},
	}
}

func (client *cnbClient) listReleases(
	ctx context.Context,
	repositoryPath string,
	page, pageSize int,
) ([]*api.Release, error) {
	releases, _, err := client.clients.release.Releases.ListReleases(
		ctx,
		repositoryPath,
		&cnb.ListReleasesOptions{Page: page, PageSize: pageSize},
	)
	if err != nil {
		return nil, client.failure.wrap("list releases "+repositoryPath, err)
	}

	return releases, nil
}

func (client *cnbClient) createRelease(
	ctx context.Context,
	repositoryPath string,
	release Release,
) (*api.Release, error) {
	created, _, err := client.clients.release.Releases.PostRelease(
		ctx,
		repositoryPath,
		&cnb.PostReleaseRequest{
			TagName:         release.TagName,
			Name:            release.Name,
			Body:            release.Body,
			Draft:           release.Draft,
			Prerelease:      release.Prerelease,
			MakeLatest:      strconv.FormatBool(release.Latest),
			TargetCommitish: release.TargetCommitish,
		},
	)
	if err != nil {
		return nil, client.failure.wrap("create release "+release.TagName, err)
	}

	return created, nil
}

func (client *cnbClient) updateRelease(
	ctx context.Context,
	repositoryPath string,
	current releaseState,
	desired Release,
) error {
	update := make(patch)
	set(update, fieldName, desired.Name, current.name)
	set(update, "body", desired.Body, current.body)
	set(update, "draft", desired.Draft, current.draft)
	set(update, "prerelease", desired.Prerelease, current.pre)
	set(
		update,
		"make_latest",
		strconv.FormatBool(desired.Latest),
		strconv.FormatBool(current.latest),
	)

	if len(update) == 0 {
		return nil
	}

	return client.failure.wrap(
		"update release "+desired.TagName,
		client.request(
			ctx,
			client.clients.release,
			http.MethodPatch,
			"/"+repositoryPath+"/-/releases/"+current.id,
			update,
		),
	)
}

func viewCNBRelease(source *api.Release) releaseState {
	assets := make([]assetState, len(source.Assets))
	for index, asset := range source.Assets {
		assets[index] = assetState{
			id:   asset.Id,
			name: asset.Name,
			size: int64(asset.Size),
			hash: assetHash(asset.HashValue),
		}
	}

	return releaseState{
		id: source.Id, tag: source.TagName, name: source.Name, body: source.Body,
		draft: source.Draft, pre: source.Prerelease, latest: source.IsLatest, assets: assets,
	}
}

func (*cnbClient) releaseAssetMatches(
	_ context.Context,
	_, _ string,
	current assetState,
	desired ReleaseAsset,
) (bool, error) {
	hash := assetHash(desired.Digest)

	return hash != "" && current.hash == hash, nil
}

func (client *cnbClient) replaceReleaseAsset(
	ctx context.Context,
	repositoryPath, releaseID string,
	current *assetState,
	desired ReleaseAsset,
) error {
	upload, _, err := client.clients.release.Releases.PostReleaseAssetUploadURL(
		ctx,
		repositoryPath,
		releaseID,
		&cnb.PostReleaseAssetUploadURLRequest{
			AssetName: desired.Name,
			Overwrite: current != nil,
			Size:      int(desired.Size),
			Ttl:       client.services.uploadTTL,
		},
	)
	if err != nil {
		return client.failure.wrap("upload release asset "+releaseID+"/"+desired.Name, err)
	}

	if upload.UploadUrl == "" {
		return client.failure.wrap(
			"upload release asset "+desired.Name,
			errUploadURLMissing,
		)
	}

	content, err := desired.open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = content.Close() }()

	err = client.services.http.upload(
		ctx,
		upload.UploadUrl,
		desired.ContentType,
		content,
	)
	if err != nil {
		return err
	}

	if upload.VerifyUrl != "" {
		return client.services.http.confirm(ctx, upload.VerifyUrl)
	}

	return nil
}

func (client *cnbClient) removeReleaseAsset(
	ctx context.Context,
	repositoryPath, releaseID, assetID string,
) error {
	_, err := client.clients.release.Releases.DeleteReleaseAsset(
		ctx,
		repositoryPath,
		releaseID,
		assetID,
	)

	return client.failure.wrap("delete release asset "+releaseID+"/"+assetID, err)
}

func (client *cnbClient) request(
	ctx context.Context,
	api *cnb.Client,
	method, endpoint string,
	body any,
) error {
	request, err := api.NewRequest(method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create CNB request: %w", err)
	}

	response, err := api.Client().Do(request.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("perform CNB request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return nil
	}

	content, _ := io.ReadAll(io.LimitReader(response.Body, client.services.http.limit))

	return httpFailure("CNB request", response.StatusCode, content)
}

func splitTopics(value string) []string {
	return canonical(strings.FieldsFunc(value, func(character rune) bool {
		return character == ',' || character == ' ' || character == '\n' || character == '\t'
	}))
}

func defaultValue(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}
