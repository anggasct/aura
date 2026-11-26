package turnstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument           ErrorCode = "invalid_argument"
	ErrorCodeSessionIDConflict         ErrorCode = "session_id_conflict"
	ErrorCodeSessionNotFound           ErrorCode = "session_not_found"
	ErrorCodeEventNotFound             ErrorCode = "event_not_found"
	ErrorCodeEventSequenceConflict     ErrorCode = "event_sequence_conflict"
	ErrorCodeEventSequenceInvalid      ErrorCode = "event_sequence_invalid"
	ErrorCodeEventSchemaVersionInvalid ErrorCode = "event_schema_version_invalid"
	ErrorCodeEventPayloadInvalid       ErrorCode = "event_payload_invalid"
)

func errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func errNilArgument(name string) error {
	return fault.New(ErrorCodeInvalidArgument, name+" must not be nil")
}

func classify(err error) error {
	return sqlite.Classify(err)
}

func isConstraintUnique(err error) bool {
	classified := sqlite.Classify(err)
	return fault.HasCode(classified, sqlite.ErrorCodeConstraintUnique) ||
		fault.HasCode(classified, sqlite.ErrorCodeConstraintPrimaryKey)
}

func isConstraintForeignKey(err error) bool {
	return fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintForeignKey)
}

func isConstraintCheck(err error) bool {
	return fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintCheck)
}

type rowChecker interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func rowExists(ctx context.Context, q rowChecker, query string, args ...any) bool {
	var one int
	return q.QueryRowContext(ctx, query, args...).Scan(&one) == nil
}
