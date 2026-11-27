package storage

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/vision"
)

type visionStoreAdapter struct {
	inner ArtifactStore
}

func NewVisionStore(inner ArtifactStore) vision.ArtifactWriter {
	return &visionStoreAdapter{inner: inner}
}

func (a *visionStoreAdapter) Put(ctx context.Context, r io.Reader, meta *vision.ArtifactMetadata) (vision.ArtifactRef, error) {
	if a == nil || a.inner == nil {
		return vision.ArtifactRef{}, errors.New("vision store is not configured")
	}
	if ctx == nil {
		return vision.ArtifactRef{}, errors.New("vision store write requires a context")
	}
	if meta == nil {
		return vision.ArtifactRef{}, errors.New("vision store metadata must not be nil")
	}
	ref, err := a.inner.Put(ctx, r, &ArtifactMetadata{
		ID:        meta.ID,
		SessionID: meta.SessionID,
		EventID:   meta.EventID,
		Filename:  meta.Filename,
		MediaType: meta.MediaType,
		Metadata:  meta.Metadata,
	})
	if err != nil {
		if code, ok := fault.CodeOf[ErrorCode](err); ok && code == ErrorCodeArtifactQuotaExceeded {
			return vision.ArtifactRef{}, fmt.Errorf("%w: %w", vision.Errorf(vision.ErrorCodeArtifactQuotaExceeded, "artifact quota is exhausted"), err)
		}
		return vision.ArtifactRef{}, err
	}
	return vision.ArtifactRef{ID: ref.ID, BlobDigest: ref.BlobDigest, SizeBytes: ref.SizeBytes}, nil
}

func (a *visionStoreAdapter) Unlink(ctx context.Context, refID string) error {
	if a == nil || a.inner == nil {
		return errors.New("vision store is not configured")
	}
	return a.inner.Unlink(ctx, refID)
}
