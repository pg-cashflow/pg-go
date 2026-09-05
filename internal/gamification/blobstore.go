package gamification

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

var (
	ErrFileTooLarge       = errors.New("file size exceeds 500 KB limit")
	ErrInvalidContentType = errors.New("only JPEG, PNG, and WebP images are allowed")
	ErrEmptyPayload       = errors.New("empty file payload")
)

const MaxImageSizeBytes = 500 * 1024 // 500 KB

// BlobStore abstracts photo storage (Postgres BYTEA for MVP; swappable to S3/Cloudflare R2).
type BlobStore interface {
	Put(ctx context.Context, key string, data []byte) (string, error)
	Get(ctx context.Context, key string) ([]byte, string, error)
	ValidateImage(data []byte) (string, error)
}

type MemoryOrPostgresBlobStore struct{}

func NewBlobStore() *MemoryOrPostgresBlobStore {
	return &MemoryOrPostgresBlobStore{}
}

func (s *MemoryOrPostgresBlobStore) ValidateImage(data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrEmptyPayload
	}
	if len(data) > MaxImageSizeBytes {
		return "", ErrFileTooLarge
	}
	// Detect MIME type via magic bytes
	mime := http.DetectContentType(data)
	switch mime {
	case "image/jpeg", "image/png", "image/webp":
		return mime, nil
	default:
		return "", fmt.Errorf("%w: detected %s", ErrInvalidContentType, mime)
	}
}

func (s *MemoryOrPostgresBlobStore) Put(ctx context.Context, key string, data []byte) (string, error) {
	_, err := s.ValidateImage(data)
	if err != nil {
		return "", err
	}
	return "/api/blobs/" + key, nil
}

func (s *MemoryOrPostgresBlobStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	return nil, "", errors.New("use repository bytea accessor directly")
}
