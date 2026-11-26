package storage

import (
	"context"
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument           ErrorCode = "invalid_argument"
	ErrorCodeSessionNotFound           ErrorCode = "session_not_found"
	ErrorCodeEventNotFound             ErrorCode = "event_not_found"
	ErrorCodeArtifactQuotaExceeded     ErrorCode = "artifact_quota_exceeded"
	ErrorCodeArtifactBlobMissing       ErrorCode = "artifact_blob_missing"
	ErrorCodeBackupDestinationConflict ErrorCode = "backup_destination_conflict"
	ErrorCodeBackupInvalid             ErrorCode = "backup_invalid"
	ErrorCodeRestoreLocked             ErrorCode = "restore_locked"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func errNilArgument(name string) error {
	return fault.New(ErrorCodeInvalidArgument, name+" must not be nil")
}

func classify(err error) error {
	return sqlite.Classify(err)
}

func isConstraintForeignKey(err error) bool {
	return fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintForeignKey)
}

func blobExists(ctx context.Context, q sqlite.Querier, digest string) bool {
	var one int
	return q.QueryRowContext(ctx, `SELECT 1 FROM blob WHERE digest = ?`, digest).Scan(&one) == nil
}

func classifyArtifactFK(ctx context.Context, q sqlite.Querier, digest, sessionID, eventID string) error {
	if !blobExists(ctx, q, digest) {
		return fault.New(ErrorCodeArtifactBlobMissing, fmt.Sprintf("blob %s does not exist", digest))
	}
	if !turnstore.SessionExists(ctx, q, sessionID) {
		return fault.New(ErrorCodeSessionNotFound, fmt.Sprintf("session %s does not exist", sessionID))
	}
	if eventID != "" && !turnstore.EventExists(ctx, q, eventID) {
		return fault.New(ErrorCodeEventNotFound, fmt.Sprintf("event %s does not exist", eventID))
	}
	return nil
}
