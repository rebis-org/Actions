package reconcile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

var errNotFound = errors.New("not found")

var errResponseTooLarge = errors.New("response exceeds configured limit")

type services struct {
	http      transport
	pageSize  int
	uploadTTL int
	now       func() time.Time
}

func newServices(config Config) services {
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{}
	}

	clientCopy := *client
	clientCopy.Timeout = config.HTTPTimeout

	return services{
		http:      transport{client: &clientCopy, limit: config.ResponseSize},
		pageSize:  config.PageSize,
		uploadTTL: config.CNBUploadTTL,
		now:       config.Now,
	}
}

func (services services) pages(platform Platform) int {
	if services.pageSize > 0 {
		return services.pageSize
	}

	if platform == PlatformGitHub {
		return defaultGitHubPages
	}

	spec, _ := targetSpecification(platform)

	return spec.pageSize
}

type transport struct {
	client *http.Client
	limit  int64
}

type httpResponse struct {
	status int
	body   []byte
}

type httpStatusError struct {
	operation string
	status    int
	message   string
}

func (err httpStatusError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", err.operation, err.status, err.message)
}

func (transport transport) perform(request *http.Request) (httpResponse, error) {
	response, err := transport.client.Do(request) //nolint:gosec
	if err != nil {
		return httpResponse{}, fmt.Errorf("perform request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, transport.limit))
	if err != nil {
		return httpResponse{}, fmt.Errorf("read response: %w", err)
	}

	return httpResponse{status: response.StatusCode, body: body}, nil
}

func (transport transport) send(request *http.Request, operation string) error {
	response, err := transport.perform(request)
	if err != nil {
		return err
	}

	return response.requireSuccess(operation)
}

func (transport transport) stream(request *http.Request, operation string) (io.ReadCloser, error) {
	response, err := transport.client.Do(request) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}

	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return response.Body, nil
	}
	defer func() { _ = response.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(response.Body, transport.limit))

	return nil, httpFailure(operation, response.StatusCode, body)
}

func (response httpResponse) requireSuccess(operation string) error {
	if response.status >= http.StatusOK && response.status < http.StatusMultipleChoices {
		return nil
	}

	return httpFailure(operation, response.status, response.body)
}

func httpFailure(operation string, status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(status)
	}

	return httpStatusError{operation: operation, status: status, message: message}
}

func newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("create %s request: %w", strings.ToLower(method), err)
	}

	return request, nil
}

func (transport transport) uploadForm(
	ctx context.Context,
	url string,
	fields map[string]string,
	filename string,
	data []byte,
) error {
	var body bytes.Buffer

	form := multipart.NewWriter(&body)
	for key, value := range fields {
		err := form.WriteField(key, value)
		if err != nil {
			return fmt.Errorf("write form field %s: %w", key, err)
		}
	}

	file, err := form.CreateFormFile("file", filename)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}

	_, err = file.Write(data)
	if err != nil {
		return fmt.Errorf("write form data: %w", err)
	}

	err = form.Close()
	if err != nil {
		return fmt.Errorf("close form: %w", err)
	}

	request, err := newRequest(ctx, http.MethodPost, url, &body)
	if err != nil {
		return err
	}

	request.Header.Set("Content-Type", form.FormDataContentType())

	return transport.send(request, "upload to "+url)
}

func (transport transport) upload(
	ctx context.Context,
	url, contentType string,
	body io.Reader,
) error {
	request, err := newRequest(ctx, http.MethodPut, url, body)
	if err != nil {
		return err
	}

	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	return transport.send(request, "upload to "+url)
}

func (transport transport) confirm(ctx context.Context, url string) error {
	request, err := newRequest(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	return transport.send(request, "confirm upload to "+url)
}

type platformError Platform

func (platform platformError) wrap(operation string, err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%s: %s: %w", platformLabel(Platform(platform)), operation, err)
}

func platformLabel(platform Platform) string {
	switch platform {
	case PlatformGitHub:
		return "GitHub"
	case PlatformCNB:
		return "CNB"
	case PlatformCodeberg:
		return "Codeberg"
	default:
		return string(platform)
	}
}

//nolint:ireturn
func ensureValue[T any](
	platform platformError,
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

type Avatar struct {
	Data        []byte
	ContentType string
	SourceURL   string
}

func (transport transport) downloadAvatar(ctx context.Context, url string) (Avatar, error) {
	request, err := newRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Avatar{}, err
	}

	response, err := transport.client.Do(request)
	if err != nil {
		return Avatar{}, fmt.Errorf("download avatar: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return Avatar{}, httpFailure("download avatar", response.StatusCode, nil)
	}

	content, err := io.ReadAll(io.LimitReader(response.Body, transport.limit+1))
	if err != nil {
		return Avatar{}, fmt.Errorf("read avatar: %w", err)
	}

	if int64(len(content)) > transport.limit {
		return Avatar{}, fmt.Errorf(
			"%w: avatar limit %d bytes",
			errResponseTooLarge,
			transport.limit,
		)
	}

	return Avatar{
		Data:        content,
		ContentType: response.Header.Get("Content-Type"),
		SourceURL:   url,
	}, nil
}

func (transport transport) retryAvatar(
	ctx context.Context,
	avatar Avatar,
	upload func(context.Context, Avatar) error,
) error {
	var last error

	for _, size := range [...]int{0, 256, 128, 64} {
		candidate := avatar
		if size > 0 {
			if avatar.SourceURL == "" {
				break
			}

			candidate, last = transport.downloadAvatar(ctx, avatarURL(avatar.SourceURL, size))
			if last != nil {
				continue
			}
		}

		last = upload(ctx, candidate)
		if last == nil {
			return nil
		}
	}

	return last
}

func avatarURL(url string, size int) string {
	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}

	return fmt.Sprintf("%s%ss=%d", url, separator, size)
}

func avatarExtension(contentType string) string {
	switch {
	case strings.HasPrefix(contentType, "image/jpeg"):
		return "jpg"
	case strings.HasPrefix(contentType, "image/webp"):
		return "webp"
	case strings.HasPrefix(contentType, "image/gif"):
		return "gif"
	default:
		return "png"
	}
}

type cachedAsset struct {
	fetch func(context.Context) (io.ReadCloser, error)
	mutex sync.Mutex
	path  string
}

func (asset *cachedAsset) open(ctx context.Context) (io.ReadCloser, error) {
	asset.mutex.Lock()
	defer asset.mutex.Unlock()

	if asset.path == "" {
		var source io.ReadCloser

		source, err := asset.fetch(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = source.Close() }()

		path, err := writeTemp(source)
		if err != nil {
			return nil, err
		}

		asset.path = path
	}

	file, err := os.Open(asset.path)
	if err != nil {
		return nil, fmt.Errorf("open cached asset: %w", err)
	}

	return file, nil
}

func (asset *cachedAsset) close() {
	asset.mutex.Lock()
	defer asset.mutex.Unlock()

	if asset.path != "" {
		_ = os.Remove(asset.path)
		asset.path = ""
	}
}

func writeTemp(source io.Reader) (string, error) {
	file, err := os.CreateTemp("", "reconcile-asset-*")
	if err != nil {
		return "", fmt.Errorf("create temporary asset file: %w", err)
	}

	path, keep := file.Name(), false
	defer func() {
		if !keep {
			_ = os.Remove(path)
		}
	}()

	_, err = io.Copy(file, source)
	if err != nil {
		_ = file.Close()

		return "", fmt.Errorf("write temporary asset file: %w", err)
	}

	err = file.Close()
	if err != nil {
		return "", fmt.Errorf("close temporary asset file: %w", err)
	}

	keep = true

	return path, nil
}

func hashReader(reader io.Reader) (string, error) {
	hash := sha256.New()

	_, err := io.Copy(hash, reader)
	if err != nil {
		return "", fmt.Errorf("hash asset: %w", err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

func assetHash(digest string) string {
	_, value, found := strings.Cut(digest, ":")
	if found {
		return value
	}

	return digest
}

func canonical(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)

	return values
}

func normalizedSite(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if !strings.Contains(value, "://") {
		value = "https://" + value
	}

	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}

	return value
}

type clients[T any] struct {
	organization T
	repository   T
	release      T
}

func connectClients[T any](
	credentials Credentials,
	connect func(string) (T, error),
) (clients[T], error) {
	var result clients[T]

	for _, target := range [...]struct {
		slot  *T
		token string
	}{
		{slot: &result.organization, token: credentials.Organization},
		{slot: &result.repository, token: credentials.Repository},
		{slot: &result.release, token: credentials.Release},
	} {
		client, err := connect(target.token)
		if err != nil {
			return clients[T]{}, err
		}

		*target.slot = client
	}

	return result, nil
}
