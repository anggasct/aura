package store

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeEventSequenceConflict     ErrorCode = "event_sequence_conflict"
	ErrorCodeEventSequenceInvalid      ErrorCode = "event_sequence_invalid"
	ErrorCodeEventSchemaVersionInvalid ErrorCode = "event_schema_version_invalid"
	ErrorCodeEventPayloadInvalid       ErrorCode = "event_payload_invalid"
	ErrorCodeInvalidArgument           ErrorCode = "invalid_argument"
	ErrorCodeSessionIDConflict         ErrorCode = "session_id_conflict"
	ErrorCodeMigrationChecksumMismatch ErrorCode = "migration_checksum_mismatch"
	ErrorCodeMigrationSequenceInvalid  ErrorCode = "migration_sequence_invalid"
	ErrorCodeArtifactQuotaExceeded     ErrorCode = "artifact_quota_exceeded"
	ErrorCodeArtifactBlobMissing       ErrorCode = "artifact_blob_missing"
	ErrorCodeBackupDestinationConflict ErrorCode = "backup_destination_conflict"
	ErrorCodeBackupInvalid             ErrorCode = "backup_invalid"
	ErrorCodeRestoreLocked             ErrorCode = "restore_locked"
	ErrorCodeStorageBusy               ErrorCode = "storage_busy"
	ErrorCodeStorageUnavailable        ErrorCode = "storage_unavailable"
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

func errNilArgument(name string) error {
	return &Error{Code: ErrorCodeInvalidArgument, Detail: name + " must not be nil"}
}
