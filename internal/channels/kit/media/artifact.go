package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/storage"
)

type ArtifactPutter interface {
	Put(ctx context.Context, r io.Reader, meta *storage.ArtifactMetadata) (storage.ArtifactRef, error)
}

type ArtifactStore struct {
	store ArtifactPutter
}

func NewArtifactStore(store ArtifactPutter) (*ArtifactStore, error) {
	if store == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "artifact store must not be nil")
	}
	return &ArtifactStore{store: store}, nil
}

func (s *ArtifactStore) Put(ctx context.Context, content io.Reader, meta *Meta) (Receipt, error) {
	if s == nil || s.store == nil || meta == nil {
		return Receipt{}, fault.New(ErrorCodeInvalidArgument, "artifact store and metadata must not be nil")
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Receipt{}, err
	}
	ref, err := s.store.Put(ctx, content, &storage.ArtifactMetadata{
		ID:        "art-" + hex.EncodeToString(raw[:]),
		SessionID: meta.SessionID,
		Filename:  meta.Filename,
		MediaType: meta.MediaType,
		Metadata:  meta.Extra,
	})
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{RefID: ref.ID, Digest: ref.BlobDigest, SizeBytes: ref.SizeBytes}, nil
}
