package actions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var errResponseTooLarge = errors.New("response exceeds configured limit")

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
		return Avatar{}, fmt.Errorf("download avatar: %w", sanitizeTransportError(err))
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
