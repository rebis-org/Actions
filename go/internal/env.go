package actions

import (
	"cmp"
	"fmt"
	"net/url"
	"os"
	"strconv"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

type configurationError string

func (err configurationError) Error() string {
	return string(err)
}

//nolint:ireturn
func envValue[T any](key string, parse func(string) (T, error)) (T, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		var zero T

		return zero, nil
	}

	parsed, err := parse(value)
	if err != nil {
		var zero T

		return zero, configurationError(fmt.Sprintf("%s has an invalid value %q", key, value))
	}

	return parsed, nil
}

func envDefault(key, fallback string) string {
	return cmp.Or(os.Getenv(key), fallback)
}

func parseInt64(value string) (int64, error) {
	return strconv.ParseInt(value, 10, 64) //nolint:wrapcheck // caller replaces the error
}

func validEndpointURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return false
	}

	return parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" &&
		parsed.Fragment == "" && (parsed.Scheme == schemeHTTP || parsed.Scheme == schemeHTTPS)
}
