package reconcile

import (
	"errors"
	"strings"
)

type namedOperations[D, E any] struct {
	desiredName  func(D) string
	existingName func(E) string
	equal        func(D, E) bool
	create       func(D) error
	update       func(D, E) error
	remove       func(E) error
}

func reconcileNamed[D, E any](desired []D, existing []E, operations namedOperations[D, E]) error {
	byName := make(map[string]E, len(existing))
	for _, item := range existing {
		byName[metadataKey(operations.existingName(item))] = item
	}

	wanted := make(map[string]bool, len(desired))
	failures := make([]error, 0)

	for _, item := range desired {
		key := metadataKey(operations.desiredName(item))
		wanted[key] = true

		current, found := byName[key]
		if !found {
			failures = append(failures, operations.create(item))
		} else if !operations.equal(item, current) {
			failures = append(failures, operations.update(item, current))
		}
	}

	err := errors.Join(failures...)
	if err != nil {
		return err
	}

	failures = failures[:0]

	for _, item := range existing {
		if !wanted[metadataKey(operations.existingName(item))] {
			failures = append(failures, operations.remove(item))
		}
	}

	return errors.Join(failures...)
}

func metadataKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func sameColor(left, right string) bool {
	return strings.EqualFold(strings.TrimPrefix(left, "#"), strings.TrimPrefix(right, "#"))
}
