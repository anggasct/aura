package principal

import (
	"context"
	"database/sql"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeAPIKeyConflict  ErrorCode = "apikey_conflict"
	ErrorCodeAPIKeyNotFound  ErrorCode = "apikey_not_found"
	ErrorCodeAPIKeyInvalid   ErrorCode = "apikey_invalid"
	ErrorCodeKeyMalformed    ErrorCode = "key_malformed"
	ErrorCodeKeyGeneration   ErrorCode = "key_generation_failed"
	ErrorCodeErasureBlocked  ErrorCode = "erasure_blocked_by_live_runs"
)

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

func isConstraintCheck(err error) bool {
	return fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintCheck)
}

type rowScanner interface {
	Scan(dest ...any) error
}

type rowChecker interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func rowExists(ctx context.Context, q rowChecker, query string, args ...any) bool {
	var one int
	return q.QueryRowContext(ctx, query, args...).Scan(&one) == nil
}
