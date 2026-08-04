package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	stateVersion  = 1
	stateFileMode = 0o644
	stateDirMode  = 0o755
)

type record struct {
	Data map[string]any `json:"data"`
	Hash string         `json:"hash"`
}

func newRecord(data map[string]any) (*record, error) {
	content, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode state record: %w", err)
	}

	hash := sha256.Sum256(content)

	return &record{Data: data, Hash: hex.EncodeToString(hash[:])}, nil
}

type platformState struct {
	Organization *record            `json:"organization,omitempty"`
	Repositories map[string]*record `json:"repositories,omitempty"`
}

func newPlatformState() *platformState {
	return &platformState{Repositories: make(map[string]*record)}
}

type state struct {
	Version     int
	GeneratedAt time.Time
	Platforms   map[Platform]*platformState
	Unknown     map[string]json.RawMessage
}

type stateSnapshot struct {
	platform Platform
	state    *platformState
}

func newState(now time.Time) *state {
	return &state{
		Version:     stateVersion,
		GeneratedAt: now.UTC(),
		Platforms:   make(map[Platform]*platformState),
		Unknown:     make(map[string]json.RawMessage),
	}
}

func (state state) MarshalJSON() ([]byte, error) {
	payload := make(map[string]any)
	for name, content := range state.Unknown {
		payload[name] = content
	}

	payload["version"] = state.Version

	payload["generated_at"] = state.GeneratedAt
	for platform, snapshot := range state.Platforms {
		payload[string(platform)] = snapshot
	}

	content, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode state payload: %w", err)
	}

	return content, nil
}

func (state *state) UnmarshalJSON(content []byte) error {
	var payload map[string]json.RawMessage

	err := json.Unmarshal(content, &payload)
	if err != nil {
		return fmt.Errorf("decode state payload: %w", err)
	}

	err = json.Unmarshal(payload["version"], &state.Version)
	if err != nil {
		return fmt.Errorf("decode state version: %w", err)
	}

	err = json.Unmarshal(payload["generated_at"], &state.GeneratedAt)
	if err != nil {
		return fmt.Errorf("decode state timestamp: %w", err)
	}

	delete(payload, "version")
	delete(payload, "generated_at")

	state.Platforms = make(map[Platform]*platformState, len(payload))
	state.Unknown = make(map[string]json.RawMessage)

	for name, content := range payload {
		platform := Platform(name)
		if !knownPlatform(platform) {
			state.Unknown[name] = slices.Clone(content)

			continue
		}

		var snapshot platformState

		err := json.Unmarshal(content, &snapshot)
		if err != nil {
			return fmt.Errorf("decode %s state: %w", name, err)
		}

		if snapshot.Repositories == nil {
			snapshot.Repositories = make(map[string]*record)
		}

		state.Platforms[platform] = &snapshot
	}

	return nil
}

func knownPlatform(platform Platform) bool {
	if platform == PlatformGitHub {
		return true
	}

	_, known := targetSpecification(platform)

	return known
}

func githubSnapshot(organization *Organization, repositories []Repository) (*platformState, error) {
	snapshot := newPlatformState()

	if organization != nil {
		record, err := newRecord(map[string]any{
			fieldName:        organization.Name,
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

	for _, repository := range repositories {
		record, err := newRecord(map[string]any{
			"owner":              repository.Owner,
			fieldName:            repository.Name,
			fieldDescription:     repository.Description,
			"homepage":           repository.Homepage,
			fieldTopics:          canonical(repository.Topics),
			"default_branch":     repository.DefaultBranch,
			"license":            repository.License,
			fieldArchived:        repository.Archived,
			"has_issues":         repository.HasIssues,
			"has_wiki":           repository.HasWiki,
			"has_projects":       repository.HasProjects,
			"has_downloads":      repository.HasDownloads,
			"allow_squash_merge": repository.AllowSquashMerge,
			"allow_merge_commit": repository.AllowMergeCommit,
			"allow_rebase_merge": repository.AllowRebaseMerge,
		})
		if err != nil {
			return nil, err
		}

		snapshot.Repositories[repository.fullName()] = record
	}

	return snapshot, nil
}

func (current *state) diff(previous *state) []string {
	if previous == nil {
		return nil
	}

	return diffMap("", current.Platforms, previous.Platforms, diffPlatform)
}

func diffPlatform(name string, current, previous *platformState) []string {
	changes := diffRecord(name+" organization", current.Organization, previous.Organization)

	changes = append(changes, diffMap(
		name+" repository ",
		current.Repositories,
		previous.Repositories,
		diffRecord,
	)...)

	return changes
}

func diffMap[K ~string, V any](
	prefix string,
	current, previous map[K]V,
	modified func(string, V, V) []string,
) []string {
	changes := make([]string, 0)

	for _, key := range orderedKeys(current, previous) {
		before, hadBefore := previous[key]
		after, hasAfter := current[key]
		label := prefix + string(key)

		switch {
		case !hadBefore:
			changes = append(changes, label+" added")
		case !hasAfter:
			changes = append(changes, label+" removed")
		default:
			changes = append(changes, modified(label, after, before)...)
		}
	}

	return changes
}

func diffRecord(label string, current, previous *record) []string {
	switch {
	case previous == nil && current != nil:
		return []string{label + " added"}
	case previous != nil && current == nil:
		return []string{label + " removed"}
	case previous != nil && current != nil && previous.Hash != current.Hash:
		return []string{label + " modified: " + fieldChanges(previous.Data, current.Data)}
	default:
		return nil
	}
}

func fieldChanges(previous, current map[string]any) string {
	changes := make([]string, 0)

	for _, key := range orderedKeys(previous, current) {
		before, hadBefore := previous[key]

		after, hasAfter := current[key]
		switch {
		case !hadBefore:
			changes = append(changes, key+" added")
		case !hasAfter:
			changes = append(changes, key+" removed")
		case fmt.Sprint(before) != fmt.Sprint(after):
			changes = append(changes, fmt.Sprintf("%s: %v -> %v", key, before, after))
		}
	}

	return strings.Join(changes, ", ")
}

func orderedKeys[K ~string, V any](maps ...map[K]V) []K {
	unique := make(map[K]bool)

	for _, values := range maps {
		for key := range values {
			unique[key] = true
		}
	}

	keys := make([]K, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	return keys
}

func recordState(
	ctx context.Context,
	config Config,
	organization *Organization,
	repositories []Repository,
	targets []mirrorTarget,
) {
	if config.StateFile == "" {
		return
	}

	previous, err := loadState(config.StateFile)
	if err != nil {
		slog.Warn("cannot load state", "path", config.StateFile, "error", err)

		return
	}

	current := newState(config.Now())

	github, err := githubSnapshot(organization, repositories)
	if err != nil {
		slog.Warn("cannot snapshot GitHub", "error", err)

		return
	}

	current.Platforms[PlatformGitHub] = github

	snapshots, err := snapshotTargets(ctx, targets)
	if err != nil {
		slog.Warn("cannot record state", "error", err)

		return
	}

	for _, snapshot := range snapshots {
		if snapshot.state != nil {
			current.Platforms[snapshot.platform] = snapshot.state
		}
	}

	preserveUnknown(current, previous)

	if !config.Force {
		for _, change := range current.diff(previous) {
			slog.Info("state change", "change", change)
		}
	}

	err = current.save(config.StateFile)
	if err != nil {
		slog.Warn("cannot write state", "path", config.StateFile, "error", err)
	}
}

func snapshotTargets(
	ctx context.Context,
	targets []mirrorTarget,
) ([]stateSnapshot, error) {
	return parallel(
		ctx,
		len(targets),
		targets,
		func(target mirrorTarget) (stateSnapshot, error) {
			snapshot, err := target.provider.snapshot(ctx)
			if err != nil {
				return stateSnapshot{}, platformError(target.platform).wrap("snapshot", err)
			}

			return stateSnapshot{platform: target.platform, state: snapshot}, nil
		},
	)
}

func preserveUnknown(current, previous *state) {
	if previous == nil {
		return
	}

	for name, content := range previous.Unknown {
		current.Unknown[name] = slices.Clone(content)
	}
}

func loadState(path string) (*state, error) {
	content, err := os.ReadFile(path) //nolint:gosec
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil
	}

	if err != nil {
		return nil, fmt.Errorf("read state file %s: %w", path, err)
	}

	var decoded state

	err = json.Unmarshal(content, &decoded)
	if err != nil {
		return nil, fmt.Errorf("decode state file %s: %w", path, err)
	}

	if decoded.Version != stateVersion {
		return nil, stateVersionError{path: path, actual: decoded.Version}
	}

	return &decoded, nil
}

func (state *state) save(path string) error {
	content, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	directory := filepath.Dir(path)

	err = os.MkdirAll(directory, stateDirMode)
	if err != nil {
		return fmt.Errorf("create state directory %s: %w", directory, err)
	}

	temporary, err := os.CreateTemp(directory, "state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}

	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	_, err = temporary.Write(content)
	if err != nil {
		_ = temporary.Close()

		return fmt.Errorf("write temporary state file: %w", err)
	}

	err = temporary.Chmod(stateFileMode)
	if err != nil {
		_ = temporary.Close()

		return fmt.Errorf("set state file mode: %w", err)
	}

	err = temporary.Sync()
	if err != nil {
		_ = temporary.Close()

		return fmt.Errorf("sync temporary state file: %w", err)
	}

	err = temporary.Close()
	if err != nil {
		return fmt.Errorf("close temporary state file: %w", err)
	}

	err = os.Rename(temporaryPath, path)
	if err != nil {
		return fmt.Errorf("replace state file %s: %w", path, err)
	}

	return nil
}

type stateVersionError struct {
	path   string
	actual int
}

func (err stateVersionError) Error() string {
	return fmt.Sprintf(
		"state file %s has version %d, expected %d",
		err.path,
		err.actual,
		stateVersion,
	)
}
