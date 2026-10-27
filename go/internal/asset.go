package actions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

type cachedAsset struct {
	fetch func(context.Context) (io.ReadCloser, error)
	mutex sync.Mutex
	path  string
}

func (asset *cachedAsset) open(ctx context.Context) (io.ReadCloser, error) {
	asset.mutex.Lock()
	defer asset.mutex.Unlock()

	if asset.path == "" {
		source, err := asset.fetch(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = source.Close() }()

		asset.path, err = writeTemp(source)
		if err != nil {
			return nil, err
		}
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
