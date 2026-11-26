package vision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

type BlobOpener func(ctx context.Context, refID string) (io.ReadCloser, error)

type BlobReader struct {
	open BlobOpener
}

func NewBlobReader(open BlobOpener) *BlobReader {
	return &BlobReader{open: open}
}

func (r *BlobReader) ReadBlob(ctx context.Context, refID string, maxBytes int64) ([]byte, error) {
	if r == nil || r.open == nil {
		return nil, errors.New("vision blob store is not configured")
	}
	if ctx == nil {
		return nil, errors.New("vision blob read requires a context")
	}
	if strings.TrimSpace(refID) == "" {
		return nil, errors.New("vision blob reference is empty")
	}
	if maxBytes <= 0 {
		return nil, errors.New("vision blob read bound is invalid")
	}
	stream, err := r.open(ctx, refID)
	if err != nil {
		return nil, fmt.Errorf("vision blob open did not complete: %w", err)
	}
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(io.LimitReader(stream, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("vision blob read did not complete: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("vision blob exceeds the read bound")
	}
	return data, nil
}
