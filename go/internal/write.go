package actions

import (
	"errors"
	"reflect"
)

//nolint:ireturn
func ensureValue[T any](
	platform Platform,
	resource string,
	read func() (T, error),
	create func() error,
) (T, error) {
	current, err := read()
	if err == nil {
		return current, nil
	}

	if !errors.Is(err, errNotFound) {
		var zero T

		return zero, platform.wrap("read "+resource, err)
	}

	err = create()
	if err != nil {
		var zero T

		return zero, platform.wrap("create "+resource, err)
	}

	current, err = read()
	if err != nil {
		var zero T

		return zero, platform.wrap("read "+resource, err)
	}

	return current, nil
}

type patch map[string]any

func set[T comparable](patch patch, field string, desired, current T) {
	if desired != current {
		patch[field] = desired
	}
}

func changed(value any) bool {
	return !reflect.ValueOf(value).IsZero()
}

func changedPointer[T comparable](desired, current T) *T {
	if desired == current {
		return nil
	}

	return new(desired)
}
