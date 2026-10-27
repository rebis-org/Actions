package actions

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

const (
	defaultConcurrency = 2
	maximumConcurrency = 10
)

func boundedConcurrency(value int, environment string) (int, error) {
	value = cmp.Or(value, defaultConcurrency)

	if value < 1 || value > maximumConcurrency {
		return 0, configurationError(fmt.Sprintf(
			"%s must be an integer between 1 and %d",
			environment,
			maximumConcurrency,
		))
	}

	return value, nil
}

type outcome[T any] struct {
	value T
	err   error
}

func cancellation(ctx context.Context) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("run canceled: %w", err)
	}

	return nil
}

func parallel[I, O any](
	ctx context.Context,
	limit int,
	inputs []I,
	run func(I) (O, error),
) ([]O, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	results := make([]outcome[O], len(inputs))

	var next atomic.Int64

	worker := func() {
		for {
			position := int(next.Add(1)) - 1
			if position >= len(inputs) {
				return
			}

			err := ctx.Err()
			if err != nil {
				results[position].err = err

				continue
			}

			results[position].value, results[position].err = run(inputs[position])
		}
	}

	var workers sync.WaitGroup

	for range max(1, min(limit, len(inputs))) {
		workers.Go(worker)
	}

	workers.Wait()

	values := make([]O, 0, len(inputs))
	failures := make([]error, 0)

	for _, result := range results {
		if result.err != nil {
			failures = append(failures, result.err)
		} else {
			values = append(values, result.value)
		}
	}

	return values, errors.Join(failures...)
}

func each[I any](ctx context.Context, limit int, inputs []I, run func(I) error) error {
	_, err := parallel(ctx, limit, inputs, func(input I) (struct{}, error) {
		return struct{}{}, run(input)
	})

	return err
}

func paginate[T any](pageSize int, fetch func(int) ([]T, error), each func(T) error) error {
	for page := 1; ; page++ {
		items, err := fetch(page)
		if err != nil {
			return err
		}

		for _, item := range items {
			err := each(item)
			if err != nil {
				return err
			}
		}

		if len(items) < pageSize {
			return nil
		}
	}
}

func collect[T any](pageSize int, fetch func(int) ([]T, error)) ([]T, error) {
	all := make([]T, 0)

	err := paginate(pageSize, fetch, func(item T) error {
		all = append(all, item)

		return nil
	})

	return all, err
}

func canonical(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)

	return values
}

func transform[I, O any](items []I, convert func(I) O) []O {
	converted := make([]O, len(items))

	for index, item := range items {
		converted[index] = convert(item)
	}

	return converted
}
