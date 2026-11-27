package sqlite

type ErrorCode string

const (
	ErrorCodeInvalidArgument           ErrorCode = "invalid_argument"
	ErrorCodeUnavailable               ErrorCode = "storage_unavailable"
	ErrorCodeBusy                      ErrorCode = "storage_busy"
	ErrorCodeConstraintUnique          ErrorCode = "storage_constraint_unique"
	ErrorCodeConstraintPrimaryKey      ErrorCode = "storage_constraint_primary_key"
	ErrorCodeConstraintForeignKey      ErrorCode = "storage_constraint_foreign_key"
	ErrorCodeConstraintCheck           ErrorCode = "storage_constraint_check"
	ErrorCodeConstraintNotNull         ErrorCode = "storage_constraint_not_null"
	ErrorCodeConstraint                ErrorCode = "storage_constraint"
	ErrorCodeCorrupt                   ErrorCode = "storage_corrupt"
	ErrorCodeFailed                    ErrorCode = "storage_failed"
	ErrorCodeMigrationChecksumMismatch ErrorCode = "migration_checksum_mismatch"
	ErrorCodeMigrationSequenceInvalid  ErrorCode = "migration_sequence_invalid"
	ErrorCodeSchemaIncompatible        ErrorCode = "schema_incompatible"
)
