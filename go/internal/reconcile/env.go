package reconcile

import (
	"cmp"
	"fmt"
	"os"
	"strconv"
	"time"
)

//nolint:ireturn
func envValue[T any](key string, fallback T, parse func(string) (T, error)) (T, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return fallback, nil
	}

	return parse(value)
}

func boundedInteger(name string, minimum, maximum int) func(string) (int, error) {
	return func(value string) (int, error) {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < minimum || parsed > maximum {
			return 0, configurationError(fmt.Sprintf(
				"%s must be an integer between %d and %d",
				name,
				minimum,
				maximum,
			))
		}

		return parsed, nil
	}
}

func positiveInteger(name string) func(string) (int, error) {
	return func(value string) (int, error) {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return 0, configurationError(name + " must be a positive integer")
		}

		return parsed, nil
	}
}

func positiveInteger64(name string) func(string) (int64, error) {
	return func(value string) (int64, error) {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 1 {
			return 0, configurationError(name + " must be a positive integer")
		}

		return parsed, nil
	}
}

func positiveDuration(name string) func(string) (time.Duration, error) {
	return func(value string) (time.Duration, error) {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return 0, configurationError(name + " must be a positive duration (e.g. 60s)")
		}

		return parsed, nil
	}
}

func boolean(name string) func(string) (bool, error) {
	return func(value string) (bool, error) {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return false, configurationError(name + " must be true or false")
		}

		return parsed, nil
	}
}

func envDefault(key, fallback string) string {
	return cmp.Or(os.Getenv(key), fallback)
}

func environmentStateFile() string {
	if value, ok := os.LookupEnv("STATE_FILE"); ok {
		return value
	}

	return defaultStateFile
}
