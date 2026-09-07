package reconcile

import (
	"os"
	"path"
	"slices"
	"strings"
)

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
	return slices.ContainsFunc(patterns, func(pattern string) bool {
		return slices.ContainsFunc([]string{repository.fullName(), repository.Name}, func(candidate string) bool {
			match, _ := path.Match(pattern, candidate)

			return match
		})
	})
}
