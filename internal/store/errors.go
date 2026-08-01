package store

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeEventSequenceConflict     ErrorCode = "event_sequence_conflict"
	ErrorCodeMigrationChecksumMismatch ErrorCode = "migration_checksum_mismatch"
	ErrorCodeStorageBusy               ErrorCode = "storage_busy"
	ErrorCodeArtifactQuotaExceeded     ErrorCode = "artifact_quota_exceeded"
	ErrorCodeArtifactBlobMissing       ErrorCode = "artifact_blob_missing"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}
