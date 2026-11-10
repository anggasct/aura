package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"

	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/store"
)

type channelMediaStore struct {
	store store.ArtifactStore
}

func (s *channelMediaStore) Put(ctx context.Context, content io.Reader, meta *media.Meta) (media.Receipt, error) {
	if s.store == nil || meta == nil {
		return media.Receipt{}, nil
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return media.Receipt{}, err
	}
	ref, err := s.store.Put(ctx, content, &store.ArtifactMetadata{
		ID:        "art-" + hex.EncodeToString(raw[:]),
		SessionID: meta.SessionID,
		Filename:  meta.Filename,
		MediaType: meta.MediaType,
		Metadata:  meta.Extra,
	})
	if err != nil {
		return media.Receipt{}, err
	}
	return media.Receipt{RefID: ref.ID, Digest: ref.BlobDigest, SizeBytes: ref.SizeBytes}, nil
}
