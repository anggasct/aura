package codeexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"

	"github.com/anggasct/aura/internal/store"
)

type recordedPut struct {
	meta    ArtifactMeta
	content []byte
}

type memorySink struct {
	mu    sync.Mutex
	puts  []recordedPut
	err   error
	quota int64
	total int64
}

func (s *memorySink) Put(ctx context.Context, content io.Reader, meta *ArtifactMeta) (ArtifactReceipt, error) {
	if err := ctx.Err(); err != nil {
		return ArtifactReceipt{}, err
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return ArtifactReceipt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return ArtifactReceipt{}, s.err
	}
	if s.quota > 0 && s.total+int64(len(data)) > s.quota {
		return ArtifactReceipt{}, Errorf(ErrorCodeArtifactQuota, "the artifact quota is exhausted")
	}
	s.total += int64(len(data))
	s.puts = append(s.puts, recordedPut{meta: *meta, content: data})
	sum := sha256.Sum256(data)
	return ArtifactReceipt{ID: meta.ID, Digest: hex.EncodeToString(sum[:]), SizeBytes: int64(len(data))}, nil
}

func (s *memorySink) recorded() []recordedPut {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedPut(nil), s.puts...)
}

type storeSink struct {
	store store.ArtifactStore
}

func (s storeSink) Put(ctx context.Context, content io.Reader, meta *ArtifactMeta) (ArtifactReceipt, error) {
	ref, err := s.store.Put(ctx, content, &store.ArtifactMetadata{
		ID: meta.ID, SessionID: meta.SessionID, Filename: meta.Filename, MediaType: meta.MediaType, Metadata: meta.Metadata,
	})
	if err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeArtifactQuotaExceeded {
			return ArtifactReceipt{}, Errorf(ErrorCodeArtifactQuota, "the artifact quota is exhausted")
		}
		return ArtifactReceipt{}, err
	}
	return ArtifactReceipt{ID: ref.ID, Digest: ref.BlobDigest, SizeBytes: ref.SizeBytes}, nil
}
