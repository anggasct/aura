package sqlite

type ErrorCode string

const (
	ErrorCodeInvalidArgument           ErrorCode = "invalid_argument"
	ErrorCodeUnavailable               ErrorCode = "storage_unavailable"
	ErrorCodeMigrationChecksumMismatch ErrorCode = "migration_checksum_mismatch"
	ErrorCodeMigrationSequenceInvalid  ErrorCode = "migration_sequence_invalid"
)
