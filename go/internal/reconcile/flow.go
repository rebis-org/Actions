package reconcile

import (
	"context"
	"errors"
	"sync"
)

type outcome[T any] struct {
	value T
	err   error
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

	jobs := make(chan int, len(inputs))
	for index := range inputs {
		jobs <- index
	}

	close(jobs)

	var workers sync.WaitGroup
	for range max(1, min(limit, len(inputs))) {
		workers.Go(func() {
			for index := range jobs {
				err := ctx.Err()
				if err != nil {
					results[index].err = err

					continue
				}

				results[index].value, results[index].err = run(inputs[index])
			}
		})
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
