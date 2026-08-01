package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"
)

// DefaultArtifactQuotaBytes is the MVP default artifact storage quota (5 GiB).
const DefaultArtifactQuotaBytes int64 = 5 << 30

type ArtifactMetadata struct {
	ID        string
	SessionID string
	EventID   string
	Filename  string
	MediaType string
	Metadata  json.RawMessage
}

type ArtifactRef struct {
	ID         string
	BlobDigest string
	SessionID  string
	EventID    string
	Filename   string
	MediaType  string
	SizeBytes  int64
	Metadata   json.RawMessage
	CreatedAt  time.Time
}

type ArtifactLink struct {
	ID         string
	BlobDigest string
	SessionID  string
	EventID    string
	Filename   string
	Metadata   json.RawMessage
}

type ArtifactStore interface {
	Put(context.Context, io.Reader, ArtifactMetadata) (ArtifactRef, error)
	Open(context.Context, string) (io.ReadCloser, ArtifactMetadata, error)
	Link(context.Context, ArtifactLink) error
	Unlink(context.Context, string) error
}

type sqliteArtifactStore struct {
	db         *sql.DB
	root       string
	quotaBytes int64
}

// NewArtifactStore returns a content-addressed ArtifactStore rooted at root.
// Blob bytes are deduplicated by digest; each Put/Link call creates its own
// artifact_ref ownership record, so sharing a blob across sessions never
// duplicates bytes or loses per-session ownership.
func NewArtifactStore(db *sql.DB, root string, quotaBytes int64) ArtifactStore {
	return &sqliteArtifactStore{db: db, root: root, quotaBytes: quotaBytes}
}

func (s *sqliteArtifactStore) Put(ctx context.Context, r io.Reader, meta ArtifactMetadata) (ArtifactRef, error) {
	tmpDir := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return ArtifactRef{}, fmt.Errorf("create artifact temp dir: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "blob-*")
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("create artifact temp file: %w", err)
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		_ = tmp.Close()
		return ArtifactRef{}, fmt.Errorf("write artifact content: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return ArtifactRef{}, fmt.Errorf("fsync artifact content: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return ArtifactRef{}, fmt.Errorf("close artifact temp file: %w", err)
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	relPath := blobRelativePath(digest)
	absPath := filepath.Join(s.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return ArtifactRef{}, fmt.Errorf("create blob directory: %w", err)
	}
	// Atomic rename: the file only becomes visible at its final,
	// content-addressed path once fully written and fsynced.
	if err := os.Rename(tmpPath, absPath); err != nil {
		return ArtifactRef{}, fmt.Errorf("rename artifact into place: %w", err)
	}
	removeTmp = false

	createdAt := time.Now().UTC()
	ref := ArtifactRef{
		ID:         meta.ID,
		BlobDigest: digest,
		SessionID:  meta.SessionID,
		EventID:    meta.EventID,
		Filename:   meta.Filename,
		MediaType:  meta.MediaType,
		SizeBytes:  size,
		Metadata:   meta.Metadata,
		CreatedAt:  createdAt,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("begin put artifact %s: %w", meta.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingMediaType string
	err = tx.QueryRowContext(ctx, `SELECT media_type FROM blob WHERE digest = ?`, digest).Scan(&existingMediaType)
	switch {
	case err == nil:
		// Bytes already stored under this digest; reuse the recorded media type
		// and skip quota accounting since no new space is consumed.
		ref.MediaType = existingMediaType
	case errors.Is(err, sql.ErrNoRows):
		var total sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT SUM(size_bytes) FROM blob`).Scan(&total); err != nil {
			return ArtifactRef{}, fmt.Errorf("read blob storage total: %w", err)
		}
		if total.Int64+size > s.quotaBytes {
			_ = os.Remove(absPath)
			return ArtifactRef{}, &Error{
				Code:   ErrorCodeArtifactQuotaExceeded,
				Detail: fmt.Sprintf("blob %s would use %d bytes, exceeding quota of %d", digest, total.Int64+size, s.quotaBytes),
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO blob (digest, size_bytes, media_type, relative_path, created_at) VALUES (?, ?, ?, ?, ?)`,
			digest, size, meta.MediaType, relPath, formatTime(createdAt),
		); err != nil {
			return ArtifactRef{}, fmt.Errorf("insert blob %s: %w", digest, err)
		}
	default:
		return ArtifactRef{}, fmt.Errorf("read blob %s: %w", digest, err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO artifact_ref (id, blob_digest, session_id, event_id, filename, metadata_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		meta.ID, digest, meta.SessionID, nullableString(meta.EventID), meta.Filename, jsonOrEmptyObject(meta.Metadata), formatTime(createdAt),
	); err != nil {
		return ArtifactRef{}, fmt.Errorf("insert artifact ref %s: %w", meta.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return ArtifactRef{}, fmt.Errorf("commit put artifact %s: %w", meta.ID, err)
	}
	return ref, nil
}

func (s *sqliteArtifactStore) Open(ctx context.Context, refID string) (io.ReadCloser, ArtifactMetadata, error) {
	var meta ArtifactMetadata
	var digest, metadataJSON, mediaType, relPath string
	var eventID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT ar.blob_digest, ar.session_id, ar.event_id, ar.filename, ar.metadata_json, b.media_type, b.relative_path
		FROM artifact_ref ar
		JOIN blob b ON b.digest = ar.blob_digest
		WHERE ar.id = ?`, refID,
	).Scan(&digest, &meta.SessionID, &eventID, &meta.Filename, &metadataJSON, &mediaType, &relPath)
	if err != nil {
		return nil, ArtifactMetadata{}, fmt.Errorf("open artifact %s: %w", refID, err)
	}
	meta.ID = refID
	meta.MediaType = mediaType
	meta.Metadata = json.RawMessage(metadataJSON)
	if eventID.Valid {
		meta.EventID = eventID.String
	}

	absPath := filepath.Join(s.root, filepath.FromSlash(relPath))
	f, err := os.Open(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ArtifactMetadata{}, &Error{
				Code:   ErrorCodeArtifactBlobMissing,
				Detail: fmt.Sprintf("blob %s file missing at %s", digest, relPath),
			}
		}
		return nil, ArtifactMetadata{}, fmt.Errorf("open blob file for artifact %s: %w", refID, err)
	}
	return f, meta, nil
}

func (s *sqliteArtifactStore) Link(ctx context.Context, link ArtifactLink) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO artifact_ref (id, blob_digest, session_id, event_id, filename, metadata_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		link.ID, link.BlobDigest, link.SessionID, nullableString(link.EventID), link.Filename, jsonOrEmptyObject(link.Metadata), formatTime(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("link artifact %s to blob %s: %w", link.ID, link.BlobDigest, err)
	}
	return nil
}

func (s *sqliteArtifactStore) Unlink(ctx context.Context, refID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM artifact_ref WHERE id = ?`, refID); err != nil {
		return fmt.Errorf("unlink artifact %s: %w", refID, err)
	}
	return nil
}

func blobRelativePath(digest string) string {
	return path.Join("blobs", digest[:2], digest)
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func jsonOrEmptyObject(raw json.RawMessage) string {
	if raw == nil {
		return "{}"
	}
	return string(raw)
}
