package actions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var errNotFound = errors.New("not found")

type services struct {
	http               transport
	assetRetentionDays int
	concurrency        int
}

func newServices(config Config) services {
	return services{
		http: newTransport(
			config.HTTPClient,
			config.HTTPTimeout,
			config.ResponseSize,
			config.Bypass,
		),
		assetRetentionDays: config.CNBUploadTTL,
		concurrency:        config.Concurrency,
	}
}

func newTransport(
	client *http.Client,
	timeout time.Duration,
	limit int64,
	bypass Bypass,
) transport {
	hardened := http.Client{}
	if client != nil {
		hardened = *client
	}

	hardened.Timeout = timeout

	if hardened.Transport == nil {
		hardened.Transport = tunedTransport()
	}

	if bypass.Token != "" {
		hardened.Transport = bypassTransport{base: hardened.Transport, bypass: bypass}
	}

	return transport{client: &hardened, limit: limit}
}

func tunedTransport() http.RoundTripper {
	standard, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}

	tuned := standard.Clone()
	tuned.MaxIdleConnsPerHost = maximumConcurrency

	return tuned
}

func defaultTransport() transport {
	return newTransport(nil, defaultHTTPTimeout, defaultResponseSize, Bypass{})
}

type transport struct {
	client *http.Client
	limit  int64
}

type bypassTransport struct {
	base   http.RoundTripper
	bypass Bypass
}

func (transport bypassTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.bypass.matches(request.URL.String()) {
		request = request.Clone(request.Context())
		request.Header.Set(bypassHeader, transport.bypass.Token)
	}

	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, fmt.Errorf("round trip: %w", err)
	}

	return response, nil
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
		return httpResponse{}, fmt.Errorf("perform request: %w", sanitizeTransportError(err))
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

func (transport transport) stream(
	request *http.Request,
	operation string,
) (io.ReadCloser, error) {
	response, err := transport.client.Do(request) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, sanitizeTransportError(err))
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

func (transport transport) upload(
	ctx context.Context,
	operation, method, url, contentType string,
	body io.Reader,
) error {
	request, err := newRequest(ctx, method, url, body)
	if err != nil {
		return err
	}

	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	return transport.send(request, operation)
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

	return transport.send(request, "upload")
}

var errRequestWithoutCause = errors.New("request failed without cause")

func sanitizeTransportError(err error) error {
	urlErr, ok := errors.AsType[*url.Error](err)
	if !ok {
		return err
	}

	if urlErr.Err != nil {
		return fmt.Errorf("%s: %w", strings.ToLower(urlErr.Op), urlErr.Err)
	}

	return fmt.Errorf("%s: %w", strings.ToLower(urlErr.Op), errRequestWithoutCause)
}
