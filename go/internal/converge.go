package actions

import (
	"context"
	"errors"
	"strings"
)

type convergence[D, E any] struct {
	desiredKey    func(D) string
	existingKey   func(E) string
	equal         func(D, E) (bool, error)
	create        func(D) (E, error)
	update        func(D, E) error
	remove        func(E) error
	then          func(D, E) error
	slots         chan struct{}
	removeOnError bool
	limit         int
}

type keyed[T any] struct {
	key  string
	item T
}

func converge[D, E any](
	ctx context.Context,
	desired []D,
	existing []E,
	operations convergence[D, E],
) error {
	byKey := make(map[string]E, len(existing))
	for _, item := range existing {
		byKey[operations.existingKey(item)] = item
	}

	pending := transform(desired, func(item D) keyed[D] {
		return keyed[D]{key: operations.desiredKey(item), item: item}
	})

	failures := each(ctx, operations.limit, pending, func(entry keyed[D]) error {
		current, found := byKey[entry.key]

		return operations.apply(ctx, entry, current, found)
	})

	if operations.remove == nil || (failures != nil && !operations.removeOnError) {
		return failures
	}

	wanted := make(map[string]bool, len(pending))
	for _, entry := range pending {
		wanted[entry.key] = true
	}

	removals := each(ctx, operations.limit, existing, func(item E) error {
		if wanted[operations.existingKey(item)] {
			return nil
		}

		return operations.guard(ctx, func() error {
			return operations.remove(item)
		})
	})

	return errors.Join(failures, removals)
}

func (operations convergence[D, E]) guard(ctx context.Context, action func() error) error {
	if operations.slots == nil {
		return action()
	}

	select {
	case operations.slots <- struct{}{}:
	case <-ctx.Done():
		return cancellation(ctx)
	}

	defer func() { <-operations.slots }()

	return action()
}

func (operations convergence[D, E]) apply(
	ctx context.Context,
	entry keyed[D],
	current E,
	found bool,
) error {
	if !found {
		var created E

		err := operations.guard(ctx, func() error {
			var err error

			created, err = operations.create(entry.item)

			return err
		})
		if err != nil || operations.then == nil {
			return err
		}

		return operations.then(entry.item, created)
	}

	if operations.equal != nil {
		var equal bool

		err := operations.guard(ctx, func() error {
			var err error

			equal, err = operations.equal(entry.item, current)

			return err
		})
		if err != nil || equal {
			return err
		}
	}

	err := operations.guard(ctx, func() error {
		return operations.update(entry.item, current)
	})
	if err != nil || operations.then == nil {
		return err
	}

	return operations.then(entry.item, current)
}

func metadataKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func sameColor(left, right string) bool {
	return strings.EqualFold(strings.TrimPrefix(left, "#"), strings.TrimPrefix(right, "#"))
}
