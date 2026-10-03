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
	"github.com/anggasct/aura/internal/telemetry"
	"github.com/anggasct/aura/internal/vision"
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

type visionStoreAdapter struct {
	inner store.ArtifactStore
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
	ref, err := a.inner.Put(ctx, r, &store.ArtifactMetadata{
		ID:        meta.ID,
		SessionID: meta.SessionID,
		EventID:   meta.EventID,
		Filename:  meta.Filename,
		MediaType: meta.MediaType,
		Metadata:  meta.Metadata,
	})
	if err != nil {
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

func newVisionService(cfg *config.Config, db *sql.DB, artifactRoot string) *vision.Service {
	if cfg == nil || cfg.Vision == nil || !cfg.Vision.Enabled {
		return nil
	}
	if db == nil || strings.TrimSpace(artifactRoot) == "" {
		return nil
	}
	limits := vision.LimitsFromConfig(cfg.Vision)
	stores := &visionStoreAdapter{inner: store.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota))}
	opts := []vision.Option{vision.WithStore(stores)}
	if recorder, err := telemetry.NewVisionRecorder(nil); err == nil && recorder != nil {
		opts = append(opts, vision.WithObserver(visionRecorderObserver(recorder)))
	}
	svc, err := vision.NewService(&limits, opts...)
	if err != nil {
		return nil
	}
	return svc
}

func visionRecorderObserver(recorder *telemetry.VisionRecorder) vision.Observer {
	if recorder == nil {
		return nil
	}
	return func(ctx context.Context, observation *vision.Observation) {
		if observation == nil {
			return
		}
		recorder.Record(ctx, &telemetry.VisionObservation{
			Operation: observation.Operation,
			Result:    observation.Result,
			MIME:      observation.MIME,
			Images:    observation.Images,
			Version:   observation.Version,
			Protocol:  observation.Protocol,
			Duration:  observation.Duration,
		})
	}
}
