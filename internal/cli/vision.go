package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/model"
	"github.com/anggasct/aura/internal/store"
)

type visionBlobReader struct {
	blobs store.ArtifactStore
}

var _ model.VisionBlobReader = (*visionBlobReader)(nil)

func (r *visionBlobReader) ReadBlob(ctx context.Context, refID string, maxBytes int64) ([]byte, error) {
	if r == nil || r.blobs == nil {
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
	stream, _, err := r.blobs.Open(ctx, refID)
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

func visionWiringForConfig(cfg *config.Config, db *sql.DB, artifactRoot string) model.VisionWiring {
	if cfg == nil || db == nil {
		return model.VisionWiring{}
	}
	policy := model.NewVisionPolicy(cfg.Vision)
	if policy == nil {
		return model.VisionWiring{}
	}
	if strings.TrimSpace(artifactRoot) == "" {
		return model.VisionWiring{}
	}
	return model.VisionWiring{
		Policy: policy,
		Blobs:  &visionBlobReader{blobs: store.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota))},
	}
}
