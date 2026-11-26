package store

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument    ErrorCode = "invalid_argument"
	ErrorCodeStorageUnavailable ErrorCode = "storage_unavailable"
	ErrorCodeChildConflict      ErrorCode = "child_conflict"
	ErrorCodeChildNotFound      ErrorCode = "child_not_found"
	ErrorCodeChildInvalid       ErrorCode = "child_invalid"
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

func isConstraintUnique(err error) bool {
	classified := sqlite.Classify(err)
	return fault.HasCode(classified, sqlite.ErrorCodeConstraintUnique) ||
		fault.HasCode(classified, sqlite.ErrorCodeConstraintPrimaryKey)
}
